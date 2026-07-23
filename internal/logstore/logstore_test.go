package logstore

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/protocol"
)

func TestRingKeepsEverythingWhenItFits(t *testing.T) {
	r := newRingBuffer(100)
	r.Write([]byte("hello "))
	r.Write([]byte("world"))

	got, truncated := r.Bytes()
	if string(got) != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
	if truncated {
		t.Error("reported truncation despite fitting")
	}
}

// When output overflows, the *tail* must survive: a crash's traceback is at the
// end, and keeping the head would preserve the banner and lose the error.
func TestRingKeepsTailNotHead(t *testing.T) {
	r := newRingBuffer(10)
	r.Write([]byte("0123456789"))
	r.Write([]byte("ABCDE"))

	got, truncated := r.Bytes()
	if string(got) != "56789ABCDE" {
		t.Errorf("got %q, want %q (the last 10 bytes)", got, "56789ABCDE")
	}
	if !truncated {
		t.Error("dropped bytes without reporting truncation")
	}
}

func TestRingSingleWriteLargerThanBuffer(t *testing.T) {
	r := newRingBuffer(5)
	r.Write([]byte("abcdefghij"))

	got, truncated := r.Bytes()
	if string(got) != "fghij" {
		t.Errorf("got %q, want %q", got, "fghij")
	}
	if !truncated {
		t.Error("truncation not reported")
	}
}

// The write that exactly fills the buffer is the boundary between the growing
// path and the wrapping one.
func TestRingExactFill(t *testing.T) {
	r := newRingBuffer(5)
	r.Write([]byte("abcde"))

	got, truncated := r.Bytes()
	if string(got) != "abcde" {
		t.Errorf("got %q, want %q", got, "abcde")
	}
	if truncated {
		t.Error("reported truncation on an exact fit")
	}

	// One more byte must now wrap.
	r.Write([]byte("f"))
	got, truncated = r.Bytes()
	if string(got) != "bcdef" {
		t.Errorf("after wrap got %q, want %q", got, "bcdef")
	}
	if !truncated {
		t.Error("truncation not reported after wrap")
	}
}

// A write that straddles the fill boundary exercises both paths at once.
func TestRingWriteStraddlingTheFill(t *testing.T) {
	r := newRingBuffer(8)
	r.Write([]byte("abcde"))  // 5 of 8
	r.Write([]byte("XYZ123")) // 3 fit, 3 must wrap

	got, _ := r.Bytes()
	if string(got) != "deXYZ123" {
		t.Errorf("got %q, want %q", got, "deXYZ123")
	}
}

func TestRingManySmallWrites(t *testing.T) {
	r := newRingBuffer(10)
	for i := 0; i < 100; i++ {
		r.Write([]byte("x"))
	}
	r.Write([]byte("END"))

	got, _ := r.Bytes()
	if !bytes.HasSuffix(got, []byte("END")) {
		t.Errorf("got %q, want it to end with END", got)
	}
	if len(got) != 10 {
		t.Errorf("len = %d, want 10", len(got))
	}
}

func TestStoreRecordsExecOutcome(t *testing.T) {
	s := New(Config{})
	s.Begin("e1", "sb1", "sh", []string{"-c", "echo hi"})

	s.Append("e1", protocol.Frame{Type: protocol.FrameStdout, Data: []byte("hi\n")})
	s.Append("e1", protocol.Frame{Type: protocol.FrameStderr, Data: []byte("warn\n")})
	code := 3
	s.Append("e1", protocol.Frame{Type: protocol.FrameExit, ExitCode: &code})

	rec, ok := s.Get("e1")
	if !ok {
		t.Fatal("record not found")
	}
	if string(rec.Stdout) != "hi\n" {
		t.Errorf("stdout = %q", rec.Stdout)
	}
	if string(rec.Stderr) != "warn\n" {
		t.Errorf("stderr = %q", rec.Stderr)
	}
	if rec.Status != StatusExited {
		t.Errorf("status = %s, want %s", rec.Status, StatusExited)
	}
	if rec.ExitCode == nil || *rec.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", rec.ExitCode)
	}
	if rec.FinishedAt.IsZero() {
		t.Error("FinishedAt not set")
	}
}

func TestStoreDistinguishesTimeoutFromExit(t *testing.T) {
	s := New(Config{})
	s.Begin("e1", "sb1", "sleep", []string{"30"})

	code := 143
	s.Append("e1", protocol.Frame{
		Type: protocol.FrameExit, ExitCode: &code, Signal: "terminated", TimedOut: true,
	})

	rec, _ := s.Get("e1")
	// "Timed out" and "exited 143" are different facts for a caller deciding
	// whether their code is broken or just slow.
	if rec.Status != StatusTimedOut {
		t.Errorf("status = %s, want %s", rec.Status, StatusTimedOut)
	}
}

// The case this package exists for: the sandbox dies and the output must still
// be retrievable, with an honest account of why it stopped.
func TestOutputSurvivesSandboxDeath(t *testing.T) {
	s := New(Config{})
	s.Begin("e1", "sb1", "sh", []string{"-c", "long thing"})
	s.Append("e1", protocol.Frame{Type: protocol.FrameStdout, Data: []byte("got this far\n")})

	// No exit frame ever arrives: the VM was killed underneath it.
	s.SandboxGone("sb1")

	rec, ok := s.Get("e1")
	if !ok {
		t.Fatal("record lost when the sandbox died: the output is gone exactly when it was needed")
	}
	if string(rec.Stdout) != "got this far\n" {
		t.Errorf("stdout = %q, want the output from before the sandbox died", rec.Stdout)
	}
	if rec.Status != StatusVanished {
		t.Errorf("status = %s, want %s: a caller polling this would wait forever",
			rec.Status, StatusVanished)
	}
}

// An exec that reported its own exit must not be relabelled when its sandbox is
// later torn down: the agent's account beats our inference.
func TestSandboxGoneDoesNotOverwriteFinishedExecs(t *testing.T) {
	s := New(Config{})
	s.Begin("e1", "sb1", "echo", nil)
	code := 0
	s.Append("e1", protocol.Frame{Type: protocol.FrameExit, ExitCode: &code})

	s.SandboxGone("sb1")

	rec, _ := s.Get("e1")
	if rec.Status != StatusExited {
		t.Errorf("status = %s, want %s: a completed exec was relabelled", rec.Status, StatusExited)
	}
}

func TestStoreCapsOutput(t *testing.T) {
	s := New(Config{MaxBytes: 100})
	s.Begin("e1", "sb1", "yes", nil)

	// A one-line memory bomb aimed at the daemon rather than the guest.
	for i := 0; i < 1000; i++ {
		s.Append("e1", protocol.Frame{Type: protocol.FrameStdout, Data: []byte(strings.Repeat("y", 100))})
	}

	rec, _ := s.Get("e1")
	if len(rec.Stdout) > 100 {
		t.Errorf("retained %d bytes against a 100 byte cap", len(rec.Stdout))
	}
	if !rec.StdoutTruncated {
		t.Error("truncation not reported: the log looks complete but is not")
	}
}

func TestListSandbox(t *testing.T) {
	s := New(Config{})
	s.Begin("e1", "sb1", "echo", nil)
	s.Begin("e2", "sb1", "ls", nil)
	s.Begin("e3", "sb2", "pwd", nil)

	got := s.ListSandbox("sb1")
	if len(got) != 2 {
		t.Fatalf("got %d records for sb1, want 2", len(got))
	}
	if got[0].ID != "e1" || got[1].ID != "e2" {
		t.Errorf("order = %s,%s; want e1,e2 (oldest first)", got[0].ID, got[1].ID)
	}
}

func TestSweepDropsOldButKeepsRunning(t *testing.T) {
	s := New(Config{Retention: 50 * time.Millisecond})

	s.Begin("done", "sb1", "echo", nil)
	code := 0
	s.Append("done", protocol.Frame{Type: protocol.FrameExit, ExitCode: &code})

	s.Begin("running", "sb1", "sleep", nil) // never finishes

	time.Sleep(80 * time.Millisecond)

	if dropped := s.Sweep(); dropped != 1 {
		t.Errorf("dropped %d records, want 1", dropped)
	}
	if _, ok := s.Get("done"); ok {
		t.Error("finished record outlived its retention")
	}
	// A running exec still has a result coming, however old it is.
	if _, ok := s.Get("running"); !ok {
		t.Error("swept a still-running exec: its result would be unreachable")
	}
}

func TestConcurrentAppendIsSafe(t *testing.T) {
	s := New(Config{MaxBytes: 1 << 16})
	s.Begin("e1", "sb1", "sh", nil)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.Append("e1", protocol.Frame{Type: protocol.FrameStdout, Data: []byte("data")})
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.Get("e1")
			}
		}()
	}
	wg.Wait()
}

func TestAppendToUnknownExecIsHarmless(t *testing.T) {
	s := New(Config{})
	// A late frame for a swept exec must not panic the daemon.
	s.Append("ghost", protocol.Frame{Type: protocol.FrameStdout, Data: []byte("x")})
	s.Finish("ghost", StatusVanished)

	if _, ok := s.Get("ghost"); ok {
		t.Error("Get returned a record that was never begun")
	}
}
