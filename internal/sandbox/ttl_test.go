package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
)

func newTTLManager(t *testing.T) *Manager {
	t.Helper()
	logs := logstore.New(logstore.Config{})
	m := NewManager(runtimetest.New(), logs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

func newTTLSandbox(t *testing.T, id string, ttl time.Duration) *Sandbox {
	t.Helper()
	sb, err := newTTLManager(t).Create(context.Background(),
		Spec{Spec: runtime.Spec{ID: id, Image: "python"}, TTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

// The rule, walked through a sandbox's whole life without waiting for it. The
// bound is measured from creation, so the interesting cases are the ones where
// "from now" and "from creation" disagree -- which is every case except the
// first second of a sandbox's life.
func TestExtendedDeadline(t *testing.T) {
	const maxLifetime = 24 * time.Hour
	now := time.Now()

	tests := []struct {
		name    string
		created time.Time
		expires time.Time
		ttl     time.Duration
		want    time.Time
		wantErr bool
		// remaining is the room the refusal should report.
		remaining time.Duration
	}{
		{
			name:    "pushes the deadline out",
			created: now,
			expires: now.Add(time.Minute),
			ttl:     10 * time.Minute,
			want:    now.Add(10 * time.Minute),
		},
		{
			name:    "never brings the deadline forward",
			created: now,
			expires: now.Add(time.Hour),
			ttl:     time.Minute,
			want:    now.Add(time.Hour),
		},
		{
			name:    "an hour is fine at the start of a life",
			created: now,
			expires: now.Add(time.Minute),
			ttl:     time.Hour,
			want:    now.Add(time.Hour),
		},
		{
			// The same hour, on a sandbox that has already lived nearly a day.
			// Bounded from now this would be granted forever; bounded from
			// creation it is the point where extension stops buying anything.
			name:      "the same hour is refused near the end of one",
			created:   now.Add(-23*time.Hour - 30*time.Minute),
			expires:   now.Add(30 * time.Minute),
			ttl:       time.Hour,
			wantErr:   true,
			remaining: 30 * time.Minute,
		},
		{
			name:    "exactly the bound is granted",
			created: now.Add(-23 * time.Hour),
			expires: now.Add(30 * time.Minute),
			ttl:     time.Hour,
			want:    now.Add(time.Hour),
		},
		{
			name:      "nothing remains once the maximum is spent",
			created:   now.Add(-25 * time.Hour),
			expires:   now.Add(-time.Hour),
			ttl:       time.Minute,
			wantErr:   true,
			remaining: 0,
		},
		{
			name:    "zero is not an extension",
			created: now,
			expires: now.Add(time.Minute),
			ttl:     0,
			wantErr: true,
		},
		{
			name:    "negative is not an extension",
			created: now,
			expires: now.Add(time.Minute),
			ttl:     -time.Minute,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extendedDeadline(now, tc.created, tc.expires, tc.ttl, maxLifetime)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("extendedDeadline = %s, want a refusal", got)
				}
				var limit *TTLLimitError
				if !errors.As(err, &limit) {
					return // zero and negative are not limit errors
				}
				if limit.Remaining != tc.remaining {
					t.Errorf("remaining = %s, want %s", limit.Remaining, tc.remaining)
				}
				if limit.Max != maxLifetime {
					t.Errorf("max = %s, want %s", limit.Max, maxLifetime)
				}
				return
			}

			if err != nil {
				t.Fatalf("extendedDeadline: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("deadline = %s, want %s", got, tc.want)
			}
		})
	}
}

// A sandbox created with the maximum TTL has nothing left to buy, which is the
// property that keeps a caller heartbeating forever from holding a slot forever.
func TestExtendPastTheMaximumIsRefused(t *testing.T) {
	sb := newTTLSandbox(t, "sb_max", MaxTTL)

	_, err := sb.Extend(MaxTTL)
	if err == nil {
		t.Fatal("extending a sandbox already at the maximum lifetime was granted")
	}

	var limit *TTLLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("err = %v, want a *TTLLimitError", err)
	}
	if limit.Max != MaxTTL {
		t.Errorf("max = %s, want %s", limit.Max, MaxTTL)
	}
	// The deadline must be untouched: a refused extension that moved anything
	// would be a partial success nobody was told about.
	if want := sb.Info().CreatedAt.Add(MaxTTL); !sb.Info().ExpiresAt.Equal(want) {
		t.Errorf("expires = %s, want %s: a refusal moved the deadline", sb.Info().ExpiresAt, want)
	}
}

// Extension re-arms the one TTL timer. When it stacks a second one instead, the
// original deadline still fires and kills the sandbox the caller just paid for --
// which is why this test waits past the original deadline rather than only
// checking what Info reports.
func TestExtendReArmsTheTTLTimerRatherThanStacking(t *testing.T) {
	sb := newTTLSandbox(t, "sb_rearm", 80*time.Millisecond)
	before := sb.Info().ExpiresAt

	expires, err := sb.Extend(600 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !expires.After(before) {
		t.Fatalf("expires = %s, want later than %s", expires, before)
	}

	// Well past the original deadline, well short of the new one.
	time.Sleep(250 * time.Millisecond)
	if state := sb.State(); state != StateRunning {
		t.Fatalf("sandbox is %s (%s) after its original deadline: the old timer still fired",
			state, sb.Reason())
	}

	// And the re-armed timer must still fire: an extension that disarmed the TTL
	// would look identical up to here and never kill anything again.
	waitStopped(t, sb, 3*time.Second)
	if reason := sb.Reason(); reason != ReasonExpired {
		t.Errorf("stop reason = %q, want %q", reason, ReasonExpired)
	}
}

// A second extension is not additive, and a shorter one does not cut the longer
// one short. Two callers heartbeating one sandbox at different intervals is the
// case this protects.
func TestExtendIsMonotonic(t *testing.T) {
	sb := newTTLSandbox(t, "sb_mono", time.Minute)

	long, err := sb.Extend(30 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	short, err := sb.Extend(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !short.Equal(long) {
		t.Errorf("a one-minute extension moved the deadline from %s to %s", long, short)
	}
	if got := sb.Info().ExpiresAt; !got.Equal(long) {
		t.Errorf("expires = %s, want %s", got, long)
	}
}

// Extending something already gone is a refusal, not a silent success. A caller
// heartbeating a dead sandbox has a problem, and answering "fine" hides it until
// their next exec fails for reasons they cannot connect to this.
func TestExtendStoppedSandboxIsRefused(t *testing.T) {
	sb := newTTLSandbox(t, "sb_stopped", time.Minute)
	if err := sb.Stop(context.Background(), ReasonStopped); err != nil {
		t.Fatal(err)
	}

	if _, err := sb.Extend(10 * time.Minute); err == nil {
		t.Fatal("extending a stopped sandbox was granted")
	}
}

// The deadline is mutable now, so it is shared state: the supervisor reads it
// every time it wakes and every caller writes it. Run with -race, this is the
// test that fails if any of those accesses leaves the mutex.
func TestExtendRacesTheTTLFiring(t *testing.T) {
	m := newTTLManager(t)
	sb, err := m.Create(context.Background(),
		Spec{Spec: runtime.Spec{ID: "sb_race", Image: "python"}, TTL: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				// Either outcome is correct -- the TTL may fire mid-run and every
				// extension after that is refused. What must not happen is a
				// torn read, or a sandbox that outlives its deadline.
				_, _ = sb.Extend(40 * time.Millisecond)
				// Spread the calls across the deadline: without this the whole
				// loop finishes in microseconds and never overlaps the firing it
				// is here to race.
				time.Sleep(time.Millisecond)
			}
		}()
	}
	// Readers too: Info is the other side of the same field.
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = sb.Info().ExpiresAt
				_ = sb.State()
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	// Every extension asked for 40ms, so the sandbox must die shortly after the
	// last one -- not be kept alive by the stack of them.
	waitStopped(t, sb, 3*time.Second)
	if reason := sb.Reason(); reason != ReasonExpired {
		t.Errorf("stop reason = %q, want %q", reason, ReasonExpired)
	}
}

func waitStopped(t *testing.T, sb *Sandbox, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if sb.State() == StateStopped {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("sandbox %s is still %s after %s; its deadline was %s",
		sb.ID(), sb.State(), within, sb.Info().ExpiresAt)
}

// The invariant the whole feature rests on, and the one the deadline being
// mutable put at risk: an extension that was granted is honoured.
//
// stop samples the meters before it takes the sandbox's lock -- a cgroup read plus
// two sysfs reads for the TAP counters -- so between "the supervisor decided to
// kill this" and "the state says stopped" there is a filesystem-IO-wide window in
// which the sandbox still looks perfectly alive. An Extend landing there used to be
// granted, and answered 200 with a deadline an hour out for a sandbox that died a
// millisecond later.
func TestExtendIsRefusedOnceTheKillIsClaimed(t *testing.T) {
	rt := runtimetest.New()

	// Hold the final sample open, which is exactly where a real stop pauses.
	sampling, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	rt.OnStats = func(string) {
		once.Do(func() {
			close(sampling)
			<-proceed
		})
	}

	m := NewManager(rt, logstore.New(logstore.Config{}),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = m.Close(context.Background()) })

	sb, err := m.Create(context.Background(),
		Spec{Spec: runtime.Spec{ID: "sb_claim", Image: "python"}, TTL: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	<-sampling // the TTL fired, the kill is decided, the meters are being read

	// Still running as far as anything outside can tell, which is the trap: a guard
	// that reads the state cannot see the difference.
	if state := sb.State(); state != StateRunning {
		t.Fatalf("state = %q, want running: the window this test is about has already closed", state)
	}

	expires, err := sb.Extend(time.Hour)
	close(proceed)

	var claimed *ExpiredError
	if !errors.As(err, &claimed) {
		t.Fatalf("Extend returned (%s, %v), want an *ExpiredError: an hour was granted to a sandbox already being killed",
			expires, err)
	}

	waitStopped(t, sb, 3*time.Second)
	if reason := sb.Reason(); reason != ReasonExpired {
		t.Errorf("stop reason = %q, want %q", reason, ReasonExpired)
	}
}
