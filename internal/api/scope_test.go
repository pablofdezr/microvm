package api

import (
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/auth"
)

// A sandbox belongs to the tenant that made it, and an ID is not a capability.
//
// Nothing used to compare the two, so any valid token could drive any sandbox on
// the node: read files out of it -- which means out of the owner's whole
// object-store namespace, mounted inside it -- exec in it, delete it, or extend it
// and pin a slot charged to the owner's concurrency cap rather than the caller's.
// The list was the other half: it handed out every ID, tag and storage namespace on
// the box to anyone who authenticated.

// scopeHarness is a server with two ordinary tenants and one admin, the minimum
// needed to say whose sandbox is whose.
func newScopeHarness(t *testing.T) *limitHarness {
	t.Helper()
	// No Tenant set, so each token derives its own -- which is what every token on
	// a real daemon does.
	return newLimitHarness(t, map[string]*auth.Principal{
		"sk_a":     {},
		"sk_b":     {},
		"sk_admin": {Admin: true},
	})
}

func (h *limitHarness) createAs(t *testing.T, token string) apitypes.Sandbox {
	t.Helper()
	resp := h.do(t, token, "POST", "/v1/sandboxes", `{"image":"python"}`)
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create as %s: %d: %s", token, resp.StatusCode, raw)
	}
	return decode[apitypes.Sandbox](t, resp)
}

func TestAnotherTenantsSandboxIsNotFound(t *testing.T) {
	h := newScopeHarness(t)
	sb := h.createAs(t, "sk_a")

	// Every route that resolves a sandbox, because the check has to be in the
	// resolution rather than in nine handlers that must all remember it.
	routes := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"retrieve", "GET", "/v1/sandboxes/" + sb.Id, ""},
		{"extend", "POST", "/v1/sandboxes/" + sb.Id + "/extend", `{"ttl_seconds":86400}`},
		{"read a file", "GET", "/v1/sandboxes/" + sb.Id + "/files?path=/etc/hostname", ""},
		{"write a file", "POST", "/v1/sandboxes/" + sb.Id + "/files", `{"path":"/app/x","content":"eA=="}`},
		{"write a batch", "POST", "/v1/sandboxes/" + sb.Id + "/files/batch",
			`{"files":[{"path":"/app/x","content":"eA=="}]}`},
		{"make a directory", "POST", "/v1/sandboxes/" + sb.Id + "/dirs", `{"path":"/app/out"}`},
		{"run a command", "POST", "/v1/sandboxes/" + sb.Id + "/executions", `{"cmd":"python3"}`},
		{"list executions", "GET", "/v1/sandboxes/" + sb.Id + "/executions", ""},
		// Last, so a refusal that was not one does not hide the others.
		{"delete", "DELETE", "/v1/sandboxes/" + sb.Id, ""},
	}

	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(t, "sk_b", tc.method, tc.path, tc.body)
			if resp.StatusCode != http.StatusNotFound {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 404: another tenant reached this route: %s", resp.StatusCode, raw)
			}
			// 404 and not 403, so IDs stay unenumerable: a 403 would confirm which of
			// a guessed range exist.
			env := decode[apitypes.ErrorEnvelope](t, resp)
			if env.Error.Code != CodeSandboxNotFound {
				t.Errorf("code = %q, want %q", env.Error.Code, CodeSandboxNotFound)
			}
		})
	}

	// The owner is unaffected, which is the half of this that is easy to break.
	if got := h.do(t, "sk_a", "GET", "/v1/sandboxes/"+sb.Id, "").StatusCode; got != http.StatusOK {
		t.Errorf("the owner got %d for their own sandbox, want 200", got)
	}
	if got := h.do(t, "sk_a", "POST", "/v1/sandboxes/"+sb.Id+"/extend", `{"ttl_seconds":600}`).StatusCode; got != http.StatusOK {
		t.Errorf("the owner got %d extending their own sandbox, want 200", got)
	}
}

// The list is how an ID is found in the first place, so scoping the retrieve and
// not the list would leave the door open with a sign on it.
func TestListingShowsOnlyTheCallersOwnSandboxes(t *testing.T) {
	h := newScopeHarness(t)
	mine := h.createAs(t, "sk_a")
	theirs := h.createAs(t, "sk_b")

	ids := func(token string) []string {
		t.Helper()
		resp := h.do(t, token, "GET", "/v1/sandboxes", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list as %s: %d", token, resp.StatusCode)
		}
		var out []string
		for _, sb := range decode[apitypes.SandboxList](t, resp).Data {
			out = append(out, sb.Id)
		}
		return out
	}

	if got := ids("sk_a"); !slices.Equal(got, []string{mine.Id}) {
		t.Errorf("tenant a's list = %v, want only %s", got, mine.Id)
	}
	if got := ids("sk_b"); !slices.Equal(got, []string{theirs.Id}) {
		t.Errorf("tenant b's list = %v, want only %s", got, theirs.Id)
	}

	// The admin key keeps the node-wide view. It is the operator's own key -- it
	// already decides what every tenant may store -- and without it nothing can
	// answer "what is running on this box".
	got := ids("sk_admin")
	if len(got) != 2 || !slices.Contains(got, mine.Id) || !slices.Contains(got, theirs.Id) {
		t.Errorf("the admin's list = %v, want both %s and %s", got, mine.Id, theirs.Id)
	}
	if code := h.do(t, "sk_admin", "GET", "/v1/sandboxes/"+mine.Id, "").StatusCode; code != http.StatusOK {
		t.Errorf("the admin got %d retrieving another tenant's sandbox, want 200", code)
	}
}

// With auth off there is no identity to scope to, and that is the one case where a
// caller legitimately sees everything -- the same case where storage falls back to
// a per-sandbox namespace.
func TestScopingIsInertWithoutAuth(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	page := decode[apitypes.SandboxList](t, h.do(t, "GET", "/v1/sandboxes", nil))
	if len(page.Data) != 1 || page.Data[0].Id != sb.Id {
		t.Errorf("list returned %d sandboxes on an unauthenticated daemon, want the one that exists", len(page.Data))
	}
}
