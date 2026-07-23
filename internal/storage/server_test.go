package storage

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, mount Mount) (*httptest.Server, *Memory) {
	t.Helper()
	backend := NewMemory()
	srv := httptest.NewServer(NewServer(NewStore(backend, mount), discardLogger()))
	t.Cleanup(srv.Close)
	return srv, backend
}

func TestServerPutGetRoundTrip(t *testing.T) {
	srv, _ := newTestServer(t, Mount{Prefix: "sandboxes/sb_a"})

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/objects/out/result.json",
		strings.NewReader(`{"ok":true}`))
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT returned %d, want 204", res.StatusCode)
	}

	res, err = srv.Client().Get(srv.URL + "/objects/out/result.json")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("read back %q", body)
	}
}

// TestServerRangedGet is the read a filesystem actually makes: a page at an
// offset, not the whole object.
func TestServerRangedGet(t *testing.T) {
	srv, backend := newTestServer(t, Mount{Prefix: "p"})
	mustPut(t, backend, "p/data", "0123456789")

	tests := []struct {
		rangeHdr string
		want     string
	}{
		{"bytes=0-3", "0123"},
		{"bytes=4-", "456789"},
		{"bytes=5-5", "5"},
		{"", "0123456789"},
	}

	for _, tc := range tests {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/objects/data", nil)
		if tc.rangeHdr != "" {
			req.Header.Set("Range", tc.rangeHdr)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(body) != tc.want {
			t.Errorf("Range %q gave %q, want %q", tc.rangeHdr, body, tc.want)
		}
	}
}

// TestServerEscapeLooksLikeAMissingFile checks the response a prober gets.
//
// It must be indistinguishable from a file that is not there: 404 and ENOENT.
// Anything else -- a 403, a distinct message -- confirms to the caller that
// there is something outside worth reaching for.
func TestServerEscapeLooksLikeAMissingFile(t *testing.T) {
	srv, _ := newTestServer(t, Mount{Prefix: "sandboxes/sb_a"})

	escape := get(t, srv, "/objects/../sb_b/secret")
	missing := get(t, srv, "/objects/nope")

	if escape.status != missing.status {
		t.Errorf("escape returned %d but a missing file returns %d: the difference tells a prober it found something",
			escape.status, missing.status)
	}
	if escape.errno != missing.errno {
		t.Errorf("escape errno %q, missing errno %q: same difference", escape.errno, missing.errno)
	}
	if escape.status != http.StatusNotFound || escape.errno != "ENOENT" {
		t.Errorf("escape gave %d/%s, want 404/ENOENT", escape.status, escape.errno)
	}
}

// TestServerQuotaIsEDQUOT checks the errno, not just the status: the consumer is
// a filesystem, and the program inside the guest sees whatever errno we pick.
func TestServerQuotaIsEDQUOT(t *testing.T) {
	srv, _ := newTestServer(t, Mount{
		Prefix: "p",
		Quota:  Quota{MaxBytes: 10, MaxObjects: 100, MaxWritesPerMinute: 100},
	})

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/objects/big", strings.NewReader(strings.Repeat("x", 50)))
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusInsufficientStorage {
		t.Errorf("over quota returned %d, want 507", res.StatusCode)
	}
	var body errorBody
	json.NewDecoder(res.Body).Decode(&body)
	if body.Errno != "EDQUOT" {
		t.Errorf("errno %q, want EDQUOT", body.Errno)
	}
}

// TestServerExactFillSucceeds guards the bug this project has now made twice:
// treating an exact fill as an overflow. A write whose size exactly matches the
// remaining quota is the most ordinary write there is.
func TestServerExactFillSucceeds(t *testing.T) {
	srv, _ := newTestServer(t, Mount{
		Prefix: "p",
		Quota:  Quota{MaxBytes: 10, MaxObjects: 100, MaxWritesPerMinute: 100},
	})

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/objects/exact", strings.NewReader("0123456789"))
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("a write that exactly fills the quota returned %d, want 204", res.StatusCode)
	}
}

func TestServerReadOnlyIsEROFS(t *testing.T) {
	srv, _ := newTestServer(t, Mount{Prefix: "p", ReadOnly: true})

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/objects/x", strings.NewReader("hi"))
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusForbidden {
		t.Errorf("write to a read-only mount returned %d, want 403", res.StatusCode)
	}
	var body errorBody
	json.NewDecoder(res.Body).Decode(&body)
	if body.Errno != "EROFS" {
		t.Errorf("errno %q, want EROFS", body.Errno)
	}
}

func TestServerListHidesThePrefix(t *testing.T) {
	srv, backend := newTestServer(t, Mount{Prefix: "sandboxes/sb_a"})
	mustPut(t, backend, "sandboxes/sb_a/logs/one.txt", "a")
	mustPut(t, backend, "sandboxes/sb_a/logs/two.txt", "b")

	res, err := srv.Client().Get(srv.URL + "/dir/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	var listing Listing
	if err := json.NewDecoder(res.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Objects) != 2 {
		t.Fatalf("listed %d objects, want 2", len(listing.Objects))
	}
	for _, o := range listing.Objects {
		if strings.Contains(o.Key, "sandboxes") {
			t.Errorf("key %q leaks the host-side prefix to the guest", o.Key)
		}
	}
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		header           string
		wantOff, wantLen int64
		wantErr          bool
	}{
		{header: "", wantOff: 0, wantLen: -1},
		{header: "bytes=0-9", wantOff: 0, wantLen: 10}, // inclusive: 10 bytes, not 9
		{header: "bytes=5-5", wantOff: 5, wantLen: 1},  // one byte, not zero
		{header: "bytes=100-", wantOff: 100, wantLen: -1},
		{header: "bytes=4096-8191", wantOff: 4096, wantLen: 4096},
		{header: "bytes=-500", wantErr: true},      // suffix range
		{header: "bytes=0-9,20-29", wantErr: true}, // multipart
		{header: "items=0-9", wantErr: true},
		{header: "bytes=9-0", wantErr: true}, // ends before it starts
		{header: "bytes=abc-9", wantErr: true},
	}

	for _, tc := range tests {
		off, length, err := parseRange(tc.header)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseRange(%q) accepted a range it cannot serve", tc.header)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRange(%q): %v", tc.header, err)
			continue
		}
		if off != tc.wantOff || length != tc.wantLen {
			t.Errorf("parseRange(%q) = (%d, %d), want (%d, %d)", tc.header, off, length, tc.wantOff, tc.wantLen)
		}
	}
}

// TestRangeRoundTrip pins the two halves against each other. parseRange turns a
// header into (offset, length); byteRange turns (offset, length) back into a
// header. Both have an inclusive-bound off-by-one available to them, and a
// matching pair of mistakes would cancel out in each test alone.
func TestRangeRoundTrip(t *testing.T) {
	for _, header := range []string{"bytes=0-9", "bytes=100-109", "bytes=5-5", "bytes=4096-8191"} {
		off, length, err := parseRange(header)
		if err != nil {
			t.Fatalf("parseRange(%q): %v", header, err)
		}
		if got := byteRange(off, length); got != header {
			t.Errorf("%q -> (%d,%d) -> %q: the two ends disagree about inclusive bounds",
				header, off, length, got)
		}
	}
}

type response struct {
	status int
	errno  string
}

func get(t *testing.T, srv *httptest.Server, path string) response {
	t.Helper()
	// The path is sent raw: http.Client would helpfully resolve "../" away
	// client-side, and the whole point is to make the server see it.
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.Opaque = path

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	var body errorBody
	json.NewDecoder(res.Body).Decode(&body)
	return response{status: res.StatusCode, errno: body.Errno}
}

func mustPut(t *testing.T, backend *Memory, key, content string) {
	t.Helper()
	if err := backend.Put(context.Background(), key, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
}

// discardLogger keeps the escape-attempt warnings out of the test output. The
// tests that care about them assert on the response, not the log.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
