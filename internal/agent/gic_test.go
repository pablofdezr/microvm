package agent

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeDT builds a directory shaped like Linux's /proc/device-tree.
func writeDT(t *testing.T, nodes map[string]map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	for node, props := range nodes {
		dir := root
		if node != "" {
			dir = filepath.Join(root, node)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for name, value := range props {
			if err := os.WriteFile(filepath.Join(dir, name), value, 0o444); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func cells(vals ...uint32) []byte {
	b := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.BigEndian.PutUint32(b[4*i:], v)
	}
	return b
}

// The real device tree from a Firecracker arm64 guest, as measured on the Pi:
// root cells 2/2, an "intc" node compatible with arm,gic-400, and a reg holding
// the distributor at 0x3ffff000 (0x1000) then the CPU interface at 0x3fffd000
// (0x2000).
func TestFindGICv2(t *testing.T) {
	root := writeDT(t, map[string]map[string][]byte{
		"": {
			"#address-cells": cells(2),
			"#size-cells":    cells(2),
		},
		"intc": {
			"compatible": []byte("arm,gic-400\x00"),
			"reg":        cells(0, 0x3ffff000, 0, 0x1000, 0, 0x3fffd000, 0, 0x2000),
		},
		"uart@40002000": {
			"compatible": []byte("ns16550a\x00"),
			"reg":        cells(0, 0x40002000, 0, 0x1000),
		},
	})

	gic, reason, err := findGICv2(root)
	if err != nil {
		t.Fatalf("findGICv2: %v", err)
	}
	if gic == nil {
		t.Fatalf("no GICv2 found, reason %q", reason)
	}
	if gic.distBase != 0x3ffff000 || gic.distSize != 0x1000 {
		t.Errorf("distributor = %#x/%#x, want 0x3ffff000/0x1000", gic.distBase, gic.distSize)
	}
	if gic.cpuBase != 0x3fffd000 || gic.cpuSize != 0x2000 {
		t.Errorf("cpu interface = %#x/%#x, want 0x3fffd000/0x2000", gic.cpuBase, gic.cpuSize)
	}
}

// A GICv3 guest must be left alone: Firecracker saves its per-vCPU state, and
// the register this code writes is RAZ/WI on a GICv3 distributor anyway.
func TestFindGICv2SkipsGICv3(t *testing.T) {
	root := writeDT(t, map[string]map[string][]byte{
		"": {"#address-cells": cells(2), "#size-cells": cells(2)},
		"intc": {
			"compatible": []byte("arm,gic-v3\x00"),
			"reg":        cells(0, 0x3ffff000, 0, 0x10000, 0, 0x3fff0000, 0, 0x20000),
		},
	})

	gic, reason, err := findGICv2(root)
	if err != nil {
		t.Fatalf("findGICv2: %v", err)
	}
	if gic != nil {
		t.Fatalf("found a GICv2 in a GICv3 tree: %+v", gic)
	}
	if reason == "" {
		t.Error("no reason given for skipping a GICv3")
	}
}

// No device tree at all is what an x86 guest looks like. It is not an error:
// there is nothing to carry, and a snapshot there is fine as it is.
func TestFindGICv2NoDeviceTree(t *testing.T) {
	gic, reason, err := findGICv2(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("findGICv2 on a machine with no device tree: %v", err)
	}
	if gic != nil {
		t.Fatalf("found a GIC with no device tree: %+v", gic)
	}
	if reason == "" {
		t.Error("no reason given")
	}
}

// One-cell addresses and sizes are legal, so the widths must come from the tree
// rather than being assumed.
func TestFindGICv2SingleCellAddresses(t *testing.T) {
	root := writeDT(t, map[string]map[string][]byte{
		"":     {"#address-cells": cells(1), "#size-cells": cells(1)},
		"intc": {"compatible": []byte("arm,cortex-a15-gic\x00"), "reg": cells(0x8000000, 0x1000, 0x8010000, 0x2000)},
	})

	gic, _, err := findGICv2(root)
	if err != nil {
		t.Fatalf("findGICv2: %v", err)
	}
	if gic == nil || gic.distBase != 0x8000000 || gic.cpuBase != 0x8010000 {
		t.Fatalf("got %+v, want distributor 0x8000000 and cpu interface 0x8010000", gic)
	}
}

func TestFindGICv2RejectsTruncatedReg(t *testing.T) {
	root := writeDT(t, map[string]map[string][]byte{
		"": {"#address-cells": cells(2), "#size-cells": cells(2)},
		// Only the distributor: no CPU interface to repair the vCPU with.
		"intc": {"compatible": []byte("arm,gic-400\x00"), "reg": cells(0, 0x3ffff000, 0, 0x1000)},
	})

	if _, _, err := findGICv2(root); err == nil {
		t.Fatal("accepted a GIC node with only one region")
	}
}

// The registers are what the fix is, so the exact words written matter. This
// drives them against a fake window and asserts the two properties the safety of
// the whole scheme rests on: what gets written, and that nothing gets cleared.
func TestBankedStateReapply(t *testing.T) {
	dist := make(mmio, 0x1000)
	cpuif := make(mmio, 0x2000)

	// A healthy arm64 Linux guest, as measured: SGIs 0-6 and INTID 27 (the
	// virtual timer) enabled, CPU interface on, priority mask 0xf0.
	const healthyEnable = 0x0800007f
	dist.store32(gicdISENABLER0, healthyEnable)
	cpuif.store32(giccPMR, 0xf0)
	cpuif.store32(giccCTLR, 0x1)

	saved := captureBanked(dist, cpuif)
	if saved.enable != healthyEnable || saved.pmr != 0xf0 || saved.ctlr != 1 {
		t.Fatalf("captured %+v", saved)
	}
	if !saved.healthy(dist, cpuif) {
		t.Fatal("state read back as unhealthy immediately after capture")
	}

	// What a restore on a GICv2 host leaves behind on a secondary vCPU: KVM's
	// reset enable mask (no timer bit), CPU interface off, mask blocking all.
	dist.store32(gicdISENABLER0, 0x0000ffff)
	cpuif.store32(giccPMR, 0)
	cpuif.store32(giccCTLR, 0)

	if saved.healthy(dist, cpuif) {
		t.Fatal("a restored-and-broken vCPU reported healthy")
	}

	saved.reapply(dist, cpuif)

	// Every interrupt the guest had enabled is enabled again -- INTID 27, the
	// timer, being the one that matters.
	//
	// The assertion is "contains" rather than "equals" because this window is
	// plain memory and the real register is not: GICD_ISENABLER0 is
	// write-1-to-set, so on hardware this same store leaves the bits KVM's reset
	// mask had already set and lands on 0x0800ffff, which is what the Pi
	// measured. Either way the carried bits are set and nothing is cleared --
	// TestReapplyTouchesNothingElse holds the other half of that, that
	// GICD_ICENABLER0 is never written at all.
	if got := dist.load32(gicdISENABLER0); got&healthyEnable != healthyEnable {
		t.Errorf("GICD_ISENABLER0 = %#08x, missing bits from %#08x", got, uint32(healthyEnable))
	}
	if got := cpuif.load32(giccPMR); got != 0xf0 {
		t.Errorf("GICC_PMR = %#02x, want 0xf0", got)
	}
	if got := cpuif.load32(giccCTLR); got != 1 {
		t.Errorf("GICC_CTLR = %#x, want 1", got)
	}
	if !saved.healthy(dist, cpuif) {
		t.Error("state still unhealthy after reapply")
	}
}

// On a host that restores the state properly the carry must be a no-op: it may
// not disturb a vCPU that came back fine.
func TestBankedStateHealthyRestoreIsUntouched(t *testing.T) {
	dist := make(mmio, 0x1000)
	cpuif := make(mmio, 0x2000)
	dist.store32(gicdISENABLER0, 0x0800007f)
	cpuif.store32(giccPMR, 0xf0)
	cpuif.store32(giccCTLR, 0x1)

	saved := captureBanked(dist, cpuif)
	before := append(mmio(nil), dist...)
	beforeCPU := append(mmio(nil), cpuif...)

	if !saved.healthy(dist, cpuif) {
		t.Fatal("healthy state reported as needing repair")
	}
	saved.reapply(dist, cpuif)

	if string(dist) != string(before) || string(cpuif) != string(beforeCPU) {
		t.Error("reapply changed a register on an already-healthy vCPU")
	}
}

// Only the three registers may be touched. A stray write elsewhere in the
// distributor would be a write to a live interrupt controller.
func TestReapplyTouchesNothingElse(t *testing.T) {
	dist := make(mmio, 0x1000)
	cpuif := make(mmio, 0x2000)
	for i := range dist {
		dist[i] = 0xab
	}
	for i := range cpuif {
		cpuif[i] = 0xcd
	}
	saved := bankedState{enable: 0x0800007f, pmr: 0xf0, ctlr: 1}
	saved.reapply(dist, cpuif)

	for off := 0; off < len(dist); off += 4 {
		want := uint32(0xabababab)
		if off == gicdISENABLER0 {
			want = saved.enable
		}
		if got := dist.load32(off); got != want {
			t.Fatalf("distributor +%#x = %#08x, want %#08x", off, got, want)
		}
	}
	for off := 0; off < len(cpuif); off += 4 {
		want := uint32(0xcdcdcdcd)
		switch off {
		case giccCTLR:
			want = saved.ctlr
		case giccPMR:
			want = saved.pmr
		}
		if got := cpuif.load32(off); got != want {
			t.Fatalf("cpu interface +%#x = %#08x, want %#08x", off, got, want)
		}
	}
}

func TestParseCPUList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int
	}{
		{"0", []int{0}},
		{"0-1", []int{0, 1}},
		{"0-3", []int{0, 1, 2, 3}},
		{"0,2-3", []int{0, 2, 3}},
		{"0-1,4", []int{0, 1, 4}},
	} {
		got, err := parseCPUList(tc.in)
		if err != nil {
			t.Errorf("parseCPUList(%q): %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseCPUList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseCPUList(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}

	for _, bad := range []string{"x", "3-1", "1-", "-"} {
		if got, err := parseCPUList(bad); err == nil {
			t.Errorf("parseCPUList(%q) = %v, want an error", bad, got)
		}
	}
}
