package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestFetcher points a Fetcher at an httptest server.
//
// httptest listens on 127.0.0.1 with a self-signed certificate, and the policy
// refuses both. The two exemptions have no exported setter, so nothing outside
// this file can take them, and TestLoopbackRefusedOnTheDialPath asserts the
// default answer for the address the daemon actually gets.
func newTestFetcher(t *testing.T, srv *httptest.Server, cfg Config) *Fetcher {
	t.Helper()

	cfg.AllowHosts = append(cfg.AllowHosts, "127.0.0.1", "example.com")
	f, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.permitLoopback = true
	f.tlsConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	return f
}

// serveArchive is a server handing out one small tarball.
func serveArchive(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sampleTarGz(t *testing.T) []byte {
	t.Helper()
	return gzipBytes(t, buildTar(t,
		tarEntry{name: "app/", typ: tar.TypeDir},
		tarEntry{name: "app/main.go", body: "package main\n"},
		tarEntry{name: "app/run.sh", body: "#!/bin/sh\n", mode: 0o755},
	))
}

func TestFetchAndExpand(t *testing.T) {
	want := sampleTarGz(t)
	srv := serveArchive(t, want)
	f := newTestFetcher(t, srv, Config{})

	arc, err := f.Fetch(context.Background(), srv.URL+"/app.tar.gz")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer arc.Close()

	if arc.Size() != int64(len(want)) {
		t.Errorf("buffered %d bytes, want %d", arc.Size(), len(want))
	}

	var w recordingWriter
	man, err := arc.Expand(context.Background(), Options{StripComponents: 1}, &w)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if man.Files != 2 {
		t.Errorf("expanded %d files, want 2", man.Files)
	}
	if got := w.files["run.sh"]; got.mode != "0755" {
		t.Errorf("run.sh mode = %q, want 0755", got.mode)
	}
	if got := w.files["main.go"]; got.content != "package main\n" {
		t.Errorf("main.go content = %q", got.content)
	}
}

// The one test that runs with the loopback exemption off, which is how the
// daemon runs. If this ever passes, the fetcher can reach the host's own API and
// its cloud metadata service.
func TestLoopbackRefusedOnTheDialPath(t *testing.T) {
	srv := serveArchive(t, sampleTarGz(t))

	f, err := New(Config{AllowHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	f.tlsConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig

	_, err = f.Fetch(context.Background(), srv.URL+"/app.tar.gz")
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("Fetch of a loopback URL returned %v, want ErrNotPermitted", err)
	}
}

// A refusal must survive the transport's *url.Error wrapper without carrying the
// URL out with it: the query string of a presigned URL is a credential, and an
// error reaches a log.
func TestRefusalDoesNotLeakTheQueryString(t *testing.T) {
	srv := serveArchive(t, sampleTarGz(t))

	f, err := New(Config{AllowHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	f.tlsConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig

	_, err = f.Fetch(context.Background(), srv.URL+"/app.tar.gz?X-Amz-Signature=topsecret")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("the refusal quotes the credential: %v", err)
	}
}

func TestFetchWithNoAllowlistRefusesEverything(t *testing.T) {
	f, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fetch(context.Background(), "https://github.com/o/r.tar.gz"); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("Fetch with an empty allowlist returned %v, want ErrNotPermitted", err)
	}
}

// A resolved address is filtered, not trusted. The metadata address in the
// answer is dropped and the fetch continues on the address that passes -- a
// round-robin host with one bad answer is still reachable.
func TestResolvedAddressesAreFiltered(t *testing.T) {
	srv := serveArchive(t, sampleTarGz(t))
	f := newTestFetcher(t, srv, Config{})

	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("169.254.169.254"),
			netip.MustParseAddr("127.0.0.1"),
		}, nil
	}

	arc, err := f.Fetch(context.Background(), "https://example.com:"+port+"/app.tar.gz")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	arc.Close()
}

// The rebinding shape: an allowlisted name that answers with a blocked address.
// Nothing is dialled, so the second lookup that a name-based transport would do
// never happens.
func TestNameResolvingOnlyToABlockedAddress(t *testing.T) {
	srv := serveArchive(t, sampleTarGz(t))
	f := newTestFetcher(t, srv, Config{})

	for _, answer := range []string{"169.254.169.254", "127.0.0.1", "::ffff:169.254.169.254", "10.0.0.1"} {
		f.lookup = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr(answer)}, nil
		}
		f.permitLoopback = false

		_, err := f.Fetch(context.Background(), "https://example.com/app.tar.gz")
		if !errors.Is(err, ErrNotPermitted) {
			t.Errorf("a name resolving to %s returned %v, want ErrNotPermitted", answer, err)
		}
	}
}

func TestUnresolvableHostIsRefusedUniformly(t *testing.T) {
	srv := serveArchive(t, sampleTarGz(t))
	f := newTestFetcher(t, srv, Config{})
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("no such host")
	}

	_, err := f.Fetch(context.Background(), "https://example.com/app.tar.gz")
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("an unresolvable host returned %v, want ErrNotPermitted", err)
	}
	// Same class as a blocked address, so the two cannot be told apart from
	// outside. That is what stops the fetcher being a port scanner.
	if strings.Contains(err.Error(), "no such host") {
		t.Errorf("the refusal repeats the resolver's answer: %v", err)
	}
}

// Dialer.Control is the check on the syscall path, after resolution and before
// connect. Table-tested on its own because that is the only way to see it decide
// without a connection to make.
func TestControlChecksTheSockaddr(t *testing.T) {
	f, err := New(Config{AllowHosts: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		address string
		permit  bool
	}{
		{"169.254.169.254:80", false},
		{"127.0.0.1:8080", false},
		{"10.0.0.1:443", false},
		{"172.20.0.5:443", false},
		{"[::1]:443", false},
		{"[::ffff:169.254.169.254]:443", false},
		{"[fd00::1]:443", false},
		{"not-an-address", false},
		{"8.8.8.8:443", true},
		{"[2606:2800:220:1:248:1893:25c8:1946]:443", true},
	}

	for _, tc := range tests {
		err := f.control("tcp", tc.address, nil)
		if tc.permit && err != nil {
			t.Errorf("control(%q) refused: %v", tc.address, err)
		}
		if !tc.permit && err == nil {
			t.Errorf("control(%q) permitted it", tc.address)
		}
	}
}

func TestRedirects(t *testing.T) {
	body := sampleTarGz(t)

	// The hop's destination travels in the request rather than in a variable the
	// handler goroutine and the test goroutine would share.
	mux := http.NewServeMux()
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	f := newTestFetcher(t, srv, Config{})

	away := func(to string) string {
		return srv.URL + "/away?to=" + url.QueryEscape(to)
	}

	// One hop within the allowlist, which is the codeload/CDN case and must work.
	arc, err := f.Fetch(context.Background(), away(srv.URL+"/app.tar.gz"))
	if err != nil {
		t.Fatalf("a redirect inside the allowlist failed: %v", err)
	}
	arc.Close()

	tests := []struct {
		name   string
		target string
		want   error
	}{
		{"to a host that is not allowlisted", "https://evil.example/x.tgz", ErrNotPermitted},
		{"to the metadata address", "https://169.254.169.254/latest/meta-data/", ErrNotPermitted},
		{"downgraded to plain http", "http://127.0.0.1/x.tgz", ErrNotPermitted},
		{"to a file URL", "file:///etc/shadow", ErrNotPermitted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.Fetch(context.Background(), away(tc.target)); !errors.Is(err, tc.want) {
				t.Fatalf("a redirect %s returned %v, want %v", tc.name, err, tc.want)
			}
		})
	}

	t.Run("a loop hits the hop cap", func(t *testing.T) {
		_, err := f.Fetch(context.Background(), srv.URL+"/loop")
		if !errors.Is(err, ErrFetchFailed) {
			t.Fatalf("a redirect loop returned %v, want ErrFetchFailed", err)
		}
		if !strings.Contains(err.Error(), fmt.Sprint(maxRedirects)) {
			t.Errorf("the error does not name the hop cap: %v", err)
		}
	})
}

// The compressed cap is counted, not read off a header. A chunked response has
// no Content-Length at all, which is the case a length-based check misses.
func TestBodyCapIsCountedNotDeclared(t *testing.T) {
	const chunk = 4096
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 8; i++ {
			w.Write(bytes.Repeat([]byte("x"), chunk))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv, Config{MaxBytes: chunk})
	_, err := f.Fetch(context.Background(), srv.URL+"/big")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an oversized chunked body returned %v, want ErrTooLarge", err)
	}
}

func TestBodyOfExactlyTheCapIsAccepted(t *testing.T) {
	body := gzipBytes(t, buildTar(t, tarEntry{name: "a.txt", body: "hello"}))
	srv := serveArchive(t, body)

	f := newTestFetcher(t, srv, Config{MaxBytes: int64(len(body))})
	arc, err := f.Fetch(context.Background(), srv.URL+"/a.tgz")
	if err != nil {
		t.Fatalf("a body of exactly the cap was refused: %v", err)
	}
	arc.Close()
}

// A declared length may refuse early. It may never permit -- that is what the
// counting reader above is for.
func TestDeclaredLengthRefusesEarly(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		w.Write(bytes.Repeat([]byte("x"), 1<<20))
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv, Config{MaxBytes: 1024})
	if _, err := f.Fetch(context.Background(), srv.URL+"/big"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a declared oversize body returned %v, want ErrTooLarge", err)
	}
}

func TestOriginErrorsAreFetchFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"not found", http.StatusNotFound},
		{"unauthorized", http.StatusUnauthorized},
		{"server error", http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Something a caller must never be shown: the point of an
				// allowlisted origin is that its status is safe, not its body.
				http.Error(w, "internal-service-secret", tc.status)
			}))
			defer srv.Close()

			f := newTestFetcher(t, srv, Config{})
			_, err := f.Fetch(context.Background(), srv.URL+"/x.tgz")
			if !errors.Is(err, ErrFetchFailed) {
				t.Fatalf("a %d returned %v, want ErrFetchFailed", tc.status, err)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(tc.status)) {
				t.Errorf("the error does not name the status: %v", err)
			}
			if strings.Contains(err.Error(), "internal-service-secret") {
				t.Errorf("the error quotes the origin's body: %v", err)
			}
		})
	}
}

func TestEmptyBodyIsAFetchFailure(t *testing.T) {
	srv := serveArchive(t, nil)
	f := newTestFetcher(t, srv, Config{})
	if _, err := f.Fetch(context.Background(), srv.URL+"/empty"); !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("an empty body returned %v, want ErrFetchFailed", err)
	}
}

func TestSlowOriginHitsTheTimeout(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv, Config{Timeout: 100 * time.Millisecond})
	start := time.Now()
	_, err := f.Fetch(context.Background(), srv.URL+"/slow")
	if !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("a stalled body returned %v, want ErrFetchFailed", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the timeout took %s to fire", elapsed)
	}
}

// The buffer is unlinked as soon as it exists, so a fetch leaves nothing behind
// even without a Close.
func TestBufferIsNotOnDisk(t *testing.T) {
	dir := t.TempDir()
	srv := serveArchive(t, sampleTarGz(t))

	f := newTestFetcher(t, srv, Config{TempDir: dir})
	arc, err := f.Fetch(context.Background(), srv.URL+"/app.tgz")
	if err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the buffer is visible on disk: %v", entries)
	}
	// Still readable through the handle, which is the point.
	if _, err := arc.Validate(Options{}); err != nil {
		t.Errorf("the unlinked buffer is not readable: %v", err)
	}
	arc.Close()
}

func TestClosedArchiveIsNotUsable(t *testing.T) {
	srv := serveArchive(t, sampleTarGz(t))
	f := newTestFetcher(t, srv, Config{})

	arc, err := f.Fetch(context.Background(), srv.URL+"/app.tgz")
	if err != nil {
		t.Fatal(err)
	}
	arc.Close()

	if _, err := arc.Validate(Options{}); err == nil {
		t.Error("a closed archive validated")
	}
	if err := arc.Close(); err != nil {
		t.Errorf("Close is not idempotent: %v", err)
	}
}

func TestNegativeLimitsAreRejected(t *testing.T) {
	for _, cfg := range []Config{
		{MaxBytes: -1},
		{MaxExpandedBytes: -1},
		{MaxFileBytes: -1},
		{MaxFiles: -1},
		{Timeout: -time.Second},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) accepted a negative limit", cfg)
		}
	}
}

func TestWithTimeoutBoundsTheWholeOperation(t *testing.T) {
	f, err := New(Config{Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := f.WithTimeout(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("WithTimeout returned a context with no deadline")
	}
	if until := time.Until(deadline); until > time.Second {
		t.Errorf("deadline is %s away, want the configured 50ms", until)
	}
}

// unwrapSentinel has to find our own failure under whatever the transport
// wrapped it in, and drop the wrapper that quotes the URL.
func TestUnwrapSentinel(t *testing.T) {
	for _, sentinel := range []error{ErrNotPermitted, ErrTooLarge, ErrFetchFailed, ErrInvalidArchive} {
		wrapped := &url.Error{
			Op:  "Get",
			URL: "https://host/x?X-Amz-Signature=topsecret",
			Err: fmt.Errorf("%w: address 169.254.169.254 is blocked", sentinel),
		}

		got := unwrapSentinel(wrapped)
		if !errors.Is(got, sentinel) {
			t.Errorf("unwrapSentinel lost the class %v: %v", sentinel, got)
		}
		if got != nil && strings.Contains(got.Error(), "topsecret") {
			t.Errorf("unwrapSentinel kept the URL: %v", got)
		}
	}
	if got := unwrapSentinel(errors.New("something else")); got != nil {
		t.Errorf("unwrapSentinel of an unrelated error = %v, want nil", got)
	}
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
