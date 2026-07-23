//go:build linux

package storagefs_test

// This is the test that cannot run off Linux: it mounts a real FUSE filesystem
// and drives it through the kernel. Everything up to here was verified against
// interfaces and buffers; this is where an open()/read()/write()/close() from an
// ordinary program actually crosses the kernel into the FUSE daemon, out over
// the storage client, into the host server, and back. It is the whole stack.
//
// It needs /dev/fuse and fusermount, so it skips where they are absent rather
// than failing -- a CI box without FUSE is not a broken build. Run it on a host
// that has them:
//
//	GOOS=linux GOARCH=arm64 go test -c ./internal/storagefs -o storagefs.test
//	scp storagefs.test host: && ssh host ./storagefs.test -test.v
//
// The storage server it talks to is in-process with a memory backend, so the
// test exercises the FUSE and client code for real while owning its data end to
// end -- which lets it check the write path twice: once by reading back through
// the mount, and once by looking straight at the backend the host wrote to.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/pablofdezr/microvm/internal/storage"
	"github.com/pablofdezr/microvm/internal/storageclient"
	"github.com/pablofdezr/microvm/internal/storagefs"
)

const testPrefix = "tenants/t_test"

// mountForTest stands up the in-process server, mounts the filesystem, and
// returns the mount point and the backend behind it. The mount is torn down on
// cleanup.
func mountForTest(t *testing.T, readOnly bool) (string, *storage.Memory) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("no /dev/fuse: %v", err)
	}

	backend := storage.NewMemory()
	store := storage.NewStore(backend, storage.Mount{
		Prefix: testPrefix,
		// A generous quota: this test is about the filesystem, not the limits,
		// which have their own tests.
		Quota: storage.Quota{MaxBytes: 1 << 30, MaxObjects: 100000, MaxWritesPerMinute: 1000000},
	})

	sockDir := t.TempDir()
	sock := filepath.Join(sockDir, "storage.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = storage.Serve(ctx, ln, store, nil) }()

	client := storageclient.New(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	})

	mountpoint := t.TempDir()
	server, err := storagefs.Mount(mountpoint, client, readOnly, false)
	if err != nil {
		cancel()
		ln.Close()
		t.Fatalf("mount: %v", err)
	}
	t.Cleanup(func() {
		// Unmount first so the serve loop stops touching the client, then close
		// the plumbing behind it.
		if err := server.Unmount(); err != nil {
			t.Logf("unmount: %v", err)
		}
		cancel()
		ln.Close()
	})
	return mountpoint, backend
}

func TestFUSEWriteReadDelete(t *testing.T) {
	mp, backend := mountForTest(t, false)

	// Write a file the ordinary way: create, write, close.
	path := filepath.Join(mp, "hello.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The write path, checked at the host: the object landed under the prefix.
	if got, ok := backend.Object(testPrefix + "/hello.txt"); !ok || string(got) != "hello world" {
		t.Fatalf("backend has %q (ok=%v), want %q", got, ok, "hello world")
	}

	// The read path, checked through the mount.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("read back %q", got)
	}

	// Stat sees the right size.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 11 {
		t.Fatalf("size = %d, want 11", info.Size())
	}

	// Delete, then it is gone on both sides.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := backend.Object(testPrefix + "/hello.txt"); ok {
		t.Fatal("object still in backend after unlink")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after delete = %v, want not-exist", err)
	}
}

func TestFUSEReaddir(t *testing.T) {
	mp, _ := mountForTest(t, false)

	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(mp, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(mp, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mp, "sub", "inner.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(mp)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = e.IsDir()
	}
	for _, want := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, ok := names[want]; !ok {
			t.Errorf("readdir missing %q (got %v)", want, names)
		}
	}
	if isDir, ok := names["sub"]; !ok || !isDir {
		t.Errorf("sub not listed as a directory (got %v)", names)
	}

	// The nested file is reachable and lists under its directory.
	inner, err := os.ReadDir(filepath.Join(mp, "sub"))
	if err != nil {
		t.Fatalf("readdir sub: %v", err)
	}
	if len(inner) != 1 || inner[0].Name() != "inner.txt" {
		t.Errorf("sub contents = %v, want [inner.txt]", inner)
	}
}

func TestFUSEOverwrite(t *testing.T) {
	mp, backend := mountForTest(t, false)
	path := filepath.Join(mp, "f")

	if err := os.WriteFile(path, []byte("first version, longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	// O_TRUNC (WriteFile truncates) replaces it wholesale with something shorter,
	// which is the case a naive "grow only" buffer would get wrong.
	if err := os.WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := backend.Object(testPrefix + "/f"); string(got) != "second" {
		t.Fatalf("after overwrite backend has %q, want %q", got, "second")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "second" {
		t.Fatalf("after overwrite read %q", got)
	}
}

func TestFUSEAppendReadModifyWrite(t *testing.T) {
	mp, backend := mountForTest(t, false)
	path := filepath.Join(mp, "log")

	if err := os.WriteFile(path, []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Append opens without O_TRUNC, so the daemon must load the existing bytes
	// before adding to them -- object storage has no append of its own.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("line2\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, _ := backend.Object(testPrefix + "/log"); string(got) != "line1\nline2\n" {
		t.Fatalf("append produced %q, want %q", got, "line1\nline2\n")
	}
}

func TestFUSEPartialWriteAtOffset(t *testing.T) {
	mp, backend := mountForTest(t, false)
	path := filepath.Join(mp, "f")

	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("AB"), 4); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, _ := backend.Object(testPrefix + "/f"); string(got) != "0123AB6789" {
		t.Fatalf("pwrite produced %q, want %q", got, "0123AB6789")
	}
}

func TestFUSETruncate(t *testing.T) {
	mp, backend := mountForTest(t, false)
	path := filepath.Join(mp, "f")

	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 4); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if got, _ := backend.Object(testPrefix + "/f"); string(got) != "0123" {
		t.Fatalf("after truncate backend has %q, want %q", got, "0123")
	}
}

func TestFUSELargeFile(t *testing.T) {
	mp, backend := mountForTest(t, false)
	path := filepath.Join(mp, "big.bin")

	// A megabyte, larger than one FUSE write, so the daemon reassembles multiple
	// Write calls into one object.
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := backend.Object(testPrefix + "/big.bin")
	if !ok {
		t.Fatal("big file not stored")
	}
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("stored %d bytes, checksum differs from written %d", len(got), len(data))
	}

	// And it reads back byte-for-byte through the mount, exercising ranged reads.
	back, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, data) {
		t.Fatalf("read back %d bytes, differ from written", len(back))
	}
}

func TestFUSEReadOnlyRejectsWrites(t *testing.T) {
	mp, backend := mountForTest(t, true)

	// Seed an object straight into the backend so there is something to read.
	if err := backend.Put(context.Background(), testPrefix+"/seed.txt", bytes.NewReader([]byte("readable")), 8); err != nil {
		t.Fatal(err)
	}

	// Reads work.
	got, err := os.ReadFile(filepath.Join(mp, "seed.txt"))
	if err != nil {
		t.Fatalf("read on ro mount: %v", err)
	}
	if string(got) != "readable" {
		t.Fatalf("ro read got %q", got)
	}

	// Writes fail with EROFS, before touching the host.
	err = os.WriteFile(filepath.Join(mp, "new.txt"), []byte("x"), 0o644)
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("write on ro mount = %v, want EROFS", err)
	}
}

func TestFUSEStatfs(t *testing.T) {
	mp, _ := mountForTest(t, false)
	if err := os.WriteFile(filepath.Join(mp, "f"), []byte("some bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(mp, &st); err != nil {
		t.Fatalf("statfs: %v", err)
	}
	if st.Bsize == 0 || st.Blocks == 0 {
		t.Fatalf("statfs returned empty geometry: bsize=%d blocks=%d", st.Bsize, st.Blocks)
	}
	// The mount's quota (1 GiB, set in mountForTest) is the real ceiling, so the
	// total must reflect it rather than a guessed headroom. This is what makes df
	// inside the guest tell the truth.
	wantTotal := uint64((int64(1<<30) + int64(st.Bsize) - 1) / int64(st.Bsize))
	if uint64(st.Blocks) != wantTotal {
		t.Errorf("statfs total = %d blocks, want %d (the 1 GiB quota)", st.Blocks, wantTotal)
	}
}

// TestFUSEMissingFileIsENOENT confirms the errno crosses the whole stack: a
// program's open() of a missing file gets ENOENT, not a generic failure.
func TestFUSEMissingFileIsENOENT(t *testing.T) {
	mp, _ := mountForTest(t, false)
	_, err := os.Open(filepath.Join(mp, "nope"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open missing = %v, want not-exist", err)
	}
}
