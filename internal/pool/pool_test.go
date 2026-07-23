package pool

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/queue"
)

// These tests drive the pool against a fake queue rather than a real runtime.
// What is under test is the scheduling contract -- never exceed the slots,
// return work when a node dies, keep a long task's lease alive -- and none of
// that needs a VM. The real runtime is covered by the e2e suite.

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeQueue records what the pool does, and lets a test drive lease outcomes.
type fakeQueue struct {
	mu sync.Mutex

	pending  []queue.Task
	leased   map[string]bool
	results  map[string]queue.Result
	failed   map[string]string
	extends  atomic.Int64
	closed   bool
	waiters  []chan struct{}
	leaseSeq atomic.Int64
}

func newFakeQueue(tasks ...queue.Task) *fakeQueue {
	return &fakeQueue{
		pending: tasks,
		leased:  make(map[string]bool),
		results: make(map[string]queue.Result),
		failed:  make(map[string]string),
	}
}

func (f *fakeQueue) Enqueue(ctx context.Context, t queue.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, t)
	for _, w := range f.waiters {
		close(w)
	}
	f.waiters = nil
	return nil
}

func (f *fakeQueue) Lease(ctx context.Context, d time.Duration, lessee queue.Lessee) (*queue.Lease, error) {
	for {
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			return nil, queue.ErrClosed
		}
		// Resource-aware, like the real queue: return the first pending task that
		// fits the caller's free resources, stepping over any that do not. The
		// pool tests run with unbounded budgets, so reservation never triggers and
		// plain best-fit is a faithful stand-in.
		for i, t := range f.pending {
			cost := t.Cost()
			if cost.CPU <= lessee.Avail.CPU && cost.MemMiB <= lessee.Avail.MemMiB {
				f.pending = append(f.pending[:i], f.pending[i+1:]...)
				f.leased[t.ID] = true
				f.mu.Unlock()
				f.leaseSeq.Add(1)
				return &queue.Lease{Task: t, Attempt: 1, ExpiresAt: time.Now().Add(d)}, nil
			}
		}
		wake := make(chan struct{})
		f.waiters = append(f.waiters, wake)
		f.mu.Unlock()

		select {
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (f *fakeQueue) Extend(ctx context.Context, l *queue.Lease, d time.Duration) error {
	f.extends.Add(1)
	return nil
}

func (f *fakeQueue) Complete(ctx context.Context, l *queue.Lease, r queue.Result) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.leased, l.Task.ID)
	f.results[l.Task.ID] = r
	return nil
}

func (f *fakeQueue) Fail(ctx context.Context, l *queue.Lease, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.leased, l.Task.ID)
	f.failed[l.Task.ID] = reason
	return nil
}

func (f *fakeQueue) Result(ctx context.Context, id string) (queue.Result, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.results[id]
	return r, ok, nil
}

func (f *fakeQueue) Stats(ctx context.Context) (queue.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return queue.Stats{Pending: len(f.pending), Leased: len(f.leased)}, nil
}

func (f *fakeQueue) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for _, w := range f.waiters {
		close(w)
	}
	f.waiters = nil
	return nil
}

func (f *fakeQueue) failedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.failed)
}

// resultCount reads the results under the lock. Reading the map directly races
// with the workers still writing to it.
func (f *fakeQueue) resultCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.results)
}

func TestConfigRejectsHeartbeatAtOrPastExpiry(t *testing.T) {
	// A heartbeat that fires at or after the lease expires means every long task
	// is handed to a second node. Catching it at construction is the difference
	// between a config error and a fleet quietly doing everything twice.
	tests := []struct {
		name string
		cfg  Config
	}{
		{"heartbeat equals lease", Config{Slots: 1, LeaseDuration: time.Second, HeartbeatInterval: time.Second}},
		{"heartbeat past lease", Config{Slots: 1, LeaseDuration: time.Second, HeartbeatInterval: 2 * time.Second}},
		{"no slots", Config{Slots: 0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg, newFakeQueue(), nil, testLogger()); err == nil {
				t.Error("accepted a config that would duplicate work")
			}
		})
	}
}

func TestConfigDefaultsAreCoherent(t *testing.T) {
	cfg := Config{Slots: 4}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	if cfg.HeartbeatInterval >= cfg.LeaseDuration {
		t.Errorf("default heartbeat %v is not under the default lease %v",
			cfg.HeartbeatInterval, cfg.LeaseDuration)
	}
}

// The pool must never run more tasks at once than it has slots. This is the
// promise the whole capacity model rests on: exceed it and the node is
// oversubscribed in memory, which on a shared host means someone else's OOM.
func TestNeverExceedsItsSlots(t *testing.T) {
	const slots = 3
	const tasks = 30

	var (
		concurrent atomic.Int64
		maxSeen    atomic.Int64
		done       sync.WaitGroup
	)
	done.Add(tasks)

	// A stub that stands in for "create a VM and run the task", counting how
	// many are in flight.
	runner := func() {
		cur := concurrent.Add(1)
		for {
			old := maxSeen.Load()
			if cur <= old || maxSeen.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		concurrent.Add(-1)
		done.Done()
	}

	q := newFakeQueue()
	for i := 0; i < tasks; i++ {
		_ = q.Enqueue(context.Background(), queue.Task{ID: fmt.Sprintf("t%d", i), Cmd: "echo"})
	}

	p, err := New(Config{Slots: slots, LeaseDuration: 10 * time.Second}, q, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// Swap in the stub: what is under test is the slot discipline, not the VM.
	p.executeFn = func(ctx context.Context, task queue.Task, started time.Time) (queue.Result, error) {
		runner()
		code := 0
		return queue.Result{TaskID: task.ID, ExitCode: &code}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	waitOrFail(t, &done, 15*time.Second)

	if got := maxSeen.Load(); got > slots {
		t.Errorf("ran %d tasks at once against %d slots: the node is oversubscribed", got, slots)
	}
	if got := maxSeen.Load(); got < slots {
		t.Errorf("peaked at %d concurrent tasks with %d slots: the pool is not using its capacity", got, slots)
	}
}

// Work must actually spread across the slots rather than one slot doing it all.
func TestWorkSpreadsAcrossSlots(t *testing.T) {
	q := newFakeQueue()
	for i := 0; i < 40; i++ {
		_ = q.Enqueue(context.Background(), queue.Task{ID: fmt.Sprintf("t%d", i), Cmd: "echo"})
	}

	var done sync.WaitGroup
	done.Add(40)

	p, _ := New(Config{Slots: 4, LeaseDuration: 10 * time.Second}, q, nil, testLogger())
	p.executeFn = func(ctx context.Context, task queue.Task, started time.Time) (queue.Result, error) {
		time.Sleep(10 * time.Millisecond)
		done.Done()
		code := 0
		return queue.Result{TaskID: task.ID, ExitCode: &code}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	waitOrFail(t, &done, 15*time.Second)

	// The stub signals done *inside* executeFn, but Complete runs after it
	// returns -- so the last result may not have landed yet. Poll rather than
	// assert immediately, or this passes or fails on timing.
	deadline := time.Now().Add(5 * time.Second)
	for q.resultCount() < 40 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := q.resultCount(); got != 40 {
		t.Errorf("completed %d tasks, want 40", got)
	}
}

// An infrastructure failure must fail the lease so another node retries. A
// non-zero exit must not: that is the caller's code, and re-running it would
// bill them twice for the same answer.
func TestInfraFailureRetriesButBadExitDoesNot(t *testing.T) {
	t.Run("infra failure fails the lease", func(t *testing.T) {
		q := newFakeQueue(queue.Task{ID: "broken", Cmd: "echo"})
		p, _ := New(Config{Slots: 1, LeaseDuration: 10 * time.Second}, q, nil, testLogger())

		var done sync.WaitGroup
		done.Add(1)
		var once sync.Once
		p.executeFn = func(ctx context.Context, task queue.Task, started time.Time) (queue.Result, error) {
			once.Do(done.Done)
			return queue.Result{}, fmt.Errorf("no sandbox could be created")
		}

		ctx, cancel := context.WithCancel(context.Background())
		p.Start(ctx)
		waitOrFail(t, &done, 5*time.Second)
		time.Sleep(100 * time.Millisecond)
		cancel()
		p.Stop()

		if q.failedCount() == 0 {
			t.Error("an infrastructure failure did not fail the lease: the task would never be retried")
		}
	})

	t.Run("non-zero exit completes", func(t *testing.T) {
		q := newFakeQueue(queue.Task{ID: "badcode", Cmd: "echo"})
		p, _ := New(Config{Slots: 1, LeaseDuration: 10 * time.Second}, q, nil, testLogger())

		var done sync.WaitGroup
		done.Add(1)
		var once sync.Once
		p.executeFn = func(ctx context.Context, task queue.Task, started time.Time) (queue.Result, error) {
			once.Do(done.Done)
			code := 1
			return queue.Result{TaskID: task.ID, ExitCode: &code, Stderr: []byte("SyntaxError")}, nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		p.Start(ctx)
		waitOrFail(t, &done, 5*time.Second)
		time.Sleep(100 * time.Millisecond)
		cancel()
		p.Stop()

		if q.failedCount() != 0 {
			t.Error("a non-zero exit was retried: the caller's broken code would be billed twice")
		}
		res, ok, _ := q.Result(context.Background(), "badcode")
		if !ok {
			t.Fatal("no result recorded for a task that ran")
		}
		if res.ExitCode == nil || *res.ExitCode != 1 {
			t.Errorf("exit code = %v, want 1", res.ExitCode)
		}
	})
}

// A task that legitimately runs long must keep its lease alive.
func TestLongTaskHeartbeatsItsLease(t *testing.T) {
	q := newFakeQueue(queue.Task{ID: "slow", Cmd: "sleep"})
	p, _ := New(Config{
		Slots:             1,
		LeaseDuration:     300 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
	}, q, nil, testLogger())

	var done sync.WaitGroup
	done.Add(1)
	var once sync.Once
	p.executeFn = func(ctx context.Context, task queue.Task, started time.Time) (queue.Result, error) {
		time.Sleep(400 * time.Millisecond) // outlives the lease without extensions
		once.Do(done.Done)
		code := 0
		return queue.Result{TaskID: task.ID, ExitCode: &code}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	waitOrFail(t, &done, 5*time.Second)
	time.Sleep(100 * time.Millisecond)
	cancel()
	p.Stop()

	// Without heartbeats the queue would have reclaimed this task mid-run.
	if got := q.extends.Load(); got < 3 {
		t.Errorf("extended the lease %d times over a 400ms task with a 50ms heartbeat: too few", got)
	}
}

func TestStopWaitsForInFlightWork(t *testing.T) {
	q := newFakeQueue(queue.Task{ID: "inflight", Cmd: "echo"})
	p, _ := New(Config{Slots: 1, LeaseDuration: 10 * time.Second}, q, nil, testLogger())

	started := make(chan struct{})
	var finished atomic.Bool
	var once sync.Once
	p.executeFn = func(ctx context.Context, task queue.Task, s time.Time) (queue.Result, error) {
		once.Do(func() { close(started) })
		time.Sleep(200 * time.Millisecond)
		finished.Store(true)
		code := 0
		return queue.Result{TaskID: task.ID, ExitCode: &code}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	<-started
	cancel()
	p.Stop()

	// Stop must not abandon a task mid-flight: the sandbox would be orphaned and
	// the result lost.
	if !finished.Load() {
		t.Error("Stop returned while a task was still running")
	}
}

func TestBusyTracksRunningSlots(t *testing.T) {
	q := newFakeQueue(queue.Task{ID: "t1", Cmd: "echo"})
	p, _ := New(Config{Slots: 2, LeaseDuration: 10 * time.Second}, q, nil, testLogger())

	running := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p.executeFn = func(ctx context.Context, task queue.Task, s time.Time) (queue.Result, error) {
		once.Do(func() { close(running) })
		<-release
		code := 0
		return queue.Result{TaskID: task.ID, ExitCode: &code}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	<-running
	if got := p.Busy(); got != 1 {
		t.Errorf("busy = %d, want 1", got)
	}
	close(release)
	p.Stop()
}

func waitOrFail(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("timed out waiting for tasks to finish")
	}
}

// TestBinPacksByMemory proves the resource budget, not just the slot count,
// caps concurrency. A node with ten VM slots but only 1 GiB of schedulable
// memory runs two 512 MiB tasks at once and no more -- the third waits for one
// to finish and free its half, exactly as a full node should behave rather than
// oversubscribing its RAM into someone else's OOM.
func TestBinPacksByMemory(t *testing.T) {
	const tasks = 12

	var (
		concurrent atomic.Int64
		maxSeen    atomic.Int64
		done       sync.WaitGroup
	)
	done.Add(tasks)

	runner := func() {
		cur := concurrent.Add(1)
		for {
			old := maxSeen.Load()
			if cur <= old || maxSeen.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		concurrent.Add(-1)
		done.Done()
	}

	q := newFakeQueue()
	for i := 0; i < tasks; i++ {
		_ = q.Enqueue(context.Background(), queue.Task{
			ID: fmt.Sprintf("t%d", i), Cmd: "echo", MemMiB: 512,
		})
	}

	// Ten slots, but only enough memory for two 512 MiB tasks.
	p, err := New(Config{Slots: 10, MemMiB: 1024, LeaseDuration: 10 * time.Second}, q, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	p.executeFn = func(ctx context.Context, task queue.Task, started time.Time) (queue.Result, error) {
		runner()
		code := 0
		return queue.Result{TaskID: task.ID, ExitCode: &code}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	waitOrFail(t, &done, 15*time.Second)

	if got := maxSeen.Load(); got != 2 {
		t.Errorf("peaked at %d concurrent tasks, want 2: memory budget of 1 GiB / 512 MiB each = 2", got)
	}
}
