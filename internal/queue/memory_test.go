package queue

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func newMemoryForTest(t *testing.T, cfg testConfig) Queue {
	t.Helper()
	q := NewMemory(MemoryConfig{
		DefaultMaxAttempts: cfg.DefaultMaxAttempts,
		ReapInterval:       cfg.ReapInterval,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = q.Close() })
	return q
}

// TestMemoryQueue runs the Queue contract against the in-process implementation.
func TestMemoryQueue(t *testing.T) {
	runConformance(t, newMemoryForTest)
}

// The heap orders tasks and the ID index answers "is this queued?". They are
// two structures describing one set, so they can disagree -- and if they do,
// the failure is not a crash but a task that is invisible to deduplication and
// runs twice. Every path that moves a task in or out of pending is exercised
// here, and the two views are compared after each.
func TestMemoryPendingIndexTracksTheHeap(t *testing.T) {
	q := NewMemory(MemoryConfig{ReapInterval: 20 * time.Millisecond},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer q.Close()
	ctx := context.Background()

	check := func(when string) {
		t.Helper()
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.pending.Len() != len(q.pendingIDs) {
			t.Fatalf("%s: heap has %d tasks but the index has %d",
				when, q.pending.Len(), len(q.pendingIDs))
		}
		for _, qt := range q.pending {
			if _, ok := q.pendingIDs[qt.task.ID]; !ok {
				t.Fatalf("%s: task %s is in the heap but not the index; a duplicate "+
					"submission of it would be accepted and run twice", when, qt.task.ID)
			}
		}
	}

	mustEnqueue(t, q, task("a"), task("b"))
	check("after enqueue")

	l := mustLease(t, q, 60*time.Millisecond)
	check("after lease")

	// Let the lease lapse so the reaper re-queues it: a path that makes a task
	// pending again without going through Enqueue.
	time.Sleep(300 * time.Millisecond)
	check("after the reaper requeued an expired lease")

	// The requeued task must be deduplicated like any other pending task.
	if err := q.Enqueue(ctx, task(l.Task.ID)); err == nil {
		t.Errorf("a requeued task accepted a duplicate submission")
	}

	l2 := mustLease(t, q, time.Minute)
	if err := q.Fail(ctx, l2, "node exploded"); err != nil {
		t.Fatal(err)
	}
	check("after an explicit Fail requeued it")
}
