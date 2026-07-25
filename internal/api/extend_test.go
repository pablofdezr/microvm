package api

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

func (h *harness) extend(t *testing.T, sandboxID string, body any) *http.Response {
	t.Helper()
	return h.do(t, "POST", "/v1/sandboxes/"+sandboxID+"/extend", body)
}

// The reply's expires is the answer, so the test reads it rather than the
// request: a caller who computes the deadline from what they asked for is the
// caller this route is designed not to have.
func TestExtendMovesTheDeadline(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.extend(t, sb.Id, map[string]any{"ttl_seconds": 3600})
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("extend: %d: %s", resp.StatusCode, raw)
	}

	got := decode[apitypes.Sandbox](t, resp)
	if !got.Expires.After(sb.Expires) {
		t.Errorf("expires = %s, want later than %s", got.Expires, sb.Expires)
	}
	if left := time.Until(got.Expires); left < 59*time.Minute {
		t.Errorf("only %s bought by a 3600s extension", left)
	}
	if got.State != apitypes.SandboxStateRunning {
		t.Errorf("state = %q, want running", got.State)
	}
}

// A shorter extension does not cut a longer one short, which is what makes two
// callers heartbeating one sandbox safe -- and a retry of this call a no-op.
func TestExtendNeverBringsTheDeadlineForward(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	long := decode[apitypes.Sandbox](t, h.extend(t, sb.Id, map[string]any{"ttl_seconds": 3600}))
	short := decode[apitypes.Sandbox](t, h.extend(t, sb.Id, map[string]any{"ttl_seconds": 60}))

	if !short.Expires.Equal(long.Expires) {
		t.Errorf("a 60s extension moved the deadline from %s to %s", long.Expires, short.Expires)
	}
}

// Past the lifetime bound is refused, not trimmed. A caller told 200 for an hour
// they did not get plans for an hour; an error reaches them while they can still
// split the work.
func TestExtendPastTheMaximumIsRefused(t *testing.T) {
	h := newHarness(t)

	// Created with the whole allowance, so there is nothing left to buy.
	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{
		"image":       "python",
		"ttl_seconds": int(sandbox.MaxTTL / time.Second),
	})
	sb := decode[apitypes.Sandbox](t, resp)

	for _, ttl := range []int{maxExtendSeconds, maxExtendSeconds + 1} {
		resp := h.extend(t, sb.Id, map[string]any{"ttl_seconds": ttl})
		if resp.StatusCode != http.StatusBadRequest {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("ttl_seconds=%d: status = %d, want 400: %s", ttl, resp.StatusCode, raw)
		}
		env := decode[apitypes.ErrorEnvelope](t, resp)
		if env.Error.Type != apitypes.ErrorTypeInvalidRequestError {
			t.Errorf("ttl_seconds=%d: type = %q, want invalid_request_error", ttl, env.Error.Type)
		}
		if env.Error.Code != CodeParameterInvalid {
			t.Errorf("ttl_seconds=%d: code = %q, want %q", ttl, env.Error.Code, CodeParameterInvalid)
		}
		if env.Error.Param == nil || *env.Error.Param != "ttl_seconds" {
			t.Errorf("ttl_seconds=%d: param = %v, want ttl_seconds", ttl, env.Error.Param)
		}
	}

	// And the refusal changed nothing: the deadline is still the one create set.
	after := decode[apitypes.Sandbox](t, h.do(t, "GET", "/v1/sandboxes/"+sb.Id, nil))
	if !after.Expires.Equal(sb.Expires) {
		t.Errorf("expires = %s, want %s: a refused extension moved the deadline",
			after.Expires, sb.Expires)
	}
}

// A stopped sandbox is a conflict. Answering 200 would tell a caller their
// heartbeat is keeping something alive that is already gone.
func TestExtendStoppedSandboxIsAConflict(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	if resp := h.do(t, "DELETE", "/v1/sandboxes/"+sb.Id, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", resp.StatusCode)
	}

	resp := h.extend(t, sb.Id, map[string]any{"ttl_seconds": 600})
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, raw)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeSandboxNotRunning {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeSandboxNotRunning)
	}
}

func TestExtendNeedsAPositiveTTL(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	tests := []struct {
		name string
		body map[string]any
		code string
	}{
		{"absent", map[string]any{}, CodeParameterMissing},
		{"zero", map[string]any{"ttl_seconds": 0}, CodeParameterMissing},
		{"negative", map[string]any{"ttl_seconds": -60}, CodeParameterInvalid},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.extend(t, sb.Id, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			env := decode[apitypes.ErrorEnvelope](t, resp)
			if env.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.code)
			}
			if env.Error.Param == nil || *env.Error.Param != "ttl_seconds" {
				t.Errorf("param = %v, want ttl_seconds", env.Error.Param)
			}
		})
	}
}

// An unknown sandbox is a 404 rather than a 409 or a 500: the route resolves the
// path before it reads the body, like every other sandbox route.
func TestExtendUnknownSandbox(t *testing.T) {
	h := newHarness(t)

	resp := h.extend(t, "sb_01JZ8QK3M4N5P6R7S8T9V0W1X2", map[string]any{"ttl_seconds": 600})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if env := decode[apitypes.ErrorEnvelope](t, resp); env.Error.Code != CodeSandboxNotFound {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeSandboxNotFound)
	}
}
