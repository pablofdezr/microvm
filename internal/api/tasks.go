package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/id"
	"github.com/pablofdezr/microvm/internal/queue"
)

// handleCreateTask queues work for the fleet.
//
// The difference from creating a sandbox is the whole reason the queue exists:
// a sandbox is a reservation and fails when the node is full, whereas a task
// waits for a slot anywhere. A caller with ten thousand tasks and ten slots
// should not have to implement backoff.
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	if s.q == nil {
		s.writeAPIError(w, r, notFoundError(CodeRouteNotFound, "queue", "this node takes no queued work"))
		return
	}

	var params apitypes.TaskCreateParams
	if err := decodeBody(w, r, &params); err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	if params.Image == "" {
		s.writeAPIError(w, r, missingParamError("image"))
		return
	}
	if params.Cmd == "" {
		s.writeAPIError(w, r, missingParamError("cmd"))
		return
	}
	// Priority is a 0-10 dial, not an open int: bounding it keeps one caller from
	// parking itself permanently ahead of everyone with a priority no one else
	// can match, which is the abuse an unbounded field invites.
	if p := deref(params.Priority); p < 0 || p > 10 {
		s.writeAPIError(w, r, invalidParamError("priority", "must be between 0 and 10"))
		return
	}

	task := queue.Task{
		ID:          id.New(id.TaskPrefix),
		Image:       params.Image,
		Cmd:         params.Cmd,
		Timeout:     time.Duration(deref(params.TimeoutSeconds)) * time.Second,
		Network:     deref(params.Network),
		VCPUs:       deref(params.Vcpus),
		MemMiB:      deref(params.MemMib),
		CPUCores:    deref(params.CpuCores),
		Priority:    deref(params.Priority),
		MaxAttempts: deref(params.MaxAttempts),
	}
	if params.Args != nil {
		task.Args = *params.Args
	}
	if params.Env != nil {
		task.Env = *params.Env
	}
	if params.Files != nil {
		task.Files = make(map[string][]byte, len(*params.Files))
		for path, content := range *params.Files {
			task.Files[path] = content
		}
	}

	if err := s.q.Enqueue(r.Context(), task); err != nil {
		// A duplicate cannot happen here -- the ID is server-generated, so it is
		// new by construction -- but the queue is the authority on that, and an
		// adapter that decides for it would be wrong the day that changes.
		// Retry safety comes from Idempotency-Key instead, which covers the
		// whole request rather than one field of it.
		if errors.Is(err, queue.ErrDuplicate) {
			s.writeAPIError(w, r, conflictError(CodeAlreadyExists, err.Error()))
			return
		}
		s.writeAPIError(w, r, queueUnreachableError(err))
		return
	}

	writeJSON(w, http.StatusAccepted, apitypes.Task{
		Id:      task.ID,
		Object:  apitypes.TaskObjectTask,
		Status:  apitypes.TaskStatusPending,
		Created: ptr(task.EnqueuedAt),
	})
}

func (s *Server) handleRetrieveTask(w http.ResponseWriter, r *http.Request) {
	if s.q == nil {
		s.writeAPIError(w, r, notFoundError(CodeRouteNotFound, "queue", "this node takes no queued work"))
		return
	}

	taskID := r.PathValue("task")
	if err := id.Parse(taskID, id.TaskPrefix); err != nil {
		s.writeAPIError(w, r, invalidParamError("task", err.Error()))
		return
	}

	res, ok, err := s.q.Result(r.Context(), taskID)
	if err != nil {
		s.writeAPIError(w, r, queueUnreachableError(err))
		return
	}
	if !ok {
		// No result yet. From a shared queue this node cannot tell "queued",
		// "running" and "never submitted" apart, and inventing a distinction it
		// cannot see would be worse than the honest answer.
		writeJSON(w, http.StatusOK, apitypes.Task{
			Id:     taskID,
			Object: apitypes.TaskObjectTask,
			Status: apitypes.TaskStatusPending,
		})
		return
	}

	writeJSON(w, http.StatusOK, toAPITask(taskID, res))
}

func toAPITask(taskID string, res queue.Result) apitypes.Task {
	out := apitypes.Task{
		Id:              taskID,
		Object:          apitypes.TaskObjectTask,
		ExitCode:        res.ExitCode,
		Stdout:          ptr(string(res.Stdout)),
		Stderr:          ptr(string(res.Stderr)),
		Attempts:        ptr(res.Attempts),
		ActiveCpuMs:     ptr(int(res.ActiveCPU.Milliseconds())),
		MemoryPeakBytes: ptr(int(res.MemoryPeak)),
	}

	// An infrastructure failure and a non-zero exit are different outcomes: one
	// is ours to answer for, the other is the caller's own code doing exactly
	// what they wrote. A task whose process exits 1 is done, not failed.
	out.Status = apitypes.TaskStatusDone
	if res.Error != "" {
		out.Status = apitypes.TaskStatusFailed
		out.Error = ptr(res.Error)
	}

	if !res.StartedAt.IsZero() {
		out.Started = &res.StartedAt
	}
	if !res.FinishedAt.IsZero() {
		out.Finished = &res.FinishedAt
	}
	return out
}
