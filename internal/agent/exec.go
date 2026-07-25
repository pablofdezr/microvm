package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// chunkSize bounds how much output one frame carries. Smaller chunks mean more
// JSON overhead; larger ones add latency to interactive output.
const chunkSize = 32 * 1024

var (
	errNotFound = errors.New("exec not found")
	// errNotTTY is a resize aimed at an exec that has no pty, and errTTYUnsupported
	// a tty exec on a platform with no ptys at all. They are kept apart because they
	// mean different things: the first is the caller resizing the wrong exec, the
	// second is the guest image being run somewhere it cannot serve a terminal.
	// errTTYUnsupported is used only in the non-Linux build (see pty_other.go); it
	// lives here so both builds share one definition.
	errNotTTY         = errors.New("exec has no tty to resize")
	errTTYUnsupported = errors.New("tty is not supported on this platform")
)

// process is a command running inside the guest, tracked so that later signal,
// stdin and resize calls can find it.
type process struct {
	id  string
	cmd *exec.Cmd

	// pgid is the process group of the command. Signals go to the whole group
	// so that a shell's children die with it. Both the plain and the tty path put
	// the child in its own group whose id equals its pid -- Setpgid for one,
	// Setsid for the other -- so -pgid reaches every descendant either way.
	pgid int

	// stdin is where the caller's input goes: the command's stdin pipe for a plain
	// exec, the pty master for a tty one. An io.Writer, not a WriteCloser, because
	// the two ends are closed differently -- see closeStdin.
	stdin io.Writer

	// pty is the pseudo-terminal a tty exec runs against, or nil for a plain one.
	// It owns the master and slave and is what resize acts on.
	pty *ptyPair

	// done is closed once the process has been reaped.
	done chan struct{}

	// timedOut records that the agent, not the caller, killed the process.
	mu       sync.Mutex
	timedOut bool
}

// closeStdin ends the caller's input to the process.
//
// For a plain exec it closes the stdin pipe, so a program reading to EOF proceeds.
// A pty has no half-close -- closing the master hangs the session up and takes the
// output with it -- so for a tty exec this is a no-op, and the caller ends input
// in band (Ctrl-D) or by cancelling the exec.
func (p *process) closeStdin() error {
	if p.pty != nil {
		return nil
	}
	if c, ok := p.stdin.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// resize changes a tty exec's window, or reports errNotTTY for a plain one.
func (p *process) resize(rows, cols uint16) error {
	if p.pty == nil {
		return errNotTTY
	}
	return p.pty.resize(rows, cols)
}

func (p *process) markTimedOut() {
	p.mu.Lock()
	p.timedOut = true
	p.mu.Unlock()
}

func (p *process) didTimeOut() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.timedOut
}

// signal delivers sig to the process group.
func (p *process) signal(sig syscall.Signal) error {
	// A negative pid targets the whole process group. If the leader already
	// exited but children linger, this still reaches them.
	if err := syscall.Kill(-p.pgid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // already gone; treat as success
		}
		return err
	}
	return nil
}

// registry tracks running processes by exec ID.
type registry struct {
	mu sync.Mutex
	m  map[string]*process
}

func newRegistry() *registry {
	return &registry{m: make(map[string]*process)}
}

func (r *registry) add(p *process) {
	r.mu.Lock()
	r.m[p.id] = p
	r.mu.Unlock()
}

func (r *registry) get(id string) (*process, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[id]
	return p, ok
}

func (r *registry) remove(id string) {
	r.mu.Lock()
	delete(r.m, id)
	r.mu.Unlock()
}

// run starts the requested command and streams its output as frames until the
// process exits. It returns only once the process has been reaped.
//
// ctx cancellation (the host hanging up the HTTP request) kills the process
// group: an aborted stream must not leave orphans burning CPU in the guest.
//
// A tty request runs the command against a pseudo-terminal instead of pipes; the
// two paths diverge only in how the process is wired up and rejoin at drive,
// which owns the frame loop, the watchdog and the terminal frame.
func (a *Agent) run(ctx context.Context, req protocol.ExecRequest, emit func(protocol.Frame) error) error {
	cmd := exec.Command(req.Cmd, req.Args...)
	cmd.Dir = a.workdir
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = buildEnv(req.Env)

	if req.TTY {
		return a.runTTY(ctx, cmd, req, emit)
	}
	return a.runPlain(ctx, cmd, req, emit)
}

// runPlain wires the command to pipes, keeping stdout and stderr as separate
// streams, and drives it to completion.
func (a *Agent) runPlain(ctx context.Context, cmd *exec.Cmd, req protocol.ExecRequest, emit func(protocol.Frame) error) error {
	// Setpgid puts the child in a new process group whose ID equals its PID, so
	// that killing -PID reaches every descendant it spawns.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", req.Cmd, err)
	}

	p := &process{
		id:    req.ID,
		cmd:   cmd,
		pgid:  cmd.Process.Pid, // equals the pgid thanks to Setpgid
		stdin: stdin,
		done:  make(chan struct{}),
	}
	a.execs.add(p)
	defer a.execs.remove(req.ID)
	defer close(p.done)

	// frames carries output from both pipe readers to the single emitting
	// goroutine below, since emit is not safe for concurrent use.
	frames := make(chan protocol.Frame, 16)
	var pumps sync.WaitGroup
	pumps.Add(2)
	go pump(&pumps, stdout, protocol.FrameStdout, frames)
	go pump(&pumps, stderr, protocol.FrameStderr, frames)
	go func() {
		pumps.Wait()
		close(frames)
	}()

	return a.drive(ctx, req, p, frames, emit)
}

// runTTY allocates a pseudo-terminal, runs the command against it as its
// controlling terminal, and drives it to completion. stderr is merged into
// stdout because a terminal has one output stream, so every frame is a stdout
// frame.
func (a *Agent) runTTY(ctx context.Context, cmd *exec.Cmd, req protocol.ExecRequest, emit func(protocol.Frame) error) error {
	pty, err := openPTY(req.Rows, req.Cols)
	if err != nil {
		return fmt.Errorf("allocate tty: %w", err)
	}

	// The slave is the process's stdin, stdout and stderr. Setsid makes the child a
	// session leader -- with a process group whose ID is its PID, so -PID still
	// reaches the whole group -- and Setctty makes the slave (fd 0) its controlling
	// terminal, which is what lets it receive SIGWINCH and job-control signals.
	cmd.Stdin = pty.slaveFile()
	cmd.Stdout = pty.slaveFile()
	cmd.Stderr = pty.slaveFile()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if err := cmd.Start(); err != nil {
		pty.close()
		return fmt.Errorf("start %s: %w", req.Cmd, err)
	}
	// The parent keeps only the master. Dropping the slave is also what makes a
	// read of the master return EIO once the child exits, which is how the pump
	// learns the process is gone -- a pty master never reports a clean EOF.
	pty.closeSlave()

	p := &process{
		id:    req.ID,
		cmd:   cmd,
		pgid:  cmd.Process.Pid, // equals the pgid thanks to Setsid
		stdin: pty.masterFile(),
		pty:   pty,
		done:  make(chan struct{}),
	}
	a.execs.add(p)
	defer a.execs.remove(req.ID)
	defer close(p.done)
	defer pty.close()

	frames := make(chan protocol.Frame, 16)
	var pumps sync.WaitGroup
	pumps.Add(1)
	// pump treats the master's EIO-on-exit as the end of the stream, the same way
	// it treats a pipe's EOF: any read error ends the pump without an error frame.
	go pump(&pumps, pty.masterFile(), protocol.FrameStdout, frames)
	go func() {
		pumps.Wait()
		close(frames)
	}()

	return a.drive(ctx, req, p, frames, emit)
}

// drive runs a started process to completion: it emits FrameStarted, feeds any
// initial stdin, streams frames to the caller, enforces the timeout and abort,
// and emits the terminal frame. It is the half runPlain and runTTY share.
func (a *Agent) drive(ctx context.Context, req protocol.ExecRequest, p *process, frames <-chan protocol.Frame, emit func(protocol.Frame) error) error {
	if err := emit(protocol.Frame{Type: protocol.FrameStarted, PID: p.cmd.Process.Pid}); err != nil {
		_ = p.signal(syscall.SIGKILL)
		_ = p.cmd.Wait()
		return err
	}

	if len(req.Stdin) > 0 {
		// Write in the background: a large payload can block until the process
		// drains it, and the process may never read at all. closeStdin is the
		// EOF a plain exec needs and the no-op a tty one wants.
		go func() {
			_, _ = p.stdin.Write(req.Stdin)
			_ = p.closeStdin()
		}()
	}

	// Watchdogs: either the caller aborts or the deadline passes. Both escalate
	// to the process group rather than the leader alone.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go a.watch(ctx, p, req.Timeout, watchDone)

	var emitErr error
	for f := range frames {
		if emitErr != nil {
			continue // drain so the pumps can finish and the streams close
		}
		if err := emit(f); err != nil {
			emitErr = err
			// The host is gone. Kill the process so we stop producing output
			// nobody will read, then keep draining until the streams close.
			_ = p.signal(syscall.SIGKILL)
		}
	}

	waitErr := p.cmd.Wait()
	if emitErr != nil {
		return emitErr
	}

	return emit(exitFrame(waitErr, p.didTimeOut()))
}

// watch kills p when the caller aborts or the timeout elapses. It returns when
// done is closed, which run does once the process has been reaped.
func (a *Agent) watch(ctx context.Context, p *process, timeout time.Duration, done <-chan struct{}) {
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}

	select {
	case <-done:
		return
	case <-ctx.Done():
		// Caller aborted. SIGKILL rather than SIGTERM: an abort is a hard stop,
		// and a wedged process must not be able to ignore it.
		_ = p.signal(syscall.SIGKILL)
	case <-deadline:
		p.markTimedOut()
		// Give the process a chance to flush output and clean up, then insist.
		_ = p.signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(gracePeriod):
			_ = p.signal(syscall.SIGKILL)
		}
	}
}

// gracePeriod is how long a timed-out process has to handle SIGTERM before it
// is killed outright.
const gracePeriod = 2 * time.Second

// pump reads r until EOF, emitting one frame per chunk.
func pump(wg *sync.WaitGroup, r io.Reader, typ protocol.FrameType, out chan<- protocol.Frame) {
	defer wg.Done()
	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			// Copy: buf is reused on the next iteration and the frame outlives
			// this loop by sitting in the channel.
			data := make([]byte, n)
			copy(data, buf[:n])
			out <- protocol.Frame{Type: typ, Data: data}
		}
		if err != nil {
			return // includes io.EOF and the "file already closed" race on kill
		}
	}
}

// exitFrame converts the result of cmd.Wait into the terminal frame.
func exitFrame(waitErr error, timedOut bool) protocol.Frame {
	f := protocol.Frame{Type: protocol.FrameExit, TimedOut: timedOut}

	if waitErr == nil {
		code := 0
		f.ExitCode = &code
		return f
	}

	var ee *exec.ExitError
	if !errors.As(waitErr, &ee) {
		// Not an exit status: the agent failed to reap the process at all.
		return protocol.Frame{Type: protocol.FrameError, Message: waitErr.Error()}
	}

	status, ok := ee.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		sig := status.Signal()
		f.Signal = sig.String()
		// Mirror the shell convention so callers have a single number to check.
		code := 128 + int(sig)
		f.ExitCode = &code
		return f
	}

	code := ee.ExitCode()
	f.ExitCode = &code
	return f
}

// buildEnv merges the request's variables over the guest's base environment.
func buildEnv(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return os.Environ()
	}

	base := os.Environ()
	env := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]bool, len(overrides))

	for _, kv := range base {
		name, _, ok := splitEnv(kv)
		if ok && hasKey(overrides, name) {
			continue // replaced below
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		if seen[k] {
			continue
		}
		seen[k] = true
		env = append(env, k+"="+v)
	}
	return env
}

func splitEnv(kv string) (name, value string, ok bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return kv, "", false
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

// signalByName maps the names accepted on the wire to signals. Only signals
// that make sense to deliver to a sandboxed process are listed; anything else
// is rejected rather than silently coerced.
var signalByName = map[string]syscall.Signal{
	"SIGHUP":  syscall.SIGHUP,
	"SIGINT":  syscall.SIGINT,
	"SIGQUIT": syscall.SIGQUIT,
	"SIGKILL": syscall.SIGKILL,
	"SIGTERM": syscall.SIGTERM,
	"SIGUSR1": syscall.SIGUSR1,
	"SIGUSR2": syscall.SIGUSR2,
}

func parseSignal(name string) (syscall.Signal, error) {
	sig, ok := signalByName[name]
	if !ok {
		return 0, fmt.Errorf("unsupported signal %q", name)
	}
	return sig, nil
}
