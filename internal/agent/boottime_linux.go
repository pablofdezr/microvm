//go:build linux

package agent

import (
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// Environment keys carrying the boot breakdown across the init-to-supervisor
// re-exec.
//
// The re-exec is why these exist at all: PID 1 measures the boot, then execs a
// child to do the serving (see startSupervisor), and the child is a fresh
// process with none of PID 1's memory. The environment is the one channel that
// survives, so the two figures ride across in it.
//
// They are named as internal plumbing rather than as configuration because that
// is what they are: nothing outside this package sets them, and a guest process
// that forged them would only be lying to a log line -- see the trust note on
// protocol.HealthResponse.
const (
	kernelBootEnvKey = "MICROVM_BOOT_KERNEL_US"
	guestInitEnvKey  = "MICROVM_BOOT_INIT_US"
)

// sinceBoot returns how long the kernel has been up.
//
// CLOCK_BOOTTIME rather than /proc/uptime, and the reason is resolution: uptime
// is formatted to hundredths of a second, so an init phase that takes 8ms reads
// as either 0.00 or 0.01 and the whole breakdown collapses. This clock is
// nanosecond-resolution and monotonic from boot, which is exactly the origin
// the measurement wants -- CLOCK_MONOTONIC would do on Linux today but is not
// specified to start at boot, and that is the one property being relied on.
//
// The bool is false if the clock is unavailable, which means "do not report"
// rather than "took no time".
func sinceBoot() (time.Duration, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, false
	}
	return time.Duration(ts.Nano()), true
}

// bootTimings is the boot breakdown this guest will report, filled in by
// InitGuest and read by startSupervisor as it builds the child's environment.
//
// A package-level value is safe here because the whole sequence is
// single-threaded and strictly ordered: InitGuest runs to completion, then
// RunInit execs the child. Nothing concurrent ever touches it.
var bootTimings struct {
	kernel time.Duration
	init   time.Duration
	ok     bool
}

// bootEnv returns the environment entries carrying the boot breakdown, or
// nothing when it was never measured -- an absent variable is what makes the
// serving side report zero and the host treat the split as unavailable.
func bootEnv() []string {
	if !bootTimings.ok {
		return nil
	}
	return []string{
		kernelBootEnvKey + "=" + strconv.FormatInt(bootTimings.kernel.Microseconds(), 10),
		guestInitEnvKey + "=" + strconv.FormatInt(bootTimings.init.Microseconds(), 10),
	}
}

// readBootEnv recovers the boot breakdown in the re-executed serving process.
//
// A malformed or missing value reports zero rather than failing: this is
// diagnostic plumbing, and a guest that cannot say how long it booted is still
// a guest that booted.
func readBootEnv() (kernel, init time.Duration) {
	return microsFromEnv(kernelBootEnvKey), microsFromEnv(guestInitEnvKey)
}

func microsFromEnv(key string) time.Duration {
	us, err := strconv.ParseInt(os.Getenv(key), 10, 64)
	if err != nil || us < 0 {
		return 0
	}
	return time.Duration(us) * time.Microsecond
}
