//go:build linux

package agent

import (
	"strings"
	"testing"
	"time"
)

func TestSinceBootIsMonotonicAndNonZero(t *testing.T) {
	first, ok := sinceBoot()
	if !ok {
		t.Skip("CLOCK_BOOTTIME unavailable on this kernel")
	}
	if first <= 0 {
		t.Fatalf("sinceBoot() = %v, want a positive time since boot", first)
	}

	second, _ := sinceBoot()
	if second < first {
		t.Fatalf("sinceBoot() went backwards: %v then %v", first, second)
	}
}

// The measurement has to survive the init-to-supervisor re-exec, and the
// environment is the only channel that does. A round trip that loses precision
// or silently zeroes would make every published guest figure wrong in a way
// nothing else would catch.
func TestBootEnvRoundTrip(t *testing.T) {
	bootTimings.kernel = 87_500 * time.Microsecond
	bootTimings.init = 12_250 * time.Microsecond
	bootTimings.ok = true
	t.Cleanup(func() { bootTimings.kernel, bootTimings.init, bootTimings.ok = 0, 0, false })

	env := bootEnv()
	if len(env) != 2 {
		t.Fatalf("bootEnv() = %v, want two entries", env)
	}

	for _, e := range env {
		key, value, found := strings.Cut(e, "=")
		if !found {
			t.Fatalf("bootEnv() entry %q is not key=value", e)
		}
		t.Setenv(key, value)
	}

	kernel, init := readBootEnv()
	if kernel != bootTimings.kernel {
		t.Errorf("kernel round-tripped to %v, want %v", kernel, bootTimings.kernel)
	}
	if init != bootTimings.init {
		t.Errorf("init round-tripped to %v, want %v", init, bootTimings.init)
	}
}

// An unmeasured boot must contribute no environment at all: an absent variable
// is what makes the serving side report zero and the host treat the split as
// unavailable, which is different from claiming the guest booted instantly.
func TestBootEnvIsEmptyWhenUnmeasured(t *testing.T) {
	bootTimings.ok = false
	t.Cleanup(func() { bootTimings.ok = false })

	if env := bootEnv(); env != nil {
		t.Fatalf("bootEnv() = %v, want nil when nothing was measured", env)
	}
}

func TestReadBootEnvToleratesGarbage(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"absent", ""},
		{"not a number", "fast"},
		{"negative", "-500"},
		{"overflows int64", "99999999999999999999999"},
		{"float", "12.5"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(kernelBootEnvKey, tc.value)

			// Diagnostic plumbing: a guest that cannot say how long it booted is
			// still a guest that booted, so this reports zero rather than failing
			// the boot over an unparseable number.
			if got := microsFromEnv(kernelBootEnvKey); got != 0 {
				t.Fatalf("microsFromEnv(%q) = %v, want 0", tc.value, got)
			}
		})
	}
}

func TestMicrosFromEnvParsesMicroseconds(t *testing.T) {
	t.Setenv(kernelBootEnvKey, "1500")

	if got := microsFromEnv(kernelBootEnvKey); got != 1500*time.Microsecond {
		t.Fatalf("microsFromEnv = %v, want 1.5ms", got)
	}
}

// The two keys must differ, or one phase overwrites the other and the breakdown
// reports the same number twice.
func TestBootEnvKeysAreDistinct(t *testing.T) {
	if kernelBootEnvKey == guestInitEnvKey {
		t.Fatal("the kernel and init environment keys are identical")
	}
}
