// Package netpool allocates the host-side networking each sandbox needs: a TAP
// device, a pair of addresses, and a MAC.
//
// Every sandbox gets its own /30 rather than sharing one bridged subnet. Two
// sandboxes are therefore never on the same link, so guest-to-guest traffic has
// to be routed through the host, where the firewall drops it. Isolation between
// tenants comes from the topology itself instead of relying on a rule to be
// present and correct.
package netpool

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
)

// slotBits is the size of each sandbox's slice of the base network. A /30 holds
// exactly four addresses: network, host, guest, broadcast -- the smallest block
// that can carry a conventional point-to-point link.
const slotBits = 30

const (
	hostOffset  = 1 // x.x.x.1 -- the TAP device on the host, the guest's gateway
	guestOffset = 2 // x.x.x.2 -- the address configured inside the VM
	slotSize    = 4 // addresses per /30
)

// TapPrefix names every sandbox TAP device. The firewall matches on this prefix
// ("fctap*"), which is what lets the ruleset stay static: adding a sandbox
// creates an interface that the existing rules already cover, with no rule
// churn per VM and so no way to leak an unfiltered interface.
const TapPrefix = "fctap"

// Lease is one sandbox's network allocation.
type Lease struct {
	// Slot is the index of this /30 within the base network.
	Slot int

	TapName string

	// HostIP is the gateway address, configured on the TAP device.
	HostIP netip.Addr
	// GuestIP is the address the guest configures on eth0.
	GuestIP netip.Addr

	MAC net.HardwareAddr
}

// GuestCIDR renders the guest address in the form the guest's kernel command
// line expects.
func (l Lease) GuestCIDR() string {
	return fmt.Sprintf("%s/%d", l.GuestIP, slotBits)
}

// HostCIDR renders the host-side address for configuring the TAP device.
func (l Lease) HostCIDR() string {
	return fmt.Sprintf("%s/%d", l.HostIP, slotBits)
}

// Pool hands out non-overlapping /30 slots from a base network.
type Pool struct {
	base netip.Prefix

	mu    sync.Mutex
	used  map[int]bool
	next  int // round-robin cursor, so a released slot is not immediately reused
	slots int
}

// New returns a pool carving base into /30 slots.
//
// base must be IPv4 and no smaller than a /30. A /16 yields 16384 sandboxes,
// which is far beyond what a single host can run, so the pool is never the
// binding constraint.
func New(base netip.Prefix) (*Pool, error) {
	if !base.Addr().Is4() {
		return nil, fmt.Errorf("base network %s must be IPv4", base)
	}
	if base.Bits() > slotBits {
		return nil, fmt.Errorf("base network %s is smaller than a /%d", base, slotBits)
	}

	slots := 1 << (slotBits - base.Bits())
	return &Pool{
		base:  base.Masked(),
		used:  make(map[int]bool),
		slots: slots,
	}, nil
}

// Capacity is the number of sandboxes the pool can address at once.
func (p *Pool) Capacity() int { return p.slots }

// InUse is the number of slots currently leased.
func (p *Pool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.used)
}

// Allocate reserves the next free slot.
func (p *Pool) Allocate() (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.used) >= p.slots {
		return Lease{}, fmt.Errorf("network pool exhausted: all %d slots in use", p.slots)
	}

	// Scan forward from the cursor rather than from zero. Reusing a just-freed
	// slot risks a new sandbox inheriting the previous tenant's address while
	// the old TAP teardown or a peer's stale ARP entry is still settling.
	for i := 0; i < p.slots; i++ {
		slot := (p.next + i) % p.slots
		if p.used[slot] {
			continue
		}
		p.used[slot] = true
		p.next = (slot + 1) % p.slots
		return p.leaseFor(slot), nil
	}

	// Unreachable: the capacity check above already covered a full pool.
	return Lease{}, fmt.Errorf("network pool exhausted")
}

// Release returns a slot to the pool. Releasing a slot that is not held is a
// no-op, so teardown paths can be unconditional.
func (p *Pool) Release(l Lease) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used, l.Slot)
}

func (p *Pool) leaseFor(slot int) Lease {
	baseInt := addrToUint32(p.base.Addr())
	slotBase := baseInt + uint32(slot*slotSize)

	return Lease{
		Slot:    slot,
		TapName: fmt.Sprintf("%s%d", TapPrefix, slot),
		HostIP:  uint32ToAddr(slotBase + hostOffset),
		GuestIP: uint32ToAddr(slotBase + guestOffset),
		MAC:     macForSlot(slot),
	}
}

// macForSlot derives a stable MAC from the slot index.
//
// The 0x02 prefix marks it locally administered and unicast, which is the range
// reserved for exactly this: addresses invented by software that must not
// collide with any real vendor's.
func macForSlot(slot int) net.HardwareAddr {
	return net.HardwareAddr{
		0x02, 0x00,
		byte(slot >> 24), byte(slot >> 16), byte(slot >> 8), byte(slot),
	}
}

// isSandboxTap reports whether an interface name is one this package created.
//
// Used by orphan cleanup, which deletes what it matches -- so it must never
// match the host's own NIC or another runtime's bridge. Requiring a numeric
// suffix, not just the prefix, is what keeps a device like "fctapfoo" safe.
func isSandboxTap(name string) bool {
	if len(name) <= len(TapPrefix) || name[:len(TapPrefix)] != TapPrefix {
		return false
	}
	for _, c := range name[len(TapPrefix):] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func addrToUint32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func uint32ToAddr(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
