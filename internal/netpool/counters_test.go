package netpool

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTapStats lays out a fake /sys/class/net for one device.
func writeTapStats(t *testing.T, root, tap string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, tap, "statistics")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadCounters(t *testing.T) {
	root := t.TempDir()
	// Trailing newline: sysfs writes one, and a parse that does not trim it reads
	// every counter as malformed.
	writeTapStats(t, root, "fctap0", map[string]string{
		"rx_bytes": "4096\n",
		"tx_bytes": "1024\n",
	})

	c, ok, err := readCounters(root, "fctap0")
	if err != nil {
		t.Fatalf("readCounters: %v", err)
	}
	if !ok {
		t.Fatal("ok = false for a device that exists")
	}
	if c.RxBytes != 4096 || c.TxBytes != 1024 {
		t.Errorf("counters = %+v, want rx=4096 tx=1024", c)
	}
}

// A sandbox created with network: false has no TAP, and neither does one whose
// device has already been torn down. Both must read as "nothing to count" -- an
// error here would fail the whole sample and lose the CPU numbers with it.
func TestReadCountersMissingDevice(t *testing.T) {
	root := t.TempDir()
	writeTapStats(t, root, "fctap0", map[string]string{
		"rx_bytes": "10\n",
		"tx_bytes": "20\n",
	})

	c, ok, err := readCounters(root, "fctap7")
	if err != nil {
		t.Fatalf("readCounters for a missing device: %v", err)
	}
	if ok {
		t.Error("ok = true for a device that does not exist")
	}
	if c != (Counters{}) {
		t.Errorf("counters = %+v, want zero", c)
	}
}

// Half a reading is worse than none: it would report real traffic in one
// direction and silence in the other, which reads as a one-way transfer that
// never happened.
func TestReadCountersHalfPresentDeviceIsAbsent(t *testing.T) {
	root := t.TempDir()
	writeTapStats(t, root, "fctap1", map[string]string{"rx_bytes": "512\n"})

	c, ok, err := readCounters(root, "fctap1")
	if err != nil {
		t.Fatalf("readCounters: %v", err)
	}
	if ok || c != (Counters{}) {
		t.Errorf("ok = %v, counters = %+v; want false and zero when tx_bytes is missing", ok, c)
	}
}

func TestReadCountersRejectsGarbage(t *testing.T) {
	root := t.TempDir()
	writeTapStats(t, root, "fctap2", map[string]string{
		"rx_bytes": "not a number\n",
		"tx_bytes": "1\n",
	})

	if _, ok, err := readCounters(root, "fctap2"); err == nil {
		t.Errorf("unparseable counter accepted: ok = %v", ok)
	}
}

// The name lands in a path. A device name that is not a name would read whatever
// it climbs out to, and report a host interface's traffic as a sandbox's.
func TestReadCountersRejectsNonNames(t *testing.T) {
	root := t.TempDir()
	writeTapStats(t, root, "eth0", map[string]string{
		"rx_bytes": "999\n",
		"tx_bytes": "999\n",
	})

	for _, tap := range []string{"", ".", "..", "../eth0", "fctap0/../eth0", "/fctap0"} {
		if _, _, err := readCounters(root, tap); err == nil {
			t.Errorf("readCounters(%q) succeeded, want a refusal", tap)
		}
	}
}

// The host's counters are the mirror of the guest's. This is the one place the
// crossing happens, so it is the one place it can be got wrong.
func TestCountersGuestViewInvertsTheHostsView(t *testing.T) {
	c := Counters{RxBytes: 700, TxBytes: 3}

	rx, tx := c.Guest()
	if tx != c.RxBytes {
		t.Errorf("guest tx = %d, want the host's rx (%d): the guest's egress is what the host received", tx, c.RxBytes)
	}
	if rx != c.TxBytes {
		t.Errorf("guest rx = %d, want the host's tx (%d)", rx, c.TxBytes)
	}
}
