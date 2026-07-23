package sandbox

import (
	"context"
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
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 2, MemMiB: 512, Count: 2}})
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
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 2, MemMiB: 512, Count: 2}})
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
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 1, MemMiB: 256, Count: 1}})
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
	p := newWarmPool(rt, discardLog(), []WarmSpec{{Image: "python", VCPUs: 1, MemMiB: 256, Count: 2}})
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
