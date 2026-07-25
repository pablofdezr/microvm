package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pablofdezr/microvm/internal/runtime"
)

// ErrSnapshotUnsupported is a suspend or resume on a node whose runtime cannot
// snapshot. It is configuration, not a fault: a node with no SnapshotDir set has
// no way to freeze a VM to disk, so the whole suspend/resume family is off there.
var ErrSnapshotUnsupported = errors.New("this node is not configured for suspend/resume (no snapshot support)")

// ErrSandboxBusy is a suspend refused because an execution is in flight.
//
// Snapshotting freezes the VM's memory, and a process caught mid-run is captured
// mid-syscall -- not corrupt, but resumed into a state its caller has no way to
// reason about, since the exec's stream ended when the VM was frozen. Refusing is
// the honest answer: finish or cancel the execution, then suspend.
var ErrSandboxBusy = errors.New("the sandbox has an execution in flight; suspend once it finishes")

// Suspend snapshots the sandbox's VM to disk and tears the VM down, leaving the
// sandbox resumable under the same id. It is the durable half of resume-after-stop:
// a suspended sandbox costs no CPU or memory, only the snapshot on disk, and Resume
// boots a fresh VM from it.
//
// The tenant slot and the name are kept, not released: a suspend that gave them up
// could find, on resume, that the cap is full or the name is taken -- so the one
// operation whose contract is "you'll get this back" could fail to. Holding them
// makes a resume of a suspended sandbox always possible.
func (s *Sandbox) Suspend(ctx context.Context) error {
	snap, ok := s.mgr.rt.(runtime.Snapshotter)
	if !ok {
		return ErrSnapshotUnsupported
	}

	s.mu.Lock()
	if s.state != StateRunning || s.stopping {
		st, reason := s.state, s.reason
		s.mu.Unlock()
		return fmt.Errorf("sandbox %s is %s (%s) and cannot be suspended", s.id, st, reason)
	}
	if s.running > 0 {
		s.mu.Unlock()
		return ErrSandboxBusy
	}
	// Claim the sandbox: stopping blocks Extend and any new exec, and it is set
	// before the supervisor is cancelled so nothing slips in during the teardown.
	s.stopping = true
	s.mu.Unlock()

	// Sample the meters while the VM is still alive, exactly as stop does: the
	// snapshot below stops the source VM, after which its cgroup is gone.
	final, statsErr := s.inst.Stats()

	// The supervisor enforces TTL and idle against a live VM; a suspended sandbox
	// has neither, so it is stood down. A resume starts a fresh one.
	s.cancelSupervisor()

	ref, err := snap.Snapshot(ctx, s.inst) // pauses, captures, and stops the source VM
	if err != nil {
		// The VM was left in an unknown state by a failed capture -- often already
		// gone. Funnel through Stop, which is idempotent, so the sandbox ends as a
		// clean failure rather than a half-suspended limbo, and the slot and name
		// are released exactly once.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
		_ = s.Stop(stopCtx, ReasonFailed)
		cancel()
		return fmt.Errorf("suspend %s: %w", s.id, err)
	}

	s.mu.Lock()
	// A racing TTL expiry could have claimed a stop between the checks above and
	// here. If it did, the sandbox is gone; discard the snapshot we just took
	// rather than resurrecting a stopped sandbox around it.
	if s.state != StateRunning {
		st := s.state
		s.mu.Unlock()
		_ = snap.Discard(context.WithoutCancel(ctx), ref)
		return fmt.Errorf("sandbox %s stopped during suspend (%s)", s.id, st)
	}
	s.state = StateSuspended
	s.reason = ReasonSuspended
	s.snapshotRef = &ref
	if statsErr == nil {
		s.finalStats = final
	}
	// Parked, not on its way out: clear the claim so the state alone describes it.
	s.stopping = false
	s.mu.Unlock()

	s.log.Info("sandbox suspended", "lifetime", time.Since(s.createdAt).Round(time.Second))
	return nil
}

// Resume boots a fresh VM from a suspended sandbox's snapshot and returns a
// running sandbox under the same id. The returned sandbox replaces the suspended
// one in the manager, so a caller who holds the old handle should use the new one.
//
// It reuses the slot and name the suspend kept, so it neither reserves nor can be
// refused for either. The old snapshot is discarded once the new VM is live: it
// has served its purpose, and the running VM will diverge from it immediately.
func (m *Manager) Resume(ctx context.Context, old *Sandbox) (*Sandbox, error) {
	snap, ok := m.rt.(runtime.Snapshotter)
	if !ok {
		return nil, ErrSnapshotUnsupported
	}

	old.mu.Lock()
	if old.state != StateSuspended || old.snapshotRef == nil {
		st := old.state
		old.mu.Unlock()
		return nil, fmt.Errorf("sandbox %s is %s, not suspended, so there is nothing to resume", old.id, st)
	}
	ref := *old.snapshotRef
	spec := old.spec
	old.mu.Unlock()

	inst, err := snap.Restore(ctx, spec.Spec, ref)
	if err != nil {
		return nil, fmt.Errorf("resume %s: %w", old.id, err)
	}

	// A fresh sandbox around the new VM, reusing the id, spec, slot and name. The
	// clock restarts: a resumed VM is a new VM, and its TTL is a fresh window.
	now := time.Now()
	sb := &Sandbox{
		id:         old.id,
		spec:       spec,
		inst:       inst,
		log:        m.log.With("sandbox", old.id),
		mgr:        m,
		createdAt:  now,
		expiresAt:  now.Add(spec.TTL),
		lastActive: now,
		state:      StateRunning,
		ttlNudge:   make(chan struct{}, 1),
		source:     old.source,
	}

	supervisorCtx, cancel := context.WithCancel(context.Background())
	sb.cancelSupervisor = cancel
	go sb.supervise(supervisorCtx)
	m.serveStorage(supervisorCtx, sb, inst)

	// Replace the suspended sandbox in the map and rebind its name, under one lock
	// so a lookup never sees the id resolve to the dead suspended object while the
	// name still points at it, or the reverse.
	m.mu.Lock()
	m.sandboxes[old.id] = sb
	if spec.Name != "" {
		m.named[nameKey{spec.Tenant, spec.Name}] = sb
	}
	m.mu.Unlock()

	// The snapshot has done its job. Discarded after the new VM is published, so a
	// discard failure cannot cost the resume -- the sandbox is already live.
	if err := snap.Discard(context.WithoutCancel(ctx), ref); err != nil {
		m.log.Warn("could not discard a resumed sandbox's snapshot", "sandbox", old.id, "err", err)
	}

	sb.log.Info("sandbox resumed")
	return sb, nil
}

// discardSnapshot reclaims a suspended sandbox's snapshot, if it has one. Called
// from stop so a sandbox deleted while suspended does not leave a full copy of a
// guest's RAM on disk with nothing able to open it again.
func (s *Sandbox) discardSnapshot(ctx context.Context) {
	s.mu.Lock()
	ref := s.snapshotRef
	s.snapshotRef = nil
	s.mu.Unlock()
	if ref == nil {
		return
	}
	if snap, ok := s.mgr.rt.(runtime.Snapshotter); ok {
		if err := snap.Discard(context.WithoutCancel(ctx), *ref); err != nil {
			s.log.Warn("could not discard a suspended sandbox's snapshot", "err", err)
		}
	}
}
