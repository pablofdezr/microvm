package id

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestNewHasThePrefixAndShape(t *testing.T) {
	for _, prefix := range []string{SandboxPrefix, ExecutionPrefix, TaskPrefix} {
		got := New(prefix)
		if !strings.HasPrefix(got, prefix+"_") {
			t.Errorf("New(%q) = %q, which does not carry its prefix", prefix, got)
		}
		if err := Parse(got, prefix); err != nil {
			t.Errorf("New(%q) = %q, which does not parse: %v", prefix, got, err)
		}
		if len(got) != len(prefix)+1+encodedLen {
			t.Errorf("New(%q) = %q, length %d", prefix, got, len(got))
		}
	}
}

// The property the whole ID scheme exists for. Cursor pagination is built on it:
// if sorting IDs as strings did not sort them by creation, `starting_after`
// would return the wrong page, and it would do so silently.
func TestIDsSortChronologically(t *testing.T) {
	const n = 50

	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, New(SandboxPrefix))
		// A millisecond apart, so each ID lands in a distinct timestamp bucket.
		// Within one millisecond only the random bits differ and order is not
		// promised -- which is fine, and worth being precise about.
		time.Sleep(time.Millisecond)
	}

	shuffled := append([]string(nil), ids...)
	sort.Sort(sort.Reverse(sort.StringSlice(shuffled)))
	sort.Strings(shuffled)

	for i := range ids {
		if shuffled[i] != ids[i] {
			t.Fatalf("position %d: sorted %q, minted %q -- string order is not time order, "+
				"so cursor pagination would return the wrong page", i, shuffled[i], ids[i])
		}
	}
}

func TestIDsAreUnique(t *testing.T) {
	// Minted in a tight loop, so they mostly share a millisecond and only the
	// random half separates them.
	const n = 100_000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		got := New(TaskPrefix)
		if seen[got] {
			t.Fatalf("minted %q twice in %d draws", got, i)
		}
		seen[got] = true
	}
}

func TestTimeRoundTrips(t *testing.T) {
	before := time.Now().Truncate(time.Millisecond)
	got := New(SandboxPrefix)
	after := time.Now()

	stamp, err := Time(got)
	if err != nil {
		t.Fatalf("Time(%q): %v", got, err)
	}
	if stamp.Before(before) || stamp.After(after) {
		t.Errorf("Time(%q) = %v, want between %v and %v", got, stamp, before, after)
	}
}

// A real ID in the wrong slot is a different mistake from gibberish, and the
// caller needs to be told which one they made.
func TestParseSeparatesWrongTypeFromMalformed(t *testing.T) {
	sandbox := New(SandboxPrefix)

	if err := Parse(sandbox, ExecutionPrefix); !errors.Is(err, ErrWrongType) {
		t.Errorf("a sandbox id read as an execution id gave %v, want ErrWrongType", err)
	}
	if err := Parse(sandbox, SandboxPrefix); err != nil {
		t.Errorf("a sandbox id read as a sandbox id gave %v", err)
	}

	for _, tc := range []struct{ name, in string }{
		{"empty", ""},
		{"no prefix", "01JZ8QK3M4N5P6R7S8T9V0W1X2"},
		{"too short", "sb_01JZ8QK3M4N5"},
		{"too long", "sb_01JZ8QK3M4N5P6R7S8T9V0W1X2XX"},
		{"letter I is not in the alphabet", "sb_I1JZ8QK3M4N5P6R7S8T9V0W1X2"},
		{"letter L is not in the alphabet", "sb_L1JZ8QK3M4N5P6R7S8T9V0W1X2"},
		{"letter O is not in the alphabet", "sb_O1JZ8QK3M4N5P6R7S8T9V0W1X2"},
		{"letter U is not in the alphabet", "sb_U1JZ8QK3M4N5P6R7S8T9V0W1X2"},
		{"punctuation", "sb_01JZ8QK3M4N5P6R7S8T9V0W1X!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Parse(tc.in, SandboxPrefix); !errors.Is(err, ErrMalformed) {
				t.Errorf("Parse(%q) = %v, want ErrMalformed", tc.in, err)
			}
		})
	}
}

// The four excluded letters are excluded because a human misreads them. Somebody
// who types O for 0 should get their object rather than a 404 they cannot
// explain -- but only where it is unambiguous, which is case, not shape.
func TestParseAcceptsLowerCase(t *testing.T) {
	got := New(SandboxPrefix)
	lowered := strings.ToLower(got)
	if lowered == got {
		t.Skip("id happened to contain no letters")
	}
	if err := Parse(lowered, SandboxPrefix); err != nil {
		t.Errorf("Parse(%q) rejected a lowercased id: %v", lowered, err)
	}
}

// Every ID starts with 0-7: 26 base32 characters carry 130 bits and the value is
// 128, so the top two bits are always zero. Pinning it because it is the kind of
// invariant an encoder rewrite breaks without any test noticing.
func TestFirstCharacterIsNeverOverflowing(t *testing.T) {
	for i := 0; i < 1000; i++ {
		got := New(SandboxPrefix)
		body := strings.TrimPrefix(got, SandboxPrefix+"_")
		if body[0] < '0' || body[0] > '7' {
			t.Fatalf("%q starts with %q: the encoder is overflowing its 128 bits",
				got, body[0])
		}
	}
}

// The alphabet must be exactly what the spec's pattern promises, since that
// pattern is published and callers will validate against it.
func TestAlphabetMatchesTheSpec(t *testing.T) {
	const excluded = "ILOU"
	for _, c := range excluded {
		if strings.ContainsRune(crockford, c) {
			t.Errorf("alphabet contains %q, which the spec's pattern excludes", c)
		}
	}
	if len(crockford) != 32 {
		t.Errorf("alphabet is %d characters, want 32", len(crockford))
	}
}
