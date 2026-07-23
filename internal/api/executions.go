package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/id"
	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

// handleCreateExecution starts a command and returns immediately.
//
// It does not wait for the command to finish, and it does not tie the command
// to this request. Both follow from the same decision: an execution belongs to
// its sandbox, not to the HTTP call that started it. A caller whose connection
// drops mid-run keeps their run.
func (s *Server) handleCreateExecution(w http.ResponseWriter, r *http.Request) {
	sb, err := s.sandbox(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	var params apitypes.ExecutionCreateParams
	if err := decodeBody(w, r, &params); err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	if params.Cmd == "" {
		s.writeAPIError(w, r, missingParamError("cmd"))
		return
	}

	req := protocol.ExecRequest{
		ID:      id.New(id.ExecutionPrefix),
		Cmd:     params.Cmd,
		Cwd:     deref(params.Cwd),
		Timeout: time.Duration(deref(params.TimeoutSeconds)) * time.Second,
	}
	if params.Args != nil {
		req.Args = *params.Args
	}
	if params.Env != nil {
		req.Env = *params.Env
	}
	if params.Stdin != nil {
		req.Stdin = *params.Stdin
	}

	if err := sb.StartExec(req); err != nil {
		s.writeAPIError(w, r, s.sandboxStateError(sb, err))
		return
	}

	rec, ok := sb.Logs(req.ID)
	if !ok {
		s.writeAPIError(w, r, internalError(fmt.Errorf("execution %s produced no record", req.ID)))
		return
	}
	writeJSON(w, http.StatusCreated, toAPIExecution(rec))
}

func (s *Server) handleListExecutions(w http.ResponseWriter, r *http.Request) {
	sb, err := s.sandbox(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	page, err := parsePageParams(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	recs := sb.AllLogs()
	items, hasMore := paginate(recs, func(rec logstore.Record) string { return rec.ID }, page)

	data := make([]apitypes.Execution, 0, len(items))
	for _, rec := range items {
		data = append(data, toAPIExecution(rec))
	}

	writeJSON(w, http.StatusOK, apitypes.ExecutionList{
		Object:  apitypes.ExecutionListObjectList,
		Url:     fmt.Sprintf("/%s/sandboxes/%s/executions", APIVersion, sb.ID()),
		HasMore: hasMore,
		Data:    data,
	})
}

// handleRetrieveExecution returns an execution and everything it printed.
//
// This works after the sandbox is gone, which is the point: the output you most
// want to read is the output of the run that was killed.
func (s *Server) handleRetrieveExecution(w http.ResponseWriter, r *http.Request) {
	_, rec, err := s.execution(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIExecution(rec))
}

// handleStreamExecution sends the execution's frames as server-sent events.
//
// The stream replays from the beginning and then follows, so connecting late or
// reconnecting after a drop loses nothing. That is only possible because output
// is buffered on the host -- and it is why starting a command and watching it
// are two calls rather than one.
//
// SSE rather than WebSockets: the traffic is one-directional, so there is
// nothing to gain from a duplex channel and a lot to lose -- an upgrade
// handshake, keepalives, and proxies that mishandle both. Cancelling has its own
// endpoint.
func (s *Server) handleStreamExecution(w http.ResponseWriter, r *http.Request) {
	sb, _, err := s.execution(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	frames, ok := sb.StreamExec(r.PathValue("execution"))
	if !ok {
		s.writeAPIError(w, r, notFoundError(CodeExecutionNotFound, "execution", r.PathValue("execution")))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Tell any proxy in the way not to buffer, or the stream arrives all at once
	// at the end and the whole exercise is pointless.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	_ = rc.Flush()

	enc := json.NewEncoder(w)
	for {
		select {
		case f, open := <-frames:
			if !open {
				return
			}
			if _, err := io.WriteString(w, "data: "); err != nil {
				return
			}
			if err := enc.Encode(toAPIFrame(f)); err != nil {
				return
			}
			// SSE frames end with a blank line; Encode already wrote one newline.
			if _, err := io.WriteString(w, "\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}

		case <-r.Context().Done():
			// The caller hung up. The execution keeps running -- it belongs to
			// the sandbox -- and its output is still recorded, so they can
			// reconnect to this same stream and miss nothing.
			return
		}
	}
}

// handleCancelExecution signals a running command.
func (s *Server) handleCancelExecution(w http.ResponseWriter, r *http.Request) {
	sb, rec, err := s.execution(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	var params apitypes.ExecutionCancelParams
	if err := decodeOptionalBody(w, r, &params); err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	// SIGKILL by default. Hostile code is not owed a chance to clean up, and a
	// signal it can catch and ignore is a way for it to outlive its own cancel.
	signal := "SIGKILL"
	if params.Signal != nil && *params.Signal != "" {
		signal = *params.Signal
	}

	// Cancelling something that already finished is not an error: a caller
	// racing a timeout against their own cancel should not have to care who won.
	if rec.Status != logstore.StatusRunning {
		writeJSON(w, http.StatusOK, toAPIExecution(rec))
		return
	}

	if err := sb.Signal(r.Context(), rec.ID, signal); err != nil {
		s.writeAPIError(w, r, invalidParamError("signal", err.Error()))
		return
	}

	// Record that the caller cancelled. A SIGKILLed process gets no chance to
	// say why it died, so without this the record would show whatever the
	// broken stream looked like rather than the truth, which is that we killed
	// it on request.
	sb.FinishExec(rec.ID, logstore.StatusAborted)

	updated, _ := sb.Logs(rec.ID)
	writeJSON(w, http.StatusOK, toAPIExecution(updated))
}

// execution resolves the {sandbox} and {execution} path values.
func (s *Server) execution(r *http.Request) (*sandbox.Sandbox, logstore.Record, error) {
	sb, err := s.sandbox(r)
	if err != nil {
		return nil, logstore.Record{}, err
	}

	raw := r.PathValue("execution")
	if err := id.Parse(raw, id.ExecutionPrefix); err != nil {
		return nil, logstore.Record{}, invalidParamError("execution", err.Error())
	}

	rec, ok := sb.Logs(raw)
	if !ok {
		return nil, logstore.Record{}, notFoundError(CodeExecutionNotFound, "execution", raw)
	}
	// An execution ID is unique, but it is addressed under a sandbox, so a real
	// execution quoted under the wrong sandbox must not resolve. Otherwise the
	// nesting is decoration rather than structure.
	if rec.SandboxID != sb.ID() {
		return nil, logstore.Record{}, notFoundError(CodeExecutionNotFound, "execution", raw)
	}
	return sb, rec, nil
}

// sandboxStateError explains a refusal in terms of the sandbox, when that is
// why it was refused.
func (s *Server) sandboxStateError(sb *sandbox.Sandbox, cause error) error {
	if sb.State() != sandbox.StateRunning {
		return sandboxNotRunningError(sb.ID(), string(sb.State()), string(sb.Reason()))
	}
	return internalError(cause)
}

func toAPIExecution(rec logstore.Record) apitypes.Execution {
	out := apitypes.Execution{
		Id:              rec.ID,
		Object:          apitypes.ExecutionObjectExecution,
		Sandbox:         rec.SandboxID,
		Cmd:             rec.Cmd,
		Status:          toAPIExecutionStatus(rec.Status),
		ExitCode:        rec.ExitCode,
		Stdout:          string(rec.Stdout),
		Stderr:          string(rec.Stderr),
		StdoutTruncated: ptr(rec.StdoutTruncated),
		StderrTruncated: ptr(rec.StderrTruncated),
		Created:         rec.StartedAt,
		DurationMs:      ptr(int(rec.Duration().Milliseconds())),
	}
	if len(rec.Args) > 0 {
		out.Args = &rec.Args
	}
	if rec.Signal != "" {
		out.Signal = ptr(rec.Signal)
	}
	if rec.Error != "" {
		out.Error = ptr(rec.Error)
	}
	if !rec.FinishedAt.IsZero() {
		out.Finished = &rec.FinishedAt
	}
	return out
}

// toAPIExecutionStatus maps the store's status to the wire's.
//
// The two vocabularies are deliberately allowed to differ -- the store says
// "aborted", the API says "canceled" -- because renaming an internal constant
// must never be able to change what a caller sees. This function is where that
// promise is kept.
func toAPIExecutionStatus(st logstore.Status) apitypes.ExecutionStatus {
	switch st {
	case logstore.StatusRunning:
		return apitypes.ExecutionStatusRunning
	case logstore.StatusExited:
		return apitypes.ExecutionStatusExited
	case logstore.StatusTimedOut:
		return apitypes.ExecutionStatusTimedOut
	case logstore.StatusAborted:
		return apitypes.ExecutionStatusCanceled
	case logstore.StatusVanished:
		return apitypes.ExecutionStatusVanished
	case logstore.StatusFailed:
		return apitypes.ExecutionStatusFailed
	default:
		// Unreachable unless a status is added without being mapped. Reporting
		// it as failed is the safe lie: it stops a caller waiting forever on a
		// status they cannot interpret.
		return apitypes.ExecutionStatusFailed
	}
}

func toAPIFrame(f protocol.Frame) apitypes.Frame {
	out := apitypes.Frame{
		Object:   apitypes.FrameObjectFrame,
		Type:     apitypes.FrameType(f.Type),
		ExitCode: f.ExitCode,
	}
	if len(f.Data) > 0 {
		out.Data = &f.Data
	}
	if f.PID != 0 {
		out.Pid = ptr(f.PID)
	}
	if f.Signal != "" {
		out.Signal = ptr(f.Signal)
	}
	if f.TimedOut {
		out.TimedOut = ptr(true)
	}
	if f.Message != "" {
		out.Message = ptr(f.Message)
	}
	return out
}
