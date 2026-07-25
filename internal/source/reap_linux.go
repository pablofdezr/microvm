package source

import (
	"os/exec"
	"syscall"
)

// reapWithProcess ties git's whole process tree to this one.
//
// Two settings, for two ways the download outlives what asked for it:
//
//   - Pdeathsig, as internal/runtime/firecracker does for the jailer. git is
//     started by a daemon that may be stopped mid-clone, and exec.CommandContext
//     only kills on a cancellation -- at process exit there is nobody left to
//     cancel, so a SIGINT or a bare restart during a clone leaves git running.
//   - Its own process group, killed as a group. git does not speak HTTP itself:
//     git-remote-https does, and that grandchild is the one holding the socket. Kill
//     git alone and the download carries on, which makes both the deadline and the
//     size cap advisory -- they cancel a context, and the process that would notice
//     is not the process doing the work.
func reapWithProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	cmd.Cancel = func() error {
		// Negative pid: the group, which is git and every helper it started. The
		// pid is the group's leader because of Setpgid above.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
