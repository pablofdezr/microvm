package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
)

// newAdmissionManager returns a manager over a fake runtime, plus the runtime so
// a test can make booting fail.
func newAdmissionManager(t *testing.T) (*Manager, *runtimetest.Runtime) {
	t.Helper()
	rt := runtimetest.New()
	m := NewManager(rt, logstore.New(logstore.Config{}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m, rt
}

// create starts one sandbox for a tenant under a cap.
func create(t *testing.T, m *Manager, id, tenant string, limit int) (*Sandbox, error) {
	t.Helper()
	return m.Create(context.Background(), Spec{
		Spec:          runtime.Spec{ID: id, Image: "python"},
		Tenant:        tenant,
		MaxConcurrent: limit,
	})
}

// The cap is the whole feature: without it one token holds every slot on the node
// and everybody else meets a host that is permanently full.
func TestTenantConcurrencyCap(t *testing.T) {
	m, _ := newAdmissionManager(t)

	if _, err := create(t, m, "sb_1", "acme", 2); err != nil {
		t.Fatalf("first sandbox: %v", err)
	}
	if _, err := create(t, m, "sb_2", "acme", 2); err != nil {
		t.Fatalf("second sandbox, at the cap and allowed: %v", err)
	}

	_, err := create(t, m, "sb_3", "acme", 2)
	var limit *ConcurrencyLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("third sandbox past a cap of 2: err = %v, want a ConcurrencyLimitError", err)
	}
	if limit.Live != 2 || limit.Max != 2 {
		t.Errorf("error says %d of %d, want 2 of 2", limit.Live, limit.Max)
	}
}

// One tenant at its cap must not cost another tenant anything. A limit that is
// really the node's, wearing a tenant's name, would do exactly that.
func TestTheCapIsPerTenant(t *testing.T) {
	m, _ := newAdmissionManager(t)

	if _, err := create(t, m, "sb_a1", "tenant-a", 1); err != nil {
		t.Fatalf("tenant a: %v", err)
	}
	if _, err := create(t, m, "sb_a2", "tenant-a", 1); err == nil {
		t.Fatal("tenant a got a second sandbox past its cap of 1")
	}

	if _, err := create(t, m, "sb_b1", "tenant-b", 1); err != nil {
		t.Fatalf("tenant b was refused because tenant a is full: %v", err)
	}
}

// The slot is reserved before the VM boots, and this is the difference that
// makes: a cap that counted afterwards would let every create in a burst read
// the same number and pass, so it would hold only when nothing was happening.
func TestTheCapHoldsUnderConcurrentCreates(t *testing.T) {
	m, rt := newAdmissionManager(t)

	// A boot slow enough that every goroutine is inside Create at once, which is
	// exactly the window a count-afterwards cap would miss.
	rt.CreateDelay = 20 * time.Millisecond

	const limit = 3
	var (
		wg       sync.WaitGroup
		accepted atomic.Int64
	)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := create(t, m, fmt.Sprintf("sb_%d", i), "acme", limit); err == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := accepted.Load(); got != limit {
		t.Errorf("%d of 20 concurrent creates were accepted under a cap of %d", got, limit)
	}
}

// The slot comes back when the sandbox stops, not when the record is swept: a
// caller who deletes one expects to create the next immediately.
func TestStoppingReturnsTheSlot(t *testing.T) {
	m, _ := newAdmissionManager(t)

	sb, err := create(t, m, "sb_1", "acme", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := create(t, m, "sb_2", "acme", 1); err == nil {
		t.Fatal("created a second sandbox past a cap of 1")
	}

	if err := sb.Stop(context.Background(), ReasonStopped); err != nil {
		t.Fatal(err)
	}
	// The stopped sandbox is still listed for the retention window, so this also
	// checks that what is counted is running sandboxes and not remembered ones.
	if _, err := create(t, m, "sb_3", "acme", 1); err != nil {
		t.Fatalf("a slot was not returned when a sandbox stopped: %v", err)
	}
}

// Stop is idempotent, so it must not return the same slot twice -- a tenant that
// stops one sandbox twice would otherwise earn an extra one.
func TestStoppingTwiceReturnsOneSlot(t *testing.T) {
	m, _ := newAdmissionManager(t)

	sb, err := create(t, m, "sb_1", "acme", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := create(t, m, "sb_2", "acme", 2); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := sb.Stop(context.Background(), ReasonStopped); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := create(t, m, "sb_3", "acme", 2); err != nil {
		t.Fatalf("one slot back: %v", err)
	}
	if _, err := create(t, m, "sb_4", "acme", 2); err == nil {
		t.Fatal("stopping one sandbox three times returned three slots")
	}
}

// A VM that never booted spent nothing. Holding its slot would let a node that
// is failing to boot lock a tenant out of a cap they never used.
func TestAFailedBootDoesNotSpendASlot(t *testing.T) {
	m, rt := newAdmissionManager(t)

	rt.CreateErr = errors.New("no free network slot")
	if _, err := create(t, m, "sb_1", "acme", 1); err == nil {
		t.Fatal("a failing runtime produced a sandbox")
	}

	rt.CreateErr = nil
	if _, err := create(t, m, "sb_2", "acme", 1); err != nil {
		t.Fatalf("a failed boot held the tenant's only slot: %v", err)
	}
}

// No tenant means nothing to charge: a daemon with auth off, and a queued task,
// which is claimed by a node rather than sent by a caller.
func TestUntenantedCreatesAreUncounted(t *testing.T) {
	m, _ := newAdmissionManager(t)

	for i, id := range []string{"sb_1", "sb_2", "sb_3"} {
		if _, err := m.Create(context.Background(), Spec{
			Spec:          runtime.Spec{ID: id, Image: "python"},
			MaxConcurrent: 1,
		}); err != nil {
			t.Fatalf("sandbox %d with no tenant was refused: %v", i, err)
		}
	}
}

// Zero is unlimited, and it is what every existing deployment has: the cap must
// not appear on upgrade.
func TestNoCapMeansUnlimited(t *testing.T) {
	m, _ := newAdmissionManager(t)

	for i, id := range []string{"sb_1", "sb_2", "sb_3", "sb_4"} {
		if _, err := create(t, m, id, "acme", 0); err != nil {
			t.Fatalf("sandbox %d with no cap configured was refused: %v", i, err)
		}
	}
}
