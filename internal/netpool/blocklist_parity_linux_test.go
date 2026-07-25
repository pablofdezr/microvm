//go:build linux

package netpool

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/pablofdezr/microvm/internal/source"
)

// The daemon fetches caller-supplied URLs itself, from a process that sits
// outside this firewall, so internal/source carries its own copy of these ranges
// and applies them to its own socket. Two lists that must describe the same set
// drift the moment nothing fails when they do.
//
// The direction asserted is the one that matters: the daemon must be at least as
// strict as the guest. A destination closed to a sandbox but open to the process
// that boots it is worse than either -- the sandbox can just ask for it.
func TestSourceBlocklistCoversTheGuestBlocklist(t *testing.T) {
	guest := guestBlockedPrefixes(t)
	if len(guest) < 15 {
		t.Fatalf("parsed only %d prefixes out of the ruleset; the parser has stopped working", len(guest))
	}

	daemon := source.BlockedPrefixes()
	for _, want := range guest {
		if !coveredBy(want, daemon) {
			t.Errorf("the guest firewall blocks %s and internal/source does not: "+
				"add it to blockedPrefixes in internal/source/policy.go", want)
		}
	}
}

// guestBlockedPrefixes pulls the elements out of every set in the ruleset
// template, so the test reads the same text nftables is given rather than a
// second copy of it.
func guestBlockedPrefixes(t *testing.T) []netip.Prefix {
	t.Helper()

	var prefixes []netip.Prefix
	rest := rulesetTemplate
	for {
		_, after, found := strings.Cut(rest, "elements = {")
		if !found {
			return prefixes
		}
		body, remainder, found := strings.Cut(after, "}")
		if !found {
			t.Fatal("an elements block is never closed")
		}
		rest = remainder

		// A line at a time, and the annotation off each line before it is split on
		// commas: the annotations contain commas of their own, and one of them runs
		// onto the following line.
		for _, line := range strings.Split(body, "\n") {
			if cut := strings.Index(line, "#"); cut >= 0 {
				line = line[:cut]
			}
			for _, field := range strings.Split(line, ",") {
				text := strings.TrimSpace(field)
				if text == "" {
					continue
				}
				p, err := netip.ParsePrefix(text)
				if err != nil {
					t.Fatalf("ruleset element %q is not a prefix: %v", text, err)
				}
				prefixes = append(prefixes, p)
			}
		}
	}
}

// coveredBy reports whether want is entirely inside one of the given prefixes.
func coveredBy(want netip.Prefix, have []netip.Prefix) bool {
	for _, p := range have {
		if p.Bits() <= want.Bits() && p.Contains(want.Addr()) {
			return true
		}
	}
	return false
}
