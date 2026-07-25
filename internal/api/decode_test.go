package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

// The count of a batch and the count of a map of tags are both bounded, and both
// used to be bounded after the decode had already built the thing. That is where
// the cost is: the smallest legal batch entry is three bytes of JSON and the struct
// it becomes is forty-eight, so a body inside the 32 MiB cap names eleven million
// files and half a gigabyte of live heap goes up before the check runs.

func TestOverMembers(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		limit int
		want  bool
	}{
		{"an array at the limit", `[1,2,3]`, 3, false},
		{"an array one past it", `[1,2,3,4]`, 3, true},
		{"an empty array", `[]`, 3, false},
		// An object's members are two tokens each. Counting the key as a member would
		// refuse a map at half its documented limit, which is the bug this case is here
		// to keep out.
		{"an object at the limit", `{"a":"1","b":"2","c":"3"}`, 3, false},
		{"an object one past it", `{"a":"1","b":"2","c":"3","d":"4"}`, 3, true},
		{"an empty object", `{}`, 3, false},
		{"nested values are not members", `[{"a":[1,2,3,4,5]},{"b":{"c":1}}]`, 3, false},
		{"nested values still count their own level", `[[1,2],[3,4],[5,6],[7,8]]`, 3, true},
		// Malformed input is not this pass's business: the decode that follows reports
		// it, and better than a counting pass could.
		{"a scalar", `7`, 3, false},
		{"absent", ``, 3, false},
		{"truncated", `[1,2,`, 10, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := overMembers(json.RawMessage(tc.raw), tc.limit); got != tc.want {
				t.Errorf("overMembers(%s, %d) = %v, want %v", tc.raw, tc.limit, got, tc.want)
			}
		})
	}
}

// The batch cap is refused on a body whose array never becomes elements. Asserted
// through the route rather than the helper, because moving the check is the whole
// fix and a helper nobody calls would pass this on its own.
func TestAnOversizedBatchIsRefusedBeforeItIsMaterialised(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	// The cheapest legal entry there is, repeated: this is the shape that turns a
	// bounded body into an unbounded slice.
	var b strings.Builder
	b.WriteString(`{"files":[`)
	for i := range maxBatchFiles * 50 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{}`)
	}
	b.WriteString(`]}`)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files/batch", json.RawMessage(b.String()))
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeParameterInvalid {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeParameterInvalid)
	}
	if env.Error.Param == nil || *env.Error.Param != "files" {
		t.Errorf("param = %v, want files", env.Error.Param)
	}
}

// Same shape, smaller multiplier: a map of tags. The count is refused before the
// map exists, and the byte limits and the colon are still ValidateTags' to enforce
// on the map that does.
func TestATagFloodIsRefusedBeforeTheMapExists(t *testing.T) {
	h := newHarness(t)

	var b strings.Builder
	b.WriteString(`{"image":"python","tags":{`)
	for i := range sandbox.MaxTags * 100 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%d":""`, i)
	}
	b.WriteString(`}}`)

	resp := h.do(t, "POST", "/v1/sandboxes", json.RawMessage(b.String()))
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Param == nil || *env.Error.Param != "tags" {
		t.Errorf("param = %v, want tags", env.Error.Param)
	}
}

// The guard must not start refusing bodies that were always fine, and a map at
// exactly the cap is the case that breaks if the key is counted as a member.
func TestExactlyTheTagCapIsAccepted(t *testing.T) {
	h := newHarness(t)

	tags := map[string]string{}
	for i := range sandbox.MaxTags {
		tags[fmt.Sprintf("k%d", i)] = "v"
	}
	created := h.createTagged(t, tags)
	if created.Tags == nil || len(*created.Tags) != sandbox.MaxTags {
		t.Errorf("a sandbox created with exactly %d tags came back with %v", sandbox.MaxTags, created.Tags)
	}
}
