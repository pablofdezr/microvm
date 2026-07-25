//go:build linux

package agent

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ptyPair is an allocated pseudo-terminal: the master the host reads output from
// and writes input to, and the slave the process runs against as its stdin,
// stdout, stderr and controlling terminal.
//
// A pty is allocated by hand rather than with a helper library because the whole
// system takes no dependency it can implement itself, and this is four ioctls.
// The sequence is the POSIX one: open the multiplexer, unlock the slave, ask
// which slave it minted, open that.
type ptyPair struct {
	master *os.File
	slave  *os.File
}

// openPTY allocates a pseudo-terminal, sized rows x cols when either is non-zero.
func openPTY(rows, cols uint16) (*ptyPair, error) {
	// O_NOCTTY so opening the master does not make it this process's controlling
	// terminal -- the agent is PID 1 and has no business acquiring one. O_CLOEXEC
	// so the master does not leak into the child across the exec; the child gets
	// the slave and must never hold the master, or it could read its own input.
	masterFd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	// Unlock the slave. TIOCSPTLCK with a zero lock value is unlockpt(3); without
	// it the slave cannot be opened.
	if err := unix.IoctlSetPointerInt(masterFd, unix.TIOCSPTLCK, 0); err != nil {
		unix.Close(masterFd)
		return nil, fmt.Errorf("unlock pty: %w", err)
	}

	// Which slave this master is paired with. ptsname(3) by another name.
	n, err := unix.IoctlGetInt(masterFd, unix.TIOCGPTN)
	if err != nil {
		unix.Close(masterFd)
		return nil, fmt.Errorf("get pty number: %w", err)
	}
	slaveName := fmt.Sprintf("/dev/pts/%d", n)

	slaveFd, err := unix.Open(slaveName, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(masterFd)
		return nil, fmt.Errorf("open %s: %w", slaveName, err)
	}

	p := &ptyPair{
		master: os.NewFile(uintptr(masterFd), "pty-master"),
		slave:  os.NewFile(uintptr(slaveFd), slaveName),
	}
	if rows > 0 || cols > 0 {
		// Best effort: a pty that could not be sized still works, and the caller
		// can resize it later. A hard failure here would waste a working pty over a
		// cosmetic ioctl.
		_ = p.resize(rows, cols)
	}
	return p, nil
}

// slaveFile is the file the process runs against. exec dups it onto fds 0, 1 and
// 2 and, with Setctty, makes it the controlling terminal.
func (p *ptyPair) slaveFile() *os.File { return p.slave }

// masterFile is the host's end: output is read from it and input is written to
// it.
func (p *ptyPair) masterFile() *os.File { return p.master }

// closeSlave drops the parent's handle on the slave once the child has it. The
// child holds its own copy, so this leaves the pty working -- and it is what
// makes a read of the master return EIO when the child exits, which is how the
// output pump learns the process is gone.
func (p *ptyPair) closeSlave() {
	if p.slave != nil {
		_ = p.slave.Close()
		p.slave = nil
	}
}

// close releases both ends. Idempotent: closeSlave may already have taken the
// slave.
func (p *ptyPair) close() {
	p.closeSlave()
	if p.master != nil {
		_ = p.master.Close()
		p.master = nil
	}
}

// resize sets the pty window, which makes the kernel raise SIGWINCH in the
// foreground process group.
func (p *ptyPair) resize(rows, cols uint16) error {
	if p.master == nil {
		return errNotTTY
	}
	return unix.IoctlSetWinsize(int(p.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: rows,
		Col: cols,
	})
}
