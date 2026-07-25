package api

import (
	"io"
	"net/http"
	"testing"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
)

// A named create carries its name back, so a caller reading the reply sees the
// handle they gave.
func TestNamedCreateReturnsTheName(t *testing.T) {
	h := newHarness(t)

	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python", "name": "build"})
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("named create: %d: %s", resp.StatusCode, raw)
	}
	sb := decode[apitypes.Sandbox](t, resp)
	if sb.Name == nil || *sb.Name != "build" {
		t.Errorf("name = %v, want \"build\"", sb.Name)
	}
}

// A duplicate name is a 409 the caller can act on -- pick another, or ask for
// get_or_create -- not a generic failure.
func TestDuplicateNameConflicts(t *testing.T) {
	h := newHarness(t)

	first := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python", "name": "build"})
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first create: %d", first.StatusCode)
	}

	second := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python", "name": "build"})
	if second.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(second.Body)
		t.Fatalf("second create: %d, want 409: %s", second.StatusCode, raw)
	}
	env := decode[apitypes.ErrorEnvelope](t, second)
	if env.Error.Code != CodeAlreadyExists {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeAlreadyExists)
	}
}

// get_or_create resolves the name to the sandbox already running under it, with a
// 200 rather than a 201, and boots nothing.
func TestGetOrCreateReturnsExistingWith200(t *testing.T) {
	h := newHarness(t)

	first := h.do(t, "POST", "/v1/sandboxes",
		map[string]any{"image": "python", "name": "build", "get_or_create": true})
	if first.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(first.Body)
		t.Fatalf("first get_or_create: %d, want 201: %s", first.StatusCode, raw)
	}
	sb1 := decode[apitypes.Sandbox](t, first)

	second := h.do(t, "POST", "/v1/sandboxes",
		map[string]any{"image": "python", "name": "build", "get_or_create": true})
	if second.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(second.Body)
		t.Fatalf("second get_or_create: %d, want 200: %s", second.StatusCode, raw)
	}
	sb2 := decode[apitypes.Sandbox](t, second)

	if sb1.Id != sb2.Id {
		t.Errorf("get_or_create booted a second sandbox (%s vs %s)", sb1.Id, sb2.Id)
	}
	if got := h.rt.Created(); got != 1 {
		t.Errorf("the runtime built %d sandboxes, want 1", got)
	}
}

// get_or_create without a name has nothing to resolve, and saying so beats
// silently booting an anonymous sandbox the caller cannot find again.
func TestGetOrCreateNeedsAName(t *testing.T) {
	h := newHarness(t)

	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python", "get_or_create": true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeParameterInvalid {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeParameterInvalid)
	}
}

// A name outside the slug alphabet is refused before anything is spent on it.
func TestInvalidNameIsRefused(t *testing.T) {
	h := newHarness(t)

	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python", "name": "has space"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeParameterInvalid {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeParameterInvalid)
	}
	if env.Error.Param == nil || *env.Error.Param != "name" {
		t.Errorf("param = %v, want \"name\"", env.Error.Param)
	}
}

// A name freed by a stop is a name a caller can reuse at once, even while the old
// record lingers.
func TestNameReusableAfterStop(t *testing.T) {
	h := newHarness(t)

	first := decode[apitypes.Sandbox](t,
		h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python", "name": "build"}))
	h.do(t, "DELETE", "/v1/sandboxes/"+first.Id, nil)

	second := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python", "name": "build"})
	if second.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(second.Body)
		t.Fatalf("re-create after stop: %d, want 201: %s", second.StatusCode, raw)
	}
	if decode[apitypes.Sandbox](t, second).Id == first.Id {
		t.Error("re-create returned the stopped sandbox")
	}
}
