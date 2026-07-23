package queue

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// MemoryQueue is a Queue held in one process.
//
// It is the right implementation for a single host, and the wrong one for a
// fleet: nothing survives a restart, and no other host can see it. It exists so
// the single-node case carries no operational dependency, and so the Queue
// contract -- ordering, leases, expiry, retries -- is exercised by real tests
// without standing up Redis. A distributed implementation swaps in behind the
// same interface.
type MemoryQueue struct {
	log             *slog.Logger
	defaultAttempts int

	mu      sync.Mutex
	pending taskHeap
	// pendingIDs indexes pending by task ID. The heap is ordered for popping,
	// not for lookup, and answering "is this ID already queued?" is a question
	// Enqueue asks on every call -- scanning the heap would make submitting the
	// n-th task cost O(n).
	pendingIDs map[string]struct{}
	// seq gives FIFO order within a priority band; see queuedTask.seq.
	seq     uint64
	leased  map[string]*leaseState
	results map[string]Result
	// closed unblocks every waiting Lease.
	closed bool
	// waiters are signalled when a task arrives or the queue closes.
	waiters []chan struct{}

	// The single head reservation: the task a node is draining for, who holds it,
	// and when the claim lapses. Empty resTask means no reservation. There is at
	// most one, always for the current head -- see Lease.
	resTask   string
	resOwner  string
	resExpiry time.Time

	// reaper stops when the queue closes.
	reaperDone chan struct{}
}

// leaseState is a task a worker currently holds.
type leaseState struct {
	task      Task
	attempt   int
	token     string
	expiresAt time.Time
}

// MemoryConfig configures a MemoryQueue.
type MemoryConfig struct {
	// DefaultMaxAttempts applies to tasks that do not set their own. Two means
	// one retry: enough to survive a node dying, few enough that a task which
	// reliably kills its worker cannot cycle the fleet forever.
	DefaultMaxAttempts int

	// ReapInterval is how often expired leases are checked for. It bounds how
	// long a dead worker's task sits unnoticed.
	ReapInterval time.Duration
}

// NewMemory returns an in-process queue.
func NewMemory(cfg MemoryConfig, log *slog.Logger) *MemoryQueue {
	if cfg.DefaultMaxAttempts <= 0 {
		cfg.DefaultMaxAttempts = 2
	}
	if cfg.ReapInterval <= 0 {
		cfg.ReapInterval = time.Second
	}

	q := &MemoryQueue{
		log:             log,
		defaultAttempts: cfg.DefaultMaxAttempts,
		pendingIDs:      make(map[string]struct{}),
		leased:          make(map[string]*leaseState),
		results:         make(map[string]Result),
		reaperDone:      make(chan struct{}),
	}
	heap.Init(&q.pending)

	go q.reapExpired(cfg.ReapInterval)
	return q
}

// Enqueue adds a task.
func (q *MemoryQueue) Enqueue(ctx context.Context, task Task) error {
	if err := validateTask(task); err != nil {
		return err
	}
	if task.MaxAttempts <= 0 {
		task.MaxAttempts = q.defaultAttempts
	}
	task.EnqueuedAt = time.Now()

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return ErrClosed
	}
	// Both states have to be checked, and pending is the one that matters most.
	// A client retries because its submission appeared to fail, which is most
	// likely while the task is still waiting for a slot -- checking only leased
	// would guard the narrow window and miss the wide one. Two entries with one
	// ID is not merely untidy: both get leased, leaseLocked overwrites the first
	// worker's token, and that worker's real result is then discarded as stale.
	if _, queued := q.pendingIDs[task.ID]; queued {
		return fmt.Errorf("%w: %s is waiting for a slot", ErrDuplicate, task.ID)
	}
	if _, running := q.leased[task.ID]; running {
		return fmt.Errorf("%w: %s is already running", ErrDuplicate, task.ID)
	}

	q.pushPendingLocked(&queuedTask{task: task, attempt: 0, seq: q.nextSeq()})
	return nil
}

// pushPendingLocked queues a task and wakes a worker, keeping the ID index in
// step with the heap. Every path that makes a task pending goes through here so
// the two cannot drift apart.
func (q *MemoryQueue) pushPendingLocked(qt *queuedTask) {
	heap.Push(&q.pending, qt)
	q.pendingIDs[qt.task.ID] = struct{}{}
	q.wakeAll()
}

// selectLocked applies the scheduling policy and returns the task to lease, or
// nil to wait.
//
// The head is what priority says should run next. If it fits the asker, it runs.
// If it does not, the choice is between reserving for it -- draining this node
// until it fits, which only makes sense if this node could ever run it -- and
// backfilling with a smaller task, which is right when someone else is already
// draining for the head or when this node could never run it anyway.
func (q *MemoryQueue) selectLocked(lessee Lessee) *queuedTask {
	head := q.headLocked()
	if head == nil {
		return nil // nothing pending
	}
	headCost := taskCost(head.task)

	if lessee.Avail.fits(headCost) {
		// Run the head. If this node was draining for it, the wait is over.
		if q.resTask == head.task.ID {
			q.clearReservationLocked()
		}
		return q.removeLocked(head)
	}

	// The head does not fit right now.
	reservedElsewhere := q.resTask == head.task.ID &&
		q.resOwner != lessee.ID &&
		time.Now().Before(q.resExpiry)
	if reservedElsewhere {
		return q.backfillLocked(lessee.Avail)
	}
	if lessee.Total.fits(headCost) {
		// This node could run the head once it drains: claim it and wait, so the
		// small tasks behind it do not keep it starved.
		q.resTask = head.task.ID
		q.resOwner = lessee.ID
		q.resExpiry = time.Now().Add(reservationTTL)
		return nil
	}
	// This node can never run the head; keep it busy with what it can run.
	return q.backfillLocked(lessee.Avail)
}

// headLocked returns the highest-priority pending task without removing it. The
// heap keeps its root at index 0, so the head is a peek, not a scan.
func (q *MemoryQueue) headLocked() *queuedTask {
	if q.pending.Len() == 0 {
		return nil
	}
	return q.pending[0]
}

// removeLocked takes a specific task out of the pending set.
func (q *MemoryQueue) removeLocked(qt *queuedTask) *queuedTask {
	heap.Remove(&q.pending, qt.index)
	delete(q.pendingIDs, qt.task.ID)
	return qt
}

// clearReservationLocked drops the head reservation.
func (q *MemoryQueue) clearReservationLocked() {
	q.resTask = ""
	q.resOwner = ""
	q.resExpiry = time.Time{}
}

// backfillLocked is popBestFitLocked under the name that says why: it is the
// smaller task a node runs when it is not the one draining for the head.
func (q *MemoryQueue) backfillLocked(avail Resources) *queuedTask {
	return q.popBestFitLocked(avail)
}

// popBestFitLocked removes and returns the highest-priority pending task that
// fits avail, or nil if none does.
//
// It scans rather than pops the head because "highest priority that fits" is not
// the head once a task can be too big for the caller: the head might not fit
// while a lower-priority task does. The scan is over the heap's backing slice --
// unordered, but every element is examined, and heap.Less picks the winner in
// exactly the priority-then-FIFO order the head would have used.
func (q *MemoryQueue) popBestFitLocked(avail Resources) *queuedTask {
	best := -1
	for i := range q.pending {
		if !avail.fits(taskCost(q.pending[i].task)) {
			continue
		}
		if best == -1 || q.pending.Less(i, best) {
			best = i
		}
	}
	if best == -1 {
		return nil
	}
	qt := heap.Remove(&q.pending, best).(*queuedTask)
	delete(q.pendingIDs, qt.task.ID)
	return qt
}

// seq is a monotonic counter giving FIFO order within a priority. Wall-clock
// timestamps are not enough: two tasks enqueued in the same nanosecond would
// order arbitrarily, and the guarantee callers rely on is that equal priorities
// come out in the order they went in.
func (q *MemoryQueue) nextSeq() uint64 {
	q.seq++
	return q.seq
}

// Lease blocks until a task is available.
func (q *MemoryQueue) Lease(ctx context.Context, leaseFor time.Duration, lessee Lessee) (*Lease, error) {
	if leaseFor <= 0 {
		return nil, errors.New("lease duration must be positive")
	}

	for {
		q.mu.Lock()

		if q.closed {
			q.mu.Unlock()
			return nil, ErrClosed
		}

		if qt := q.selectLocked(lessee); qt != nil {
			lease := q.leaseLocked(qt, leaseFor)
			q.mu.Unlock()
			return lease, nil
		}

		// Nothing to do: register for a wake-up and wait. Registering before
		// unlocking is what keeps a task enqueued in the gap from being missed.
		wake := make(chan struct{}, 1)
		q.waiters = append(q.waiters, wake)
		q.mu.Unlock()

		select {
		case <-wake:
			// Something arrived, or the queue closed. Loop and find out which;
			// another worker may have taken it first.
		case <-ctx.Done():
			q.removeWaiter(wake)
			return nil, ctx.Err()
		}
	}
}

func (q *MemoryQueue) leaseLocked(qt *queuedTask, leaseFor time.Duration) *Lease {
	token := newToken()
	attempt := qt.attempt + 1

	q.leased[qt.task.ID] = &leaseState{
		task:      qt.task,
		attempt:   attempt,
		token:     token,
		expiresAt: time.Now().Add(leaseFor),
	}

	return &Lease{
		Task:      qt.task,
		Attempt:   attempt,
		ExpiresAt: time.Now().Add(leaseFor),
		token:     token,
	}
}

// Extend pushes a lease's expiry out.
func (q *MemoryQueue) Extend(ctx context.Context, lease *Lease, by time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	st, ok := q.leased[lease.Task.ID]
	if !ok || st.token != lease.token {
		// The lease expired and the task went to somebody else. Say so plainly:
		// the worker must stop, because whatever it is doing is now duplicate
		// work that will be thrown away.
		return fmt.Errorf("lease on task %s is no longer valid; it was reassigned", lease.Task.ID)
	}

	st.expiresAt = time.Now().Add(by)
	lease.ExpiresAt = st.expiresAt
	return nil
}

// Complete records a result and releases the lease.
func (q *MemoryQueue) Complete(ctx context.Context, lease *Lease, result Result) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	st, ok := q.leased[lease.Task.ID]
	if !ok || st.token != lease.token {
		// A worker that stalled past its expiry finishing a task somebody else
		// now owns. Dropping its result is correct: the current owner's answer
		// is the one that counts.
		return fmt.Errorf("lease on task %s is no longer valid; its result was discarded", lease.Task.ID)
	}

	result.TaskID = lease.Task.ID
	result.Attempts = st.attempt
	q.results[lease.Task.ID] = result
	delete(q.leased, lease.Task.ID)
	return nil
}

// Fail releases a lease and retries the task if it has attempts left.
func (q *MemoryQueue) Fail(ctx context.Context, lease *Lease, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	st, ok := q.leased[lease.Task.ID]
	if !ok || st.token != lease.token {
		return fmt.Errorf("lease on task %s is no longer valid", lease.Task.ID)
	}
	delete(q.leased, lease.Task.ID)

	q.retryOrFailLocked(st, reason)
	return nil
}

// retryOrFailLocked re-queues a task or records it as failed for good.
func (q *MemoryQueue) retryOrFailLocked(st *leaseState, reason string) {
	if st.attempt >= st.task.MaxAttempts {
		q.results[st.task.ID] = Result{
			TaskID:     st.task.ID,
			Error:      fmt.Sprintf("failed after %d attempts: %s", st.attempt, reason),
			Attempts:   st.attempt,
			FinishedAt: time.Now(),
		}
		q.log.Warn("task failed for good", "task", st.task.ID, "attempts", st.attempt, "reason", reason)
		return
	}

	// Back to the front of its priority band: a retry has already waited once
	// and should not go behind everything enqueued since.
	q.pushPendingLocked(&queuedTask{task: st.task, attempt: st.attempt, seq: q.nextSeq()})
	q.log.Info("task requeued", "task", st.task.ID, "attempt", st.attempt+1, "reason", reason)
}

// reapExpired returns tasks whose workers stopped checking in.
//
// This is what makes a node dying survivable: its leases expire, its tasks go
// back to the queue, and another node picks them up. Nothing needs to detect
// the death or tell anyone about it.
func (q *MemoryQueue) reapExpired(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-q.reaperDone:
			return
		case <-ticker.C:
			q.mu.Lock()
			now := time.Now()
			for id, st := range q.leased {
				if now.Before(st.expiresAt) {
					continue
				}
				delete(q.leased, id)
				q.retryOrFailLocked(st, "worker stopped responding: lease expired")
			}
			q.mu.Unlock()
		}
	}
}

// Result returns a finished task's outcome. The error is always nil: a map in
// this process cannot fail to be read. It exists for implementations whose
// storage is across a network.
func (q *MemoryQueue) Result(ctx context.Context, taskID string) (Result, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, ok := q.results[taskID]
	return r, ok, nil
}

// Stats reports the queue's depth.
func (q *MemoryQueue) Stats(ctx context.Context) (Stats, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	s := Stats{
		Pending: q.pending.Len(),
		Leased:  len(q.leased),
	}
	for _, r := range q.results {
		if r.Error != "" {
			s.Failed++
			continue
		}
		s.Done++
	}

	// The head of the heap is the next task out, so its wait is the oldest that
	// matters -- the number that says whether the fleet is big enough.
	if q.pending.Len() > 0 {
		s.OldestPending = time.Since(q.pending[0].task.EnqueuedAt)
	}
	return s, nil
}

// Close shuts the queue down and unblocks every waiting Lease.
func (q *MemoryQueue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return nil
	}
	q.closed = true
	close(q.reaperDone)

	// Wake everyone: a blocked Lease must return ErrClosed rather than hang for
	// the life of the process.
	for _, w := range q.waiters {
		close(w)
	}
	q.waiters = nil
	return nil
}

// wakeAll signals every waiting worker.
//
// One task wakes all of them, not one, because leasing is now resource-aware: a
// woken worker whose free resources do not fit this task goes back to sleep
// without taking it, so waking a single worker could wake exactly the one that
// cannot run it while the one that can sleeps on. The cost is a thundering herd
// of at most the node's worker count, on a single-process queue -- cheap next to
// the alternative of a task stalling behind a worker that could never run it.
func (q *MemoryQueue) wakeAll() {
	for _, w := range q.waiters {
		close(w)
	}
	q.waiters = nil
}

func (q *MemoryQueue) removeWaiter(want chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, w := range q.waiters {
		if w == want {
			q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
			return
		}
	}
}

func newToken() string {
	var b [16]byte
	// crypto/rand cannot fail on any platform we run on, and a token collision
	// would silently let a stale worker complete somebody else's task.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
