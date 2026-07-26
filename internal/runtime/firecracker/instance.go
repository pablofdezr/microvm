//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"time"

	"github.com/pablofdezr/microvm/internal/cgroup"
	"github.com/pablofdezr/microvm/internal/guestclient"
	"github.com/pablofdezr/microvm/internal/netpool"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
)

// instance is one running microVM and everything the host allocated for it.
type instance struct {
	// id is the caller's logical sandbox ID, used for logging and lookup.
	id string
	// jailID names the chroot and cgroup. It is generated rather than derived
	// from id, to keep the caller's input out of the vsock socket's path length.
	jailID  string
	runtime *Runtime
	log     *slog.Logger

	// startedNano is when the wall-clock meter began, in Unix nanoseconds. It is
	// atomic because AdoptMeter moves it while Stats may be sampling, and because
	// pool residency must not be charged to the tenant who is handed the VM.
	startedNano atomic.Int64

	// rootfs is the host path to the image this VM booted from. Kept because a
	// snapshot references its block devices by path rather than containing them, so
	// a template has to record which file it was captured against.
	rootfs string

	cmd     *exec.Cmd
	logFile *os.File
	logPath string

	// exited is closed by the single goroutine that owns cmd.Wait. Nothing else
	// may wait on the process: two waiters race for the exit status and the
	// loser gets ECHILD, the same trap that made PID 1's reaper steal exec exit
	// codes inside the guest.
	//
	// It means "the process we started returned", which is NOT "the VM is gone".
	// --new-pid-ns makes the jailer fork, because the init of a new PID namespace
	// must be a child, so this closes ~30ms in on a perfectly healthy VM. Reading
	// it as death broke every snapshot restore once. The VMM's liveness signal is
	// the cgroup below: it holds the process the jailer left behind.
	exited chan struct{}

	client  *guestclient.Client
	udsPath string
	// apiPath is the host path to Firecracker's control API socket, set only when
	// snapshots are enabled. Empty means the VM booted --no-api and cannot be
	// paused or snapshotted.
	apiPath string

	// hostListener accepts connections the guest opens to the host. It lives in
	// this sandbox's jail and is reachable by this sandbox alone.
	hostListener net.Listener

	group *cgroup.Group
	lease *netpool.Lease

	// tapName is the host interface backing the guest's NIC, empty for a sandbox
	// with no network. It duplicates lease.TapName on purpose: Stats reads it
	// while stop clears the lease, and metering must not race teardown for the
	// one number that cannot be re-derived afterwards.
	tapName string

	// timings is this VM's own create, phase by phase, and timingsOK reports
	// whether it was ever filled in. Both are written once by setup, before the
	// instance is published to any caller, and only read afterwards -- which is
	// what makes them safe without a lock. A VM that arrived by snapshot restore
	// leaves timingsOK false: the boot those numbers would describe happened
	// earlier and for nobody in particular.
	timings   runtime.Timings
	timingsOK bool

	stopOnce sync.Once
	stopErr  error
}

func (i *instance) ID() string { return i.id }

// CreateTimings implements runtime.TimedCreate.
func (i *instance) CreateTimings() (runtime.Timings, bool) { return i.timings, i.timingsOK }

func (i *instance) Client() runtime.GuestClient { return i.client }

// startedAt is when this sandbox's meters began.
func (i *instance) startedAt() time.Time { return time.Unix(0, i.startedNano.Load()) }

// AdoptMeter restarts the wall-clock meter, implementing runtime.MeterAdopter.
//
// The warm pool calls it as a VM is handed out. Without it a VM that waited ten
// minutes in the pool reported ten minutes of Wall and ten of Idle against its
// first tenant -- a bill for the host's own readiness, and one that contradicted
// the sandbox's created_at. The CPU meter is the cgroup's and needs no adjustment:
// a pristine pooled VM has burned effectively none, and what it burned coming up
// is the host's too but is not worth clearing the kernel's counter over.
func (i *instance) AdoptMeter() { i.startedNano.Store(time.Now().UnixNano()) }

// HostListener returns this sandbox's private inbound socket.
//
// It is nil if the listener could not be opened, which is not fatal: a sandbox
// without one simply has no storage. Returning nil rather than failing the boot
// is deliberate -- most sandboxes never touch storage, and refusing to start
// them over a facility they will not use trades a working sandbox for nothing.
func (i *instance) HostListener() net.Listener { return i.hostListener }

// Stats samples the sandbox's meters.
//
// Idle is derived rather than measured: wall-clock minus CPU actually consumed.
// A sandbox that sat waiting on a network call for a minute shows a minute of
// wall and almost no active CPU, which is exactly the distinction that makes
// usage-based billing possible.
//
// The network counters are sampled here, next to the cgroup, because both meters
// die with the VM: stop deletes the TAP and /sys/class/net/<tap> goes with it,
// exactly as the cgroup does. That is why the layer above samples once before the
// kill -- reading either afterwards yields nothing, and nothing reported as zero
// tells a caller their transfer was free.
func (i *instance) Stats() (runtime.Stats, error) {
	s, err := i.group.Stats()
	if err != nil {
		return runtime.Stats{}, fmt.Errorf("read cgroup stats for %s: %w", i.id, err)
	}

	wall := time.Since(i.startedAt())
	idle := wall - s.ActiveCPU
	// With more than one vCPU, active CPU can legitimately exceed wall-clock:
	// two cores busy for a second is two seconds of CPU. Idle is meaningless
	// then, and a negative number would be worse than none.
	if idle < 0 {
		idle = 0
	}

	out := runtime.Stats{
		ActiveCPU:     s.ActiveCPU,
		Wall:          wall,
		Idle:          idle,
		MemoryCurrent: s.MemoryCurrent,
		MemoryPeak:    s.MemoryPeak,
	}
	i.sampleNetwork(&out)
	return out, nil
}

// sampleNetwork fills in the transfer counters from the sandbox's TAP device.
//
// The counters are the host's view of that device, so they arrive the other way
// round: what the host received off the TAP is what the guest sent. runtime.Stats
// is named from the guest's side, which is why the reading goes through
// Counters.Guest rather than being copied across field by field -- rx to rx would
// report a sandbox's egress as its ingress, invisible until an abuse complaint
// blames the wrong direction.
//
// A failure leaves both fields nil rather than failing the sample, for the same
// reason cgroup.Stats tolerates a missing memory file: a broken meter must not
// take the billable CPU number down with it.
func (i *instance) sampleNetwork(out *runtime.Stats) {
	if i.tapName == "" {
		return // network: false -- there is no device, so there is nothing to count
	}

	c, ok, err := netpool.ReadCounters(i.tapName)
	if err != nil {
		i.log.Warn("read tap counters failed", "tap", i.tapName, "err", err)
		return
	}
	if !ok {
		return
	}

	rx, tx := c.Guest()
	out.NetworkRxBytes = &rx
	out.NetworkTxBytes = &tx
}

// Stop shuts the sandbox down and releases everything it held.
//
// Ordering matters and is deliberate: kill the VMM first so nothing is running
// while its network is dismantled, then release host resources. Every step runs
// even if an earlier one failed -- a partial teardown that leaks a TAP device
// or a network slot is worse than a loud error, because the leak is permanent
// and the error is not.
func (i *instance) Stop(ctx context.Context) error {
	i.stopOnce.Do(func() {
		i.stopErr = i.stop(ctx)
	})
	return i.stopErr
}

func (i *instance) stop(ctx context.Context) error {
	var errs []error

	if err := i.killVMM(ctx); err != nil {
		errs = append(errs, fmt.Errorf("kill vmm: %w", err))
	}

	// After the VMM is dead, so nothing is mid-request when the socket goes.
	// Closing it also unblocks whoever is serving on it: an Accept loop above
	// this layer ends when the listener does, which is how the storage server
	// for this sandbox learns the sandbox is gone.
	if i.hostListener != nil {
		if err := i.hostListener.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close host listener: %w", err))
		}
		i.hostListener = nil
	}

	if i.lease != nil {
		// The device's byte counters go with it, so the final transfer must
		// already have been sampled: the layer above calls Stats before Stop, the
		// same ordering the cgroup needs.
		if err := i.runtime.taps.Delete(i.lease.TapName); err != nil {
			errs = append(errs, fmt.Errorf("delete tap: %w", err))
		}
		// Release the slot only after the device is gone: handing the address
		// to a new sandbox while the old device lingers would collide.
		i.runtime.pool.Release(*i.lease)
		i.lease = nil
	}

	// The cgroup can only be removed once no process remains in it, which the
	// VMM kill above guarantees.
	if err := i.group.Delete(); err != nil {
		errs = append(errs, err)
	}

	if err := os.RemoveAll(i.runtime.jailRoot(i.jailID)); err != nil {
		errs = append(errs, fmt.Errorf("remove jail: %w", err))
	}

	i.runtime.mu.Lock()
	delete(i.runtime.insts, i.id)
	i.runtime.mu.Unlock()

	i.log.Debug("sandbox stopped")
	return errors.Join(errs...)
}

// killVMM terminates the sandbox's VMM and everything else it spawned.
//
// The kill goes through the cgroup rather than the process handle. Because the
// jailer clones into a new PID namespace, the pid we started is not the VMM
// that ends up running -- killing it leaves Firecracker alive, holding the TAP
// device and the cgroup, and the teardown then fails with EBUSY on a cgroup
// that is very much still in use. The cgroup names every process the sandbox
// has, by the kernel's own bookkeeping, so it is the only handle that cannot
// be wrong.
//
// SIGKILL, not a graceful shutdown: there is nothing inside worth saving. The
// guest's filesystem is a tmpfs about to be discarded, and offering hostile
// code a shutdown hook is just offering it a way to outlive its own stop.
func (i *instance) killVMM(ctx context.Context) error {
	if i.cmd == nil || i.cmd.Process == nil {
		return nil // never started
	}

	if !i.group.Exists() {
		// The jailer never got as far as creating it, so there is nothing to
		// kill and nothing to wait for.
		return nil
	}

	if err := i.group.Kill(); err != nil {
		// Fall back to the process handle. It is the weaker option, for the
		// reasons above, but better than leaving the VMM running.
		i.log.Warn("cgroup.kill failed, falling back to signalling the pid", "err", err)
		if perr := i.cmd.Process.Kill(); perr != nil && !errors.Is(perr, os.ErrProcessDone) {
			return errors.Join(err, perr)
		}
	}

	// Wait for the cgroup to drain before the caller removes it and the TAP.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := i.group.WaitEmpty(waitCtx); err != nil {
		return fmt.Errorf("vmm did not die: %w", err)
	}
	return nil
}

// waitReady blocks until the agent answers, the VM dies, or time runs out.
//
// Noticing a dead VM matters: Firecracker rejects a bad config in milliseconds,
// and waiting the full boot timeout for a process that is already gone reports
// "slow" about something that was never coming.
//
// But the death is detected through the cgroup, not through the process we
// launched. The jailer clones and its parent returns immediately, so our pid
// exits within milliseconds of a perfectly healthy start -- watching it would
// declare every sandbox dead before Firecracker had written its first log line.
// The cgroup is the kernel's own record of what is running, and it cannot be
// wrong about it.
// It returns the health response that ended the wait, because the guest states
// its own kernel and init timings on it and the boot path is the only place they
// can be collected.
func (i *instance) waitReadyHealth(ctx context.Context) (protocol.HealthResponse, error) {
	return i.waitReadyWithin(ctx, bootTimeout)
}

// waitReady is waitReadyHealth for the callers with no use for the breakdown.
func (i *instance) waitReady(ctx context.Context) error {
	_, err := i.waitReadyWithin(ctx, bootTimeout)
	return err
}

// readyResult is what the readiness poller reports back: the health response
// that satisfied it, or the error that ended it.
type readyResult struct {
	health protocol.HealthResponse
	err    error
}

// waitReadyWithin is waitReadyHealth with an explicit deadline. The restore path
// uses a shorter one, for the reasons measured at restoreReadyTimeout.
func (i *instance) waitReadyWithin(ctx context.Context, timeout time.Duration) (protocol.HealthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ready := make(chan readyResult, 1)
	go func() {
		health, err := i.client.WaitReadyHealth(ctx)
		ready <- readyResult{health: health, err: err}
	}()

	ticker := time.NewTicker(livenessInterval)
	defer ticker.Stop()

	// The cgroup is empty for the moment between the jailer starting and it
	// placing the VMM, so emptiness only means death once we have seen it hold
	// something. Until then, an empty cgroup is just a VM that has not started
	// yet.
	var seenPopulated bool

	for {
		select {
		case res := <-ready:
			return res.health, res.err

		case <-ctx.Done():
			// WaitReady is watching the same context, so it is about to return --
			// with the last dial failure attached, which is the only part of this
			// worth reading. Racing it to the return statement threw that away and
			// reported a bare deadline instead, so give it a moment to answer.
			select {
			case res := <-ready:
				return res.health, res.err
			case <-time.After(readyDrainGrace):
				return protocol.HealthResponse{}, fmt.Errorf("sandbox did not answer within %v: %w", timeout, ctx.Err())
			}

		case <-ticker.C:
			populated, err := i.group.Populated()
			if err != nil {
				// The jailer has not created the cgroup yet.
				continue
			}
			if populated {
				seenPopulated = true
				continue
			}
			if seenPopulated {
				return protocol.HealthResponse{}, errors.New("the VM exited before the sandbox was ready")
			}
		}
	}
}

// livenessInterval is how often the VM is checked for having died during boot.
const livenessInterval = 100 * time.Millisecond

// readyDrainGrace is how long the deadline path waits for the readiness poller's
// own error before falling back to reporting the bare timeout. It only ever
// covers one in-flight dial that has already been cancelled.
const readyDrainGrace = 250 * time.Millisecond

// ConsoleLog returns the guest's serial output, which is the only record of a
// VM that failed before its agent came up.
func (i *instance) ConsoleLog() ([]byte, error) {
	return os.ReadFile(i.logPath)
}

// consoleTailLines is how much console to attach to a startup error. Enough to
// carry a panic and its trace; not so much that a boot log buries the error.
const consoleTailLines = 25

// consoleTail returns the end of the guest console, for embedding in an error.
func (i *instance) consoleTail() string {
	raw, err := i.ConsoleLog()
	if err != nil {
		return fmt.Sprintf("(console unavailable: %v)", err)
	}
	if len(raw) == 0 {
		return "(console empty: the VMM produced no output at all)"
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > consoleTailLines {
		lines = lines[len(lines)-consoleTailLines:]
	}
	return strings.Join(lines, "\n")
}
