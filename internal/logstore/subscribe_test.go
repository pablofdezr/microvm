package logstore

import (
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/protocol"
)

func collect(t *testing.T, ch <-chan protocol.Frame, within time.Duration) []protocol.Frame {
	t.Helper()
	var got []protocol.Frame
	deadline := time.After(within)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, f)
		case <-deadline:
			t.Fatalf("stream did not close within %v; got %d frames so far", within, len(got))
			return got
		}
	}
}

func exit0() protocol.Frame {
	code := 0
	return protocol.Frame{Type: protocol.FrameExit, ExitCode: &code}
}

// The reason the stream endpoint is separate from the create endpoint: a caller
// who connects late still gets everything.
func TestSubscribeReplaysOutputProducedBeforeSubscribing(t *testing.T) {
	s := New(Config{})
	s.Begin("e1", "sb1", "python3", nil)
	s.Append("e1", protocol.Frame{Type: protocol.FrameStdout, Data: []byte("early output")})

	ch, ok := s.Subscribe("e1")
	if !ok {
		t.Fatal("Subscribe said there is no such exec")
	}

	s.Append("e1", protocol.Frame{Type: protocol.FrameStdout, Data: []byte("late output")})
	s.Append("e1", exit0())

	got := collect(t, ch, 2*time.Second)

	var text string
	for _, f := range got {
		if f.Type == protocol.FrameStdout {
			text += string(f.Data)
		}
	}
	if text != "early outputlate output" {
		t.Errorf("stream carried %q; a late subscriber lost the output that came before it", text)
	}
	if len(got) == 0 || got[len(got)-1].Type != protocol.FrameExit {
		t.Error("stream did not end with an exit frame")
	}
}

// Reconnecting to a finished exec must work: that is what makes a dropped
// connection recoverable rather than fatal.
func TestSubscribeToAFinishedExecReplaysAndCloses(t *testing.T) {
	s := New(Config{})
	s.Begin("e1", "sb1", "python3", nil)
	s.Append("e1", protocol.Frame{Type: protocol.FrameStdout, Data: []byte("all done")})
	s.Append("e1", exit0())

	ch, ok := s.Subscribe("e1")
	if !ok {
		t.Fatal("cannot subscribe to a finished exec")
	}
	got := collect(t, ch, 2*time.Second)

	if len(got) != 2 {
		t.Fatalf("got %d frames, want stdout + exit", len(got))
	}
	if string(got[0].Data) != "all done" {
		t.Errorf("replayed stdout = %q", got[0].Data)
	}
	if got[1].Type != protocol.FrameExit {
		t.Errorf("last frame is %s, want exit", got[1].Type)
	}
}

// A caller whose sandbox is taken away must be told, not left listening to
// silence. Silence and "the program is thinking" are indistinguishable.
func TestSubscribeIsToldWhenTheSandboxVanishes(t *testing.T) {
	s := New(Config{})
	s.Begin("e1", "sb1", "python3", nil)

	ch, _ := s.Subscribe("e1")
	s.SandboxGone("sb1")

	got := collect(t, ch, 2*time.Second)
	if len(got) == 0 {
		t.Fatal("the stream closed silently when the sandbox died")
	}
	last := got[len(got)-1]
	if last.Type != protocol.FrameError {
		t.Fatalf("last frame is %s, want an error explaining the sandbox went away", last.Type)
	}
	if last.Message != string(StatusVanished) {
		t.Errorf("message = %q, want %q", last.Message, StatusVanished)
	}
}

func TestSubscribeToAnUnknownExec(t *testing.T) {
	s := New(Config{})
	if _, ok := s.Subscribe("nope"); ok {
		t.Error("subscribed to an exec that does not exist")
	}
}

func TestTwoSubscribersBothGetEverything(t *testing.T) {
	s := New(Config{})
	s.Begin("e1", "sb1", "python3", nil)

	a, _ := s.Subscribe("e1")
	b, _ := s.Subscribe("e1")

	s.Append("e1", protocol.Frame{Type: protocol.FrameStdout, Data: []byte("shared")})
	s.Append("e1", exit0())

	for name, ch := range map[string]<-chan protocol.Frame{"a": a, "b": b} {
		got := collect(t, ch, 2*time.Second)
		if len(got) != 2 {
			t.Errorf("subscriber %s got %d frames, want 2", name, len(got))
		}
	}
}

// The property that protects the workload from its audience: a stream nobody is
// reading must not stall the program inside the sandbox.
func TestASlowSubscriberDoesNotBlockTheExec(t *testing.T) {
	s := New(Config{})
	s.Begin("e1", "sb1", "yes", nil)

	// Subscribe and then never read.
	if _, ok := s.Subscribe("e1"); !ok {
		t.Fatal("subscribe failed")
	}

	// Far more frames than the subscriber buffer holds. If Append blocked on a
	// subscriber, this would deadlock and the test would time out.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < subscriberBuffer*3; i++ {
			s.Append("e1", protocol.Frame{Type: protocol.FrameStdout, Data: []byte("x")})
		}
		s.Append("e1", exit0())
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a subscriber that stopped reading blocked the exec: one slow HTTP " +
			"client can stall the program inside the sandbox")
	}

	// And the output is still all in the record, which is the reason dropping
	// the stream is acceptable in the first place.
	rec, ok := s.Get("e1")
	if !ok {
		t.Fatal("no record")
	}
	if len(rec.Stdout) == 0 {
		t.Error("the record lost the output when the stream was dropped")
	}
}

// A dropped stream must not look like a finished one.
func TestADroppedSubscriberIsToldItWasDropped(t *testing.T) {
	s := New(Config{})
	s.Begin("e1", "sb1", "yes", nil)

	ch, _ := s.Subscribe("e1")

	// Overflow the buffer before reading a single frame.
	for i := 0; i < subscriberBuffer*2; i++ {
		s.Append("e1", protocol.Frame{Type: protocol.FrameStdout, Data: []byte("x")})
	}
	s.Append("e1", exit0())

	got := collect(t, ch, 5*time.Second)
	if len(got) == 0 {
		t.Fatal("no frames at all")
	}
	last := got[len(got)-1]
	if last.Type != protocol.FrameError {
		t.Fatalf("a dropped stream ended with %s, which is indistinguishable from a "+
			"clean finish; the caller would believe they had the whole output", last.Type)
	}
}
