// Package guestclient talks to the agent running inside a guest microVM.
//
// It is the host-side mirror of internal/agent: every call here maps to one of
// the agent's routes, carried as HTTP over the guest's vsock socket.
package guestclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/vsock"
)

// Client is a connection to one guest's agent. It is safe for concurrent use.
type Client struct {
	http *http.Client
	// baseURL's host is a placeholder: the transport dials the vsock socket
	// regardless of what it says. It exists only because net/http requires a
	// syntactically valid URL.
	baseURL string
}

// New returns a client for the guest whose vsock device is backed by udsPath.
func New(udsPath string) *Client {
	return &Client{
		http: &http.Client{
			Transport: vsock.Transport(udsPath, protocol.AgentPort),
			// No client-level timeout: it would apply to the whole request
			// including a streaming body, killing long execs. Callers scope
			// individual calls with a context instead.
		},
		baseURL: "http://guest",
	}
}

// WaitReady polls the agent's health endpoint until it answers or ctx expires.
//
// The gap between Firecracker reporting the VM as started and the agent being
// able to serve is real but short: the guest kernel still has to boot and init
// has to run. Handing a sandbox to a caller before this returns would surface
// as a confusing connection error on their first exec.
func (c *Client) WaitReady(ctx context.Context) error {
	const pollInterval = 5 * time.Millisecond

	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return fmt.Errorf("guest agent never became ready: %w (last attempt: %v)", err, lastErr)
			}
			return fmt.Errorf("guest agent never became ready: %w", err)
		}

		health, err := c.Health(ctx)
		if err == nil && health.OK {
			return nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
		case <-time.After(pollInterval):
		}
	}
}

// Health reports the agent's readiness.
func (c *Client) Health(ctx context.Context) (protocol.HealthResponse, error) {
	var out protocol.HealthResponse

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/health", nil)
	if err != nil {
		return out, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	if err := checkStatus(resp, http.StatusOK); err != nil {
		return out, err
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decode health: %w", err)
	}
	return out, nil
}

// Exec starts a process in the guest and calls onFrame for each frame as it
// arrives. It returns when the process has exited and every frame has been
// delivered.
//
// Cancelling ctx aborts the process: the agent kills its process group when the
// request is severed. That is the abort path, and it is why this streams rather
// than collecting.
func (c *Client) Exec(ctx context.Context, req protocol.ExecRequest, onFrame func(protocol.Frame) error) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode exec request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/exec", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("exec %s: %w", req.Cmd, err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp, http.StatusOK); err != nil {
		return err
	}

	// A json.Decoder over the body yields frames as they arrive, since it reads
	// only as far as each value needs.
	dec := json.NewDecoder(resp.Body)

	// Every healthy stream ends with a terminal frame. Tracking that is what
	// separates "the process finished" from "the connection died", which look
	// identical from here: both arrive as io.EOF.
	var sawTerminal bool

	for {
		var frame protocol.Frame
		err := dec.Decode(&frame)

		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if sawTerminal {
				return nil
			}
			// EOF with no terminal frame means the guest stopped talking
			// mid-exec -- the VM was killed, OOMed, or crashed. Returning nil
			// here would report success for a process that never finished, and
			// the caller would read an empty result as a clean empty output.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("exec %s: stream ended without an exit status; "+
				"the sandbox stopped responding mid-exec (killed, OOM, or crashed)", req.ID)
		}

		if err != nil {
			// A cancelled context surfaces here as a read error on the severed
			// body. That is an expected abort, not a failure to report.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("decode frame: %w", err)
		}

		if frame.Type == protocol.FrameExit || frame.Type == protocol.FrameError {
			sawTerminal = true
		}

		if err := onFrame(frame); err != nil {
			return err
		}
	}
}

// Result is the collected outcome of an exec, for callers that do not need to
// stream.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Signal   string
	TimedOut bool
}

// Execer is the one method Collect needs: the streaming exec.
//
// It exists so Collect can run against the runtime's GuestClient port as well
// as this concrete client. The port cannot be named here directly -- runtime
// imports this package for its interface-satisfaction assertion, so importing
// it back would be a cycle -- but the port satisfies this structurally, which
// is all Collect requires.
type Execer interface {
	Exec(ctx context.Context, req protocol.ExecRequest, onFrame func(protocol.Frame) error) error
}

// Collect runs a command and buffers its output.
//
// Buffering is derivable from streaming -- it is Exec with the frames gathered
// into a Result -- so it lives here as a free function over Execer rather than
// as a method on the interface. Putting it on the GuestClient port would force
// every implementation, including every test fake, to reimplement this identical
// loop; keeping it a function means the one implementation serves them all.
//
// Only for output known to be small: everything is held in memory, so a command
// that writes gigabytes to stdout would take the host down with it. Prefer Exec
// for anything user-controlled.
func Collect(ctx context.Context, e Execer, req protocol.ExecRequest) (Result, error) {
	var (
		res    Result
		stdout bytes.Buffer
		stderr bytes.Buffer
		failed error
	)

	err := e.Exec(ctx, req, func(f protocol.Frame) error {
		switch f.Type {
		case protocol.FrameStdout:
			stdout.Write(f.Data)
		case protocol.FrameStderr:
			stderr.Write(f.Data)
		case protocol.FrameExit:
			if f.ExitCode != nil {
				res.ExitCode = *f.ExitCode
			}
			res.Signal = f.Signal
			res.TimedOut = f.TimedOut
		case protocol.FrameError:
			failed = fmt.Errorf("agent failed to run %s: %s", req.Cmd, f.Message)
		}
		return nil
	})

	res.Stdout = stdout.Bytes()
	res.Stderr = stderr.Bytes()

	if err != nil {
		return res, err
	}
	return res, failed
}

// ExecCollect runs a command and buffers its output.
//
// A thin alias for Collect, kept because it reads naturally where a concrete
// client is already in hand. Code holding only the runtime port calls Collect.
func (c *Client) ExecCollect(ctx context.Context, req protocol.ExecRequest) (Result, error) {
	return Collect(ctx, c, req)
}

// Signal delivers a signal to a running exec's process group.
func (c *Client) Signal(ctx context.Context, execID, signal string) error {
	body, err := json.Marshal(protocol.SignalRequest{Signal: signal})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/exec/%s/signal", c.baseURL, execID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return checkStatus(resp, http.StatusNoContent)
}

// WriteFile uploads content to path inside the guest.
func (c *Client) WriteFile(ctx context.Context, path string, content io.Reader, mode string) error {
	url := fmt.Sprintf("%s/v1/files?path=%s", c.baseURL, queryEscape(path))
	if mode != "" {
		url += "&mode=" + mode
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, content)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return checkStatus(resp, http.StatusNoContent)
}

// ReadFile downloads a file from the guest. The caller must close the reader.
func (c *Client) ReadFile(ctx context.Context, path string) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s/v1/files?path=%s", c.baseURL, queryEscape(path))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp, http.StatusOK); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// Mkdir creates a directory and its parents inside the guest.
func (c *Client) Mkdir(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/mkdir?path=%s", c.baseURL, queryEscape(path)), nil)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return checkStatus(resp, http.StatusNoContent)
}

// checkStatus turns a non-expected status into an error carrying the agent's
// own message, which is far more useful than the bare code.
func checkStatus(resp *http.Response, want int) error {
	if resp.StatusCode == want {
		return nil
	}

	var apiErr protocol.ErrorResponse
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error != "" {
		return fmt.Errorf("agent returned %s: %s", resp.Status, apiErr.Error)
	}
	return fmt.Errorf("agent returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

func queryEscape(s string) string {
	return url.QueryEscape(s)
}
