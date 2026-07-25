package sandbox

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
)

func newTaggedSandbox(t *testing.T, tags map[string]string) *Sandbox {
	t.Helper()
	logs := logstore.New(logstore.Config{})
	m := NewManager(runtimetest.New(), logs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = m.Close(context.Background()) })

	sb, err := m.Create(context.Background(), Spec{
		Spec: runtime.Spec{ID: "sb_tagged", Image: "python"},
		Tags: tags,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

// The caps are the whole feature: a tag is memory a caller hands us and we hold
// until the sandbox is forgotten. The cases that matter are the ones just over
// each line, and the one where bytes and characters disagree.
func TestValidateTags(t *testing.T) {
	tests := []struct {
		name    string
		tags    map[string]string
		wantErr bool
	}{
		{
			name: "none",
			tags: nil,
		},
		{
			name: "a label",
			tags: map[string]string{"env": "ci", "owner": "billing-team"},
		},
		{
			name: "an empty value is a value",
			tags: map[string]string{"scratch": ""},
		},
		{
			name: "exactly the maximum count",
			tags: countedTags(MaxTags),
		},
		{
			name:    "one tag too many",
			tags:    countedTags(MaxTags + 1),
			wantErr: true,
		},
		{
			name:    "an empty key",
			tags:    map[string]string{"": "ci"},
			wantErr: true,
		},
		{
			name: "a key at the limit",
			tags: map[string]string{strings.Repeat("k", MaxTagKeyBytes): "ci"},
		},
		{
			name:    "a key one byte over",
			tags:    map[string]string{strings.Repeat("k", MaxTagKeyBytes+1): "ci"},
			wantErr: true,
		},
		{
			name: "a value at the limit",
			tags: map[string]string{"env": strings.Repeat("v", MaxTagValueBytes)},
		},
		{
			name:    "a value one byte over",
			tags:    map[string]string{"env": strings.Repeat("v", MaxTagValueBytes+1)},
			wantErr: true,
		},
		{
			// 17 characters, 68 bytes. The limit is bytes because bytes are what
			// is being held, so this is refused however short it looks.
			name:    "a short key that is too many bytes",
			tags:    map[string]string{strings.Repeat("🙂", 17): "ci"},
			wantErr: true,
		},
		{
			// The filter splits at the first colon, so a key containing one could
			// never be matched. Accepting it would mean a tag nobody can find.
			name:    "a key containing a colon",
			tags:    map[string]string{"env:ci": "yes"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTags(tc.tags)
			if tc.wantErr && err == nil {
				t.Fatal("accepted")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// countedTags returns n distinct tags.
func countedTags(n int) map[string]string {
	out := make(map[string]string, n)
	for i := range n {
		out[string(rune('a'+i))] = "v"
	}
	return out
}

// An error a caller reads has to name the tag it is about, and the key is the
// only name a tag has.
func TestValidateTagsNamesTheOffendingKey(t *testing.T) {
	err := ValidateTags(map[string]string{"owner": strings.Repeat("v", MaxTagValueBytes+1)})
	if err == nil {
		t.Fatal("accepted a value over the limit")
	}
	if !strings.Contains(err.Error(), "owner") {
		t.Errorf("error does not name the key: %v", err)
	}
}

// A refused key is quoted back, and a refused key is by definition long. Cutting
// it keeps the message readable -- and on a rune boundary, so a key of emoji
// does not come back as broken UTF-8.
func TestValidateTagsElidesALongKey(t *testing.T) {
	err := ValidateTags(map[string]string{strings.Repeat("🙂", 64): "v"})
	if err == nil {
		t.Fatal("accepted a key over the limit")
	}
	msg := err.Error()
	if len(msg) > 128 {
		t.Errorf("error is %d bytes; it quotes the whole key back: %s", len(msg), msg)
	}
	if !strings.Contains(msg, "...") {
		t.Errorf("a cut key should say so: %s", msg)
	}
	if !strings.ContainsRune(msg, '🙂') || strings.ContainsRune(msg, '�') {
		t.Errorf("the key was cut mid-rune: %s", msg)
	}
}

// Tags are carried from the spec to Info, which is the only way a caller ever
// sees them again.
func TestTagsReachInfo(t *testing.T) {
	sb := newTaggedSandbox(t, map[string]string{"env": "ci", "owner": "me"})

	got := sb.Info().Tags
	if len(got) != 2 || got["env"] != "ci" || got["owner"] != "me" {
		t.Fatalf("tags = %v, want env=ci owner=me", got)
	}
}

// No tags means nil, not an empty map: the API layer omits the field on nil, so
// a caller who set nothing reads nothing back rather than an empty object.
func TestNoTagsIsNil(t *testing.T) {
	sb := newTaggedSandbox(t, nil)

	if got := sb.Info().Tags; got != nil {
		t.Errorf("tags = %v, want nil", got)
	}
	if got := sb.Tags(); got != nil {
		t.Errorf("Tags() = %v, want nil", got)
	}
}

// The map handed out is a copy. The one behind it is what every tag filter
// reads, so a caller who writes into what they were given would relabel the
// sandbox for everybody looking at it.
func TestTagsAreACopy(t *testing.T) {
	sb := newTaggedSandbox(t, map[string]string{"env": "ci"})

	got := sb.Tags()
	got["env"] = "prod"
	got["injected"] = "yes"

	after := sb.Tags()
	if after["env"] != "ci" {
		t.Errorf("env = %q, want ci: the caller's write reached the sandbox", after["env"])
	}
	if _, ok := after["injected"]; ok {
		t.Error("a caller added a tag to a running sandbox")
	}
}

// Create does not borrow the caller's map either. The caps are checked before
// the call, so a map that is still the caller's is a map that can grow past them
// afterwards -- and what is here is held until the sandbox is forgotten.
func TestCreateTakesTheTagsRatherThanBorrowsThem(t *testing.T) {
	tags := map[string]string{"env": "ci"}
	sb := newTaggedSandbox(t, tags)

	tags["env"] = "prod"
	for i := range 100 {
		tags[string(rune('A'+i))] = strings.Repeat("x", 1024)
	}

	got := sb.Tags()
	if got["env"] != "ci" {
		t.Errorf("env = %q, want ci: the sandbox reads the caller's map", got["env"])
	}
	if len(got) != 1 {
		t.Errorf("the sandbox now holds %d tags, past the cap of %d", len(got), MaxTags)
	}
}
