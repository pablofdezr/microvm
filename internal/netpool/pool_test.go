package netpool

import (
	"net/netip"
	"sync"
	"testing"
)

func mustPool(t *testing.T, cidr string) *Pool {
	t.Helper()
	p, err := New(netip.MustParsePrefix(cidr))
	if err != nil {
		t.Fatalf("New(%s): %v", cidr, err)
	}
	return p
}

func TestLeaseAddressing(t *testing.T) {
	p := mustPool(t, "172.16.0.0/16")

	first, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}

	// Slot 0 occupies 172.16.0.0/30: .0 network, .1 host, .2 guest, .3 broadcast.
	if got, want := first.HostIP.String(), "172.16.0.1"; got != want {
		t.Errorf("host IP = %s, want %s", got, want)
	}
	if got, want := first.GuestIP.String(), "172.16.0.2"; got != want {
		t.Errorf("guest IP = %s, want %s", got, want)
	}
	if got, want := first.GuestCIDR(), "172.16.0.2/30"; got != want {
		t.Errorf("guest CIDR = %s, want %s", got, want)
	}
	if got, want := first.TapName, "fctap0"; got != want {
		t.Errorf("tap name = %s, want %s", got, want)
	}

	second, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	// The next slot must start a full /30 later, or the two sandboxes would
	// share a link and could reach each other directly.
	if got, want := second.HostIP.String(), "172.16.0.5"; got != want {
		t.Errorf("second host IP = %s, want %s", got, want)
	}
	if got, want := second.GuestIP.String(), "172.16.0.6"; got != want {
		t.Errorf("second guest IP = %s, want %s", got, want)
	}
}

// Two live sandboxes must never share an address or a device name.
func TestAllocateNeverOverlaps(t *testing.T) {
	p := mustPool(t, "172.16.0.0/24") // 64 slots

	seenIP := make(map[string]int)
	seenTap := make(map[string]bool)

	for i := 0; i < p.Capacity(); i++ {
		l, err := p.Allocate()
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		if prev, dup := seenIP[l.GuestIP.String()]; dup {
			t.Fatalf("guest IP %s handed out to both slot %d and slot %d", l.GuestIP, prev, l.Slot)
		}
		seenIP[l.GuestIP.String()] = l.Slot

		if seenTap[l.TapName] {
			t.Fatalf("tap name %s handed out twice", l.TapName)
		}
		seenTap[l.TapName] = true

		// The host address must never collide with another slot's guest.
		if prev, dup := seenIP[l.HostIP.String()]; dup && prev != l.Slot {
			t.Fatalf("host IP %s collides with slot %d", l.HostIP, prev)
		}
		seenIP[l.HostIP.String()] = l.Slot
	}
}

func TestPoolExhaustion(t *testing.T) {
	p := mustPool(t, "172.16.0.0/30") // exactly one slot

	if got := p.Capacity(); got != 1 {
		t.Fatalf("capacity = %d, want 1", got)
	}

	first, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}

	// Exhaustion must be an error, not a silently reused address: two guests on
	// one address would break isolation.
	if _, err := p.Allocate(); err == nil {
		t.Fatal("second allocate succeeded on an exhausted pool")
	}

	p.Release(first)
	if _, err := p.Allocate(); err != nil {
		t.Fatalf("allocate after release: %v", err)
	}
}

// A released slot should not be handed straight back out: the old TAP teardown
// and a peer's ARP cache may still be settling.
func TestReleasedSlotIsNotImmediatelyReused(t *testing.T) {
	p := mustPool(t, "172.16.0.0/28") // 4 slots

	first, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Allocate(); err != nil {
		t.Fatal(err)
	}
	p.Release(first)

	next, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if next.Slot == first.Slot {
		t.Errorf("slot %d was reused immediately after release", first.Slot)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	p := mustPool(t, "172.16.0.0/24")

	l, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}

	before := p.InUse()
	p.Release(l)
	p.Release(l) // teardown paths run unconditionally; a double release is normal

	if got := p.InUse(); got != before-1 {
		t.Errorf("in use = %d, want %d", got, before-1)
	}
}

func TestConcurrentAllocateIsSafe(t *testing.T) {
	p := mustPool(t, "172.16.0.0/24")
	const workers = 32

	var (
		mu     sync.Mutex
		leases []Lease
		wg     sync.WaitGroup
	)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Allocate()
			if err != nil {
				return
			}
			mu.Lock()
			leases = append(leases, l)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(leases) != workers {
		t.Fatalf("got %d leases, want %d", len(leases), workers)
	}
	seen := make(map[int]bool)
	for _, l := range leases {
		if seen[l.Slot] {
			t.Fatalf("slot %d allocated twice under concurrency", l.Slot)
		}
		seen[l.Slot] = true
	}
}

func TestMACIsLocallyAdministeredAndUnique(t *testing.T) {
	p := mustPool(t, "172.16.0.0/24")

	seen := make(map[string]bool)
	for i := 0; i < p.Capacity(); i++ {
		l, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		mac := l.MAC

		// Bit 1 of the first octet marks a locally administered address; bit 0
		// clear marks unicast. Getting this wrong risks colliding with a real
		// vendor's assignment.
		if mac[0]&0x02 == 0 {
			t.Errorf("MAC %s is not locally administered", mac)
		}
		if mac[0]&0x01 != 0 {
			t.Errorf("MAC %s is a multicast address", mac)
		}
		if seen[mac.String()] {
			t.Errorf("MAC %s handed out twice", mac)
		}
		seen[mac.String()] = true
	}
}

func TestNewRejectsBadNetworks(t *testing.T) {
	tests := []struct {
		name string
		cidr string
	}{
		{"smaller than a /30", "172.16.0.0/31"},
		{"ipv6", "fd00::/64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(netip.MustParsePrefix(tc.cidr)); err == nil {
				t.Errorf("New(%s) succeeded, want error", tc.cidr)
			}
		})
	}
}

func TestIsSandboxTap(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"fctap0", true},
		{"fctap123", true},
		{"fctap", false},    // prefix alone is not a device we made
		{"fctapfoo", false}, // suffix must be numeric
		{"eth0", false},     // never touch the host's own NIC
		{"docker0", false},  // nor another runtime's bridge
		{"myfctap0", false}, // prefix must be at the start
	}
	for _, tc := range tests {
		if got := isSandboxTap(tc.name); got != tc.want {
			t.Errorf("isSandboxTap(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
