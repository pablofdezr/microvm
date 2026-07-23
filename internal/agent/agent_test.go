package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/protocol"
)

func testAgent(t *testing.T) *httptest.Server {
	t.Helper()
	a := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// execFrames posts an exec and collects every frame until the stream ends.
func execFrames(t *testing.T, srv *httptest.Server, req protocol.ExecRequest) []protocol.Frame {
	t.Helper()
	resp := postExec(t, context.Background(), srv, req)
	defer resp.Body.Close()
	return readFrames(t, resp.Body)
}

func postExec(t *testing.T, ctx context.Context, srv *httptest.Server, req protocol.ExecRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/exec", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("exec returned %d: %s", resp.StatusCode, b)
	}
	return resp
}

func readFrames(t *testing.T, r io.Reader) []protocol.Frame {
	t.Helper()
	var frames []protocol.Frame
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var f protocol.Frame
		if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
			t.Fatalf("decode frame %q: %v", scanner.Text(), err)
		}
		frames = append(frames, f)
	}
	return frames
}

// collect concatenates the data of every frame of the given type.
func collect(frames []protocol.Frame, typ protocol.FrameType) string {
	var b strings.Builder
	for _, f := range frames {
		if f.Type == typ {
			b.Write(f.Data)
		}
	}
	return b.String()
}

func lastFrame(t *testing.T, frames []protocol.Frame) protocol.Frame {
	t.Helper()
	if len(frames) == 0 {
		t.Fatal("no frames received")
	}
	return frames[len(frames)-1]
}

func TestExecSeparatesStreamsAndReportsExitCode(t *testing.T) {
	srv := testAgent(t)

	frames := execFrames(t, srv, protocol.ExecRequest{
		ID:   "t1",
		Cmd:  "sh",
		Args: []string{"-c", "echo out; echo err >&2; exit 7"},
	})

	if got := collect(frames, protocol.FrameStdout); got != "out\n" {
		t.Errorf("stdout = %q, want %q", got, "out\n")
	}
	if got := collect(frames, protocol.FrameStderr); got != "err\n" {
		t.Errorf("stderr = %q, want %q", got, "err\n")
	}

	if frames[0].Type != protocol.FrameStarted {
		t.Errorf("first frame = %s, want %s", frames[0].Type, protocol.FrameStarted)
	}
	if frames[0].PID == 0 {
		t.Error("started frame carries no pid")
	}

	exit := lastFrame(t, frames)
	if exit.Type != protocol.FrameExit {
		t.Fatalf("last frame = %s, want %s", exit.Type, protocol.FrameExit)
	}
	if exit.ExitCode == nil || *exit.ExitCode != 7 {
		t.Errorf("exit code = %v, want 7", exit.ExitCode)
	}
}

// Output must reach the caller as it is produced. If the agent buffered until
// exit, every frame would land at once at the end.
func TestExecStreamsIncrementally(t *testing.T) {
	srv := testAgent(t)

	resp := postExec(t, context.Background(), srv, protocol.ExecRequest{
		ID:   "stream",
		Cmd:  "sh",
		Args: []string{"-c", "echo first; sleep 0.5; echo second"},
	})
	defer resp.Body.Close()

	start := time.Now()
	dec := json.NewDecoder(resp.Body)

	var firstAt time.Duration
	for {
		var f protocol.Frame
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if f.Type == protocol.FrameStdout && strings.Contains(string(f.Data), "first") {
			firstAt = time.Since(start)
			break
		}
	}

	// "first" is printed immediately; it must not wait on the sleep.
	if firstAt > 400*time.Millisecond {
		t.Errorf("first chunk arrived after %v, want well under the 500ms sleep: output is being buffered", firstAt)
	}
}

func TestExecTimeoutKillsProcess(t *testing.T) {
	srv := testAgent(t)

	start := time.Now()
	frames := execFrames(t, srv, protocol.ExecRequest{
		ID:      "slow",
		Cmd:     "sh",
		Args:    []string{"-c", "echo begin; sleep 30"},
		Timeout: 500 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("took %v: the timeout did not fire", elapsed)
	}

	exit := lastFrame(t, frames)
	if exit.Type != protocol.FrameExit {
		t.Fatalf("last frame = %s (%s), want %s", exit.Type, exit.Message, protocol.FrameExit)
	}
	if !exit.TimedOut {
		t.Error("exit frame does not report timed_out")
	}
	if exit.Signal == "" {
		t.Error("exit frame does not report the signal that killed the process")
	}
}

// An abort must take the whole process tree with it. A naive kill of the leader
// alone would leave the grandchild running and burning guest CPU forever.
func TestAbortKillsProcessTree(t *testing.T) {
	srv := testAgent(t)

	marker := t.TempDir() + "/grandchild-alive"
	ctx, cancel := context.WithCancel(context.Background())

	// The grandchild outlives its parent shell and touches the marker file
	// repeatedly; if it survives the abort, the file keeps reappearing.
	script := "sh -c 'while true; do touch " + marker + "; sleep 0.1; done' & echo spawned; wait"
	resp := postExec(t, ctx, srv, protocol.ExecRequest{
		ID:   "tree",
		Cmd:  "sh",
		Args: []string{"-c", script},
	})

	// Wait until the tree is definitely up.
	dec := json.NewDecoder(resp.Body)
	for {
		var f protocol.Frame
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if f.Type == protocol.FrameStdout && strings.Contains(string(f.Data), "spawned") {
			break
		}
	}
	waitFor(t, marker, true)

	cancel()
	resp.Body.Close()

	// Give the kill a moment to propagate, then prove the grandchild is gone by
	// removing the marker and checking nothing recreates it.
	time.Sleep(300 * time.Millisecond)
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("grandchild survived the abort: it is still touching the marker file")
	}
}

func waitFor(t *testing.T, path string, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if (err == nil) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to exist=%v", path, want)
}

func TestSignalReachesProcess(t *testing.T) {
	srv := testAgent(t)

	resp := postExec(t, context.Background(), srv, protocol.ExecRequest{
		ID:   "sig",
		Cmd:  "sh",
		Args: []string{"-c", `trap 'exit 3' TERM; echo ready; while true; do sleep 0.05; done`},
	})
	defer resp.Body.Close()

	// The process printing "ready" proves it has installed its trap and that
	// the agent has registered the exec, since registration precedes the first
	// frame. Polling the signal endpoint to detect readiness would not do: the
	// probe signal would itself hit the process.
	dec := json.NewDecoder(resp.Body)
	readUntilStdout(t, dec, "ready")

	sigResp, err := http.Post(srv.URL+"/v1/exec/sig/signal", "application/json",
		strings.NewReader(`{"signal":"SIGTERM"}`))
	if err != nil {
		t.Fatal(err)
	}
	sigResp.Body.Close()
	if sigResp.StatusCode != http.StatusNoContent {
		t.Fatalf("signal returned %d, want 204", sigResp.StatusCode)
	}

	exit := readUntilExit(t, dec)
	if exit.ExitCode == nil || *exit.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3 (the trap's own status)", exit.ExitCode)
	}
}

// readUntilStdout blocks until a stdout frame containing want arrives.
func readUntilStdout(t *testing.T, dec *json.Decoder, want string) {
	t.Helper()
	for {
		var f protocol.Frame
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("waiting for %q on stdout: %v", want, err)
		}
		if f.Type == protocol.FrameStdout && strings.Contains(string(f.Data), want) {
			return
		}
		if f.Type == protocol.FrameExit || f.Type == protocol.FrameError {
			t.Fatalf("stream ended (%s: %s) before %q appeared on stdout", f.Type, f.Message, want)
		}
	}
}

// readUntilExit drains frames and returns the terminal one.
func readUntilExit(t *testing.T, dec *json.Decoder) protocol.Frame {
	t.Helper()
	for {
		var f protocol.Frame
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("waiting for exit frame: %v", err)
		}
		if f.Type == protocol.FrameExit || f.Type == protocol.FrameError {
			return f
		}
	}
}

func TestSignalErrors(t *testing.T) {
	srv := testAgent(t)

	tests := []struct {
		name string
		id   string
		body string
		want int
	}{
		{"unknown exec", "ghost", `{"signal":"SIGTERM"}`, http.StatusNotFound},
		{"unsupported signal", "ghost", `{"signal":"SIGSEGV"}`, http.StatusBadRequest},
		{"malformed body", "ghost", `not json`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(srv.URL+"/v1/exec/"+tc.id+"/signal", "application/json",
				strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestExecValidation(t *testing.T) {
	srv := testAgent(t)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"missing id", `{"cmd":"echo"}`, http.StatusBadRequest},
		{"missing cmd", `{"id":"x"}`, http.StatusBadRequest},
		{"tty unsupported", `{"id":"x","cmd":"sh","tty":true}`, http.StatusBadRequest},
		{"malformed", `{`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(srv.URL+"/v1/exec", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// A command that does not exist must surface as a clean error, not a hang.
func TestExecMissingBinary(t *testing.T) {
	srv := testAgent(t)

	body, _ := json.Marshal(protocol.ExecRequest{ID: "missing", Cmd: "definitely-not-a-real-binary"})
	resp, err := http.Post(srv.URL+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	frames := readFrames(t, resp.Body)
	f := lastFrame(t, frames)
	if f.Type != protocol.FrameError {
		t.Fatalf("last frame = %s, want %s", f.Type, protocol.FrameError)
	}
	if f.Message == "" {
		t.Error("error frame carries no message")
	}
}

func TestStdinIsDelivered(t *testing.T) {
	srv := testAgent(t)

	frames := execFrames(t, srv, protocol.ExecRequest{
		ID:    "stdin",
		Cmd:   "sh",
		Args:  []string{"-c", "cat"},
		Stdin: []byte("hola mundo"),
	})

	if got := collect(frames, protocol.FrameStdout); got != "hola mundo" {
		t.Errorf("stdout = %q, want %q", got, "hola mundo")
	}
}

func TestFileRoundTrip(t *testing.T) {
	srv := testAgent(t)
	content := "print('hola desde el fichero')\n"

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/files?path=prog.py", strings.NewReader(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put returned %d, want 204", resp.StatusCode)
	}

	// The uploaded file must be visible to a subsequent exec in the workdir.
	frames := execFrames(t, srv, protocol.ExecRequest{ID: "cat", Cmd: "cat", Args: []string{"prog.py"}})
	if got := collect(frames, protocol.FrameStdout); got != content {
		t.Errorf("exec saw %q, want %q", got, content)
	}

	resp, err = http.Get(srv.URL + "/v1/files?path=prog.py")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != content {
		t.Errorf("download = %q, want %q", got, content)
	}
}

func TestFileErrors(t *testing.T) {
	srv := testAgent(t)

	tests := []struct {
		name string
		path string
		want int
	}{
		{"missing file", "?path=nope.txt", http.StatusNotFound},
		{"empty path", "?path=", http.StatusBadRequest},
		{"directory", "?path=/tmp", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + "/v1/files" + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestEnvOverridesBaseEnvironment(t *testing.T) {
	t.Setenv("MICROVM_TEST_BASE", "base-value")

	srv := testAgent(t)
	frames := execFrames(t, srv, protocol.ExecRequest{
		ID:   "env",
		Cmd:  "sh",
		Args: []string{"-c", "echo $MICROVM_TEST_BASE $MICROVM_TEST_EXTRA"},
		Env:  map[string]string{"MICROVM_TEST_BASE": "overridden", "MICROVM_TEST_EXTRA": "extra"},
	})

	if got := collect(frames, protocol.FrameStdout); got != "overridden extra\n" {
		t.Errorf("stdout = %q, want %q", got, "overridden extra\n")
	}
}

func TestBuildEnvReplacesRatherThanDuplicates(t *testing.T) {
	t.Setenv("MICROVM_DUP", "original")

	env := buildEnv(map[string]string{"MICROVM_DUP": "replacement"})

	var count int
	for _, kv := range env {
		if strings.HasPrefix(kv, "MICROVM_DUP=") {
			count++
			if kv != "MICROVM_DUP=replacement" {
				t.Errorf("got %q, want MICROVM_DUP=replacement", kv)
			}
		}
	}
	// A duplicate would leave the winner up to the exec'd program's libc.
	if count != 1 {
		t.Errorf("MICROVM_DUP appears %d times, want exactly 1", count)
	}
}

func TestHealth(t *testing.T) {
	srv := testAgent(t)

	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var h protocol.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if !h.OK {
		t.Error("health reports not ok")
	}
	if h.Version != Version {
		t.Errorf("version = %q, want %q", h.Version, Version)
	}
}

func TestParseFileMode(t *testing.T) {
	tests := []struct {
		in      string
		want    os.FileMode
		wantErr bool
	}{
		{in: "644", want: 0o644},
		{in: "755", want: 0o755},
		{in: "0600", want: 0o600},
		{in: "notoctal", wantErr: true},
		{in: "99999", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseFileMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseFileMode(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseFileMode(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseFileMode(%q) = %o, want %o", tc.in, got, tc.want)
		}
	}
}
