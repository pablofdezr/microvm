package microvm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// SandboxService is the /v1/sandboxes resource.
type SandboxService struct{ c *Client }

// Create boots a sandbox and returns once it is ready to run commands.
//
// It fails with a capacity error when the node is full -- see IsCapacity. That
// is not a fault: a sandbox is a reservation, so the caller is told at once
// rather than left waiting. If waiting is what you want, submit a task.
func (s *SandboxService) Create(ctx context.Context, params SandboxCreateParams, opts ...RequestOption) (*Sandbox, error) {
	var out Sandbox
	err := s.c.do(ctx, http.MethodPost, "/sandboxes", params, &out, opts...)
	return &out, err
}

// Retrieve returns a sandbox with live metering.
func (s *SandboxService) Retrieve(ctx context.Context, sandboxID string, opts ...RequestOption) (*Sandbox, error) {
	var out Sandbox
	err := s.c.do(ctx, http.MethodGet, "/sandboxes/"+sandboxID, nil, &out, opts...)
	return &out, err
}

// Delete kills the sandbox and returns it with its final cost.
//
// Those numbers are sampled just before the kill and cannot be had afterwards:
// the accounting dies with the VM. This reply is the only record of what the
// sandbox consumed, so it is worth reading even when you are only cleaning up.
func (s *SandboxService) Delete(ctx context.Context, sandboxID string, opts ...RequestOption) (*Sandbox, error) {
	var out Sandbox
	err := s.c.do(ctx, http.MethodDelete, "/sandboxes/"+sandboxID, nil, &out, opts...)
	return &out, err
}

// SandboxListParams filters and pages a list.
type SandboxListParams struct {
	// Limit is how many per page, 1-100. Zero uses the server's default.
	Limit int
	// StartingAfter pages forward from an ID.
	StartingAfter string
	// EndingBefore pages backward from an ID.
	EndingBefore string
	// State returns only sandboxes in it: "running" or "stopped".
	State SandboxState
}

func (p SandboxListParams) query() string {
	q := url.Values{}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.StartingAfter != "" {
		q.Set("starting_after", p.StartingAfter)
	}
	if p.EndingBefore != "" {
		q.Set("ending_before", p.EndingBefore)
	}
	if p.State != "" {
		q.Set("state", string(p.State))
	}
	return q.Encode()
}

// List returns one page of sandboxes, newest first.
//
// For everything rather than a page, use All, which handles the cursors.
func (s *SandboxService) List(ctx context.Context, params SandboxListParams) (*SandboxList, error) {
	var out SandboxList
	err := s.c.do(ctx, http.MethodGet, "/sandboxes", nil, &out,
		func(o *requestOptions) { o.query = params.query() })
	return &out, err
}

// All iterates every sandbox, fetching pages as it goes.
//
// This is the auto-paging half of the SDK's job. Paging is mechanical and easy
// to get subtly wrong -- forgetting has_more, or taking the cursor from the
// wrong end -- and it is the same loop every time, so it is written once here
// rather than in every caller.
//
//	for sb, err := range client.Sandboxes.All(ctx, params) {
//	    if err != nil { return err }
//	    ...
//	}
func (s *SandboxService) All(ctx context.Context, params SandboxListParams) func(yield func(Sandbox, error) bool) {
	return func(yield func(Sandbox, error) bool) {
		for {
			page, err := s.List(ctx, params)
			if err != nil {
				yield(Sandbox{}, err)
				return
			}
			for _, sb := range page.Data {
				if !yield(sb, nil) {
					return
				}
			}
			if !page.HasMore || len(page.Data) == 0 {
				return
			}
			// The next page starts after the last item on this one. Paging
			// backwards is not supported here: mixing the two directions in one
			// walk has no meaning.
			params.StartingAfter = page.Data[len(page.Data)-1].Id
			params.EndingBefore = ""
		}
	}
}

// ExecutionService is the /v1/sandboxes/{sandbox}/executions resource.
type ExecutionService struct{ c *Client }

// Create starts a command and returns immediately, without waiting for it.
//
// The command belongs to the sandbox, not to this call: dropping the connection
// does not kill it. Follow it with Stream, or collect it later with Retrieve.
func (s *ExecutionService) Create(ctx context.Context, sandboxID string, params ExecutionCreateParams, opts ...RequestOption) (*Execution, error) {
	var out Execution
	err := s.c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/executions", params, &out, opts...)
	return &out, err
}

// Retrieve returns an execution and everything it printed.
//
// It works after the sandbox is gone, which is the point: the output you most
// want is from the run that was killed.
func (s *ExecutionService) Retrieve(ctx context.Context, sandboxID, executionID string, opts ...RequestOption) (*Execution, error) {
	var out Execution
	err := s.c.do(ctx, http.MethodGet, "/sandboxes/"+sandboxID+"/executions/"+executionID, nil, &out, opts...)
	return &out, err
}

// List returns one page of a sandbox's executions, newest first.
func (s *ExecutionService) List(ctx context.Context, sandboxID string, params SandboxListParams) (*ExecutionList, error) {
	var out ExecutionList
	err := s.c.do(ctx, http.MethodGet, "/sandboxes/"+sandboxID+"/executions", nil, &out,
		func(o *requestOptions) { o.query = params.query() })
	return &out, err
}

// All iterates every execution of a sandbox, following pagination so the caller
// does not thread cursors by hand. As with SandboxService.All, an error ends the
// range after being yielded once, and returning false from the loop stops it.
func (s *ExecutionService) All(ctx context.Context, sandboxID string, params SandboxListParams) func(yield func(Execution, error) bool) {
	return func(yield func(Execution, error) bool) {
		for {
			page, err := s.List(ctx, sandboxID, params)
			if err != nil {
				yield(Execution{}, err)
				return
			}
			for _, e := range page.Data {
				if !yield(e, nil) {
					return
				}
			}
			if !page.HasMore || len(page.Data) == 0 {
				return
			}
			params.StartingAfter = page.Data[len(page.Data)-1].Id
			params.EndingBefore = ""
		}
	}
}

// Cancel signals a running execution.
//
// The signal reaches the whole process group, so a program that spawned
// children does not leave them behind. It defaults to SIGKILL. Cancelling
// something that already finished is not an error.
func (s *ExecutionService) Cancel(ctx context.Context, sandboxID, executionID string, params ExecutionCancelParams) (*Execution, error) {
	var out Execution
	err := s.c.do(ctx, http.MethodPost,
		"/sandboxes/"+sandboxID+"/executions/"+executionID+"/cancel", params, &out)
	return &out, err
}

// FileService is the /v1/sandboxes/{sandbox}/files resource.
type FileService struct{ c *Client }

// Create writes a file into the sandbox, making parent directories.
func (s *FileService) Create(ctx context.Context, sandboxID string, params FileCreateParams) (*File, error) {
	var out File
	err := s.c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/files", params, &out)
	return &out, err
}

// Write is Create for the common case: a path and some bytes.
func (s *FileService) Write(ctx context.Context, sandboxID, path string, content []byte) (*File, error) {
	return s.Create(ctx, sandboxID, FileCreateParams{Path: path, Content: content})
}

// Retrieve downloads a file.
func (s *FileService) Retrieve(ctx context.Context, sandboxID, path string) ([]byte, error) {
	rc, err := s.Stream(ctx, sandboxID, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Stream downloads a file without buffering it. The caller closes the reader.
func (s *FileService) Stream(ctx context.Context, sandboxID, path string) (io.ReadCloser, error) {
	resp, err := s.c.raw(ctx, http.MethodGet, "/sandboxes/"+sandboxID+"/files", nil,
		func(o *requestOptions) { o.query = url.Values{"path": {path}}.Encode() })
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// TaskService is the /v1/tasks resource.
type TaskService struct{ c *Client }

// Create queues work for the fleet.
//
// Unlike creating a sandbox this never fails for capacity: the task waits for a
// slot on any node. Use it for throughput, and a sandbox for several commands
// that share state.
func (s *TaskService) Create(ctx context.Context, params TaskCreateParams, opts ...RequestOption) (*Task, error) {
	var out Task
	err := s.c.do(ctx, http.MethodPost, "/tasks", params, &out, opts...)
	return &out, err
}

// Retrieve returns a task and its result, if it has one yet.
func (s *TaskService) Retrieve(ctx context.Context, taskID string) (*Task, error) {
	var out Task
	err := s.c.do(ctx, http.MethodGet, "/tasks/"+taskID, nil, &out)
	return &out, err
}

// QueueService is the /v1/queue resource.
type QueueService struct{ c *Client }

// Retrieve returns the queue's depth and this node's slots.
//
// The depth is the fleet's; the slots are this node's alone. No node knows the
// fleet's capacity, which is exactly what lets one be added without telling
// anything else.
func (s *QueueService) Retrieve(ctx context.Context) (*Queue, error) {
	var out Queue
	err := s.c.do(ctx, http.MethodGet, "/queue", nil, &out)
	return &out, err
}

// TenantService is the /v1/tenants resource, and it is administrative: a
// tenant's storage cap is set by an operator, never by the code that runs under
// it. Setting a policy needs an admin token; an ordinary key is refused with a
// 403 (see IsForbidden). Reading a tenant is likewise an admin view -- an
// ordinary caller learns its own cap from the sandbox it creates, not here.
type TenantService struct{ c *Client }

// Update sets a tenant's byte cap and what a write does when it is reached,
// replacing any previous policy. It needs an admin token.
func (s *TenantService) Update(ctx context.Context, tenantID string, params TenantUpdateParams, opts ...RequestOption) (*Tenant, error) {
	var out Tenant
	err := s.c.do(ctx, http.MethodPut, "/tenants/"+tenantID, params, &out, opts...)
	return &out, err
}

// SetLimit is Update for the common case: a byte cap and a policy. Pass
// microvm.Preserve to reject writes when full, or microvm.Evict to delete the
// oldest objects to make room. A maxBytes of 0 means unlimited.
func (s *TenantService) SetLimit(ctx context.Context, tenantID string, maxBytes int64, policy TenantFullPolicy, opts ...RequestOption) (*Tenant, error) {
	return s.Update(ctx, tenantID, TenantUpdateParams{MaxBytes: maxBytes, Policy: policy}, opts...)
}

// Retrieve returns a tenant's policy and its current usage, the usage read live
// from the bucket at call time (so it costs a listing -- see Tenant.UsageBytes).
func (s *TenantService) Retrieve(ctx context.Context, tenantID string, opts ...RequestOption) (*Tenant, error) {
	var out Tenant
	err := s.c.do(ctx, http.MethodGet, "/tenants/"+tenantID, nil, &out, opts...)
	return &out, err
}

// List returns every configured tenant. A tenant with no policy set is absent:
// it is unlimited, and there is nothing to report.
func (s *TenantService) List(ctx context.Context, opts ...RequestOption) (*TenantList, error) {
	var out TenantList
	err := s.c.do(ctx, http.MethodGet, "/tenants", nil, &out, opts...)
	return &out, err
}

// ImageService is the /v1/images resource.
type ImageService struct{ c *Client }

// List returns the language images this node can boot.
func (s *ImageService) List(ctx context.Context) (*ImageList, error) {
	var out ImageList
	err := s.c.do(ctx, http.MethodGet, "/images", nil, &out)
	return &out, err
}

// --- conveniences on the resource types -------------------------------------

// Done reports whether the execution has finished, however it finished.
func (e *Execution) Done() bool { return e.Status != ExecutionStatusRunning }

// Err reports why an execution did not simply run to completion.
//
// It returns nil for a non-zero exit: the process ran and that is its own
// verdict, not a failure of ours. It returns an error for the endings that are
// not the code's doing -- a timeout, a cancel, a VM taken away, a command that
// never started -- because those are the ones a caller must not mistake for a
// program that decided to fail.
func (e *Execution) Err() error {
	switch e.Status {
	case ExecutionStatusRunning, ExecutionStatusExited:
		return nil
	case ExecutionStatusTimedOut:
		return fmt.Errorf("microvm: execution %s exceeded its timeout and was killed", e.Id)
	case ExecutionStatusCanceled:
		return fmt.Errorf("microvm: execution %s was cancelled", e.Id)
	case ExecutionStatusVanished:
		return fmt.Errorf("microvm: the sandbox holding execution %s was taken away "+
			"(its TTL, the idle reclaim, or the VM died); your code did not fail", e.Id)
	case ExecutionStatusFailed:
		msg := "the command could never start"
		if e.Error != nil {
			msg = *e.Error
		}
		return fmt.Errorf("microvm: execution %s: %s", e.Id, msg)
	default:
		return fmt.Errorf("microvm: execution %s has an unknown status %q", e.Id, e.Status)
	}
}

// ExitCodeOr returns the execution's exit code, or def if it has none -- which
// happens whenever the process never got to exit on its own.
func (e *Execution) ExitCodeOr(def int) int {
	if e.ExitCode == nil {
		return def
	}
	return *e.ExitCode
}
