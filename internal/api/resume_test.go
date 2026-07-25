package api

import (
	"io"
	"net/http"
	"testing"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
)

// Suspend then resume round-trips a sandbox through the durable snapshot and back
// to running, under the same id.
func TestSuspendResumeRoundTrip(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	suspended := decode[apitypes.Sandbox](t, h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/suspend", nil))
	if suspended.State != apitypes.SandboxStateSuspended {
		t.Fatalf("state = %q, want suspended", suspended.State)
	}

	resumed := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/resume", nil)
	if resumed.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resumed.Body)
		t.Fatalf("resume: %d, want 200: %s", resumed.StatusCode, raw)
	}
	got := decode[apitypes.Sandbox](t, resumed)
	if got.State != apitypes.SandboxStateRunning {
		t.Fatalf("state = %q, want running", got.State)
	}
	if got.Id != sb.Id {
		t.Errorf("resume changed the id: %s -> %s", sb.Id, got.Id)
	}

	// It runs commands again.
	exe := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions", map[string]any{"cmd": "python3"})
	if exe.StatusCode != http.StatusCreated {
		t.Errorf("exec after resume: %d, want 201", exe.StatusCode)
	}
}

// Resuming a running sandbox has nothing to resume: a 409, not a second VM.
func TestResumeARunningSandboxConflicts(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/resume", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeNotSuspended {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeNotSuspended)
	}
}

// A suspended sandbox refuses executions: there is no VM until it is resumed.
func TestExecutingInASuspendedSandboxConflicts(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)
	h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/suspend", nil)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/executions", map[string]any{"cmd": "python3"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}
