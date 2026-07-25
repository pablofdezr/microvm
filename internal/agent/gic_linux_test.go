package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// testAgentNoGIC returns an agent whose carry looks at an empty device tree.
//
// Nothing in a unit test may reach the real one: on an arm64 host
// /proc/device-tree describes the *host's* GIC-400, and arming against it would
// mmap and write the interrupt controller of the machine running the test.
func testAgentNoGIC(t *testing.T) *httptest.Server {
	t.Helper()
	a := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.gic.dtRoot = t.TempDir()
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// A guest with nothing to carry answers rather than failing: that is how the
// host tells "this platform is fine" from "this platform is broken", and it is
// the answer every x86 and GICv3 guest gives.
func TestSnapshotArmWithNothingToCarry(t *testing.T) {
	srv := testAgentNoGIC(t)

	resp, err := http.Post(srv.URL+"/v1/snapshot/arm", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	var out protocol.SnapshotArmResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Armed {
		t.Error("armed a guest with no GIC in its device tree")
	}
	if out.Detail == "" {
		t.Error("no detail explaining why nothing was armed")
	}
}

// Disarming is unconditional on the restore path, so a guest that was never
// armed -- every restore on a host that needs no carry -- must accept it, and must
// say that there was nothing armed rather than claiming vCPUs it never covered.
func TestSnapshotDisarmWhenNeverArmed(t *testing.T) {
	srv := testAgentNoGIC(t)

	resp, err := http.Post(srv.URL+"/v1/snapshot/disarm", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	var out protocol.SnapshotDisarmResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Armed || out.CPUs != 0 || out.Checked != 0 {
		t.Errorf("got %+v, want an unarmed stand-down", out)
	}
}
