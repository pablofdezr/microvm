package microvm

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
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

// GetOrCreate returns your running sandbox of the given name if one exists, and
// otherwise creates it from params.
//
// It is how a stable identity outlives a single process: name a sandbox once,
// then reach "the one called build" again from anywhere without storing its id.
// name is filled in for you and get_or_create is set; the rest of params is the
// shape used only when there is nothing to reuse, and ignored when a sandbox of
// that name is already running. Two callers racing this with the same name get
// the same sandbox, not two.
func (s *SandboxService) GetOrCreate(ctx context.Context, name string, params SandboxCreateParams, opts ...RequestOption) (*Sandbox, error) {
	params.Name = &name
	getOrCreate := true
	params.GetOrCreate = &getOrCreate
	var out Sandbox
	err := s.c.do(ctx, http.MethodPost, "/sandboxes", params, &out, opts...)
	return &out, err
}

// TarballSource builds the Source of a create that seeds the sandbox from a tar
// or tar.gz archive.
//
// stripComponents is almost always 1: a release tarball wraps everything in one
// directory named after the version, and without dropping it the project arrives
// a level deeper than every path inside it expects. Zero is left out of the
// request rather than sent, since it is the server's own default.
//
// The daemon fetches and expands the archive on the host and writes the tree in
// over the endpoints an upload uses, so nothing in the guest touches the network
// and a sandbox created with Network false is seeded just the same. It is refused
// until an operator enables fetching and allowlists the host -- see
// IsSourceNotPermitted.
func TarballSource(url string, stripComponents int) *SandboxSourceParams {
	p := &SandboxSourceParams{Type: SandboxSourceParamsTypeTarball, Url: url}
	if stripComponents > 0 {
		p.StripComponents = &stripComponents
	}
	return p
}

// GitSource builds the Source of a create that seeds the sandbox from a git
// clone. An empty ref takes the remote's default branch.
//
// Prefer a commit SHA: it is the only one of branch, tag and SHA that resolves to
// the same code twice, and the sandbox reports back the commit it actually got in
// Source.Commit.
//
// For a private repository set CredentialRef on the result to a name the operator
// configured. The name travels and the secret never does -- the clone happens on
// the host and only the working tree is copied in, .git and credential both left
// behind, which is the whole reason this is not a clone inside the guest.
func GitSource(url, ref string) *SandboxSourceParams {
	p := &SandboxSourceParams{Type: SandboxSourceParamsTypeGit, Url: url}
	if ref != "" {
		p.Ref = &ref
	}
	return p
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

// Extend pushes the sandbox's TTL deadline out to ttlSeconds from now.
//
// It never brings a deadline forward, so heartbeating it on a timer is safe and
// so is retrying it. Read Expires off the returned sandbox rather than computing
// it: the request says how long you want, and the reply says what you have. It
// fails with an invalid-request error when the sandbox has less life left than
// you asked for -- lifetimes are bounded from creation, so extending buys time
// and never immortality.
func (s *SandboxService) Extend(ctx context.Context, sandboxID string, ttlSeconds int, opts ...RequestOption) (*Sandbox, error) {
	var out Sandbox
	err := s.c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/extend",
		SandboxExtendParams{TtlSeconds: ttlSeconds}, &out, append(opts, replayable)...)
	return &out, err
}

// Suspend snapshots the sandbox's VM to disk and tears the VM down, leaving it
// resumable under the same id with Resume.
//
// A suspended sandbox costs no CPU or memory, only the snapshot, and keeps its
// slot and name so a later Resume is guaranteed both. It is kept until you resume
// or delete it, unlike a stopped sandbox. An execution in flight is refused with a
// conflict (see IsConflict); finish or cancel it first. It needs a node configured
// for snapshots.
func (s *SandboxService) Suspend(ctx context.Context, sandboxID string, opts ...RequestOption) (*Sandbox, error) {
	var out Sandbox
	err := s.c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/suspend", nil, &out, opts...)
	return &out, err
}

// Resume boots a fresh VM from a suspended sandbox's snapshot and returns it
// running under the same id, with a fresh TTL window. Only a suspended sandbox can
// be resumed; a running or stopped one is a conflict.
func (s *SandboxService) Resume(ctx context.Context, sandboxID string, opts ...RequestOption) (*Sandbox, error) {
	var out Sandbox
	err := s.c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/resume", nil, &out, opts...)
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
	// Tags returns only sandboxes carrying every one of these labels. They AND,
	// so a second tag narrows the result rather than widening it. The filter is
	// node-local, like the list itself.
	Tags map[string]string
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
	// One `tag=key:value` each, in key order: map order is random, and a filter
	// that produces a different URL on every identical call is one nobody can
	// cache, compare or read in a log.
	for _, k := range slices.Sorted(maps.Keys(p.Tags)) {
		q.Add("tag", k+":"+p.Tags[k])
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

// Resize changes a running tty execution's terminal size, so a full-screen
// program inside repaints to the new dimensions. Wire it to your own terminal's
// resize event to keep the two in step.
//
// Only for an execution started with Tty set: a plain one has no terminal and is
// refused with a conflict (see IsConflict). A resize is a set, not an increment,
// so retrying it lands on the same window and is safe.
func (s *ExecutionService) Resize(ctx context.Context, sandboxID, executionID string, rows, cols int, opts ...RequestOption) error {
	return s.c.do(ctx, http.MethodPost,
		"/sandboxes/"+sandboxID+"/executions/"+executionID+"/resize",
		ExecutionResizeParams{Rows: rows, Cols: cols}, nil, append(opts, replayable)...)
}

// FileService is the /v1/sandboxes/{sandbox}/files resource.
type FileService struct{ c *Client }

// Create writes a file into the sandbox, making parent directories.
func (s *FileService) Create(ctx context.Context, sandboxID string, params FileCreateParams) (*File, error) {
	var out File
	err := s.c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/files", params, &out, replayable)
	return &out, err
}

// Write is Create for the common case: a path and some bytes.
func (s *FileService) Write(ctx context.Context, sandboxID, path string, content []byte) (*File, error) {
	return s.Create(ctx, sandboxID, FileCreateParams{Path: path, Content: content})
}

// CreateBatch writes a set of files in one request, in the order given.
//
// One round trip instead of one per file, which is the difference between staging
// a project and staging it slowly. Validation is all-or-nothing and writing is
// not, because writing cannot be: a batch with one bad mode writes nothing, and a
// batch that fails partway names the entry it stopped at, so the order tells you
// which files landed. Each path may be named only once.
func (s *FileService) CreateBatch(ctx context.Context, sandboxID string, files []FileCreateParams) (*FileList, error) {
	var out FileList
	err := s.c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/files/batch",
		FileBatchCreateParams{Files: files}, &out, replayable)
	return &out, err
}

// Mkdir creates a directory and its parents.
//
// Uploading a file already creates its parents, so this is for the directory that
// stays empty: somewhere a command writes its output, or a layout a build tool
// expects before it starts. It is mkdir -p, so a directory that already exists is
// success and a retry is safe; a *file* in the way is a conflict.
func (s *FileService) Mkdir(ctx context.Context, sandboxID, path string) (*Directory, error) {
	var out Directory
	err := s.c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/dirs",
		DirectoryCreateParams{Path: path}, &out, replayable)
	return &out, err
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
