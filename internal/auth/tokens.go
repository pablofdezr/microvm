package auth

import "strings"

// Where tokens come from is a security property, not a convenience. A secret on
// a command line is a secret in `ps`, in shell history, and in whatever unit
// file started the daemon -- so the daemon reads them from a file or the
// environment as well as from a flag. The parsing lives here, beside the
// directory they end up in, so every source is read by exactly the same rules.

// ParseTokens splits one source's tokens.
//
// Commas and newlines both separate, so this reads a flag ("a,b"), an
// environment variable, and a file with one token per line without the caller
// having to say which it holds.
func ParseTokens(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		// Whole-line comments go before the commas are read: a comment like
		// "# rotated 2026-01, owner billing" would otherwise contribute its
		// second half as a live token.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, token := range strings.Split(line, ",") {
			if token = strings.TrimSpace(token); token != "" {
				out = append(out, token)
			}
		}
	}
	return out
}

// MergeTokens unions several sources, keeping the first occurrence of each.
//
// Sources add up rather than override, so an operator moving a key from a flag
// into a file has both work for the length of the rotation. Duplicates are
// dropped because resolution scans every pair in constant time: a token listed
// twice would lengthen that scan and buy nothing.
func MergeTokens(sources ...[]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, source := range sources {
		for _, token := range source {
			if _, dup := seen[token]; dup {
				continue
			}
			seen[token] = struct{}{}
			out = append(out, token)
		}
	}
	return out
}
