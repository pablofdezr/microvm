package api

import (
	"net/http"
	"sort"
	"strconv"
)

// Pagination limits. A caller who wants everything must ask page by page:
// an unbounded list endpoint is a way to make the daemon marshal its entire
// state into one response, which is a denial of service with a polite name.
const (
	defaultLimit = 10
	maxLimit     = 100
)

// pageParams is a parsed cursor request.
type pageParams struct {
	limit int
	// startingAfter and endingBefore are IDs, and because IDs sort
	// chronologically they *are* positions. That is what makes this exact: the
	// cursor does not name an object that has to still exist, it names a point
	// in the ordering. A page taken after the cursor's object was deleted is
	// still the right page.
	startingAfter string
	endingBefore  string
}

func parsePageParams(r *http.Request) (pageParams, error) {
	p := pageParams{
		limit:         defaultLimit,
		startingAfter: r.URL.Query().Get("starting_after"),
		endingBefore:  r.URL.Query().Get("ending_before"),
	}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return p, invalidParamError("limit", "must be an integer")
		}
		if n < 1 || n > maxLimit {
			return p, invalidParamError("limit",
				"must be between 1 and "+strconv.Itoa(maxLimit))
		}
		p.limit = n
	}

	// Both at once has no meaning: one asks for what follows a point, the other
	// for what precedes one. Silently honouring whichever we check first would
	// give a caller a page they did not ask for and no hint why.
	if p.startingAfter != "" && p.endingBefore != "" {
		return p, invalidParamError("starting_after",
			"cannot be combined with ending_before; pass one or the other")
	}
	return p, nil
}

// paginate applies a cursor to a list of items.
//
// Items are identified by id and sorted newest first, which is what a caller
// almost always wants to see and what every cursor here is relative to.
//
// It returns the page and whether more exist past it.
func paginate[T any](items []T, id func(T) string, p pageParams) ([]T, bool) {
	// Newest first. IDs sort chronologically, so this is one string sort rather
	// than a comparison against timestamps that a stopped sandbox may no longer
	// be able to produce.
	sorted := make([]T, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool { return id(sorted[i]) > id(sorted[j]) })

	switch {
	case p.startingAfter != "":
		// Everything strictly older than the cursor. Note this does not look the
		// cursor's object up: an ID is a position, so a cursor works even if what
		// it names is gone.
		cut := 0
		for cut < len(sorted) && id(sorted[cut]) >= p.startingAfter {
			cut++
		}
		sorted = sorted[cut:]

	case p.endingBefore != "":
		// Everything strictly newer than the cursor, keeping the page *adjacent*
		// to it: the last `limit` of them, not the first.
		end := 0
		for end < len(sorted) && id(sorted[end]) > p.endingBefore {
			end++
		}
		sorted = sorted[:end]
		if len(sorted) > p.limit {
			return sorted[len(sorted)-p.limit:], true
		}
		return sorted, false
	}

	if len(sorted) > p.limit {
		return sorted[:p.limit], true
	}
	return sorted, false
}
