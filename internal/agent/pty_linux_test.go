//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// A tty exec runs against a real terminal: `test -t 1` succeeds only when stdout
// is one, so this proves the process got a controlling terminal rather than a
// pipe.
func TestTTYExecGivesARealTerminal(t *testing.T) {
	srv := testAgent(t)
	frames := execFrames(t, srv, protocol.ExecRequest{
		ID: "tty-real", Cmd: "sh", Args: []string{"-c", "test -t 1 && echo istty"},
		TTY: true, Timeout: 5 * time.Second,
	})
	if out := collect(frames, protocol.FrameStdout); !strings.Contains(out, "istty") {
		t.Errorf("stdout = %q, want it to contain \"istty\": the process had no terminal", out)
	}
}

// A terminal has one output stream, so stderr is merged into stdout and there are
// no stderr frames at all.
func TestTTYMergesStderrIntoStdout(t *testing.T) {
	srv := testAgent(t)
	frames := execFrames(t, srv, protocol.ExecRequest{
		ID: "tty-merge", Cmd: "sh", Args: []string{"-c", "echo out; echo err 1>&2"},
		TTY: true, Timeout: 5 * time.Second,
	})
	out := collect(frames, protocol.FrameStdout)
	if !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Errorf("stdout = %q, want both streams merged into it", out)
	}
	if serr := collect(frames, protocol.FrameStderr); serr != "" {
		t.Errorf("stderr frames = %q, want none: a tty has one output stream", serr)
	}
}

// The initial window size reaches the guest: stty reads it back off the pty.
func TestTTYInitialWindowSize(t *testing.T) {
	srv := testAgent(t)
	frames := execFrames(t, srv, protocol.ExecRequest{
		ID: "tty-size", Cmd: "stty", Args: []string{"size"},
		TTY: true, Rows: 40, Cols: 120, Timeout: 5 * time.Second,
	})
	if out := strings.TrimSpace(collect(frames, protocol.FrameStdout)); out != "40 120" {
		t.Errorf("stty size = %q, want \"40 120\"", out)
	}
}

// A resize mid-exec reaches the pty: stty reads the new size after it lands.
func TestTTYResizeChangesTheWindow(t *testing.T) {
	srv := testAgent(t)

	const id = "tty-resize"
	// The command sleeps first, so the resize lands before stty reads the size.
	resp := postExec(t, context.Background(), srv, protocol.ExecRequest{
		ID: id, Cmd: "sh", Args: []string{"-c", "sleep 0.3; stty size"},
		TTY: true, Rows: 24, Cols: 80, Timeout: 5 * time.Second,
	})
	defer resp.Body.Close()

	time.Sleep(50 * time.Millisecond)
	if code := postResize(t, srv, id, 50, 200); code != http.StatusNoContent {
		t.Fatalf("resize status = %d, want 204", code)
	}

	frames := readFrames(t, resp.Body)
	if out := strings.TrimSpace(collect(frames, protocol.FrameStdout)); out != "50 200" {
		t.Errorf("stty size after resize = %q, want \"50 200\"", out)
	}
}

// Resizing a plain exec is a conflict, not a silent success: the caller has
// misunderstood what they started.
func TestResizeAPlainExecIsAConflict(t *testing.T) {
	srv := testAgent(t)

	const id = "plain-resize"
	resp := postExec(t, context.Background(), srv, protocol.ExecRequest{
		ID: id, Cmd: "sh", Args: []string{"-c", "sleep 0.3"},
		Timeout: 5 * time.Second,
	})
	defer resp.Body.Close()

	time.Sleep(50 * time.Millisecond)
	if code := postResize(t, srv, id, 50, 200); code != http.StatusConflict {
		t.Fatalf("resize of a plain exec = %d, want 409", code)
	}
	_ = readFrames(t, resp.Body) // drain the stream so the handler returns
}

// postResize sends a resize and returns the status code.
func postResize(t *testing.T, srv *httptest.Server, id string, rows, cols uint16) int {
	t.Helper()
	body, _ := json.Marshal(protocol.ResizeRequest{Rows: rows, Cols: cols})
	resp, err := http.Post(srv.URL+"/v1/exec/"+id+"/resize", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
