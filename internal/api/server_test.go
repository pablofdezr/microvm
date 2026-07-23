package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/queue"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

const testToken = "test-token"

type harness struct {
	srv *httptest.Server
	rt  *runtimetest.Runtime
	q   queue.Queue
	mgr *sandbox.Manager
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	rt := runtimetest.New()
	rt.Script["python3"] = runtimetest.Output{Stdout: "hello\n"}

	logs := logstore.New(logstore.Config{})
	mgr := sandbox.NewManager(rt, logs, log)
	q := queue.NewMemory(queue.MemoryConfig{}, log)

	api := NewServer(Config{
		Tokens: []string{testToken},
		Images: []string{"python", "node"},
	}, mgr, q, nil, log)

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = q.Close()
		_ = mgr.Close(t.Context())
	})

	return &harness{srv: srv, rt: rt, q: q, mgr: mgr}
}

func (h *harness) do(t *testing.T, method, path string, body any, headers ...string) *http.Response {
	t.Helper()

	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(t.Context(), method, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var out T
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %T from %s: %v\nbody: %s", out, resp.Request.URL.Path, err, raw)
	}
	return out
}

func (h *harness) createSandbox(t *testing.T) apitypes.Sandbox {
	t.Helper()
	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python"})
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create sandbox: %d: %s", resp.StatusCode, raw)
	}
	return decode[apitypes.Sandbox](t, resp)
}

// --- resource shape ---------------------------------------------------------

// Every resource carries its type and a prefixed ID. It is what lets a caller
// tell one opaque string from another, in a log or a screenshot.
func TestResourcesAreSelfDescribing(t *testing.T) {
	h := newHarness(t)

	sb := h.createSandbox(t)
	if sb.Object != apitypes.SandboxObjectSandbox {
		t.Errorf("object = %q, want sandbox", sb.Object)
	}
	if !strings.HasPrefix(sb.Id, "sb_") {
		t.Errorf("id = %q, want an sb_ prefix", sb.Id)
	}
	if sb.Image != "python" {
		t.Errorf("image = %q, want python: the image was dropped somewhere", sb.Image)
	}

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions",
		map[string]any{"cmd": "python3", "args": []string{"main.py"}})
	exe := decode[apitypes.Execution](t, resp)
	if exe.Object != apitypes.ExecutionObjectExecution {
		t.Errorf("object = %q, want execution", exe.Object)
	}
	if !strings.HasPrefix(exe.Id, "exe_") {
		t.Errorf("id = %q, want an exe_ prefix", exe.Id)
	}
	if exe.Sandbox != sb.Id {
		t.Errorf("sandbox = %q, want %q", exe.Sandbox, sb.Id)
	}
}

// The list envelope is the one every list shares, so a client writes its paging
// once rather than per endpoint.
func TestListsAreEnveloped(t *testing.T) {
	h := newHarness(t)
	h.createSandbox(t)

	resp := h.do(t, "GET", "/v1/sandboxes", nil)
	list := decode[apitypes.SandboxList](t, resp)

	if list.Object != apitypes.SandboxListObjectList {
		t.Errorf("object = %q, want list", list.Object)
	}
	if list.Url != "/v1/sandboxes" {
		t.Errorf("url = %q", list.Url)
	}
	if len(list.Data) != 1 {
		t.Errorf("data has %d items, want 1", len(list.Data))
	}
}

// --- errors -----------------------------------------------------------------

// Errors are the part of an API developers meet on their worst day, so the
// shape has to be reliable: one envelope, always, including for the routes we
// never wrote.
func TestErrorsAreEnveloped(t *testing.T) {
	h := newHarness(t)

	tests := []struct {
		name       string
		method     string
		path       string
		body       any
		wantStatus int
		wantType   apitypes.ErrorType
		wantCode   string
	}{
		{
			name: "unknown sandbox", method: "GET", path: "/v1/sandboxes/sb_01JZ8QK3M4N5P6R7S8T9V0W1X2",
			wantStatus: 404, wantType: apitypes.ErrorTypeInvalidRequestError, wantCode: CodeSandboxNotFound,
		},
		{
			name: "malformed id", method: "GET", path: "/v1/sandboxes/not-an-id",
			wantStatus: 400, wantType: apitypes.ErrorTypeInvalidRequestError, wantCode: CodeParameterInvalid,
		},
		{
			name: "an execution id in a sandbox slot", method: "GET", path: "/v1/sandboxes/exe_01JZ8QK3M4N5P6R7S8T9V0W1X2",
			wantStatus: 400, wantType: apitypes.ErrorTypeInvalidRequestError, wantCode: CodeParameterInvalid,
		},
		{
			name: "missing image", method: "POST", path: "/v1/sandboxes", body: map[string]any{},
			wantStatus: 400, wantType: apitypes.ErrorTypeInvalidRequestError, wantCode: CodeParameterMissing,
		},
		{
			name: "misspelled field", method: "POST", path: "/v1/sandboxes",
			body:       map[string]any{"image": "python", "mem_mib_typo": 256},
			wantStatus: 400, wantType: apitypes.ErrorTypeInvalidRequestError, wantCode: CodeBodyInvalid,
		},
		{
			name: "unknown route", method: "GET", path: "/v1/nonsense",
			wantStatus: 404, wantType: apitypes.ErrorTypeInvalidRequestError, wantCode: CodeRouteNotFound,
		},
		{
			name: "unknown task", method: "GET", path: "/v1/tasks/tsk_01JZ8QK3M4N5P6R7S8T9V0W1X2",
			wantStatus: 200, // no result yet is not an error; see the comment on the handler
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(t, tc.method, tc.path, tc.body)
			if resp.StatusCode != tc.wantStatus {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.wantStatus, raw)
			}
			if tc.wantCode == "" {
				return
			}

			env := decode[apitypes.ErrorEnvelope](t, resp)
			if env.Error.Type != tc.wantType {
				t.Errorf("type = %q, want %q", env.Error.Type, tc.wantType)
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
			if env.Error.Message == "" {
				t.Error("message is empty; a caller has nothing to read")
			}
			if env.Error.RequestId == nil || *env.Error.RequestId == "" {
				t.Error("no request_id: an error a caller cannot quote is an error we cannot find")
			}
		})
	}
}

// A 404 that does not say what it could not find leaves a caller unable to tell
// a typo from a deleted object.
func TestNotFoundQuotesTheId(t *testing.T) {
	h := newHarness(t)
	const missing = "sb_01JZ8QK3M4N5P6R7S8T9V0W1X2"

	resp := h.do(t, "GET", "/v1/sandboxes/"+missing, nil)
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if !strings.Contains(env.Error.Message, missing) {
		t.Errorf("message = %q, want it to quote %s", env.Error.Message, missing)
	}
}

func TestAuth(t *testing.T) {
	h := newHarness(t)

	tests := []struct {
		name     string
		header   string
		wantCode string
	}{
		{"no header", "", CodeTokenMissing},
		{"not a bearer token", "Basic abc", CodeTokenMissing},
		{"wrong token", "Bearer wrong", CodeTokenInvalid},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(t.Context(), "GET", h.srv.URL+"/v1/sandboxes", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := h.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			env := decode[apitypes.ErrorEnvelope](t, resp)
			if env.Error.Type != apitypes.ErrorTypeAuthenticationError {
				t.Errorf("type = %q, want authentication_error", env.Error.Type)
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

// Health is the one route that must work without credentials, or nothing can
// check whether the daemon is up.
func TestHealthNeedsNoToken(t *testing.T) {
	h := newHarness(t)

	resp, err := h.srv.Client().Get(h.srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	health := decode[apitypes.Health](t, resp)
	if !health.Ok {
		t.Error("ok = false")
	}
}

// --- capacity ---------------------------------------------------------------

// A full node and a broken node are different things, and a client must be able
// to tell them apart without reading prose.
func TestAFullNodeReportsCapacityNotFailure(t *testing.T) {
	h := newHarness(t)
	h.rt.CreateErr = errFull{}

	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Type != apitypes.ErrorTypeCapacityError {
		t.Errorf("type = %q, want capacity_error: a client cannot tell to back off", env.Error.Type)
	}
	// The internals must not leak: the caller learns they should retry, not how
	// our allocator is built.
	if strings.Contains(env.Error.Message, "internal") {
		t.Errorf("message leaks internals: %q", env.Error.Message)
	}
}

type errFull struct{}

func (errFull) Error() string { return "no free network slot: internal pool exhausted" }

// --- idempotency ------------------------------------------------------------

// The problem a key solves has no client-side fix: a request whose reply is
// lost cannot be known to have happened.
func TestIdempotencyKeyReplaysRatherThanRepeats(t *testing.T) {
	h := newHarness(t)

	first := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python"},
		"Idempotency-Key", "key-1")
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first create: %d", first.StatusCode)
	}
	sb1 := decode[apitypes.Sandbox](t, first)

	// The caller never saw the reply and retries.
	second := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python"},
		"Idempotency-Key", "key-1")
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("retry: %d", second.StatusCode)
	}
	sb2 := decode[apitypes.Sandbox](t, second)

	if sb1.Id != sb2.Id {
		t.Errorf("retry produced a different sandbox (%s vs %s): the caller now has two "+
			"VMs and knows about one", sb1.Id, sb2.Id)
	}
	if second.Header.Get("Idempotent-Replayed") != "true" {
		t.Error("a replayed reply is not marked as one")
	}
	if got := h.rt.Created(); got != 1 {
		t.Errorf("the runtime built %d sandboxes for one logical request, want 1", got)
	}
}

// A key reused with a different body means the client thinks it is retrying
// something it is not. Silently replaying the old answer would be the worst
// possible response.
func TestIdempotencyKeyReusedWithADifferentBody(t *testing.T) {
	h := newHarness(t)

	h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python"}, "Idempotency-Key", "key-2")

	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "node"}, "Idempotency-Key", "key-2")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Type != apitypes.ErrorTypeIdempotencyError {
		t.Errorf("type = %q, want idempotency_error", env.Error.Type)
	}
}

// Two tenants picking the same key is not a coincidence to plan around; "1" is
// an obvious key. They must not see each other's replies.
func TestIdempotencyIsScopedToTheToken(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := runtimetest.New()
	logs := logstore.New(logstore.Config{})
	mgr := sandbox.NewManager(rt, logs, log)
	api := NewServer(Config{Tokens: []string{"tenant-a", "tenant-b"}}, mgr, nil, nil, log)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	defer mgr.Close(t.Context())

	create := func(token string) apitypes.Sandbox {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"image": "python"})
		req, _ := http.NewRequestWithContext(t.Context(), "POST", srv.URL+"/v1/sandboxes", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "1")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("create as %s: %d: %s", token, resp.StatusCode, raw)
		}
		return decode[apitypes.Sandbox](t, resp)
	}

	a := create("tenant-a")
	b := create("tenant-b")

	if a.Id == b.Id {
		t.Fatal("two tenants using the key \"1\" got the same sandbox: one tenant is " +
			"holding a handle on another's VM")
	}
}

// Pinning a failure to a key would burn it: the one retry that could have
// worked would replay the failure forever.
func TestIdempotencyDoesNotCacheFailures(t *testing.T) {
	h := newHarness(t)
	h.rt.CreateErr = errFull{}

	first := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python"},
		"Idempotency-Key", "key-3")
	if first.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("first: %d, want 429", first.StatusCode)
	}

	// Whatever was wrong is now fixed, and the caller retries with the same key.
	h.rt.CreateErr = nil
	second := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python"},
		"Idempotency-Key", "key-3")
	if second.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(second.Body)
		t.Fatalf("retry after a transient failure: %d: %s -- the key cached the failure, "+
			"so the caller can never succeed with it", second.StatusCode, raw)
	}
}

// --- executions -------------------------------------------------------------

// Creating an execution must not wait for it. It is what lets a caller start
// something long and watch it separately -- and what stops a dropped connection
// from killing the work.
func TestCreateExecutionReturnsBeforeTheCommandFinishes(t *testing.T) {
	h := newHarness(t)
	h.rt.Script["slow"] = runtimetest.Output{Stdout: "eventually\n", Delay: 2 * time.Second}
	sb := h.createSandbox(t)

	start := time.Now()
	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions", map[string]any{"cmd": "slow"})
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if elapsed > time.Second {
		t.Errorf("create took %v: it waited for the command instead of starting it", elapsed)
	}

	exe := decode[apitypes.Execution](t, resp)
	if exe.Status != apitypes.ExecutionStatusRunning {
		t.Errorf("status = %q, want running", exe.Status)
	}
}

func TestRetrieveExecutionCollectsOutput(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions", map[string]any{"cmd": "python3"})
	exe := decode[apitypes.Execution](t, resp)

	// Poll: the command runs in the background now.
	var final apitypes.Execution
	for i := 0; i < 100; i++ {
		time.Sleep(20 * time.Millisecond)
		got := h.do(t, "GET", "/v1/sandboxes/"+sb.Id+"/executions/"+exe.Id, nil)
		final = decode[apitypes.Execution](t, got)
		if final.Status != apitypes.ExecutionStatusRunning {
			break
		}
	}

	if final.Status != apitypes.ExecutionStatusExited {
		t.Fatalf("status = %q, want exited", final.Status)
	}
	if final.Stdout != "hello\n" {
		t.Errorf("stdout = %q, want %q", final.Stdout, "hello\n")
	}
	if final.ExitCode == nil || *final.ExitCode != 0 {
		t.Errorf("exit_code = %v, want 0", final.ExitCode)
	}
}

// The nesting has to be real. A valid execution ID quoted under the wrong
// sandbox must not resolve, or one tenant can read another's output by
// guessing.
func TestAnExecutionCannotBeReadThroughTheWrongSandbox(t *testing.T) {
	h := newHarness(t)
	owner := h.createSandbox(t)
	other := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+owner.Id+"/executions", map[string]any{"cmd": "python3"})
	exe := decode[apitypes.Execution](t, resp)

	got := h.do(t, "GET", "/v1/sandboxes/"+other.Id+"/executions/"+exe.Id, nil)
	if got.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404: an execution resolved through a sandbox that "+
			"does not own it", got.StatusCode)
	}
}

func TestCancelExecutionSignalsTheProcess(t *testing.T) {
	h := newHarness(t)
	h.rt.Script["forever"] = runtimetest.Output{Block: true}
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions", map[string]any{"cmd": "forever"})
	exe := decode[apitypes.Execution](t, resp)

	cancel := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions/"+exe.Id+"/cancel", nil)
	if cancel.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(cancel.Body)
		t.Fatalf("cancel: %d: %s", cancel.StatusCode, raw)
	}
	got := decode[apitypes.Execution](t, cancel)
	if got.Status != apitypes.ExecutionStatusCanceled {
		t.Errorf("status = %q, want canceled", got.Status)
	}

	// It must actually have signalled, not merely answered 200.
	inst, ok := h.rt.Instance(sb.Id)
	if !ok {
		t.Fatal("no instance")
	}
	sent := inst.SignalsSent()
	if len(sent) != 1 {
		t.Fatalf("delivered %d signals, want 1", len(sent))
	}
	if sent[0].Signal != "SIGKILL" {
		t.Errorf("signal = %q, want SIGKILL: hostile code must not get a signal it can ignore",
			sent[0].Signal)
	}
}

// --- files ------------------------------------------------------------------

func TestFileRoundTrip(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	const body = "print('hi')\n"
	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files",
		map[string]any{"path": "/app/main.py", "content": []byte(body)})
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: %d: %s", resp.StatusCode, raw)
	}
	file := decode[apitypes.File](t, resp)
	if file.SizeBytes != len(body) {
		t.Errorf("size = %d, want %d", file.SizeBytes, len(body))
	}

	got := h.do(t, "GET", "/v1/sandboxes/"+sb.Id+"/files?path=/app/main.py", nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("download: %d", got.StatusCode)
	}
	raw, _ := io.ReadAll(got.Body)
	if string(raw) != body {
		t.Errorf("downloaded %q, want %q", raw, body)
	}
}

// --- sandbox lifecycle ------------------------------------------------------

// Deleting returns the final cost, because after this the cgroup is gone and
// nobody can ever ask again.
func TestDeleteReturnsTheFinalCost(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "DELETE", "/v1/sandboxes/"+sb.Id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the sandbox", resp.StatusCode)
	}

	got := decode[apitypes.Sandbox](t, resp)
	if got.State != apitypes.SandboxStateStopped {
		t.Errorf("state = %q, want stopped", got.State)
	}
	if got.StopReason == nil || *got.StopReason != apitypes.SandboxStopReasonStopped {
		t.Errorf("stop_reason = %v, want stopped", got.StopReason)
	}
	if got.Stats.ActiveCpuMs == 0 {
		t.Error("active_cpu_ms = 0 on delete: the final cost was lost with the VM, and " +
			"there is no second chance to read it")
	}
}

// Work aimed at a dead sandbox gets a reason, not a transport error from a VM
// that is not there.
func TestExecutingInAStoppedSandbox(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)
	h.do(t, "DELETE", "/v1/sandboxes/"+sb.Id, nil)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions", map[string]any{"cmd": "python3"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeSandboxNotRunning {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeSandboxNotRunning)
	}
	if !strings.Contains(env.Error.Message, "stopped") {
		t.Errorf("message = %q, want it to say why", env.Error.Message)
	}
}

// The output of a run is most wanted exactly when its VM was taken away.
func TestOutputSurvivesTheSandbox(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions", map[string]any{"cmd": "python3"})
	exe := decode[apitypes.Execution](t, resp)

	// Let it finish, then take the VM away.
	for i := 0; i < 100; i++ {
		time.Sleep(20 * time.Millisecond)
		got := h.do(t, "GET", "/v1/sandboxes/"+sb.Id+"/executions/"+exe.Id, nil)
		if decode[apitypes.Execution](t, got).Status != apitypes.ExecutionStatusRunning {
			break
		}
	}
	h.do(t, "DELETE", "/v1/sandboxes/"+sb.Id, nil)

	got := h.do(t, "GET", "/v1/sandboxes/"+sb.Id+"/executions/"+exe.Id, nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: the output died with the VM", got.StatusCode)
	}
	final := decode[apitypes.Execution](t, got)
	if final.Stdout != "hello\n" {
		t.Errorf("stdout = %q, want it to have survived", final.Stdout)
	}
}
