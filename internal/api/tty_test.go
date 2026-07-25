package api

import (
	"io"
	"net/http"
	"testing"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
)

// A tty execution is accepted like any other: the terminal lives in the guest,
// so from the API's side tty is just a flag on the create.
func TestCreateTTYExecution(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions",
		map[string]any{"cmd": "python3", "tty": true, "rows": 40, "cols": 120})
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("tty exec: %d, want 201: %s", resp.StatusCode, raw)
	}
}

// Resizing a running tty execution reaches the guest, not merely answers 204.
func TestResizeExecutionReachesTheGuest(t *testing.T) {
	h := newHarness(t)
	// A command that keeps running, so the execution is still alive to resize.
	h.rt.Script["top"] = runtimetest.Output{Block: true}
	sb := h.createSandbox(t)

	exe := decode[apitypes.Execution](t,
		h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions",
			map[string]any{"cmd": "top", "tty": true}))

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions/"+exe.Id+"/resize",
		map[string]any{"rows": 50, "cols": 200})
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("resize: %d, want 204: %s", resp.StatusCode, raw)
	}

	inst, ok := h.rt.Instance(sb.Id)
	if !ok {
		t.Fatal("no instance")
	}
	got := inst.ResizesSent()
	if len(got) != 1 {
		t.Fatalf("delivered %d resizes, want 1", len(got))
	}
	if got[0].Rows != 50 || got[0].Cols != 200 {
		t.Errorf("resize was %dx%d, want 50x200", got[0].Rows, got[0].Cols)
	}
}

// A window dimension outside a terminal's range is refused before it reaches the
// guest.
func TestResizeRejectsAnOutOfRangeDimension(t *testing.T) {
	h := newHarness(t)
	h.rt.Script["top"] = runtimetest.Output{Block: true}
	sb := h.createSandbox(t)

	exe := decode[apitypes.Execution](t,
		h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions",
			map[string]any{"cmd": "top", "tty": true}))

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions/"+exe.Id+"/resize",
		map[string]any{"rows": 70000, "cols": 80})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeParameterInvalid {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeParameterInvalid)
	}
}
