package api

import (
	"net/http"
	"testing"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
)

// setTransfer makes every instance report a transfer, already in the guest's
// terms -- the runtime turns the host's TAP counters round before this layer sees
// them.
func (h *harness) setTransfer(t *testing.T, rx, tx uint64) {
	t.Helper()
	h.rt.Stats.NetworkRxBytes = &rx
	h.rt.Stats.NetworkTxBytes = &tx
}

// The transfer has to survive the kill, for the same reason the CPU number does:
// the TAP device is deleted with the VM, and its counters go with it. The DELETE
// reply is the last chance anyone has to learn what a sandbox sent -- and a zero
// there tells a caller their egress was free.
func TestDeleteReturnsTheFinalTransfer(t *testing.T) {
	h := newHarness(t)
	// Deliberately lopsided: equal numbers would pass even if the two fields were
	// swapped somewhere on the way out.
	h.setTransfer(t, 4096, 7)

	sb := h.createSandbox(t)

	resp := h.do(t, "DELETE", "/v1/sandboxes/"+sb.Id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decode[apitypes.Sandbox](t, resp)

	if got.Stats.NetworkRxBytes == nil || got.Stats.NetworkTxBytes == nil {
		t.Fatalf("delete reply has no transfer: rx=%v tx=%v -- it was sampled after "+
			"the TAP was gone", got.Stats.NetworkRxBytes, got.Stats.NetworkTxBytes)
	}
	if *got.Stats.NetworkRxBytes != 4096 {
		t.Errorf("network_rx_bytes = %d, want 4096", *got.Stats.NetworkRxBytes)
	}
	if *got.Stats.NetworkTxBytes != 7 {
		t.Errorf("network_tx_bytes = %d, want 7 (the guest's egress, not the host's)", *got.Stats.NetworkTxBytes)
	}

	// And it keeps saying so for the retention window: a biller reading the
	// sandbox back after the fact gets the same numbers, not zeros.
	after := decode[apitypes.Sandbox](t, h.do(t, "GET", "/v1/sandboxes/"+sb.Id, nil))
	if after.Stats.NetworkTxBytes == nil || *after.Stats.NetworkTxBytes != 7 {
		t.Errorf("network_tx_bytes after delete = %v, want 7", after.Stats.NetworkTxBytes)
	}
}

// A running sandbox reports the meter as it stands, so a caller can watch egress
// climb rather than only learning the total once the VM is gone.
func TestRunningSandboxReportsTransfer(t *testing.T) {
	h := newHarness(t)
	h.setTransfer(t, 1, 999)

	sb := h.createSandbox(t)
	if sb.Stats.NetworkTxBytes == nil || *sb.Stats.NetworkTxBytes != 999 {
		t.Errorf("create reply network_tx_bytes = %v, want 999", sb.Stats.NetworkTxBytes)
	}

	got := decode[apitypes.Sandbox](t, h.do(t, "GET", "/v1/sandboxes/"+sb.Id, nil))
	if got.Stats.NetworkTxBytes == nil || *got.Stats.NetworkTxBytes != 999 {
		t.Errorf("retrieve network_tx_bytes = %v, want 999", got.Stats.NetworkTxBytes)
	}
}

// With no network device there is nothing to count, and that is not the same
// claim as zero bytes: both fields are absent rather than 0, so a caller can tell
// "never had a NIC" from "had one and sent nothing".
func TestSandboxWithoutNetworkOmitsTransfer(t *testing.T) {
	h := newHarness(t)

	sb := h.createSandbox(t)
	if sb.Stats.NetworkRxBytes != nil || sb.Stats.NetworkTxBytes != nil {
		t.Errorf("transfer reported without a TAP: rx=%v tx=%v, want both absent",
			sb.Stats.NetworkRxBytes, sb.Stats.NetworkTxBytes)
	}

	// The CPU meters are unaffected: a missing network device is not a failed
	// sample.
	if sb.Stats.WallMs == 0 {
		t.Error("wall_ms = 0: a sandbox with no network lost its other meters too")
	}

	got := decode[apitypes.Sandbox](t, h.do(t, "DELETE", "/v1/sandboxes/"+sb.Id, nil))
	if got.Stats.NetworkRxBytes != nil || got.Stats.NetworkTxBytes != nil {
		t.Errorf("delete reported transfer without a TAP: rx=%v tx=%v",
			got.Stats.NetworkRxBytes, got.Stats.NetworkTxBytes)
	}
}
