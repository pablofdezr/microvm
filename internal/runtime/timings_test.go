package runtime

import (
	"testing"
	"time"
)

func TestTimingsUnattributed(t *testing.T) {
	t.Run("is the gap between the measured total and the named phases", func(t *testing.T) {
		got := Timings{
			Network:  10 * time.Millisecond,
			Stage:    20 * time.Millisecond,
			StartVMM: 5 * time.Millisecond,
			BootWait: 100 * time.Millisecond,
			Total:    150 * time.Millisecond,
		}.Unattributed()

		if want := 15 * time.Millisecond; got != want {
			t.Fatalf("Unattributed() = %v, want %v", got, want)
		}
	})

	t.Run("is zero when the phases account for the whole total", func(t *testing.T) {
		got := Timings{
			Stage:    20 * time.Millisecond,
			StartVMM: 5 * time.Millisecond,
			BootWait: 100 * time.Millisecond,
			Total:    125 * time.Millisecond,
		}.Unattributed()

		if got != 0 {
			t.Fatalf("Unattributed() = %v, want 0", got)
		}
	})

	// Total is measured against its own clock while the phases are measured
	// against theirs, so rounding can put the sum a hair above the total. That
	// must read as "nothing unattributed" rather than underflow into an enormous
	// positive duration, which is what an unclamped subtraction on an unsigned
	// reading would produce and what would then get published.
	t.Run("clamps rather than going negative when the phases exceed the total", func(t *testing.T) {
		got := Timings{
			Stage:    100 * time.Millisecond,
			BootWait: 100 * time.Millisecond,
			Total:    150 * time.Millisecond,
		}.Unattributed()

		if got != 0 {
			t.Fatalf("Unattributed() = %v, want 0", got)
		}
	})

	t.Run("is the whole total when no phase was recorded", func(t *testing.T) {
		got := Timings{Total: 42 * time.Millisecond}.Unattributed()

		if want := 42 * time.Millisecond; got != want {
			t.Fatalf("Unattributed() = %v, want %v", got, want)
		}
	})
}

// A zero Timings is what a caller gets from a backend that does not measure, so
// it must not panic or invent a remainder.
func TestTimingsZeroValue(t *testing.T) {
	var zero Timings

	if got := zero.Unattributed(); got != 0 {
		t.Fatalf("Unattributed() on the zero value = %v, want 0", got)
	}
	if zero.GuestReported {
		t.Fatal("GuestReported is true on the zero value; a guest that said nothing must not read as having reported")
	}
}
