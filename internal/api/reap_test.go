package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

// A reaped sandbox is an object that no longer exists, so it answers like every
// other one that does not: the same 404, the same code all three SDKs switch on.
// A caller who kept an ID has to be able to handle its disappearance with the
// branch they already wrote.
func TestReapedSandboxIsAnOrdinaryNotFound(t *testing.T) {
	h := newHarness(t, sandbox.WithRetention(50*time.Millisecond))

	sb := h.createSandbox(t)
	if resp := h.do(t, "DELETE", "/v1/sandboxes/"+sb.Id, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp.StatusCode)
	}

	// Past the window, then the sweep the daemon runs on a ticker.
	time.Sleep(80 * time.Millisecond)
	if dropped := h.mgr.Sweep(); dropped != 1 {
		t.Fatalf("swept %d sandboxes, want 1", dropped)
	}

	resp := h.do(t, "GET", "/v1/sandboxes/"+sb.Id, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeSandboxNotFound {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeSandboxNotFound)
	}
	if env.Error.Type != apitypes.ErrorTypeInvalidRequestError {
		t.Errorf("type = %q, want %q", env.Error.Type, apitypes.ErrorTypeInvalidRequestError)
	}

	// And it is off the list: the page growing for the process's whole life is the
	// other half of the leak.
	list := decode[apitypes.SandboxList](t, h.do(t, "GET", "/v1/sandboxes", nil))
	for _, item := range list.Data {
		if item.Id == sb.Id {
			t.Errorf("%s is still listed after being forgotten", sb.Id)
		}
	}
}

// Inside the window nothing changes, which is what makes the record worth keeping
// at all: the final metering is sampled just before the kill and cannot be read
// again, so this reply is the only place a bill can come from.
func TestSandboxInsideItsWindowStillServesItsFinalStats(t *testing.T) {
	h := newHarness(t, sandbox.WithRetention(time.Hour))
	h.setTransfer(t, 4096, 7)

	sb := h.createSandbox(t)
	deleted := decode[apitypes.Sandbox](t, h.do(t, "DELETE", "/v1/sandboxes/"+sb.Id, nil))

	// A sweep that ran now must take nothing.
	if dropped := h.mgr.Sweep(); dropped != 0 {
		t.Fatalf("swept %d sandboxes inside an hour-long window, want 0", dropped)
	}

	resp := h.do(t, "GET", "/v1/sandboxes/"+sb.Id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a sandbox stopped a moment ago was forgotten", resp.StatusCode)
	}
	got := decode[apitypes.Sandbox](t, resp)

	if got.State != apitypes.SandboxStateStopped {
		t.Errorf("state = %q, want stopped", got.State)
	}
	if got.StopReason == nil || *got.StopReason != apitypes.SandboxStopReasonStopped {
		t.Errorf("stop_reason = %v, want stopped", got.StopReason)
	}
	if got.Stats.ActiveCpuMs != deleted.Stats.ActiveCpuMs || got.Stats.MemoryPeakBytes != deleted.Stats.MemoryPeakBytes {
		t.Errorf("stats = %+v, want the delete reply's %+v: the meters are gone with the cgroup, "+
			"so a difference here is a number nobody can recover", got.Stats, deleted.Stats)
	}
	if got.Stats.NetworkTxBytes == nil || *got.Stats.NetworkTxBytes != 7 {
		t.Errorf("network_tx_bytes = %v, want 7", got.Stats.NetworkTxBytes)
	}
}

// The list is a window, not a history, and only the stopped end of it moves: a
// running sandbox older than the window is still there, because forgetting it
// would leave a live VM nothing can reach.
func TestReapingLeavesRunningSandboxesListed(t *testing.T) {
	h := newHarness(t, sandbox.WithRetention(50*time.Millisecond))

	live := h.createSandbox(t)
	stopped := h.createSandbox(t)
	h.do(t, "DELETE", "/v1/sandboxes/"+stopped.Id, nil)

	time.Sleep(80 * time.Millisecond)
	h.mgr.Sweep()

	list := decode[apitypes.SandboxList](t, h.do(t, "GET", "/v1/sandboxes", nil))
	if len(list.Data) != 1 || list.Data[0].Id != live.Id {
		t.Fatalf("list = %+v, want only the running %s", list.Data, live.Id)
	}
	if resp := h.do(t, "GET", "/v1/sandboxes/"+live.Id, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("running sandbox status = %d, want 200", resp.StatusCode)
	}
}
