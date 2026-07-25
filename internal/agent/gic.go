package agent

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"unsafe"
)

// This file holds the platform-independent half of the guest's snapshot carry:
// finding the interrupt controller in the device tree, and reading and writing
// its registers. The half that needs Linux (mmap of /dev/mem, pinning a thread
// to a vCPU, the spin across the snapshot) is in gic_linux.go, and the reason
// any of it exists at all is documented there.

// deviceTreeRoot is where Linux exposes the flattened device tree it booted
// from, one directory per node and one file per property.
const deviceTreeRoot = "/proc/device-tree"

// Register offsets within the two GICv2 MMIO windows. Only these three carry
// state that a Firecracker snapshot loses; see gic_linux.go.
const (
	// gicdISENABLER0 is set-enable for INTIDs 0-31, the SGIs and PPIs. It is
	// *banked*: each vCPU reading or writing this one address sees its own
	// private copy, which is exactly why it has to be written from every vCPU.
	// Writes are write-1-to-set, so writing a saved mask can only ever enable an
	// interrupt and can never disable one.
	gicdISENABLER0 = 0x100

	// giccCTLR enables the CPU interface: with it clear, that vCPU is signalled
	// no interrupts at all, however the distributor is programmed.
	giccCTLR = 0x000
	// giccPMR is the priority mask. Zero blocks every interrupt; Linux sets 0xf0.
	giccPMR = 0x004
)

// gicv2 is where a guest's GICv2 lives in its own physical address space.
type gicv2 struct {
	distBase, distSize uint64
	cpuBase, cpuSize   uint64
}

// findGICv2 locates a GICv2 in a device tree mounted at root.
//
// It returns (nil, reason, nil) when there is nothing to do, which is the
// common case and not a failure: an x86 guest has no device tree at all, and a
// GICv3 guest needs no help because Firecracker saves a GICv3's per-vCPU state
// correctly. The reason is human-readable and travels back to the host, so an
// operator reading a log can tell "not needed here" from "could not tell".
func findGICv2(root string) (*gicv2, string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "no device tree: this guest has no GIC to carry", nil
		}
		return nil, "", fmt.Errorf("read device tree %s: %w", root, err)
	}

	// The root node's cell counts decide how wide each address and size in a
	// child's "reg" property is. Reading them rather than assuming 2/2 keeps this
	// correct against a Firecracker that changes its device tree.
	addrCells, err := dtCells(root, "#address-cells")
	if err != nil {
		return nil, "", err
	}
	sizeCells, err := dtCells(root, "#size-cells")
	if err != nil {
		return nil, "", err
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		node := filepath.Join(root, e.Name())
		compatible, err := os.ReadFile(filepath.Join(node, "compatible"))
		if err != nil {
			continue // not a device node, or one without a compatible string
		}
		// Device tree strings are NUL-terminated and a property may hold several.
		compat := string(compatible)

		if dtHasCompatible(compat, "arm,gic-v3") {
			return nil, "guest GIC is v3: Firecracker saves its per-vCPU state, so there is nothing to carry", nil
		}
		if !dtHasCompatible(compat, "arm,gic-400") &&
			!dtHasCompatible(compat, "arm,cortex-a15-gic") &&
			!dtHasCompatible(compat, "arm,arm11mp-gic") {
			continue
		}

		reg, err := os.ReadFile(filepath.Join(node, "reg"))
		if err != nil {
			return nil, "", fmt.Errorf("read %s/reg: %w", node, err)
		}
		ranges, err := dtRanges(reg, addrCells, sizeCells)
		if err != nil {
			return nil, "", fmt.Errorf("parse %s/reg: %w", node, err)
		}
		// A GICv2 always publishes the distributor first and the CPU interface
		// second. Both are needed: the state lost by a snapshot straddles them.
		if len(ranges) < 2 {
			return nil, "", fmt.Errorf("%s/reg has %d ranges, want the distributor and the CPU interface", node, len(ranges))
		}
		return &gicv2{
			distBase: ranges[0].base, distSize: ranges[0].size,
			cpuBase: ranges[1].base, cpuSize: ranges[1].size,
		}, "", nil
	}
	return nil, "no GIC in the device tree: nothing to carry", nil
}

// dtHasCompatible reports whether a NUL-separated compatible property lists id.
func dtHasCompatible(property, id string) bool {
	for _, s := range strings.Split(property, "\x00") {
		if s == id {
			return true
		}
	}
	return false
}

type dtRange struct{ base, size uint64 }

// dtRanges splits a "reg" property into address/size pairs of the given widths.
func dtRanges(reg []byte, addrCells, sizeCells int) ([]dtRange, error) {
	stride := 4 * (addrCells + sizeCells)
	if stride == 0 || len(reg)%stride != 0 {
		return nil, fmt.Errorf("%d bytes is not a whole number of %d-byte entries", len(reg), stride)
	}
	var out []dtRange
	for off := 0; off+stride <= len(reg); off += stride {
		base, err := dtCell(reg[off:off+4*addrCells], addrCells)
		if err != nil {
			return nil, err
		}
		size, err := dtCell(reg[off+4*addrCells:off+stride], sizeCells)
		if err != nil {
			return nil, err
		}
		out = append(out, dtRange{base: base, size: size})
	}
	return out, nil
}

// dtCell reads a big-endian device-tree integer of n 32-bit cells.
func dtCell(b []byte, n int) (uint64, error) {
	if n < 1 || n > 2 {
		return 0, fmt.Errorf("unsupported device tree cell width %d", n)
	}
	if n == 1 {
		return uint64(binary.BigEndian.Uint32(b)), nil
	}
	return binary.BigEndian.Uint64(b), nil
}

// dtCells reads a #address-cells / #size-cells property, defaulting the way the
// device tree spec does when it is absent.
func dtCells(node, name string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(node, name))
	if err != nil {
		if os.IsNotExist(err) {
			return 2, nil // the spec's default for both
		}
		return 0, fmt.Errorf("read %s/%s: %w", node, name, err)
	}
	if len(raw) != 4 {
		return 0, fmt.Errorf("%s/%s is %d bytes, want 4", node, name, len(raw))
	}
	n := int(binary.BigEndian.Uint32(raw))
	if n < 1 || n > 2 {
		return 0, fmt.Errorf("%s/%s is %s, which this code cannot handle", node, name, strconv.Itoa(n))
	}
	return n, nil
}

// parseCPUList reads the kernel's CPU-list format ("0-3", "0,2-3"), as written
// in /sys/devices/system/cpu/online.
func parseCPUList(s string) ([]int, error) {
	var out []int
	if s == "" {
		return nil, nil
	}
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("cpu list %q: %w", s, err)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("cpu list %q: %w", s, err)
			}
		}
		if last < first {
			return nil, fmt.Errorf("cpu list %q: range %s runs backwards", s, part)
		}
		for cpu := first; cpu <= last; cpu++ {
			out = append(out, cpu)
		}
	}
	return out, nil
}

// mmio is a memory-mapped register window.
//
// Every access goes through sync/atomic, and not for concurrency: these are
// single naturally aligned 32-bit accesses to Device memory, where the compiler
// must not split, coalesce, or -- the one that would silently break the carry --
// hoist a loop-invariant store out of the spin loop. An atomic store is none of
// those things, and on arm64 it compiles to the single store instruction the
// mapping requires.
type mmio []byte

func (m mmio) load32(off int) uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Pointer(&m[off : off+4][0])))
}

func (m mmio) store32(off int, v uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&m[off : off+4][0])), v)
}

// bankedState is the per-vCPU interrupt-controller state that a Firecracker
// GICv2 snapshot does not save. See gic_linux.go for what that costs and why
// these three registers are the whole of it.
type bankedState struct {
	// enable is GICD_ISENABLER0: which of INTIDs 0-31 this vCPU has enabled.
	// On a Linux guest that is the SGIs it uses for IPIs plus INTID 27, the
	// virtual timer -- the only clockevent device an arm64 microVM has.
	enable uint32
	// pmr and ctlr are the vCPU's GIC CPU interface: its priority mask and
	// whether the interface is on at all.
	pmr, ctlr uint32
}

// capture reads the state to be carried. It must run on the vCPU it is being
// captured for: all three registers are banked, so a read here answers for the
// reading vCPU and no other.
func captureBanked(dist, cpuif mmio) bankedState {
	return bankedState{
		enable: dist.load32(gicdISENABLER0),
		pmr:    cpuif.load32(giccPMR),
		ctlr:   cpuif.load32(giccCTLR),
	}
}

// reapply puts the carried state back. Like capture, it answers for the vCPU it
// runs on, which is why the carry pins one thread per vCPU.
//
// The order is Linux's own in gic_cpu_init: interrupts enabled in the
// distributor, then the priority mask, then the CPU interface last, so the
// interface is never turned on while its mask would drop everything.
func (b bankedState) reapply(dist, cpuif mmio) {
	dist.store32(gicdISENABLER0, b.enable)
	cpuif.store32(giccPMR, b.pmr)
	cpuif.store32(giccCTLR, b.ctlr)
}

// healthy reports whether the live registers already match what was carried, so
// a caller can tell a snapshot that lost the state from one that did not.
func (b bankedState) healthy(dist, cpuif mmio) bool {
	live := captureBanked(dist, cpuif)
	// Set-only semantics: extra enabled interrupts (KVM's reset mask enables all
	// 16 SGIs) are not a discrepancy, a missing one is.
	return live.enable&b.enable == b.enable && live.pmr == b.pmr && live.ctlr == b.ctlr
}
