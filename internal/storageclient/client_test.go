package storageclient_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/pablofdezr/microvm/internal/storage"
	"github.com/pablofdezr/microvm/internal/storageclient"
)

// serve stands up the real host storage.Server over a loopback listener and
// returns a client wired to dial it. Testing against the actual server, rather
// than a hand-rolled fake, is the point: this is the one place the guest's idea
// of the protocol meets the host's, and a mismatch here is invisible until a VM
// is booted.
func serve(t *testing.T, mount storage.Mount) (*storageclient.Client, *storage.Memory) {
	t.Helper()
	backend := storage.NewMemory()
	store := storage.NewStore(backend, mount)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = storage.Serve(ctx, ln, store, nil) }()
	t.Cleanup(func() { cancel(); ln.Close() })

	addr := ln.Addr().String()
	client := storageclient.New(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
	return client, backend
}

func mustPut(t *testing.T, c *storageclient.Client, path, data string) {
	t.Helper()
	if err := c.Put(context.Background(), path, strings.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func getAll(t *testing.T, c *storageclient.Client, path string, offset, length int64) string {
	t.Helper()
	rc, err := c.Get(context.Background(), path, offset, length)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestPutThenGetRoundTrips(t *testing.T) {
	c, _ := serve(t, storage.Mount{Prefix: "tenants/t_a"})
	mustPut(t, c, "/hello.txt", "hello world")
	if got := getAll(t, c, "/hello.txt", 0, -1); got != "hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestGetRangeReadsASlice(t *testing.T) {
	c, _ := serve(t, storage.Mount{Prefix: "tenants/t_a"})
	mustPut(t, c, "/f", "0123456789")

	// A middle slice: offset 3, four bytes. This is the read a filesystem issues
	// for a pread, and getting the inclusive-range arithmetic wrong shows up here
	// as an off-by-one rather than anywhere near the FUSE layer.
	if got := getAll(t, c, "/f", 3, 4); got != "3456" {
		t.Fatalf("range read got %q, want %q", got, "3456")
	}
	// Open-ended from an offset.
	if got := getAll(t, c, "/f", 7, -1); got != "789" {
		t.Fatalf("open-ended range got %q, want %q", got, "789")
	}
}

func TestStatReportsSize(t *testing.T) {
	c, _ := serve(t, storage.Mount{Prefix: "tenants/t_a"})
	mustPut(t, c, "/dir/file", "twelve bytes")

	info, err := c.Stat(context.Background(), "/dir/file")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size != 12 {
		t.Fatalf("size = %d, want 12", info.Size)
	}
}

func TestStatMissingIsENOENT(t *testing.T) {
	c, _ := serve(t, storage.Mount{Prefix: "tenants/t_a"})
	_, err := c.Stat(context.Background(), "/nope")
	if got := storageclient.ErrnoOf(err); got != "ENOENT" {
		t.Fatalf("errno = %q, want ENOENT (err=%v)", got, err)
	}
}

func TestListShowsObjectsAndDirs(t *testing.T) {
	c, _ := serve(t, storage.Mount{Prefix: "tenants/t_a"})
	mustPut(t, c, "/a.txt", "a")
	mustPut(t, c, "/b.txt", "bb")
	mustPut(t, c, "/sub/c.txt", "ccc")

	out, err := c.List(context.Background(), "/", "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out.Objects) != 2 {
		t.Fatalf("objects = %d, want 2 (%+v)", len(out.Objects), out.Objects)
	}
	if len(out.CommonPrefixes) != 1 {
		t.Fatalf("dirs = %d, want 1 (%+v)", len(out.CommonPrefixes), out.CommonPrefixes)
	}
}

func TestDeleteRemoves(t *testing.T) {
	c, _ := serve(t, storage.Mount{Prefix: "tenants/t_a"})
	mustPut(t, c, "/gone", "x")
	if err := c.Delete(context.Background(), "/gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Stat(context.Background(), "/gone"); storageclient.ErrnoOf(err) != "ENOENT" {
		t.Fatalf("still there after delete: %v", err)
	}
}

func TestPutNegativeSizeRefusedLocally(t *testing.T) {
	c, _ := serve(t, storage.Mount{Prefix: "tenants/t_a"})
	// A negative size never reaches the host: object storage cannot take a write
	// whose length it does not know, and the client says so as EINVAL rather than
	// streaming something the host will reject less clearly.
	err := c.Put(context.Background(), "/x", strings.NewReader("data"), -1)
	if storageclient.ErrnoOf(err) != "EINVAL" {
		t.Fatalf("errno = %q, want EINVAL", storageclient.ErrnoOf(err))
	}
}

func TestReadOnlyMountRejectsWriteAsEROFS(t *testing.T) {
	c, _ := serve(t, storage.Mount{Prefix: "tenants/t_a", ReadOnly: true})
	err := c.Put(context.Background(), "/x", strings.NewReader("data"), 4)
	if got := storageclient.ErrnoOf(err); got != "EROFS" {
		t.Fatalf("errno = %q, want EROFS (err=%v)", got, err)
	}
}

func TestQuotaExceededIsEDQUOT(t *testing.T) {
	// A tiny byte quota so the second write is over the line.
	c, _ := serve(t, storage.Mount{
		Prefix: "tenants/t_a",
		Quota:  storage.Quota{MaxBytes: 4, MaxObjects: 100, MaxWritesPerMinute: 100},
	})
	mustPut(t, c, "/a", "aaaa") // exactly fills it
	err := c.Put(context.Background(), "/b", strings.NewReader("b"), 1)
	if got := storageclient.ErrnoOf(err); got != "EDQUOT" {
		t.Fatalf("errno = %q, want EDQUOT (err=%v)", got, err)
	}
}

func TestUsageReflectsWrites(t *testing.T) {
	c, _ := serve(t, storage.Mount{Prefix: "tenants/t_a"})
	mustPut(t, c, "/a", "hello") // 5 bytes

	u, err := c.Usage(context.Background())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.BytesWritten != 5 || u.Objects != 1 {
		t.Fatalf("usage = %+v, want 5 bytes / 1 object", u)
	}
	// The mount named no quota, so it defaulted to 1 GiB, and that is the ceiling
	// statfs should report as the total. A zero here would mean the limit never
	// crossed the wire and df would be back to guessing.
	if u.Limit != 1<<30 {
		t.Fatalf("limit = %d, want the default 1 GiB (%d)", u.Limit, int64(1<<30))
	}
}

func TestUsageReportsTenantCapWhenTighter(t *testing.T) {
	// A tenant cap below the per-sandbox quota is the binding ceiling, and the
	// one statfs must report.
	c, _ := serve(t, storage.Mount{
		Prefix: "tenants/t_a",
		Quota:  storage.Quota{MaxBytes: 1 << 30, MaxObjects: 100, MaxWritesPerMinute: 100},
		Tenant: storage.TenantPolicy{MaxBytes: 4 << 20, OnFull: storage.Preserve},
	})
	u, err := c.Usage(context.Background())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.Limit != 4<<20 {
		t.Fatalf("limit = %d, want the tenant cap 4 MiB (%d)", u.Limit, int64(4<<20))
	}
}

func TestErrorAsUnwrapsToStorageError(t *testing.T) {
	c, _ := serve(t, storage.Mount{Prefix: "tenants/t_a"})
	_, err := c.Stat(context.Background(), "/missing")
	var se *storageclient.Error
	if !errors.As(err, &se) {
		t.Fatalf("error is not *storageclient.Error: %T", err)
	}
	if se.Status != 404 {
		t.Fatalf("status = %d, want 404", se.Status)
	}
}

// TestEscapeAttemptIsENOENT confirms the guest cannot climb out of its prefix
// through the client: the host answers a "../" exactly as it answers a missing
// file, and the client surfaces that unchanged.
func TestEscapeAttemptIsENOENT(t *testing.T) {
	c, _ := serve(t, storage.Mount{Prefix: "tenants/t_a"})
	_, err := c.Stat(context.Background(), "/../t_b/secret")
	if got := storageclient.ErrnoOf(err); got != "ENOENT" {
		t.Fatalf("errno = %q, want ENOENT", got)
	}
}
