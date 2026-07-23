package microvm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// maxFrameBytes bounds one SSE event. Output is capped server-side, but a
// client should not depend on the server's cap for its own memory safety.
const maxFrameBytes = 8 << 20

// Stream follows an execution's output as it is produced.
//
// The stream replays from the beginning before it follows, so connecting late
// -- or reconnecting after a dropped connection -- loses nothing. Cancel ctx to
// stop watching; the execution keeps running, because it belongs to its sandbox
// rather than to this connection. To stop the execution itself, use Cancel.
//
//	for frame, err := range client.Executions.Stream(ctx, sbID, exeID) {
//	    if err != nil { return err }
//	    os.Stdout.Write(frame.Data())
//	}
//
// The iteration ends when the execution does. A frame of type "exit" carries
// the status; one of type "error" means it ended without the process reporting
// for itself.
func (s *ExecutionService) Stream(ctx context.Context, sandboxID, executionID string) func(yield func(Frame, error) bool) {
	return func(yield func(Frame, error) bool) {
		// A stream has no business inheriting the client's request timeout: a
		// command that legitimately runs for an hour would be cut off after
		// thirty seconds by a timer meant for ordinary calls.
		streaming := *s.c.http
		streaming.Timeout = 0

		client := *s.c
		client.http = &streaming

		resp, err := client.raw(ctx, http.MethodGet,
			"/sandboxes/"+sandboxID+"/executions/"+executionID+"/stream", nil)
		if err != nil {
			yield(Frame{}, err)
			return
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)

		for scanner.Scan() {
			line := scanner.Text()
			// SSE separates events with blank lines and may carry comments and
			// other field names. Only data lines matter here.
			payload, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}

			var frame Frame
			if err := json.Unmarshal([]byte(payload), &frame); err != nil {
				yield(Frame{}, fmt.Errorf("microvm: malformed frame in the stream: %w", err))
				return
			}
			if !yield(frame, nil) {
				return
			}
		}

		if err := scanner.Err(); err != nil {
			// A cancelled context is the caller stopping on purpose, not a
			// failure worth reporting back to them as one.
			if ctx.Err() != nil {
				return
			}
			yield(Frame{}, fmt.Errorf("microvm: reading the stream: %w", err))
		}
	}
}

// Data returns a frame's bytes.
func (f Frame) Bytes() []byte {
	if f.Data == nil {
		return nil
	}
	return *f.Data
}

// Wait blocks until an execution finishes and returns it.
//
// It polls rather than streams, because the two answer different questions:
// streaming is for showing output as it appears, waiting is for knowing the
// result. Polling is also the one that survives a dropped connection without
// any work from the caller.
func (s *ExecutionService) Wait(ctx context.Context, sandboxID, executionID string) (*Execution, error) {
	backoff := pollBackoff{min: 25 * time.Millisecond, max: time.Second}

	for {
		exe, err := s.Retrieve(ctx, sandboxID, executionID)
		if err != nil {
			return nil, err
		}
		if exe.Done() {
			return exe, nil
		}
		if err := backoff.wait(ctx); err != nil {
			return nil, err
		}
	}
}

// Wait blocks until a task has a result.
func (s *TaskService) Wait(ctx context.Context, taskID string) (*Task, error) {
	backoff := pollBackoff{min: 50 * time.Millisecond, max: 2 * time.Second}

	for {
		task, err := s.Retrieve(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if task.Status != TaskStatusPending && task.Status != TaskStatusRunning {
			return task, nil
		}
		if err := backoff.wait(ctx); err != nil {
			return nil, err
		}
	}
}

// pollBackoff spaces out polls.
//
// It grows because the two cases have opposite needs: a command that finishes in
// 50ms should be noticed in 50ms, while one that runs for ten minutes should not
// be asked about twelve thousand times. Starting tight and easing off serves
// both without the caller choosing.
type pollBackoff struct {
	min, max time.Duration
	current  time.Duration
}

func (b *pollBackoff) wait(ctx context.Context) error {
	if b.current == 0 {
		b.current = b.min
	}

	t := time.NewTimer(b.current)
	defer t.Stop()

	select {
	case <-t.C:
		b.current *= 2
		if b.current > b.max {
			b.current = b.max
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run is create-and-wait, for the common case.
//
// It starts a command in an existing sandbox and blocks until it finishes. The
// returned Execution carries the output and the exit code; check Err for the
// endings that are not the code's own doing.
func (c *Client) Run(ctx context.Context, sandboxID, cmd string, args ...string) (*Execution, error) {
	exe, err := c.Executions.Create(ctx, sandboxID, ExecutionCreateParams{
		Cmd:  cmd,
		Args: &args,
	})
	if err != nil {
		return nil, err
	}
	return c.Executions.Wait(ctx, sandboxID, exe.Id)
}
