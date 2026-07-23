package storage

import (
	"errors"
	"testing"
)

// TestClimbsAboveRoot pins the line between an ordinary path and an attempt.
//
// The distinction matters in both directions: calling "a/../b" an escape would
// bury the real signal in noise from programs doing nothing wrong, and missing
// "/.." would mean the log stays empty while somebody probes.
func TestClimbsAboveRoot(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// Ordinary paths that happen to contain dots.
		{"/data/file.txt", false},
		{"/data/../other.txt", false}, // pops back to the root, does not leave
		{"/a/b/../../c", false},       // exactly to the root
		{"/./x", false},
		{"/", false},
		{"", false},

		// Attempts.
		{"/../sb_b/secret", true},
		{"..", true},
		{"/a/../../etc/passwd", true}, // one pop too many
		{"/a/b/../../../x", true},
		{"/../../../../etc/shadow", true},
	}

	for _, tc := range tests {
		if got := climbsAboveRoot(tc.path); got != tc.want {
			t.Errorf("climbsAboveRoot(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestResolveReportsEscapes checks that the attempt is now visible rather than
// silently clamped. Before this, "/../sb_b/secret" resolved happily to
// "sandboxes/sb_a/sb_b/secret" and nothing anywhere recorded the attempt.
func TestResolveReportsEscapes(t *testing.T) {
	_, err := resolve("sandboxes/sb_a", "/../sb_b/secret")
	if !errors.Is(err, ErrEscapesPrefix) {
		t.Fatalf("resolve of an escape returned err=%v, want ErrEscapesPrefix", err)
	}
}

// TestResolveAllowsOrdinaryDots makes sure the detector did not break the
// normal case it sits next to.
func TestResolveAllowsOrdinaryDots(t *testing.T) {
	key, err := resolve("sandboxes/sb_a", "/data/../out/x.txt")
	if err != nil {
		t.Fatalf("an ordinary path was refused: %v", err)
	}
	if key != "sandboxes/sb_a/out/x.txt" {
		t.Errorf("resolved to %q", key)
	}
}
