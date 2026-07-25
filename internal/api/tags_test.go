package api

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

func (h *harness) createTagged(t *testing.T, tags map[string]string) apitypes.Sandbox {
	t.Helper()
	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python", "tags": tags})
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create sandbox with tags %v: %d: %s", tags, resp.StatusCode, raw)
	}
	return decode[apitypes.Sandbox](t, resp)
}

// listIDs returns the IDs a filtered list came back with, newest first.
func (h *harness) listIDs(t *testing.T, query string) []string {
	t.Helper()
	resp := h.do(t, "GET", "/v1/sandboxes?"+query, nil)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("list ?%s: %d: %s", query, resp.StatusCode, raw)
	}
	page := decode[apitypes.SandboxList](t, resp)

	out := make([]string, 0, len(page.Data))
	for _, sb := range page.Data {
		out = append(out, sb.Id)
	}
	return out
}

// Tags come back, which is the difference between them and env: a label you
// cannot read is not a label. They are returned by create and by retrieve, since
// a caller who did not keep the create reply is the one who needs them most.
func TestTagsAreReturned(t *testing.T) {
	h := newHarness(t)

	created := h.createTagged(t, map[string]string{"env": "ci", "owner": "billing-team"})
	if created.Tags == nil {
		t.Fatal("create dropped the tags")
	}
	if got := *created.Tags; got["env"] != "ci" || got["owner"] != "billing-team" {
		t.Errorf("tags = %v, want env=ci owner=billing-team", got)
	}

	fetched := decode[apitypes.Sandbox](t, h.do(t, "GET", "/v1/sandboxes/"+created.Id, nil))
	if fetched.Tags == nil || (*fetched.Tags)["env"] != "ci" {
		t.Errorf("retrieve returned tags %v, want env=ci", fetched.Tags)
	}
}

// The knob is off unless asked for: a sandbox created without tags has no tags
// field at all, rather than an empty object a client has to special-case.
func TestUntaggedSandboxHasNoTagsField(t *testing.T) {
	h := newHarness(t)

	sb := h.createSandbox(t)
	if sb.Tags != nil {
		t.Errorf("tags = %v, want absent", *sb.Tags)
	}

	// Explicitly empty is the same thing. It says nothing, so it stores nothing.
	empty := h.createTagged(t, map[string]string{})
	if empty.Tags != nil {
		t.Errorf("tags = %v, want absent for an empty object", *empty.Tags)
	}
}

// The caps are refused at the door, with the field named. These are held for the
// sandbox's whole life, so this is the only moment a caller can be told no --
// and a 400 that does not say which field is a 400 nobody can act on.
func TestCreateRejectsTagsPastTheLimits(t *testing.T) {
	h := newHarness(t)

	tooMany := map[string]string{}
	for i := range sandbox.MaxTags + 1 {
		tooMany[string(rune('a'+i))] = "v"
	}

	tests := []struct {
		name string
		tags map[string]string
	}{
		{"too many", tooMany},
		{"an empty key", map[string]string{"": "ci"}},
		{"a key over the byte limit", map[string]string{strings.Repeat("k", sandbox.MaxTagKeyBytes+1): "ci"}},
		{"a value over the byte limit", map[string]string{"env": strings.Repeat("v", sandbox.MaxTagValueBytes+1)}},
		{"a key containing a colon", map[string]string{"env:ci": "yes"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{"image": "python", "tags": tc.tags})
			if resp.StatusCode != http.StatusBadRequest {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
			}
			env := decode[apitypes.ErrorEnvelope](t, resp)
			if env.Error.Type != apitypes.ErrorTypeInvalidRequestError {
				t.Errorf("type = %q, want invalid_request_error", env.Error.Type)
			}
			if env.Error.Code != CodeParameterInvalid {
				t.Errorf("code = %q, want %q", env.Error.Code, CodeParameterInvalid)
			}
			if env.Error.Param == nil || *env.Error.Param != "tags" {
				t.Errorf("param = %v, want tags", env.Error.Param)
			}
		})
	}

	// Nothing was created by any of those, so the refusals cost no VMs.
	if got := h.listIDs(t, ""); len(got) != 0 {
		t.Errorf("%d sandboxes exist after only refused creates: %v", len(got), got)
	}
}

// One tag narrows the list to the sandboxes carrying it, and repeating the
// parameter narrows it further: they AND. ORing them would make a second tag
// widen the answer, which is the opposite of what a filter is for.
func TestListFiltersByTag(t *testing.T) {
	h := newHarness(t)

	ci := h.createTagged(t, map[string]string{"env": "ci", "owner": "me"})
	ciTheirs := h.createTagged(t, map[string]string{"env": "ci", "owner": "them"})
	prod := h.createTagged(t, map[string]string{"env": "prod", "owner": "me"})
	untagged := h.createSandbox(t)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"one tag", "tag=env:ci", []string{ci.Id, ciTheirs.Id}},
		{"two tags and", "tag=env:ci&tag=owner:me", []string{ci.Id}},
		{"a key nobody set", "tag=team:platform", nil},
		{"a value nobody set", "tag=env:staging", nil},
		{"one key, two values, matching nothing", "tag=env:ci&tag=env:prod", nil},
		{"beside the state filter", "state=running&tag=env:prod", []string{prod.Id}},
		{"no filter at all", "", []string{ci.Id, ciTheirs.Id, prod.Id, untagged.Id}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := h.listIDs(t, tc.query)
			if len(got) != len(tc.want) {
				t.Fatalf("?%s returned %v, want %v", tc.query, got, tc.want)
			}
			for _, id := range tc.want {
				if !contains(got, id) {
					t.Errorf("?%s did not return %s: got %v", tc.query, id, got)
				}
			}
		})
	}
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// The split is at the first colon, so a value may contain them. A key may not,
// which is why one is refused at create time.
func TestListFiltersOnAValueContainingAColon(t *testing.T) {
	h := newHarness(t)

	sb := h.createTagged(t, map[string]string{"url": "https://example.com:8443"})
	h.createTagged(t, map[string]string{"url": "https://example.com"})

	got := h.listIDs(t, "tag="+url.QueryEscape("url:https://example.com:8443"))
	if len(got) != 1 || got[0] != sb.Id {
		t.Errorf("got %v, want just %s", got, sb.Id)
	}
}

// A tag with nothing to split on is refused rather than silently ignored. An
// unknown key is a real question with an empty answer; this one names no key at
// all, so answering it with a page would be answering something else.
func TestListRejectsAMalformedTag(t *testing.T) {
	h := newHarness(t)
	h.createTagged(t, map[string]string{"env": "ci"})

	for _, raw := range []string{"env", ":ci", ""} {
		t.Run(raw, func(t *testing.T) {
			resp := h.do(t, "GET", "/v1/sandboxes?tag="+url.QueryEscape(raw), nil)
			if resp.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
			}
			env := decode[apitypes.ErrorEnvelope](t, resp)
			if env.Error.Code != CodeParameterInvalid {
				t.Errorf("code = %q, want %q", env.Error.Code, CodeParameterInvalid)
			}
			if env.Error.Param == nil || *env.Error.Param != "tag" {
				t.Errorf("param = %v, want tag", env.Error.Param)
			}
		})
	}
}
