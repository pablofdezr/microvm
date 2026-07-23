package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/queue"
)

// TestTaskPriorityMustBeInRange pins the 0-10 bound. An unbounded priority is an
// abuse vector: one caller parks itself permanently ahead of everyone with a
// number no one else can match.
func TestTaskPriorityMustBeInRange(t *testing.T) {
	h := newHarness(t)
	for _, p := range []int{-1, 11, 100} {
		resp := h.do(t, "POST", "/v1/tasks", map[string]any{
			"image": "python", "cmd": "python3", "priority": p,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("priority %d returned %d, want 400", p, resp.StatusCode)
		}
	}
}

// TestTaskPriorityFlowsToTheQueue confirms a valid priority reaches the queued
// task, where the scheduler will order by it. Bounds at the edges are accepted.
func TestTaskPriorityFlowsToTheQueue(t *testing.T) {
	h := newHarness(t)

	for _, p := range []int{0, 7, 10} {
		resp := h.do(t, "POST", "/v1/tasks", map[string]any{
			"image": "python", "cmd": "python3", "priority": p,
		})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("priority %d create returned %d, want 202", p, resp.StatusCode)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		lease, err := h.q.Lease(ctx, time.Minute, queue.Lessee{Avail: queue.Unbounded, Total: queue.Unbounded})
		cancel()
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if lease.Task.Priority != p {
			t.Errorf("queued task priority = %d, want %d", lease.Task.Priority, p)
		}
		_ = h.q.Complete(context.Background(), lease, queue.Result{})
	}
}
