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

	"time"

	"github.com/pablofdezr/microvm/internal/cgroup"
	"github.com/pablofdezr/microvm/internal/guestclient"
	"github.com/pablofdezr/microvm/internal/netpool"
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
	started time.Time

	cmd     *exec.Cmd
	logFile *os.File
	logPath string

	// exited is closed by the single goroutine that owns cmd.Wait. Nothing else
	// may wait on the process: two waiters race for the exit status and the
	// loser gets ECHILD, the same trap that made PID 1's reaper steal exec exit
	// codes inside the guest.
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

	stopOnce sync.Once
	stopErr  error
}

func (i *instance) ID() string { return i.id }

func (i *instance) Client() runtime.GuestClient { return i.client }

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
func (i *instance) Stats() (runtime.Stats, error) {
	s, err := i.group.Stats()
	if err != nil {
		return runtime.Stats{}, fmt.Errorf("read cgroup stats for %s: %w", i.id, err)
	}

	wall := time.Since(i.started)
	idle := wall - s.ActiveCPU
	// With more than one vCPU, active CPU can legitimately exceed wall-clock:
	// two cores busy for a second is two seconds of CPU. Idle is meaningless
	// then, and a negative number would be worse than none.
	if idle < 0 {
		idle = 0
	}

	return runtime.Stats{
		ActiveCPU:     s.ActiveCPU,
		Wall:          wall,
		Idle:          idle,
		MemoryCurrent: s.MemoryCurrent,
		MemoryPeak:    s.MemoryPeak,
	}, nil
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
func (i *instance) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()

	ready := make(chan error, 1)
	go func() { ready <- i.client.WaitReady(ctx) }()

	ticker := time.NewTicker(livenessInterval)
	defer ticker.Stop()

	// The cgroup is empty for the moment between the jailer starting and it
	// placing the VMM, so emptiness only means death once we have seen it hold
	// something. Until then, an empty cgroup is just a VM that has not started
	// yet.
	var seenPopulated bool

	for {
		select {
		case err := <-ready:
			return err

		case <-ctx.Done():
			return fmt.Errorf("sandbox did not answer within %v: %w", bootTimeout, ctx.Err())

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
				return errors.New("the VM exited before the sandbox was ready")
			}
		}
	}
}

// livenessInterval is how often the VM is checked for having died during boot.
const livenessInterval = 100 * time.Millisecond

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
