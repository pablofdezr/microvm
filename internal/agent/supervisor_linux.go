//go:build linux

package agent

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// RunInit is the PID 1 main loop. It never returns while the guest is healthy.
//
// PID 1 does two jobs and deliberately no more: it starts the supervisor, and
// it reaps orphans. It does *not* serve the API, because the two roles conflict
// at the wait4 level.
//
// The conflict is worth spelling out, because it is invisible until it bites.
// PID 1 must call wait4(-1) to reap processes the kernel reparents onto it when
// their real parent dies. But wait4(-1) reaps *any* child, including the ones
// os/exec is concurrently waiting on -- and whoever calls wait4 first consumes
// the exit status. The loser gets ECHILD and never learns how the process
// exited, so every exec would report "waitid: no child processes" instead of an
// exit code.
//
// Splitting the roles removes the conflict entirely rather than papering over it
// with locks: the supervisor's execs are its own children, waited on by os/exec
// with nothing else competing, while orphaned grandchildren reparent past the
// supervisor to PID 1, whose wait4(-1) has no os/exec children to steal from.
func RunInit(log *slog.Logger) error {
	supervisor, err := startSupervisor(log)
	if err != nil {
		return fmt.Errorf("start supervisor: %w", err)
	}
	log.Info("supervisor started", "pid", supervisor.Process.Pid)

	return reapUntilSupervisorExits(log, supervisor.Process.Pid)
}

// supervisorEnvKey marks the re-executed child so it takes the serving path
// instead of recursing into init duties.
const supervisorEnvKey = "MICROVM_ROLE"

// IsSupervisor reports whether this process is the re-executed child that
// serves the API, rather than PID 1.
func IsSupervisor() bool {
	return os.Getenv(supervisorEnvKey) == "supervisor"
}

// startSupervisor re-executes this same binary with the same arguments, marked
// so that it serves rather than initialising the guest again.
func startSupervisor(log *slog.Logger) (*exec.Cmd, error) {
	// /proc/self/exe rather than os.Args[0]: after pivot_root the path the
	// binary was originally invoked by may no longer resolve, but the kernel's
	// link to the running image always does.
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Env = append(os.Environ(), supervisorEnvKey+"=supervisor")

	// Inherit the console so the supervisor's logs reach the serial port, which
	// is the only way to debug a guest that never gets far enough to serve.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// reapUntilSupervisorExits blocks forever, reaping orphans. It returns only if
// the supervisor itself dies, which is unrecoverable: nothing would serve the
// API, so the caller is expected to bring the VM down rather than linger as a
// guest that boots but answers nothing.
//
// Note this deliberately does not call cmd.Wait on the supervisor: that would
// reintroduce the very wait4 race this split exists to avoid. The supervisor's
// exit status arrives through the same reaping loop as everything else.
func reapUntilSupervisorExits(log *slog.Logger, supervisorPID int) error {
	for {
		var status unix.WaitStatus
		// Blocking wait: there is nothing else for PID 1 to do, so there is no
		// reason to spin with WNOHANG.
		pid, err := unix.Wait4(-1, &status, 0, nil)
		switch {
		case err == unix.EINTR:
			continue // a signal interrupted the wait; nothing exited
		case err == unix.ECHILD:
			// Every child is gone, including the supervisor. Should be
			// unreachable, since the supervisor's death is caught below first.
			return fmt.Errorf("no children remain; supervisor is gone")
		case err != nil:
			return fmt.Errorf("wait4: %w", err)
		}

		if pid == supervisorPID {
			return fmt.Errorf("supervisor exited unexpectedly: %s", describeStatus(status))
		}
		// Anything else is an orphan the kernel reparented onto us. Reaping it
		// is the whole point: without this it would sit as a zombie holding a
		// PID until the guest ran out.
		log.Debug("reaped orphan", "pid", pid, "status", describeStatus(status))
	}
}

func describeStatus(status unix.WaitStatus) string {
	if status.Signaled() {
		return fmt.Sprintf("killed by %s", status.Signal())
	}
	return fmt.Sprintf("exit status %d", status.ExitStatus())
}
