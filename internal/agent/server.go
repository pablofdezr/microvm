// Package agent implements the supervisor that runs as PID 1 inside a guest
// microVM. It serves HTTP over AF_VSOCK: the host dials the VM's vsock socket
// and speaks ordinary HTTP/1.1 over it, so both sides use net/http and neither
// needs a bespoke framing layer.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// Version is stamped into health responses so the host can detect a guest
// image built against a stale agent.
const Version = "0.1.0"

// maxUploadSize caps a single file upload. The guest's writable layer is small
// and an unbounded write would fill it and wedge the sandbox.
const maxUploadSize = 256 << 20 // 256 MiB

// Agent serves the guest control API.
type Agent struct {
	// workdir is the default cwd for execs and the root that file paths are
	// resolved against.
	workdir string

	execs   *registry
	started time.Time
	log     *slog.Logger
}

// New returns an Agent that runs commands under workdir.
func New(workdir string, log *slog.Logger) *Agent {
	return &Agent{
		workdir: workdir,
		execs:   newRegistry(),
		started: time.Now(),
		log:     log,
	}
}

// Handler returns the agent's HTTP routes.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", a.handleHealth)
	mux.HandleFunc("POST /v1/exec", a.handleExec)
	mux.HandleFunc("POST /v1/exec/{id}/signal", a.handleSignal)
	mux.HandleFunc("POST /v1/exec/{id}/stdin", a.handleStdin)
	mux.HandleFunc("PUT /v1/files", a.handlePutFile)
	mux.HandleFunc("GET /v1/files", a.handleGetFile)
	mux.HandleFunc("POST /v1/mkdir", a.handleMkdir)
	return mux
}

func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, protocol.HealthResponse{
		OK:       true,
		Version:  Version,
		UptimeMS: time.Since(a.started).Milliseconds(),
	})
}

// handleExec starts a process and streams its output back as NDJSON. The
// response is chunked and flushed per frame, so the host sees output as it is
// produced rather than at exit.
func (a *Agent) handleExec(w http.ResponseWriter, r *http.Request) {
	req, err := decodeExecRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if _, exists := a.execs.get(req.ID); exists {
		writeError(w, http.StatusConflict, fmt.Errorf("exec %s already running", req.ID))
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	// Frames must reach the host as they happen; a proxy or buffer that
	// coalesces them would defeat streaming.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	var mu sync.Mutex

	emit := func(f protocol.Frame) error {
		mu.Lock()
		defer mu.Unlock()
		if err := enc.Encode(f); err != nil {
			return err
		}
		return rc.Flush()
	}

	// r.Context() is cancelled when the host hangs up, which is how an abort
	// reaches the process: run kills the process group on cancellation.
	if err := a.run(r.Context(), req, emit); err != nil {
		// The status line is already sent, so failures can only be reported in
		// band. If the write itself is what failed, this is best-effort.
		a.log.Error("exec failed", "id", req.ID, "err", err)
		_ = emit(protocol.Frame{Type: protocol.FrameError, Message: err.Error()})
	}
}

func (a *Agent) handleSignal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req protocol.SignalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}

	sig, err := parseSignal(req.Signal)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	p, ok := a.execs.get(id)
	if !ok {
		// The process may have exited between the host deciding to signal and
		// the call arriving. That is not the caller's fault, but they do need
		// to know the signal went nowhere.
		writeError(w, http.StatusNotFound, errNotFound)
		return
	}

	if err := p.signal(sig); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("signal %s: %w", req.Signal, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStdin streams the request body into a running process's stdin. Closing
// the pipe is signalled by the ?close=true query parameter, since many programs
// only act once they see EOF.
func (a *Agent) handleStdin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	p, ok := a.execs.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errNotFound)
		return
	}

	if _, err := io.Copy(p.stdin, r.Body); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("write stdin: %w", err))
		return
	}
	if r.URL.Query().Get("close") == "true" {
		if err := p.stdin.Close(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("close stdin: %w", err))
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handlePutFile(w http.ResponseWriter, r *http.Request) {
	path, err := a.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	mode := os.FileMode(0o644)
	if m := r.URL.Query().Get("mode"); m != "" {
		parsed, err := parseFileMode(m)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		mode = parsed
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()

	n, err := io.Copy(f, io.LimitReader(r.Body, maxUploadSize+1))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if n > maxUploadSize {
		// Remove the truncated file rather than leaving a corrupt prefix that
		// looks like a successful upload.
		_ = os.Remove(path)
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("file exceeds %d bytes", maxUploadSize))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleGetFile(w http.ResponseWriter, r *http.Request) {
	path, err := a.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s is a directory", path))
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
	_, _ = io.Copy(w, f)
}

func (a *Agent) handleMkdir(w http.ResponseWriter, r *http.Request) {
	path, err := a.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolve turns a caller-supplied path into an absolute one under workdir.
// Relative paths are joined to workdir; absolute paths are honoured as-is,
// since a sandbox legitimately reaches /usr/lib and friends.
//
// There is deliberately no traversal check here: the guest is the security
// boundary, not this function. Everything inside the VM is already untrusted
// and disposable, so a path escaping workdir buys an attacker nothing they
// could not get by running `cat` themselves.
func (a *Agent) resolve(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	if !strings.HasPrefix(path, "/") {
		return filepath.Join(a.workdir, path), nil
	}
	return filepath.Clean(path), nil
}

func parseFileMode(s string) (os.FileMode, error) {
	var mode uint32
	if _, err := fmt.Sscanf(s, "%o", &mode); err != nil {
		return 0, fmt.Errorf("invalid mode %q: %w", s, err)
	}
	if mode > 0o7777 {
		return 0, fmt.Errorf("invalid mode %q", s)
	}
	return os.FileMode(mode), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, protocol.ErrorResponse{Error: err.Error()})
}

// decodeExecRequest reads and validates an exec request body.
func decodeExecRequest(r *http.Request) (protocol.ExecRequest, error) {
	var req protocol.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, fmt.Errorf("decode body: %w", err)
	}
	if req.ID == "" {
		return req, errors.New("id is required")
	}
	if req.Cmd == "" {
		return req, errors.New("cmd is required")
	}
	if req.TTY {
		// Accepting the field and ignoring it would silently give the caller
		// merged streams and no job control. Reject until it is implemented.
		return req, errors.New("tty is not supported yet")
	}
	return req, nil
}
