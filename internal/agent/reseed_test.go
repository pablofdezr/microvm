package agent

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// The tests in this file pin the one property that makes a snapshot-backed warm
// pool safe: a restored guest's CSPRNG must actually be rotated, and a host that
// cannot rotate it must be told so rather than reassured.
//
// The mechanism they guard replaced a plain write to /dev/urandom. That write
// mixes bytes into the kernel's input pool and nothing else -- it credits no
// entropy and it does not re-derive base_crng.key, which is what getrandom(2)
// actually returns bytes from. The key is rotated on a 60-second jiffies deadline
// that a snapshot restores along with the jiffies, so every restore of one
// template shared the same fast key for the same first minute of guest execution:
// identical secrets.token_hex, identical TLS keys, identical session tokens. The
// write succeeded, so the old code reported that as a successful reseed.

// testAgentReseeding returns an agent whose CSPRNG rotation is reseed.
//
// Injected rather than reached for: the real one is an ioctl on the running
// kernel's /dev/urandom, which is the last thing a unit test on somebody's
// workstation should perform by accident.
func testAgentReseeding(t *testing.T, reseed func([]byte) error) *httptest.Server {
	t.Helper()
	a := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.reseed = reseed
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func postReseed(t *testing.T, srv *httptest.Server, token []byte) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/snapshot/reseed", "application/octet-stream", bytes.NewReader(token))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The reseed must reach the kernel's CSPRNG, not just the pool: the handler hands
// the token to the rotation primitive and reports success only if that returned.
func TestSnapshotReseedRotatesTheCSPRNG(t *testing.T) {
	var got []byte
	srv := testAgentReseeding(t, func(token []byte) error {
		got = append([]byte(nil), token...)
		return nil
	})

	token := bytes.Repeat([]byte{0xa5}, protocol.ReseedTokenBytes)
	if resp := postReseed(t, srv, token); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %s, want 204", resp.Status)
	}
	if !bytes.Equal(got, token) {
		t.Errorf("the kernel was handed %x, want the host's token %x", got, token)
	}
}

// The whole point. A kernel that will not rotate its CSPRNG -- no CAP_SYS_ADMIN,
// an ioctl the guest kernel does not implement, a CRNG that is not ready -- must
// fail the restore. Reporting success would hand a tenant a VM whose "random"
// numbers another tenant already has.
func TestSnapshotReseedFailsClosedWhenTheKernelWillNotRotate(t *testing.T) {
	srv := testAgentReseeding(t, func([]byte) error {
		return errors.New("ioctl RNDRESEEDCRNG: operation not permitted")
	})

	resp := postReseed(t, srv, bytes.Repeat([]byte{1}, protocol.ReseedTokenBytes))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %s, want 500: a reseed that did not rotate must not report success", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	// The host puts this in the error that fails the restore; a bare 500 would
	// send the operator looking at vsock.
	if !bytes.Contains(body, []byte("RNDRESEEDCRNG")) {
		t.Errorf("body %q dropped the kernel's reason", body)
	}
}

// A token of the wrong size is the caller misunderstanding the call, and a reseed
// is not a place to be lenient: a truncated or empty token is answered rather
// than stirred in and reported as a rotation.
func TestSnapshotReseedRejectsAWrongSizedToken(t *testing.T) {
	for _, n := range []int{0, 1, protocol.ReseedTokenBytes - 1, protocol.ReseedTokenBytes + 1} {
		called := false
		srv := testAgentReseeding(t, func([]byte) error { called = true; return nil })

		resp := postReseed(t, srv, bytes.Repeat([]byte{7}, n))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%d-byte token: status = %s, want 400", n, resp.Status)
		}
		if called {
			t.Errorf("%d-byte token: reached the kernel anyway", n)
		}
	}
}

// The default is the real thing, not a stub. A build that forgot to wire the
// platform implementation in would pass every test above and reseed nothing.
func TestAgentReseedDefaultsToTheKernel(t *testing.T) {
	a := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if a.reseed == nil {
		t.Fatal("a fresh agent has no reseed implementation")
	}
	// On a non-Linux build this is the stub that refuses, which is itself the
	// fail-closed behaviour; on Linux it is reseedCSPRNG. Either way it must not
	// silently succeed without touching a kernel, so an obviously bogus call has
	// to come back as an error rather than nil.
	if err := a.reseed(nil); err == nil {
		t.Error("the default reseed accepted an empty token without error")
	}
}
