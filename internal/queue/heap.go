package queue

// queuedTask is a task waiting for a slot.
type queuedTask struct {
	task    Task
	attempt int

	// seq is a monotonic enqueue counter. It is what makes equal priorities
	// strictly FIFO: timestamps collide at nanosecond resolution and would let
	// two tasks enqueued together come out in either order, which is exactly
	// the guarantee callers depend on.
	seq uint64

	// index is maintained by container/heap.
	index int
}

// taskHeap orders tasks by priority, then by enqueue order.
//
// A heap rather than a plain slice: the queue is popped from the head far more
// often than it is scanned, and a fleet pulling from it concurrently makes the
// O(log n) pop worth having over an O(n) scan for the highest priority.
type taskHeap []*queuedTask

func (h taskHeap) Len() int { return len(h) }

func (h taskHeap) Less(i, j int) bool {
	// Higher priority first.
	if h[i].task.Priority != h[j].task.Priority {
		return h[i].task.Priority > h[j].task.Priority
	}
	// Within a priority: first in, first out.
	return h[i].seq < h[j].seq
}

func (h taskHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *taskHeap) Push(x any) {
	qt := x.(*queuedTask)
	qt.index = len(*h)
	*h = append(*h, qt)
}

func (h *taskHeap) Pop() any {
	old := *h
	n := len(old)
	qt := old[n-1]
	// Clear the slot so the popped task is not kept alive by the backing array.
	old[n-1] = nil
	qt.index = -1
	*h = old[:n-1]
	return qt
}
