package vsock

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostListenerPath(t *testing.T) {
	// The underscore is Firecracker's convention and getting it wrong is silent:
	// the guest's connection is simply refused, with nothing on the host to say
	// why. Pin the exact spelling.
	tests := []struct {
		udsPath string
		port    uint32
		want    string
	}{
		{"/srv/jail/root/v.sock", 5001, "/srv/jail/root/v.sock_5001"},
		{"/x/v.sock", 5000, "/x/v.sock_5000"},
		{"v.sock", 1, "v.sock_1"},
	}
	for _, tc := range tests {
		if got := HostListenerPath(tc.udsPath, tc.port); got != tc.want {
			t.Errorf("HostListenerPath(%q, %d) = %q, want %q", tc.udsPath, tc.port, got, tc.want)
		}
	}
}

// shortTempDir is t.TempDir() with the test's name left out of the path.
//
// t.TempDir() embeds the test name, and on macOS the base is already a 47-byte
// /var/folders/... path -- together they blow past sun_path (104 bytes there,
// 108 on Linux) and bind fails with a bare EINVAL that mentions nothing about
// length. Writing this file, the long-named tests failed and the short-named
// ones passed, which is precisely the trap checkVsockPath exists to catch.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// listenForTest opens a listener in a temp dir, owned by whoever runs the test.
//
// Chowning to the real VMM uid needs root; chowning to yourself is a no-op the
// kernel still permits, which is enough to prove the call is made with sane
// arguments and does not fail.
func listenForTest(t *testing.T, port uint32) (net.Listener, string) {
	t.Helper()
	uds := filepath.Join(shortTempDir(t), "v.sock")
	l, err := Listen(uds, port, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, HostListenerPath(uds, port)
}

// TestListenDoesNotConsumeAHandshake is the point of this file.
//
// Guest-initiated connections carry no CONNECT line: Firecracker resolves the
// port from the socket's *name* and then passes bytes through untouched. If
// anyone ever "restores the symmetry" with Dial by reading a handshake here,
// the first bytes of the guest's real request get eaten and the connection
// hangs waiting for a newline that never arrives.
//
// So: the client says nothing but the payload, and the payload must survive
// byte for byte.
func TestListenDoesNotConsumeAHandshake(t *testing.T) {
	l, path := listenForTest(t, 5001)

	const payload = "POST /storage HTTP/1.1\r\nHost: h\r\n\r\n"

	go func() {
		c, err := net.Dial("unix", path)
		if err != nil {
			return
		}
		defer c.Close()
		io.WriteString(c, payload)
	}()

	conn, err := l.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read payload: %v (a handshake read here would eat these bytes)", err)
	}
	if string(got) != payload {
		t.Errorf("got %q, want %q -- the connection is not a raw stream", got, payload)
	}
}

// TestListenRoundTrip proves the stream is bidirectional after accept, which is
// what lets net/http run over it in both directions.
func TestListenRoundTrip(t *testing.T) {
	l, path := listenForTest(t, 5001)

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()

	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping\n" {
		t.Errorf("echoed %q, want %q", got, "ping\n")
	}
}

// TestListenReplacesAStaleSocket covers the failure that only shows up after a
// crash: the socket file outlives the process, and bind fails with EADDRINUSE
// forever after.
func TestListenReplacesAStaleSocket(t *testing.T) {
	uds := filepath.Join(shortTempDir(t), "v.sock")
	path := HostListenerPath(uds, 5001)

	// A leftover file where the socket goes, exactly as a killed daemon leaves.
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	l, err := Listen(uds, 5001, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	defer l.Close()

	// And it must be a working socket, not merely a created file.
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial the replaced socket: %v", err)
	}
	c.Close()
}

// TestListenIsPrivate checks the mode the sandbox's isolation rests on.
//
// The socket is the identity: anything that can connect to it is treated as
// that sandbox. A world-writable one would let any local user on the host speak
// for any sandbox, which is a quieter hole than it sounds -- everything would
// work perfectly.
func TestListenIsPrivate(t *testing.T) {
	_, path := listenForTest(t, 5001)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode is %04o, want 0600: anyone on the host could speak for this sandbox", perm)
	}
}

// TestListenTwiceOnDifferentPorts checks that ports are separate files rather
// than one multiplexed socket -- the structural difference from Dial.
func TestListenTwiceOnDifferentPorts(t *testing.T) {
	uds := filepath.Join(shortTempDir(t), "v.sock")

	a, err := Listen(uds, 5001, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	b, err := Listen(uds, 5002, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("a second port must be a second socket, not a conflict: %v", err)
	}
	defer b.Close()

	if a.Addr().String() == b.Addr().String() {
		t.Fatal("both ports landed on the same socket")
	}
}
