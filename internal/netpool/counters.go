package netpool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sysClassNet is where the kernel publishes per-interface counters. Reading them
// from sysfs rather than asking Firecracker keeps the numbers outside the guest's
// reach: the counters belong to a host device the sandbox cannot see, let alone
// rewrite, so they are still trustworthy when the code inside is not.
const sysClassNet = "/sys/class/net"

// Counters is a TAP device's cumulative byte totals, named from the HOST's side
// because that is whose counters these are: rx is what the host received off the
// device.
//
// A sandbox's own view is the mirror image, and the flip belongs to whoever
// reports guest-facing numbers. Naming these from the guest's side here would
// leave two layers each assuming the other had already turned them round.
type Counters struct {
	// RxBytes is what the host received off the TAP -- the guest's egress.
	RxBytes uint64
	// TxBytes is what the host wrote to the TAP -- the guest's ingress.
	TxBytes uint64
}

// Guest returns the same totals from the sandbox's point of view, which is the
// reverse of how the host counted them: the host received what the guest sent.
//
// Every guest-facing report crosses them here so it happens exactly once, in a
// place a test can pin. Done from memory at each call site instead, the mistake
// costs a sandbox's egress -- the number a bandwidth cap and an abuse complaint
// are about -- reported as the direction nobody looks at.
func (c Counters) Guest() (rx, tx uint64) {
	return c.TxBytes, c.RxBytes
}

// ReadCounters returns the byte totals for a TAP device.
//
// ok is false when the device has no counters to read, which is not an error and
// not zero either: a sandbox created without networking never had a TAP, and one
// being torn down has already lost it.
func ReadCounters(tap string) (Counters, bool, error) {
	return readCounters(sysClassNet, tap)
}

func readCounters(root, tap string) (Counters, bool, error) {
	// The name becomes a path element. Names are minted here (fctap<slot>) and
	// never come from a caller, so anything that could climb out of
	// /sys/class/net is a bug upstream -- refuse it instead of reading whatever
	// it points at.
	if tap == "" || tap != filepath.Base(tap) || strings.HasPrefix(tap, ".") {
		return Counters{}, false, fmt.Errorf("tap counters: %q is not an interface name", tap)
	}

	dir := filepath.Join(root, tap, "statistics")

	rx, haveRx, err := readCounter(filepath.Join(dir, "rx_bytes"))
	if err != nil {
		return Counters{}, false, err
	}
	tx, haveTx, err := readCounter(filepath.Join(dir, "tx_bytes"))
	if err != nil {
		return Counters{}, false, err
	}
	// Either one missing means the device is not there, or went while we were
	// reading it. Half a reading is worse than none: it would report a real
	// transfer in one direction and nothing in the other.
	if !haveRx || !haveTx {
		return Counters{}, false, nil
	}

	return Counters{RxBytes: rx, TxBytes: tx}, true, nil
}

// readCounter reads one sysfs counter file. A missing file is reported as absent
// rather than as an error, since that is how a deleted device presents.
func readCounter(path string) (uint64, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read %s: %w", path, err)
	}

	v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return v, true, nil
}
