package logstore

import (
	"github.com/pablofdezr/microvm/internal/protocol"
)

// subscriberBuffer is how many frames a stream may fall behind by.
//
// Generous, because falling behind costs the caller their stream, and a burst
// of output is normal rather than abusive. Not unbounded, because the whole
// point is to have a limit that is hit instead of memory that is not.
const subscriberBuffer = 512

// subscriber is one live stream of an exec.
type subscriber struct {
	ch chan protocol.Frame
	// lagged records that this stream was dropped for falling behind, so the
	// reader can say so rather than end as though the process finished.
	lagged bool
	closed bool
}

// Subscribe returns every frame of an exec: what it has already printed, then
// what it prints next, until it ends.
//
// This is what makes a stream reconnectable. Output is already buffered on the
// host, so a caller who connects late -- or reconnects after their network
// dropped -- can be given the history and then joined to the live feed, and
// loses nothing. Without replay, "start the command" and "watch the command"
// would have to be one request, and a broken connection would mean output that
// no longer exists anywhere.
//
// The returned channel is closed when the exec ends. The bool is false if there
// is no such exec.
//
// One thing this does not preserve: the interleaving of stdout and stderr in
// the replayed part. The two are buffered separately, so replay emits all of
// stdout then all of stderr, and only the live part keeps their true order.
// Restoring it would mean storing every frame rather than two ring buffers,
// which is a lot of memory to spend on the ordering of two streams that the
// program itself never promised to interleave in any particular way.
func (s *Store) Subscribe(execID string) (<-chan protocol.Frame, bool) {
	e := s.entry(execID)
	if e == nil {
		return nil, false
	}

	// The snapshot and the registration happen under one lock. If they did not,
	// a frame arriving between them would be in neither the replay nor the live
	// feed -- lost, silently, and only under load.
	e.mu.Lock()

	var replay []protocol.Frame
	if out, _ := e.stdout.Bytes(); len(out) > 0 {
		replay = append(replay, protocol.Frame{Type: protocol.FrameStdout, Data: out})
	}
	if errOut, _ := e.stderr.Bytes(); len(errOut) > 0 {
		replay = append(replay, protocol.Frame{Type: protocol.FrameStderr, Data: errOut})
	}

	finished := e.rec.Status != StatusRunning
	if finished {
		replay = append(replay, terminalFrame(e.rec))
	}

	var sub *subscriber
	if !finished {
		sub = &subscriber{ch: make(chan protocol.Frame, subscriberBuffer)}
		e.subs = append(e.subs, sub)
	}
	e.mu.Unlock()

	out := make(chan protocol.Frame)
	go func() {
		defer close(out)
		for _, f := range replay {
			out <- f
		}
		if sub == nil {
			return
		}
		for f := range sub.ch {
			out <- f
		}
		// The live channel closed. If it closed because this reader could not
		// keep up, say so: ending quietly would look exactly like the process
		// finishing, and the caller would believe they had the whole output.
		e.mu.Lock()
		lagged := sub.lagged
		e.mu.Unlock()
		if lagged {
			out <- protocol.Frame{
				Type: protocol.FrameError,
				Message: "this stream fell behind and was dropped; the output was still " +
					"recorded, so retrieve the execution to get all of it",
			}
		}
	}()

	return out, true
}

// terminalFrame reconstructs the frame that ended an exec, for a replay that
// starts after it already has.
func terminalFrame(rec Record) protocol.Frame {
	if rec.Status == StatusExited || rec.Status == StatusTimedOut {
		return protocol.Frame{
			Type:     protocol.FrameExit,
			ExitCode: rec.ExitCode,
			Signal:   rec.Signal,
			TimedOut: rec.Status == StatusTimedOut,
		}
	}
	// Everything else ended without the process reporting its own status:
	// it never started, the caller cancelled, or the VM was taken away.
	msg := rec.Error
	if msg == "" {
		msg = string(rec.Status)
	}
	return protocol.Frame{Type: protocol.FrameError, Message: msg}
}

// fanOutLocked hands a frame to every live stream. The caller holds e.mu.
//
// It never blocks. This runs on the exec's own output path, so waiting on a
// subscriber would let one slow HTTP client throttle the program inside the
// sandbox -- turning a reader's problem into the workload's problem. A stream
// that cannot keep up is dropped instead; its output is in the ring buffer
// regardless, so nothing is actually lost.
func (e *entry) fanOutLocked(f protocol.Frame) {
	for _, sub := range e.subs {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- f:
		default:
			sub.lagged = true
			close(sub.ch)
			sub.closed = true
		}
	}
}

// closeSubsLocked ends every live stream. The caller holds e.mu.
func (e *entry) closeSubsLocked() {
	for _, sub := range e.subs {
		if !sub.closed {
			close(sub.ch)
			sub.closed = true
		}
	}
	e.subs = nil
}
