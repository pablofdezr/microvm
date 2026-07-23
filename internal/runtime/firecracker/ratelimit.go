//go:build linux

package firecracker

// Rate limiting happens inside the VMM rather than on the host.
//
// Firecracker meters the virtual devices themselves, so a guest cannot get
// around it: there is no interface it can reconfigure and no queue it can jump.
// The host-side alternatives -- tc on the TAP, io.max on the cgroup -- work too,
// but they act after the packets have already been produced and copied, and tc
// in particular is a second stateful thing to keep in sync with every VM's
// lifecycle. The VMM already knows when a VM dies.

// refillPeriodMS is the window a token bucket refills over. One second makes
// the configured number read as plain bytes-per-second, and is short enough
// that a burst cannot stall a VM for a noticeable time.
const refillPeriodMS = 1000

// tokenBucket is Firecracker's rate limiter primitive.
type tokenBucket struct {
	// Size is the bucket's capacity: how many tokens accumulate before the
	// limiter starts throttling.
	Size int64 `json:"size"`
	// OneTimeBurst lets a VM spend this much before the limiter engages at all.
	// It exists so that short, bursty work -- fetching a package index, reading
	// the rootfs on boot -- is not slowed by a limit meant for sustained abuse.
	OneTimeBurst int64 `json:"one_time_burst,omitempty"`
	// RefillTime is how long the bucket takes to refill, in milliseconds.
	RefillTime int64 `json:"refill_time"`
}

// rateLimiter caps bandwidth, operations, or both.
type rateLimiter struct {
	// Bandwidth is measured in bytes.
	Bandwidth *tokenBucket `json:"bandwidth,omitempty"`
	// Ops is measured in operations: IOPS for a disk, packets for a NIC.
	Ops *tokenBucket `json:"ops,omitempty"`
}

// newRateLimiter builds a limiter, or nil when nothing is capped.
//
// Returning nil rather than a limiter with zero fields matters: Firecracker
// reads a zero-size bucket as "no tokens, ever" and the device stops dead.
func newRateLimiter(bps, ops int64) *rateLimiter {
	if bps <= 0 && ops <= 0 {
		return nil
	}

	rl := &rateLimiter{}
	if bps > 0 {
		rl.Bandwidth = &tokenBucket{
			Size:       bps,
			RefillTime: refillPeriodMS,
			// A second's worth of burst. Without it, a limited VM pays the
			// throttle on its very first read -- boot included -- and the limit
			// meant for sustained abuse shows up as a slow start.
			OneTimeBurst: bps,
		}
	}
	if ops > 0 {
		rl.Ops = &tokenBucket{
			Size:         ops,
			RefillTime:   refillPeriodMS,
			OneTimeBurst: ops,
		}
	}
	return rl
}
