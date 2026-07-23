package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// This file is the Queue contract, written as tests.
//
// The interface exists so a single host can run an in-process queue and a fleet
// can run Redis, with no caller changing. That promise is only worth anything if
// both implementations actually behave the same, and "both implement the
// interface" does not establish that -- a Queue that loses tasks, runs one twice
// or lets a stale worker overwrite a result compiles perfectly. Every property
// the rest of the system leans on is checked here, once, against whatever is
// behind the interface.
//
// A new implementation is correct when it passes this suite. That is the whole
// definition, and it is why these tests take a factory rather than a type.

// testConfig is the subset of configuration the contract tests need to control.
// Implementations translate it into their own config.
type testConfig struct {
	DefaultMaxAttempts int
	ReapInterval       time.Duration
}

// queueFactory builds a fresh, empty queue. Implementations must isolate each
// call -- a shared Redis needs a unique key prefix per queue, or one test's
// leftovers become another's mystery failure.
type queueFactory func(t *testing.T, cfg testConfig) Queue

// runConformance runs the Queue contract against one implementation.
func runConformance(t *testing.T, newQueue queueFactory) {
	t.Helper()

	tests := []struct {
		name string
		run  func(t *testing.T, newQueue queueFactory)
	}{
		{"FIFO", testFIFO},
		{"FIFOHoldsAcrossDigitBoundaries", testFIFOAtScale},
		{"PriorityBeatsFIFOButTiesStayFIFO", testPriority},
		{"TaskGoesToExactlyOneWorker", testExactlyOnce},
		{"LeaseBlocksUntilWorkArrives", testLeaseBlocks},
		{"LeaseRespectsContextCancellation", testLeaseCancel},
		{"ExpiredLeaseReturnsTaskToTheQueue", testLeaseExpiry},
		{"StaleLeaseCannotComplete", testStaleComplete},
		{"ExtendKeepsALongTaskAlive", testExtend},
		{"ExtendFailsOnceReissued", testExtendReissued},
		{"FailRetriesUntilAttemptsRunOut", testRetries},
		{"CompleteStoresResult", testCompleteStores},
		{"StatsReportDepthAndWait", testStats},
		{"CloseUnblocksWaitingWorkers", testCloseUnblocks},
		{"EnqueueValidates", testValidation},
		{"EnqueueRejectsDuplicateWhilePending", testDuplicatePending},
		{"EnqueueRejectsDuplicateWhileRunning", testDuplicateRunning},
		{"IDIsReusableOnceTaskFinishes", testIDReuse},
		{"ResultIsAbsentUntilFinished", testNoResultYet},
		{"LeaseSkipsTasksThatDoNotFit", testLeaseResourceFit},
		{"CapacityBeatsPriorityWhenTaskDoesNotFit", testLeasePriorityYieldsToFit},
		{"ReservationHoldsCapacityForTheHead", testReservationHoldsCapacityForHead},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newQueue) })
	}
}

// --- helpers -----------------------------------------------------------------

func task(id string) Task {
	return Task{ID: id, Image: "python", Cmd: "python3", Args: []string{"-c", "print(1)"}}
}

func mustEnqueue(t *testing.T, q Queue, tasks ...Task) {
	t.Helper()
	for _, tk := range tasks {
		if err := q.Enqueue(context.Background(), tk); err != nil {
			t.Fatalf("enqueue %s: %v", tk.ID, err)
		}
	}
}

func mustLease(t *testing.T, q Queue, d time.Duration) *Lease {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := q.Lease(ctx, d, Lessee{Avail: Unbounded, Total: Unbounded})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	return l
}

// --- the contract ------------------------------------------------------------

// FIFO is the guarantee callers reason about, so it is the first thing to pin.
//
// The IDs are deliberately not in alphabetical order. With ids "a", "b", "c"
// this test passes against a queue that has no arrival ordering at all and
// merely sorts by ID -- it would be checking that the alphabet is in order, not
// that the queue is. Enqueueing against the alphabet makes the two orders
// disagree, so only the right one passes.
func testFIFO(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})
	mustEnqueue(t, q, task("charlie"), task("alpha"), task("bravo"))

	for _, want := range []string{"charlie", "alpha", "bravo"} {
		if got := mustLease(t, q, time.Minute).Task.ID; got != want {
			t.Fatalf("got %s, want %s: the queue is not FIFO", got, want)
		}
	}
}

// FIFO with three tasks proves almost nothing. An implementation that orders by
// a stringified sequence is perfectly FIFO for the first nine tasks and then
// puts task 10 ahead of task 9, because "10" sorts before "9" -- and an
// implementation that packs priority and sequence into one float is exact until
// the sequence outgrows the mantissa. Both survive a three-task test and fail
// in production, so the contract is checked across a digit boundary and deep
// enough that ordering has to be real rather than accidental.
func testFIFOAtScale(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})

	const n = 30
	want := make([]string, n)
	for i := 0; i < n; i++ {
		want[i] = fmt.Sprintf("task-%d", i)
		mustEnqueue(t, q, task(want[i]))
	}

	for i, expect := range want {
		got := mustLease(t, q, time.Minute).Task.ID
		if got != expect {
			t.Fatalf("position %d: got %s, want %s: FIFO order breaks down at scale",
				i, got, expect)
		}
	}
}

func testPriority(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})

	high := task("zulu")
	high.Priority = 10
	// The high-priority task goes in last and sorts last alphabetically, so only
	// priority can pull it forward. The two low tasks are likewise enqueued
	// against alphabetical order, so the tie-break has to be arrival.
	mustEnqueue(t, q, task("yankee"), task("xray"), high)

	if got := mustLease(t, q, time.Minute).Task.ID; got != "zulu" {
		t.Errorf("first = %s, want zulu: priority is not respected", got)
	}
	// Equal priorities must still come out in enqueue order.
	if got := mustLease(t, q, time.Minute).Task.ID; got != "yankee" {
		t.Errorf("second = %s, want yankee: FIFO broken within a priority band", got)
	}
	if got := mustLease(t, q, time.Minute).Task.ID; got != "xray" {
		t.Errorf("third = %s, want xray", got)
	}
}

// The property the whole fleet rests on: two workers running one task means
// billing twice and, worse, executing someone's side effects twice.
func testExactlyOnce(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})

	const tasks = 200
	for i := 0; i < tasks; i++ {
		mustEnqueue(t, q, task(fmt.Sprintf("t%d", i)))
	}

	var (
		mu   sync.Mutex
		seen = make(map[string]int)
		wg   sync.WaitGroup
	)
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				l, err := q.Lease(ctx, time.Minute, Lessee{Avail: Unbounded, Total: Unbounded})
				cancel()
				if err != nil {
					return // drained
				}
				mu.Lock()
				seen[l.Task.ID]++
				mu.Unlock()
				_ = q.Complete(context.Background(), l, Result{})
			}
		}()
	}
	wg.Wait()

	if len(seen) != tasks {
		t.Errorf("saw %d distinct tasks, want %d: the queue lost work", len(seen), tasks)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("task %s was leased %d times: two workers ran the same task", id, count)
		}
	}
}

// Lease must block rather than spin: a worker's loop is "ask for work, do it,
// ask again", and that only works if asking parks the worker.
func testLeaseBlocks(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})

	got := make(chan string, 1)
	go func() {
		l, err := q.Lease(context.Background(), time.Minute, Lessee{Avail: Unbounded, Total: Unbounded})
		if err != nil {
			return
		}
		got <- l.Task.ID
	}()

	select {
	case id := <-got:
		t.Fatalf("leased %q from an empty queue", id)
	case <-time.After(150 * time.Millisecond):
	}

	mustEnqueue(t, q, task("late"))

	select {
	case id := <-got:
		if id != "late" {
			t.Errorf("got %s, want late", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked worker was never woken by an enqueue")
	}
}

func testLeaseCancel(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if _, err := q.Lease(ctx, time.Minute, Lessee{Avail: Unbounded, Total: Unbounded}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

// The case leases exist for: a node dies holding work, and the work comes back
// rather than vanishing.
func testLeaseExpiry(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{ReapInterval: 20 * time.Millisecond})
	mustEnqueue(t, q, task("orphan"))

	// A worker takes it, then "dies": never completes, never extends.
	if first := mustLease(t, q, 100*time.Millisecond); first.Task.ID != "orphan" {
		t.Fatal("unexpected task")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	second, err := q.Lease(ctx, time.Minute, Lessee{Avail: Unbounded, Total: Unbounded})
	if err != nil {
		t.Fatalf("task never came back after its worker died: %v", err)
	}
	if second.Task.ID != "orphan" {
		t.Errorf("got %s, want orphan", second.Task.ID)
	}
	if second.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", second.Attempt)
	}
}

// A worker that stalls past its expiry must not complete a task that has since
// been given to someone else.
func testStaleComplete(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{ReapInterval: 20 * time.Millisecond})
	mustEnqueue(t, q, task("contested"))

	stale := mustLease(t, q, 80*time.Millisecond)

	fresh := mustLease(t, q, time.Minute) // takes over once the lease lapses
	if fresh.Task.ID != "contested" {
		t.Fatal("task was not reissued")
	}

	code := 0
	if err := q.Complete(context.Background(), stale, Result{ExitCode: &code}); err == nil {
		t.Error("a stale lease completed a task owned by another worker")
	}
	// The current owner's result is the one that counts.
	if err := q.Complete(context.Background(), fresh, Result{ExitCode: &code}); err != nil {
		t.Errorf("current owner could not complete: %v", err)
	}
}

func testExtend(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{ReapInterval: 20 * time.Millisecond})
	mustEnqueue(t, q, task("slow"))

	l := mustLease(t, q, 200*time.Millisecond)

	// A task that legitimately runs long keeps checking in.
	for i := 0; i < 5; i++ {
		time.Sleep(60 * time.Millisecond)
		if err := q.Extend(context.Background(), l, 200*time.Millisecond); err != nil {
			t.Fatalf("extend %d: a task that was still running lost its lease: %v", i, err)
		}
	}

	// It must still own the task, well past the original expiry.
	if err := q.Complete(context.Background(), l, Result{}); err != nil {
		t.Errorf("complete after extending: %v", err)
	}
}

func testExtendReissued(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{ReapInterval: 20 * time.Millisecond})
	mustEnqueue(t, q, task("lost"))

	l := mustLease(t, q, 60*time.Millisecond)
	_ = mustLease(t, q, time.Minute) // takes over after expiry

	// The old worker must be told to stop, not silently allowed to continue.
	if err := q.Extend(context.Background(), l, time.Minute); err == nil {
		t.Error("extended a lease that had been reassigned")
	}
}

func testRetries(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})

	tk := task("flaky")
	tk.MaxAttempts = 3
	mustEnqueue(t, q, tk)

	for attempt := 1; attempt <= 3; attempt++ {
		l := mustLease(t, q, time.Minute)
		if l.Attempt != attempt {
			t.Fatalf("attempt = %d, want %d", l.Attempt, attempt)
		}
		if err := q.Fail(context.Background(), l, "node exploded"); err != nil {
			t.Fatal(err)
		}
	}

	// Out of attempts: recorded as failed rather than cycling forever.
	res, ok, _ := q.Result(context.Background(), "flaky")
	if !ok {
		t.Fatal("no result for an exhausted task")
	}
	if res.Error == "" {
		t.Error("exhausted task has no error recorded")
	}
	if res.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", res.Attempts)
	}

	stats, _ := q.Stats(context.Background())
	if stats.Pending != 0 {
		t.Errorf("pending = %d, want 0: an exhausted task is still being retried", stats.Pending)
	}
}

func testCompleteStores(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})
	mustEnqueue(t, q, task("done"))

	l := mustLease(t, q, time.Minute)
	code := 7
	err := q.Complete(context.Background(), l, Result{
		ExitCode:   &code,
		Stdout:     []byte("hello"),
		Stderr:     []byte("warning"),
		ActiveCPU:  1500 * time.Millisecond,
		MemoryPeak: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, ok, _ := q.Result(context.Background(), "done")
	if !ok {
		t.Fatal("no result")
	}
	if res.ExitCode == nil || *res.ExitCode != 7 {
		t.Errorf("exit code = %v, want 7", res.ExitCode)
	}
	if string(res.Stdout) != "hello" {
		t.Errorf("stdout = %q, want hello", res.Stdout)
	}
	if string(res.Stderr) != "warning" {
		t.Errorf("stderr = %q, want warning", res.Stderr)
	}
	// The cost must survive: it is what a caller is billed on.
	if res.ActiveCPU != 1500*time.Millisecond {
		t.Errorf("active CPU = %v, want 1.5s", res.ActiveCPU)
	}
	if res.MemoryPeak != 64<<20 {
		t.Errorf("memory peak = %d, want %d", res.MemoryPeak, 64<<20)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", res.Attempts)
	}
}

func testStats(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})
	mustEnqueue(t, q, task("a"), task("b"), task("c"))

	time.Sleep(60 * time.Millisecond)
	l := mustLease(t, q, time.Minute)

	stats, err := q.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 2 {
		t.Errorf("pending = %d, want 2", stats.Pending)
	}
	if stats.Leased != 1 {
		t.Errorf("leased = %d, want 1", stats.Leased)
	}
	// The head's wait is what says whether the fleet is big enough.
	if stats.OldestPending < 40*time.Millisecond {
		t.Errorf("oldest pending = %v, want at least ~60ms", stats.OldestPending)
	}

	_ = q.Complete(context.Background(), l, Result{})
	if stats, _ = q.Stats(context.Background()); stats.Done != 1 {
		t.Errorf("done = %d, want 1", stats.Done)
	}
}

func testCloseUnblocks(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})

	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, err := q.Lease(context.Background(), time.Minute, Lessee{Avail: Unbounded, Total: Unbounded})
			errs <- err
		}()
	}
	time.Sleep(200 * time.Millisecond)

	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	// A worker blocked on an empty queue must not hang for the life of the
	// process when the daemon is shutting down.
	for i := 0; i < 4; i++ {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrClosed) {
				t.Errorf("err = %v, want ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close left a worker blocked in Lease")
		}
	}
}

func testValidation(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})

	for _, tc := range []struct {
		name string
		task Task
	}{
		{"no id", Task{Cmd: "echo"}},
		{"no cmd", Task{ID: "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := q.Enqueue(context.Background(), tc.task); err == nil {
				t.Error("accepted an invalid task")
			}
		})
	}
}

// A caller-supplied ID is an idempotency key, and pending is the window that
// matters: a client retries because its submission seemed to fail, which is
// most likely while the task is still waiting for a slot. Getting this wrong
// runs the caller's code twice.
func testDuplicatePending(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})
	mustEnqueue(t, q, task("dup"))

	err := q.Enqueue(context.Background(), task("dup"))
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate: a retried submission was queued twice", err)
	}

	if stats, _ := q.Stats(context.Background()); stats.Pending != 1 {
		t.Errorf("pending = %d, want 1: the same task is queued twice", stats.Pending)
	}
}

func testDuplicateRunning(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})
	mustEnqueue(t, q, task("dup"))
	_ = mustLease(t, q, time.Minute)

	// Re-enqueuing a running task would have two workers doing it at once, which
	// is what the lease exists to prevent.
	if err := q.Enqueue(context.Background(), task("dup")); !errors.Is(err, ErrDuplicate) {
		t.Errorf("err = %v, want ErrDuplicate", err)
	}
}

// The queue remembers what it is running, not everything it has ever run. Once
// a task finishes, its ID is free -- otherwise a caller using a natural key
// ("nightly-report") could only ever run it once.
func testIDReuse(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})
	mustEnqueue(t, q, task("recurring"))

	l := mustLease(t, q, time.Minute)
	if err := q.Complete(context.Background(), l, Result{}); err != nil {
		t.Fatal(err)
	}

	if err := q.Enqueue(context.Background(), task("recurring")); err != nil {
		t.Fatalf("could not re-run a finished task's ID: %v", err)
	}
}

func testNoResultYet(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})

	if _, ok, _ := q.Result(context.Background(), "never-submitted"); ok {
		t.Error("returned a result for a task that was never submitted")
	}

	mustEnqueue(t, q, task("waiting"))
	if _, ok, _ := q.Result(context.Background(), "waiting"); ok {
		t.Error("returned a result for a task that has not run")
	}
}

// leaseWithin leases with a bounded wait, so a test can assert both "leased X"
// and "drained (leased nothing)" -- the latter as a deadline, since a node
// holding capacity for the head is meant to wait rather than take a smaller task.
func leaseWithin(q Queue, lessee Lessee, timeout time.Duration) (*Lease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return q.Lease(ctx, time.Minute, lessee)
}

// smallNode can never run the big task; bigNode can, once it drains. Both start
// with room only for the small task.
func smallNode(id string) Lessee {
	return Lessee{ID: id, Avail: Resources{CPU: 4, MemMiB: 512}, Total: Resources{CPU: 4, MemMiB: 512}}
}

func bigNode(id string, availMem int) Lessee {
	return Lessee{ID: id, Avail: Resources{CPU: 8, MemMiB: availMem}, Total: Resources{CPU: 8, MemMiB: 16384}}
}

// testLeaseResourceFit: a node that could never run the head steps over it and
// runs the smaller task it can, rather than idling for a task it cannot help.
func testLeaseResourceFit(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})
	// The big task is enqueued first, so it is ahead of the small one in FIFO.
	mustEnqueue(t, q,
		Task{ID: "big", Image: "python", Cmd: "run", MemMiB: 8192},
		Task{ID: "small", Image: "python", Cmd: "run", MemMiB: 256},
	)

	// A node too small to ever run big backfills the small task, though big is
	// first in line -- reserving for a task it can never run would only waste it.
	l, err := leaseWithin(q, smallNode("tiny"), 5*time.Second)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if l.Task.ID != "small" {
		t.Fatalf("leased %q, want small (a 512 MiB node can never run big)", l.Task.ID)
	}

	// A node big enough picks up the big one, which was waiting, not lost.
	l2, err := leaseWithin(q, bigNode("large", 16384), 5*time.Second)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if l2.Task.ID != "big" {
		t.Fatalf("leased %q, want big", l2.Task.ID)
	}
}

// testLeasePriorityYieldsToFit: capacity beats priority for a node that cannot
// fit the higher-priority task -- it runs the lower one it can rather than idle.
func testLeasePriorityYieldsToFit(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})
	mustEnqueue(t, q,
		Task{ID: "high-big", Image: "python", Cmd: "run", MemMiB: 8192, Priority: 9},
		Task{ID: "low-small", Image: "python", Cmd: "run", MemMiB: 256, Priority: 1},
	)

	l, err := leaseWithin(q, smallNode("tiny"), 5*time.Second)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if l.Task.ID != "low-small" {
		t.Fatalf("leased %q, want low-small (a small node cannot run the priority-9 task)", l.Task.ID)
	}
}

// testReservationHoldsCapacityForHead is the whole point of reservation: a node
// that could run the big head but has no room now drains for it -- takes nothing
// -- while another node keeps working; then the drained node runs it. Without
// this the big task would be starved by the endless small ones behind it.
func testReservationHoldsCapacityForHead(t *testing.T, newQueue queueFactory) {
	q := newQueue(t, testConfig{})
	mustEnqueue(t, q,
		Task{ID: "big", Image: "python", Cmd: "run", MemMiB: 8192},
		Task{ID: "small", Image: "python", Cmd: "run", MemMiB: 256},
	)

	// Node A could run big but has no room now: it reserves big and waits, taking
	// nothing. The deadline is the assertion -- a reserving node blocks.
	if l, err := leaseWithin(q, bigNode("A", 512), 300*time.Millisecond); err == nil {
		t.Fatalf("A should have drained for the head, but leased %q", l.Task.ID)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("A err = %v, want DeadlineExceeded (draining)", err)
	}

	// Nothing was leased: A did not backfill the small task past the reserved head.
	if s, _ := q.Stats(context.Background()); s.Pending != 2 {
		t.Fatalf("pending = %d, want 2 (the reserving node must take nothing)", s.Pending)
	}

	// Node B could also run big, but A holds the reservation, so B keeps the fleet
	// busy with the small task instead of idling too.
	l, err := leaseWithin(q, bigNode("B", 512), 2*time.Second)
	if err != nil {
		t.Fatalf("B lease: %v", err)
	}
	if l.Task.ID != "small" {
		t.Fatalf("B leased %q, want small (backfill while A drains for big)", l.Task.ID)
	}
	_ = q.Complete(context.Background(), l, Result{})

	// A, now with room, runs the big task and releases the reservation.
	l2, err := leaseWithin(q, bigNode("A", 16384), 2*time.Second)
	if err != nil {
		t.Fatalf("A second lease: %v", err)
	}
	if l2.Task.ID != "big" {
		t.Fatalf("A leased %q, want big", l2.Task.ID)
	}
}
