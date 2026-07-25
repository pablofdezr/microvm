package guestclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// testClientAgainst points a Client at an ordinary HTTP server. The production
// constructor dials a vsock socket and speaks its CONNECT handshake first, which
// needs a running microVM; the wire shape of these two calls does not.
func testClientAgainst(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{http: srv.Client(), baseURL: srv.URL}
}

func TestArmSnapshot(t *testing.T) {
	var gotMethod, gotPath string
	c := testClientAgainst(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.SnapshotArmResponse{Armed: true, CPUs: 2})
	}))

	resp, err := c.ArmSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ArmSnapshot: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/snapshot/arm" {
		t.Errorf("called %s %s, want POST /v1/snapshot/arm", gotMethod, gotPath)
	}
	if !resp.Armed || resp.CPUs != 2 {
		t.Errorf("got %+v, want armed on 2 cpus", resp)
	}
}

// A guest that needs no carry is a success with Armed false, not an error: the
// host must not refuse to snapshot on a platform that never needed this.
func TestArmSnapshotNothingToCarry(t *testing.T) {
	c := testClientAgainst(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.SnapshotArmResponse{Detail: "guest GIC is v3"})
	}))

	resp, err := c.ArmSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ArmSnapshot: %v", err)
	}
	if resp.Armed || resp.Detail == "" {
		t.Errorf("got %+v, want a reasoned refusal to arm", resp)
	}
}

// An image built before the route existed answers 404. That has to arrive as the
// sentinel, because the host's response to it (warn, keep going) is the opposite
// of its response to a real failure (refuse to snapshot).
func TestArmSnapshotOldImage(t *testing.T) {
	c := testClientAgainst(t, http.NotFoundHandler())

	if _, err := c.ArmSnapshot(context.Background()); !errors.Is(err, ErrSnapshotArmUnsupported) {
		t.Fatalf("err = %v, want ErrSnapshotArmUnsupported", err)
	}
	if _, err := c.DisarmSnapshot(context.Background()); !errors.Is(err, ErrSnapshotArmUnsupported) {
		t.Fatalf("DisarmSnapshot err = %v, want ErrSnapshotArmUnsupported", err)
	}
}

// A guest that has a GIC to carry but cannot reach it must surface as an error,
// so the snapshot is refused rather than written and found unrestorable later.
func TestArmSnapshotAgentError(t *testing.T) {
	c := testClientAgainst(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(protocol.ErrorResponse{Error: "open /dev/mem: operation not permitted"})
	}))

	_, err := c.ArmSnapshot(context.Background())
	if err == nil {
		t.Fatal("no error from a failed arm")
	}
	if errors.Is(err, ErrSnapshotArmUnsupported) {
		t.Fatalf("a real failure came back as the old-image sentinel: %v", err)
	}
	// The agent's own message is what says which of a dozen causes it was.
	if got := err.Error(); !strings.Contains(got, "/dev/mem") {
		t.Errorf("error %q dropped the agent's explanation", got)
	}
}

func TestDisarmSnapshot(t *testing.T) {
	var gotPath string
	c := testClientAgainst(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(protocol.SnapshotDisarmResponse{
			Armed: true, CPUs: 2, Checked: 2, Reapplied: 2,
		})
	}))

	resp, err := c.DisarmSnapshot(context.Background())
	if err != nil {
		t.Fatalf("DisarmSnapshot: %v", err)
	}
	if gotPath != "/v1/snapshot/disarm" {
		t.Errorf("called %s, want /v1/snapshot/disarm", gotPath)
	}
	// The counts are what let the host tell a fully repaired guest from a guest
	// with one dead vCPU, so losing them in transit would be losing the check.
	if resp.CPUs != 2 || resp.Checked != 2 || resp.Reapplied != 2 {
		t.Errorf("got %+v, want 2 vCPUs all accounted for", resp)
	}
}

// The token reaches the guest byte for byte on the route that rotates the CSPRNG.
// A reseed that went to the file route instead would be mixed into the entropy
// pool and change nothing about what the guest's getrandom(2) returns.
func TestReseedEntropy(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	c := testClientAgainst(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	token := bytes.Repeat([]byte{0x3c}, protocol.ReseedTokenBytes)
	if err := c.ReseedEntropy(context.Background(), token); err != nil {
		t.Fatalf("ReseedEntropy: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/snapshot/reseed" {
		t.Errorf("called %s %s, want POST /v1/snapshot/reseed", gotMethod, gotPath)
	}
	if !bytes.Equal(gotBody, token) {
		t.Errorf("guest received %x, want %x", gotBody, token)
	}
}

// An image without the reseed route is an image that cannot be restored safely.
// Unlike the arm route, this is not tolerable: the host must fail the restore, so
// the error must arrive as an error.
func TestReseedEntropyFailsOnAnImageWithoutTheRoute(t *testing.T) {
	c := testClientAgainst(t, http.NotFoundHandler())

	err := c.ReseedEntropy(context.Background(), bytes.Repeat([]byte{1}, protocol.ReseedTokenBytes))
	if err == nil {
		t.Fatal("a guest with no reseed route was accepted")
	}
	if !strings.Contains(err.Error(), "share its random numbers") {
		t.Errorf("error %q does not say what is at stake", err)
	}
}

// A guest that could not rotate its CSPRNG must surface as an error carrying the
// kernel's reason, because that error is what fails the restore.
func TestReseedEntropyFailsClosedOnAgentError(t *testing.T) {
	c := testClientAgainst(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(protocol.ErrorResponse{
			Error: "ioctl RNDRESEEDCRNG on /dev/urandom: operation not permitted",
		})
	}))

	err := c.ReseedEntropy(context.Background(), bytes.Repeat([]byte{1}, protocol.ReseedTokenBytes))
	if err == nil {
		t.Fatal("a failed rotation was reported as a successful reseed")
	}
	if !strings.Contains(err.Error(), "RNDRESEEDCRNG") {
		t.Errorf("error %q dropped the kernel's reason", err)
	}
}

// The size is fixed and checked before anything is sent: a short token is a
// caller that has misunderstood the call, and the guest would reject it anyway.
func TestReseedEntropyRejectsAWrongSizedToken(t *testing.T) {
	sent := false
	c := testClientAgainst(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = true
		w.WriteHeader(http.StatusNoContent)
	}))

	if err := c.ReseedEntropy(context.Background(), []byte("short")); err == nil {
		t.Fatal("a 5-byte token was accepted")
	}
	if sent {
		t.Error("a wrong-sized token was put on the wire")
	}
}
