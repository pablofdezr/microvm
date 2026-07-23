package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

func newTestStore(t *testing.T, mount Mount) (*Store, *Memory) {
	t.Helper()
	backend := NewMemory()
	if mount.Prefix == "" {
		mount.Prefix = "sandboxes/sb_test"
	}
	return NewStore(backend, mount), backend
}

func write(t *testing.T, s *Store, path, body string) error {
	t.Helper()
	return s.Create(context.Background(), path, strings.NewReader(body), int64(len(body)))
}

// --- the security boundary --------------------------------------------------

// The path comes from hostile code, so this is the one test that matters most.
// Everything downstream trusts whatever resolve() lets through.
func TestPathsCannotEscapeThePrefix(t *testing.T) {
	s, backend := newTestStore(t, Mount{Prefix: "sandboxes/sb_a"})

	escapes := []struct {
		name string
		path string
	}{
		{"parent", "../sb_b/secret"},
		{"parent, repeatedly", "../../../../../../etc/passwd"},
		{"parent in the middle", "data/../../sb_b/secret"},
		{"absolute", "/etc/passwd"},
		{"absolute into another sandbox", "/sandboxes/sb_b/secret"},
		{"trailing parent", "data/subdir/../../../sb_b/x"},
		{"dot slash prefix", "./../sb_b/x"},
		{"parent disguised as a name", "..%2f..%2fsb_b"},
	}

	for _, tc := range escapes {
		t.Run(tc.name, func(t *testing.T) {
			// Writing must not land outside.
			err := write(t, s, tc.path, "stolen")
			if err == nil {
				// If it was accepted, prove where it went. A path that resolved
				// *inside* the prefix is fine, however it was spelled; one that
				// landed outside is a breach.
				for _, k := range backend.Keys() {
					if !strings.HasPrefix(k, "sandboxes/sb_a") {
						t.Fatalf("path %q wrote to %q, outside the sandbox's prefix", tc.path, k)
					}
				}
				return
			}
			if !errors.Is(err, ErrEscapesPrefix) && !errors.Is(err, ErrNotFound) {
				t.Errorf("path %q was refused with %v; expected an escape or not-found", tc.path, err)
			}

			// Reading must not reach outside either.
			if _, err := s.Open(context.Background(), tc.path, 0, -1); err == nil {
				t.Errorf("path %q could be read", tc.path)
			}
		})
	}
}

// One sandbox must not be able to name another's object, however it spells it.
func TestOneSandboxCannotReachAnother(t *testing.T) {
	backend := NewMemory()
	a := NewStore(backend, Mount{Prefix: "sandboxes/sb_a"})
	b := NewStore(backend, Mount{Prefix: "sandboxes/sb_b"})

	if err := write(t, b, "secret.txt", "b's data"); err != nil {
		t.Fatal(err)
	}

	// Every spelling of "reach into sb_b" that a determined caller might try.
	for _, attempt := range []string{
		"../sb_b/secret.txt",
		"/sandboxes/sb_b/secret.txt",
		"./../sb_b/secret.txt",
		"x/../../sb_b/secret.txt",
	} {
		rc, err := a.Open(context.Background(), attempt, 0, -1)
		if err == nil {
			body, _ := io.ReadAll(rc)
			rc.Close()
			t.Errorf("sandbox A read B's object via %q: got %q", attempt, body)
		}
	}
}

// Paths that merely look alarming but resolve inside must still work. A guard
// that refuses "/data/../out.json" is a guard that breaks ordinary code.
func TestPathsThatResolveInsideAreAllowed(t *testing.T) {
	s, backend := newTestStore(t, Mount{Prefix: "sandboxes/sb_a"})

	for _, tc := range []struct{ path, wantKey string }{
		{"out.json", "sandboxes/sb_a/out.json"},
		{"/out.json", "sandboxes/sb_a/out.json"},
		{"data/../out.json", "sandboxes/sb_a/out.json"},
		{"./data/./nested/x", "sandboxes/sb_a/data/nested/x"},
		{"a/b/c/../../d", "sandboxes/sb_a/a/d"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if err := write(t, s, tc.path, "ok"); err != nil {
				t.Fatalf("write %q: %v", tc.path, err)
			}
			if _, ok := backend.Object(tc.wantKey); !ok {
				t.Errorf("write %q did not land at %q; keys: %v", tc.path, tc.wantKey, backend.Keys())
			}
		})
	}
}

// The guest must never see the prefix: a path it can read is a path it will
// eventually try to write.
func TestTheGuestNeverSeesThePrefix(t *testing.T) {
	s, _ := newTestStore(t, Mount{Prefix: "sandboxes/sb_a"})

	if err := write(t, s, "data/out.json", "{}"); err != nil {
		t.Fatal(err)
	}

	info, err := s.Stat(context.Background(), "data/out.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(info.Key, "sandboxes/sb_a") {
		t.Errorf("Stat leaked the prefix: %q", info.Key)
	}
	if info.Key != "/data/out.json" {
		t.Errorf("key = %q, want /data/out.json", info.Key)
	}

	listing, err := s.List(context.Background(), "/", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, cp := range listing.CommonPrefixes {
		if strings.Contains(cp, "sandboxes/sb_a") {
			t.Errorf("List leaked the prefix: %q", cp)
		}
	}
}

// --- quota ------------------------------------------------------------------

// The failure mode of storage is not a crash, it is an invoice. This is the
// test that keeps the invoice bounded.
func TestQuotaStopsRunawayWrites(t *testing.T) {
	s, _ := newTestStore(t, Mount{
		Prefix: "sandboxes/sb_a",
		Quota:  Quota{MaxBytes: 100, MaxObjects: 1000, MaxWritesPerMinute: 1000},
	})

	if err := write(t, s, "a.txt", strings.Repeat("x", 60)); err != nil {
		t.Fatalf("first write within quota: %v", err)
	}
	err := write(t, s, "b.txt", strings.Repeat("x", 60))
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded: the sandbox wrote past its limit", err)
	}

	if got := s.Usage().BytesWritten; got != 60 {
		t.Errorf("bytes written = %d, want 60: the rejected write was still counted", got)
	}
}

// A declared size is a claim, and a claim from hostile code is not a fact.
// This is the difference between an advisory quota and a real one.
func TestQuotaIsEnforcedOnBytesNotOnTheDeclaredSize(t *testing.T) {
	s, backend := newTestStore(t, Mount{
		Prefix: "sandboxes/sb_a",
		Quota:  Quota{MaxBytes: 100, MaxObjects: 1000, MaxWritesPerMinute: 1000},
	})

	// Declares 10 bytes, sends 10,000.
	liar := strings.NewReader(strings.Repeat("x", 10_000))
	err := s.Create(context.Background(), "lie.txt", liar, 10)

	if err == nil {
		body, _ := backend.Object("sandboxes/sb_a/lie.txt")
		t.Fatalf("a write that declared 10 bytes and sent 10,000 succeeded, storing %d bytes: "+
			"the quota is advisory, not enforced", len(body))
	}
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("err = %v, want ErrQuotaExceeded", err)
	}
}

// A size of -1 means "I do not know yet", which is legitimate for a stream. It
// must still be bounded.
func TestQuotaBoundsAStreamOfUnknownSize(t *testing.T) {
	s, _ := newTestStore(t, Mount{
		Prefix: "sandboxes/sb_a",
		Quota:  Quota{MaxBytes: 50, MaxObjects: 1000, MaxWritesPerMinute: 1000},
	})

	err := s.Create(context.Background(), "stream.bin", strings.NewReader(strings.Repeat("x", 5000)), -1)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded: an unsized stream was unbounded", err)
	}
}

// Ten concurrent writes must not each check against the same free space and all
// pass. This is why bytes are reserved before the write rather than counted
// after it.
func TestConcurrentWritesCannotOversubscribeTheQuota(t *testing.T) {
	s, backend := newTestStore(t, Mount{
		Prefix: "sandboxes/sb_a",
		Quota:  Quota{MaxBytes: 1000, MaxObjects: 1000, MaxWritesPerMinute: 10_000},
	})

	const each = 200
	const writers = 20 // 20 * 200 = 4000 bytes against a 1000-byte limit

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Create(context.Background(), fmt.Sprintf("f%d.bin", i),
				bytes.NewReader(bytes.Repeat([]byte("x"), each)), each)
		}(i)
	}
	wg.Wait()

	var total int
	for _, k := range backend.Keys() {
		body, _ := backend.Object(k)
		total += len(body)
	}
	if total > 1000 {
		t.Errorf("stored %d bytes against a 1000-byte quota: concurrent writes each saw the "+
			"same free space and all passed", total)
	}
}

// Bytes alone do not bound the bill: a million empty objects cost almost no
// bytes and plenty of money.
func TestQuotaBoundsObjectCount(t *testing.T) {
	s, _ := newTestStore(t, Mount{
		Prefix: "sandboxes/sb_a",
		Quota:  Quota{MaxBytes: 1 << 30, MaxObjects: 3, MaxWritesPerMinute: 1000},
	})

	for i := 0; i < 3; i++ {
		if err := write(t, s, fmt.Sprintf("f%d", i), ""); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := write(t, s, "one-too-many", ""); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("err = %v, want ErrQuotaExceeded: empty objects are free in bytes and not in money", err)
	}
}

func TestQuotaBoundsWriteRate(t *testing.T) {
	s, _ := newTestStore(t, Mount{
		Prefix: "sandboxes/sb_a",
		Quota:  Quota{MaxBytes: 1 << 30, MaxObjects: 1_000_000, MaxWritesPerMinute: 5},
	})

	for i := 0; i < 5; i++ {
		if err := write(t, s, fmt.Sprintf("f%d", i), "x"); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := write(t, s, "too-fast", "x"); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("err = %v, want ErrQuotaExceeded", err)
	}
}

// An unset quota must not mean unlimited. A default of "no limit" on something
// that costs money is a decision to trust the code, and the code is the one
// thing nobody here trusts.
func TestAnUnsetQuotaIsBoundedNotUnlimited(t *testing.T) {
	s, _ := newTestStore(t, Mount{Prefix: "sandboxes/sb_a"})

	if got := s.mount.Quota.MaxBytes; got == 0 {
		t.Error("MaxBytes is 0 with no quota set: a sandbox could write forever")
	}
	if got := s.mount.Quota.MaxObjects; got == 0 {
		t.Error("MaxObjects is 0 with no quota set")
	}
	if got := s.mount.Quota.MaxWritesPerMinute; got == 0 {
		t.Error("MaxWritesPerMinute is 0 with no quota set")
	}
}

// --- semantics --------------------------------------------------------------

// The refusals are the design, not a gap. Each one has to say enough that a
// developer knows what to do instead.
func TestUnsupportedOperationsFailLoudlyAndExplain(t *testing.T) {
	s, _ := newTestStore(t, Mount{Prefix: "sandboxes/sb_a"})

	err := s.Rename(context.Background(), "a.txt", "b.txt")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	// The message has to carry the reason. "EOPNOTSUPP" alone sends a developer
	// hunting through our source for an explanation we already know.
	for _, want := range []string{"atomic rename", "instead"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Rename error %q does not mention %q", err, want)
		}
	}
}

func TestReadOnlyMountRefusesWrites(t *testing.T) {
	s, _ := newTestStore(t, Mount{Prefix: "sandboxes/sb_a", ReadOnly: true})

	if err := write(t, s, "x.txt", "data"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("write to a read-only mount: err = %v, want ErrReadOnly", err)
	}
	if err := s.Remove(context.Background(), "x.txt"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("delete on a read-only mount: err = %v, want ErrReadOnly", err)
	}
}

// --- ordinary use -----------------------------------------------------------

func TestRoundTrip(t *testing.T) {
	s, _ := newTestStore(t, Mount{Prefix: "sandboxes/sb_a"})
	const body = "resultados\n"

	if err := write(t, s, "/out/result.txt", body); err != nil {
		t.Fatal(err)
	}

	rc, err := s.Open(context.Background(), "/out/result.txt", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, _ := io.ReadAll(rc)
	if string(got) != body {
		t.Errorf("read %q, want %q", got, body)
	}
}

// A filesystem read is a ranged get. Without it, reading 4KB of a 5GB object
// fetches 5GB.
func TestRangedRead(t *testing.T) {
	s, _ := newTestStore(t, Mount{Prefix: "sandboxes/sb_a"})
	if err := write(t, s, "big.txt", "0123456789"); err != nil {
		t.Fatal(err)
	}

	rc, err := s.Open(context.Background(), "big.txt", 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, _ := io.ReadAll(rc)
	if string(got) != "3456" {
		t.Errorf("ranged read = %q, want %q", got, "3456")
	}
}

// Reading past the end is an empty read, not an error: that is what a
// filesystem does, and the layer above this is a filesystem.
func TestReadPastTheEnd(t *testing.T) {
	s, _ := newTestStore(t, Mount{Prefix: "sandboxes/sb_a"})
	if err := write(t, s, "small.txt", "abc"); err != nil {
		t.Fatal(err)
	}

	rc, err := s.Open(context.Background(), "small.txt", 100, 10)
	if err != nil {
		t.Fatalf("reading past the end errored: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if len(got) != 0 {
		t.Errorf("read %q past the end, want nothing", got)
	}
}

func TestListLooksLikeDirectories(t *testing.T) {
	s, _ := newTestStore(t, Mount{Prefix: "sandboxes/sb_a"})

	for _, p := range []string{"top.txt", "data/a.txt", "data/b.txt", "data/deep/c.txt"} {
		if err := write(t, s, p, "x"); err != nil {
			t.Fatal(err)
		}
	}

	listing, err := s.List(context.Background(), "/", "", 100)
	if err != nil {
		t.Fatal(err)
	}

	if len(listing.Objects) != 1 || listing.Objects[0].Key != "/top.txt" {
		t.Errorf("root objects = %v, want just /top.txt", listing.Objects)
	}
	if len(listing.CommonPrefixes) != 1 || listing.CommonPrefixes[0] != "/data/" {
		t.Errorf("root directories = %v, want just /data/", listing.CommonPrefixes)
	}
}

func TestStatOnSomethingMissing(t *testing.T) {
	s, _ := newTestStore(t, Mount{Prefix: "sandboxes/sb_a"})
	if _, err := s.Stat(context.Background(), "nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
