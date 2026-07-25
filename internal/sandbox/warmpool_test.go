package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWarmPoolFillsToTargetAndNoFurther(t *testing.T) {
	rt := runtimetest.New()
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 2, MemMiB: 512, Count: 2}}, false)
	p.start()
	defer p.close(context.Background())

	waitFor(t, func() bool { return rt.Created() >= 2 }, 2*time.Second, "warm pool to fill")

	// It must pre-boot exactly the target and then stop; a pool that kept minting
	// would run the node out of memory.
	time.Sleep(50 * time.Millisecond)
	if got := rt.Created(); got != 2 {
		t.Fatalf("warm pool minted %d VMs, want exactly 2", got)
	}
}

func TestWarmPoolHandsOutAndRefills(t *testing.T) {
	rt := runtimetest.New()
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 2, MemMiB: 512, Count: 2}}, false)
	p.start()
	defer p.close(context.Background())
	waitFor(t, func() bool { return rt.Created() >= 2 }, 2*time.Second, "fill")

	key := warmKeyOf(runtime.Spec{Image: "python", VCPUs: 2, MemMiB: 512})
	if p.checkout(key) == nil || p.checkout(key) == nil {
		t.Fatal("expected two warm VMs to hand out")
	}
	if p.hits.Load() != 2 {
		t.Fatalf("hit counter = %d, want 2", p.hits.Load())
	}
	// The pool tops itself back up to the target after the checkouts.
	waitFor(t, func() bool { return rt.Created() >= 4 }, 2*time.Second, "refill after checkout")
}

func TestWarmPoolMissesOnDifferentShape(t *testing.T) {
	rt := runtimetest.New()
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 1, MemMiB: 256, Count: 1}}, false)
	p.start()
	defer p.close(context.Background())
	waitFor(t, func() bool { return rt.Created() >= 1 }, 2*time.Second, "fill")

	if p.checkout(warmKeyOf(runtime.Spec{Image: "go", VCPUs: 1, MemMiB: 256})) != nil {
		t.Error("a different image must miss")
	}
	if p.checkout(warmKeyOf(runtime.Spec{Image: "python", VCPUs: 4, MemMiB: 256})) != nil {
		t.Error("a different vcpu count must miss")
	}
	if p.checkout(warmKeyOf(runtime.Spec{Image: "python", VCPUs: 1, MemMiB: 256})) == nil {
		t.Error("the exact shape must hit")
	}
}

func TestWarmPoolCloseDrainsInstances(t *testing.T) {
	rt := runtimetest.New()
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 1, MemMiB: 256, Count: 2}}, false)
	p.start()
	waitFor(t, func() bool { return rt.Created() >= 2 }, 2*time.Second, "fill")

	p.close(context.Background())
	p.close(context.Background()) // idempotent

	p.mu.Lock()
	remaining := 0
	for _, insts := range p.ready {
		remaining += len(insts)
	}
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("warm pool left %d VMs after close, want 0", remaining)
	}
}

func TestWarmPoolRestoresFromSnapshotWhenEnabled(t *testing.T) {
	rt := runtimetest.New()
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 2, MemMiB: 512, Count: 2}}, true)
	p.start()
	defer p.close(context.Background())

	// The pool captures one template (a single cold boot + snapshot) and then
	// fills by restoring, so the ready VMs are restores rather than cold boots.
	waitFor(t, func() bool { return rt.Restored() >= 2 }, 2*time.Second, "warm pool to fill by restore")
	if snaps := len(rt.Snapshots()); snaps != 1 {
		t.Errorf("expected exactly one template snapshot, got %d", snaps)
	}
	if c := rt.Created(); c != 1 {
		t.Errorf("expected exactly one cold boot (the template), got %d", c)
	}
	if p.checkout(warmKeyOf(runtime.Spec{Image: "python", VCPUs: 2, MemMiB: 512})) == nil {
		t.Error("a restored warm VM should be handed out")
	}
}

// A snapshot failure must not disable snapshots for the shape forever. It used to:
// one slow restore -- a memory image whose page cache had been reclaimed, on an SD
// card -- latched the shape to cold boots for the daemon's whole life, on the one
// code path whose entire purpose is to be faster than a cold boot.
func TestWarmPoolRetriesSnapshotsAfterAFailure(t *testing.T) {
	rt := runtimetest.New()
	rt.SetRestoreErr(errors.New("restore: guest never became ready"))

	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 2, MemMiB: 512, Count: 1}}, true)
	key := warmKeyOf(runtime.Spec{Image: "python", VCPUs: 2, MemMiB: 512})
	p.start()
	defer p.close(context.Background())

	// The failure falls the shape back to cold boots for now, and the pool still
	// reaches its target -- a snapshot problem degrades to slower, never to none.
	waitFor(t, func() bool { return p.isColdOnly(key) && p.readyCount(key) == 1 }, 2*time.Second,
		"the failed restore to fall back to cold boots")

	// ...and the cooldown is a cooldown, not a latch. Bring it forward rather than
	// sleeping through the real one; the property under test is that expiry re-opens
	// the path, not how long the wall clock takes to get there.
	rt.SetRestoreErr(nil)
	p.mu.Lock()
	p.snapFails[key].until = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if p.isColdOnly(key) {
		t.Fatal("an expired cooldown still reads as cold-only")
	}

	// A shape at target is not refilled, so make room and let the loop try again.
	if inst := p.checkout(key); inst == nil {
		t.Fatal("no warm VM to check out")
	}
	waitFor(t, func() bool { return rt.Restored() >= 1 }, 2*time.Second, "snapshots to be retried")

	// The successful restore clears the record entirely.
	p.mu.Lock()
	_, stillRecorded := p.snapFails[key]
	p.mu.Unlock()
	if stillRecorded {
		t.Error("a working restore left the shape's failure record behind")
	}
}

// Repeated failure does eventually stop: at that point it is not one slow restore
// but a host that cannot do this, and each attempt costs a cold boot plus a full
// dump of the guest's memory.
func TestWarmPoolGivesUpOnSnapshotsEventually(t *testing.T) {
	rt := runtimetest.New()
	rt.SetRestoreErr(errors.New("restore: load snapshot: no KVM"))
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 1, MemMiB: 256, Count: 1}}, true)
	key := warmKeyOf(runtime.Spec{Image: "python", VCPUs: 1, MemMiB: 256})

	for i := 0; i < snapGiveUpAfter; i++ {
		p.snapshotFailed(key, "restore", rt.RestoreErr)
	}
	p.mu.Lock()
	p.snapFails[key].until = time.Now().Add(-time.Hour) // the cooldown has passed
	p.mu.Unlock()

	if !p.isColdOnly(key) {
		t.Fatalf("still trying snapshots after %d consecutive failures", snapGiveUpAfter)
	}
}

// A failure throws the shape's template away. It is a candidate cause -- captured
// against an image since rebuilt, captured from a guest that did not carry its
// interrupt-controller state -- and it is half a gigabyte of guest RAM on disk that
// nothing would otherwise reclaim.
func TestWarmPoolDiscardsTheTemplateOfAFailedShape(t *testing.T) {
	rt := runtimetest.New()
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 1, MemMiB: 256, Count: 1}}, true)
	p.start()
	waitFor(t, func() bool { return len(rt.Snapshots()) == 1 }, 2*time.Second, "a template to be captured")
	defer p.close(context.Background())

	key := warmKeyOf(runtime.Spec{Image: "python", VCPUs: 1, MemMiB: 256})
	p.snapshotFailed(key, "restore", errors.New("boom"))

	if got := rt.Discarded(); got != 1 {
		t.Fatalf("discarded %d templates after a failure, want 1", got)
	}
	p.mu.Lock()
	_, kept := p.templates[key]
	p.mu.Unlock()
	if kept {
		t.Error("the failed shape's template is still held")
	}
}

// Closing the pool reclaims its templates. Each is a full copy of a guest's RAM
// and the ref discarded here is the only handle that ever existed to it, so
// anything left behind is a file nothing will open again -- and a complete guest
// memory image at rest for as long as the box lives.
func TestWarmPoolCloseDiscardsTemplates(t *testing.T) {
	rt := runtimetest.New()
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 1, MemMiB: 256, Count: 1}}, true)
	p.start()
	waitFor(t, func() bool { return rt.Restored() >= 1 }, 2*time.Second, "fill by restore")

	p.close(context.Background())
	p.close(context.Background()) // idempotent

	if got := rt.Discarded(); got != 1 {
		t.Fatalf("discarded %d templates on close, want 1", got)
	}
	if left := rt.Snapshots(); len(left) != 0 {
		t.Fatalf("%d snapshots survived the close", len(left))
	}
}

// A pooled VM exists before anybody owns it, so its meters restart when it is
// handed out. Without this a VM that waited ten minutes in the pool reported ten
// minutes of billable wall and ten of idle against its first tenant, next to a
// created_at stamped at checkout -- so the meter read longer than the sandbox had
// apparently existed.
func TestWarmPoolRestartsTheMetersOnCheckout(t *testing.T) {
	rt := runtimetest.New()
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 1, MemMiB: 256, Count: 1}}, false)
	p.start()
	defer p.close(context.Background())
	waitFor(t, func() bool { return rt.Created() >= 1 }, 2*time.Second, "fill")

	inst := p.checkout(warmKeyOf(runtime.Spec{Image: "python", VCPUs: 1, MemMiB: 256}))
	if inst == nil {
		t.Fatal("no warm VM to check out")
	}
	fake, ok := inst.(*runtimetest.Instance)
	if !ok {
		t.Fatalf("checkout returned %T", inst)
	}
	if got := fake.MetersAdopted(); got != 1 {
		t.Fatalf("meters restarted %d times on checkout, want exactly 1", got)
	}
}

// Networked shapes never take the snapshot path: a snapshot carries the guest's
// MAC and IP in its memory image, so every restore of one template would claim the
// same address. Asking anyway would be one guaranteed failure and cooldown per
// networked shape.
func TestWarmPoolNeverSnapshotsANetworkedShape(t *testing.T) {
	rt := runtimetest.New()
	p := newWarmPool(rt, discardLog(),
		[]WarmSpec{{Image: "python", VCPUs: 1, MemMiB: 256, Network: true, Count: 1}}, true)
	p.start()
	defer p.close(context.Background())

	waitFor(t, func() bool { return rt.Created() >= 1 }, 2*time.Second, "a networked shape to cold-boot")
	time.Sleep(50 * time.Millisecond)
	if snaps := len(rt.Snapshots()); snaps != 0 {
		t.Errorf("captured %d templates for a networked shape, want 0", snaps)
	}
	if rt.Restored() != 0 {
		t.Errorf("restored %d networked VMs, want 0", rt.Restored())
	}
}

func TestManagerUsesWarmPool(t *testing.T) {
	rt := runtimetest.New()
	m := NewManager(rt, logstore.New(logstore.Config{}), discardLog(), WithWarmPool([]WarmSpec{{Image: "python", VCPUs: 2, MemMiB: 512, Count: 1}}))
	defer m.Close(context.Background())

	waitFor(t, func() bool { return rt.Created() >= 1 }, 2*time.Second, "warm pool to fill")

	// A compatible Create is served from the pool: the underlying runtime does
	// not get a fresh cold-boot request (it stays at the one warm mint, though it
	// then refills back to target).
	sb, err := m.Create(context.Background(), Spec{Spec: runtime.Spec{ID: "task-1", Image: "python", VCPUs: 2, MemMiB: 512}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.ID() != "task-1" {
		t.Errorf("sandbox id = %q, want task-1", sb.ID())
	}
	if m.warm.hits.Load() != 1 {
		t.Fatalf("expected the create to be served from the warm pool (hits=%d)", m.warm.hits.Load())
	}
}
