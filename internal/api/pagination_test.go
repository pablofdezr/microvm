package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/id"
)

// ids returns n IDs, oldest first.
//
// A millisecond apart, deliberately. An ID's order comes from its timestamp,
// which has millisecond resolution, so two minted in the same millisecond are
// separated only by their random halves and their relative order is arbitrary.
//
// That is not a problem for paging -- the order is still total and stable, so
// cursors never skip or repeat -- but it does mean a test asserting *which*
// order has to give each ID its own millisecond, or it is asserting something
// the scheme never promised and will fail roughly at random.
func ids(t *testing.T, n int) []string {
	t.Helper()
	out := make([]string, n)
	for i := range out {
		out[i] = id.New(id.SandboxPrefix)
		time.Sleep(time.Millisecond)
	}
	return out
}

func self(s string) string { return s }

func TestPaginateReturnsNewestFirst(t *testing.T) {
	all := ids(t, 5)

	got, hasMore := paginate(all, self, pageParams{limit: 10})
	if hasMore {
		t.Error("has_more is true with everything on one page")
	}
	// Newest first: the reverse of how they were minted.
	for i := range got {
		want := all[len(all)-1-i]
		if got[i] != want {
			t.Fatalf("position %d: got %s, want %s: the list is not newest-first", i, got[i], want)
		}
	}
}

func TestPaginateWalksForwardWithoutSkippingOrRepeating(t *testing.T) {
	all := ids(t, 25)

	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ {
		got, hasMore := paginate(all, self, pageParams{limit: 10, startingAfter: cursor})
		seen = append(seen, got...)
		if !hasMore {
			break
		}
		if len(got) == 0 {
			t.Fatal("has_more was true but the page was empty: paging would never end")
		}
		cursor = got[len(got)-1]
	}

	if len(seen) != len(all) {
		t.Fatalf("walked %d items, want %d: paging skipped or lost rows", len(seen), len(all))
	}
	unique := map[string]bool{}
	for _, s := range seen {
		if unique[s] {
			t.Fatalf("%s appeared on two pages", s)
		}
		unique[s] = true
	}
}

// The property that ULIDs buy. With unordered IDs the cursor has to be looked
// up, so deleting it strands the caller mid-walk; here the cursor is a
// position, so it keeps working.
func TestCursorSurvivesItsObjectBeingDeleted(t *testing.T) {
	all := ids(t, 6)

	first, _ := paginate(all, self, pageParams{limit: 3})
	cursor := first[len(first)-1]

	// Whatever the cursor named is deleted between the two calls.
	var remaining []string
	for _, s := range all {
		if s != cursor {
			remaining = append(remaining, s)
		}
	}

	next, _ := paginate(remaining, self, pageParams{limit: 3, startingAfter: cursor})
	if len(next) != 3 {
		t.Fatalf("got %d items after the cursor's object was deleted, want 3", len(next))
	}
	for _, s := range next {
		for _, seen := range first {
			if s == seen {
				t.Errorf("%s was returned twice across a deletion", s)
			}
		}
	}
}

func TestPaginateBackwardsReturnsThePageAdjacentToTheCursor(t *testing.T) {
	all := ids(t, 10) // oldest..newest

	// Page forward twice to land in the middle.
	page1, _ := paginate(all, self, pageParams{limit: 3})
	page2, _ := paginate(all, self, pageParams{limit: 3, startingAfter: page1[len(page1)-1]})

	// Going back from page 2's first item must reproduce page 1 exactly -- the
	// items *adjacent* to the cursor, not the newest three overall (which here
	// happen to be the same, so the test uses page 3 to tell them apart).
	page3, _ := paginate(all, self, pageParams{limit: 3, startingAfter: page2[len(page2)-1]})
	back, _ := paginate(all, self, pageParams{limit: 3, endingBefore: page3[0]})

	if len(back) != len(page2) {
		t.Fatalf("paging back gave %d items, want %d", len(back), len(page2))
	}
	for i := range back {
		if back[i] != page2[i] {
			t.Errorf("position %d: paging back gave %s, want %s: ending_before returned the "+
				"newest page rather than the one before the cursor", i, back[i], page2[i])
		}
	}
}

func TestPaginateEmpty(t *testing.T) {
	got, hasMore := paginate([]string{}, self, pageParams{limit: 10})
	if len(got) != 0 || hasMore {
		t.Errorf("got %v has_more=%v, want empty and false", got, hasMore)
	}
}

func TestPaginateCursorPastTheEnd(t *testing.T) {
	all := ids(t, 3)
	// A cursor older than everything: nothing follows it.
	got, hasMore := paginate(all, self, pageParams{limit: 10, startingAfter: "sb_00000000000000000000000000"})
	if len(got) != 0 || hasMore {
		t.Errorf("got %d items past the oldest cursor, want 0", len(got))
	}
}

func TestParsePageParams(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantLimit int
		wantErr   bool
	}{
		{"defaults", "", defaultLimit, false},
		{"explicit limit", "?limit=50", 50, false},
		{"limit at the ceiling", "?limit=100", 100, false},
		{"limit over the ceiling", "?limit=101", 0, true},
		{"limit of zero", "?limit=0", 0, true},
		{"negative limit", "?limit=-1", 0, true},
		{"limit that is not a number", "?limit=lots", 0, true},
		{"one cursor", "?starting_after=sb_1", defaultLimit, false},
		{"both cursors at once", "?starting_after=sb_1&ending_before=sb_2", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/sandboxes"+tc.query, nil)
			got, err := parsePageParams(r)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePageParams(%q) accepted an invalid page request", tc.query)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePageParams(%q): %v", tc.query, err)
			}
			if got.limit != tc.wantLimit {
				t.Errorf("limit = %d, want %d", got.limit, tc.wantLimit)
			}
		})
	}
}
