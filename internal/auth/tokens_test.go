package auth

import (
	"slices"
	"testing"
)

// One parser reads a flag, an environment variable and a file, so the cases that
// matter are the ones where those three disagree about what a separator is.
func TestParseTokens(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: ""},
		{name: "one", in: "sk_a", want: []string{"sk_a"}},
		{name: "a flag's commas", in: "sk_a,sk_b", want: []string{"sk_a", "sk_b"}},
		{name: "a file's lines", in: "sk_a\nsk_b\n", want: []string{"sk_a", "sk_b"}},
		{name: "both at once", in: "sk_a,sk_b\nsk_c", want: []string{"sk_a", "sk_b", "sk_c"}},
		{name: "padding and blank lines", in: " sk_a \n\n\tsk_b\t\n", want: []string{"sk_a", "sk_b"}},
		{name: "crlf", in: "sk_a\r\nsk_b\r\n", want: []string{"sk_a", "sk_b"}},
		{name: "a comment", in: "# team a\nsk_a\n", want: []string{"sk_a"}},
		{
			// The reason comments are dropped whole-line and before the commas:
			// otherwise this file quietly grants the token "rotated 2026-01".
			name: "a comment containing a comma",
			in:   "# owner billing, rotated 2026-01\nsk_a\n",
			want: []string{"sk_a"},
		},
		{name: "only comments", in: "# nothing here\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseTokens(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ParseTokens(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Sources add up, so moving off the flag is a rotation rather than a cutover: for
// one restart both the old and the new token work.
func TestMergeTokensUnionsSources(t *testing.T) {
	got := MergeTokens([]string{"sk_file"}, []string{"sk_env"}, []string{"sk_flag"})
	want := []string{"sk_file", "sk_env", "sk_flag"}
	if !slices.Equal(got, want) {
		t.Errorf("MergeTokens = %q, want %q", got, want)
	}
}

// A token listed in two sources is one token. Resolution scans every pair, so a
// duplicate lengthens that scan and grants nothing new.
func TestMergeTokensDropsDuplicates(t *testing.T) {
	got := MergeTokens([]string{"sk_a", "sk_b"}, []string{"sk_a"}, []string{"sk_b", "sk_c"})
	want := []string{"sk_a", "sk_b", "sk_c"}
	if !slices.Equal(got, want) {
		t.Errorf("MergeTokens = %q, want %q", got, want)
	}
}

// The merged list is what the directory is built from, so it has to resolve.
func TestMergedTokensResolve(t *testing.T) {
	d := NewDirectory(MergeTokens(ParseTokens("sk_from_file\n"), ParseTokens("sk_from_flag")))

	for _, token := range []string{"sk_from_file", "sk_from_flag"} {
		if _, ok := d.Resolve(token); !ok {
			t.Errorf("%s did not resolve", token)
		}
	}
}

// The limits are carried by the identity, not by the node: an operator sets them
// on a key and they survive into the principal every request resolves to.
func TestLimitsReachThePrincipal(t *testing.T) {
	d := NewDirectoryOf(map[string]*Principal{
		"sk_small": {MaxConcurrent: 2, MaxRequestsPerSecond: 5},
		"sk_big":   {},
	})

	small, ok := d.Resolve("sk_small")
	if !ok {
		t.Fatal("token did not resolve")
	}
	if small.MaxConcurrent != 2 || small.MaxRequestsPerSecond != 5 {
		t.Errorf("limits = %d/%v, want 2/5", small.MaxConcurrent, small.MaxRequestsPerSecond)
	}

	// Zero is unlimited, and it is the default: a deployment that configures
	// nothing must behave exactly as it did before these fields existed.
	big, _ := d.Resolve("sk_big")
	if big.MaxConcurrent != 0 || big.MaxRequestsPerSecond != 0 {
		t.Errorf("an unconfigured principal has limits: %d/%v",
			big.MaxConcurrent, big.MaxRequestsPerSecond)
	}
}
