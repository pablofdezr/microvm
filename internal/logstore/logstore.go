// Package logstore keeps the output of each exec on the host.
//
// It lives here rather than inside the guest for one reason: the moment a
// caller most needs the output is when the sandbox was killed -- by a timeout,
// by its TTL, by the OOM killer. Output buffered inside the guest dies with it,
// exactly then. Buffering on the host means a caller can always ask what
// happened, even to a VM that no longer exists.
package logstore

import (
	"sync"
	"time"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// DefaultMaxBytes caps how much output one exec may retain.
//
// A sandbox runs untrusted code, and `yes > /dev/stdout` is a one-line memory
// bomb aimed at the daemon rather than at the guest. The cap turns that into
// truncated logs instead of a dead host.
const DefaultMaxBytes = 1 << 20 // 1 MiB per stream

// Status is where an exec got to.
type Status string

const (
	// StatusRunning means the process is still going.
	StatusRunning Status = "running"
	// StatusExited means it finished on its own, successfully or not.
	StatusExited Status = "exited"
	// StatusTimedOut means the agent killed it for exceeding its timeout.
	StatusTimedOut Status = "timed_out"
	// StatusAborted means the caller cancelled it.
	StatusAborted Status = "aborted"
	// StatusVanished means the sandbox died underneath it -- TTL, OOM, crash.
	// The distinction matters: it is the difference between "your code failed"
	// and "we took your VM away".
	StatusVanished Status = "vanished"
	// StatusFailed means the agent could never start the process: the binary
	// was missing, or the working directory did not exist. The caller's command
	// was wrong, as opposed to their code being wrong.
	StatusFailed Status = "failed"
)

// Record is everything known about one exec.
type Record struct {
	ID        string
	SandboxID string

	Cmd  string
	Args []string

	Status   Status
	ExitCode *int
	Signal   string
	// Error is why the process could never start, set with StatusFailed.
	Error string

	StartedAt  time.Time
	FinishedAt time.Time

	Stdout []byte
	Stderr []byte

	// StdoutTruncated is set when output exceeded the cap and the oldest bytes
	// were dropped. Callers must be told: silently truncated logs are worse
	// than none, because they look complete.
	StdoutTruncated bool
	StderrTruncated bool
}

// Duration is how long the exec ran. For a running exec it is time so far.
func (r *Record) Duration() time.Duration {
	if r.FinishedAt.IsZero() {
		return time.Since(r.StartedAt)
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// entry is a record plus the buffers that are still filling.
type entry struct {
	mu     sync.Mutex
	rec    Record
	stdout *ringBuffer
	stderr *ringBuffer
	// subs are the live streams watching this exec. See Subscribe.
	subs []*subscriber
}

// Store holds exec records in memory.
type Store struct {
	maxBytes int
	// retention is how long a finished exec's output is kept once its sandbox
	// is gone.
	retention time.Duration

	mu      sync.RWMutex
	entries map[string]*entry
	// bySandbox lets a sandbox's whole history be fetched or dropped at once.
	bySandbox map[string][]string
}

// Config configures a Store.
type Config struct {
	// MaxBytes caps each stream of each exec. Zero uses DefaultMaxBytes.
	MaxBytes int
	// Retention is how long records outlive their sandbox. Zero means forever,
	// which is only sensible in tests.
	Retention time.Duration
}

// New returns a Store.
func New(cfg Config) *Store {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	return &Store{
		maxBytes:  cfg.MaxBytes,
		retention: cfg.Retention,
		entries:   make(map[string]*entry),
		bySandbox: make(map[string][]string),
	}
}

// Begin registers an exec that is about to start.
func (s *Store) Begin(execID, sandboxID, cmd string, args []string) {
	e := &entry{
		rec: Record{
			ID:        execID,
			SandboxID: sandboxID,
			Cmd:       cmd,
			Args:      args,
			Status:    StatusRunning,
			StartedAt: time.Now(),
		},
		stdout: newRingBuffer(s.maxBytes),
		stderr: newRingBuffer(s.maxBytes),
	}

	s.mu.Lock()
	s.entries[execID] = e
	s.bySandbox[sandboxID] = append(s.bySandbox[sandboxID], execID)
	s.mu.Unlock()
}

// Append records a frame. It is called from the exec's streaming path, so it
// must stay cheap: a slow store would show up as latency in the caller's own
// output stream.
func (s *Store) Append(execID string, f protocol.Frame) {
	e := s.entry(execID)
	if e == nil {
		return // never began, or already dropped; nothing to attach output to
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	switch f.Type {
	case protocol.FrameStdout:
		e.stdout.Write(f.Data)
	case protocol.FrameStderr:
		e.stderr.Write(f.Data)
	case protocol.FrameExit:
		e.rec.ExitCode = f.ExitCode
		e.rec.Signal = f.Signal
		e.rec.Status = StatusExited
		if f.TimedOut {
			e.rec.Status = StatusTimedOut
		}
		e.rec.FinishedAt = time.Now()

	case protocol.FrameError:
		// The agent could not run the command at all -- a missing binary, a bad
		// working directory. This is terminal just as much as an exit is, and
		// without handling it the record stays "running" forever: a caller
		// polling for a result waits on a process that was never born.
		e.rec.Status = StatusFailed
		e.rec.Error = f.Message
		e.rec.FinishedAt = time.Now()
	}

	// Live streams see the frame only after it is recorded. If a stream were
	// served first it could show a caller output that a crash one instant later
	// left absent from the record -- the stream and the stored result would
	// disagree, and the stored one is the one that has to be right.
	e.fanOutLocked(f)
	if isTerminal(f.Type) {
		e.closeSubsLocked()
	}
}

// isTerminal reports whether a frame ends an exec.
func isTerminal(t protocol.FrameType) bool {
	return t == protocol.FrameExit || t == protocol.FrameError
}

// Finish marks an exec as ended with the given status, for the endings the
// agent cannot report itself: an abort, or a sandbox that vanished mid-exec.
func (s *Store) Finish(execID string, status Status) {
	e := s.entry(execID)
	if e == nil {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// An exec that already reported its own exit is not overridden: the agent's
	// account of how a process ended is better than the caller's inference.
	if e.rec.Status != StatusRunning {
		return
	}
	e.rec.Status = status
	e.rec.FinishedAt = time.Now()

	// Anyone watching has to be told why it stopped. These are exactly the
	// endings the process never got to report -- it was cancelled, or its VM was
	// taken away -- so without this the stream would simply go quiet, and a
	// caller cannot tell silence from a program that is merely thinking.
	e.fanOutLocked(protocol.Frame{Type: protocol.FrameError, Message: string(status)})
	e.closeSubsLocked()
}

// Get returns a snapshot of an exec's record.
func (s *Store) Get(execID string) (Record, bool) {
	e := s.entry(execID)
	if e == nil {
		return Record{}, false
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Copy: the caller must not see the buffers mutate under them, and the
	// record keeps filling if the exec is still running.
	rec := e.rec
	rec.Stdout, rec.StdoutTruncated = e.stdout.Bytes()
	rec.Stderr, rec.StderrTruncated = e.stderr.Bytes()
	return rec, true
}

// ListSandbox returns every exec belonging to a sandbox, oldest first.
func (s *Store) ListSandbox(sandboxID string) []Record {
	s.mu.RLock()
	ids := append([]string(nil), s.bySandbox[sandboxID]...)
	s.mu.RUnlock()

	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		if rec, ok := s.Get(id); ok {
			out = append(out, rec)
		}
	}
	return out
}

// SandboxGone marks every still-running exec of a sandbox as vanished.
//
// Called when a sandbox dies for a reason its execs never learn: a TTL, an OOM
// kill, a crash. Without this their records would say "running" forever, and a
// caller polling for a result would wait for one that can never arrive.
func (s *Store) SandboxGone(sandboxID string) {
	s.mu.RLock()
	ids := append([]string(nil), s.bySandbox[sandboxID]...)
	s.mu.RUnlock()

	for _, id := range ids {
		s.Finish(id, StatusVanished)
	}
}

// Sweep drops records whose retention has elapsed, returning how many went.
// Without it the store grows for the daemon's whole life.
func (s *Store) Sweep() int {
	if s.retention <= 0 {
		return 0
	}

	cutoff := time.Now().Add(-s.retention)

	s.mu.Lock()
	defer s.mu.Unlock()

	var dropped int
	for id, e := range s.entries {
		e.mu.Lock()
		finished := e.rec.FinishedAt
		sandboxID := e.rec.SandboxID
		e.mu.Unlock()

		// Never drop a running exec, however old: it still has a result coming.
		if finished.IsZero() || finished.After(cutoff) {
			continue
		}

		delete(s.entries, id)
		s.bySandbox[sandboxID] = removeString(s.bySandbox[sandboxID], id)
		if len(s.bySandbox[sandboxID]) == 0 {
			delete(s.bySandbox, sandboxID)
		}
		dropped++
	}
	return dropped
}

func (s *Store) entry(execID string) *entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entries[execID]
}

func removeString(list []string, want string) []string {
	out := list[:0]
	for _, s := range list {
		if s != want {
			out = append(out, s)
		}
	}
	return out
}
