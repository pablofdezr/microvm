package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisQueue is a Queue shared by every node in a fleet.
//
// It is the same contract as MemoryQueue -- the conformance suite is the proof
// -- with the two properties a fleet needs and a process-local map cannot have:
// the queue survives a node restart, and every node sees the same work.
//
// # Why Lua for everything
//
// Every operation here is read-then-write: lease reads the head and marks it
// leased, complete reads the token and then writes a result. Between the read
// and the write, another node is running the same code. Done as separate
// commands, two nodes lease the same task -- the exact failure the lease exists
// to prevent, reintroduced at a lower layer. Redis runs a script to completion
// before serving anyone else, so a script is the only transaction available and
// every compound operation is one.
//
// # Why one Redis node, deliberately
//
// Keys are wrapped in a hash tag -- "{microvm}:pending" -- so Redis Cluster maps
// every key to a single slot. That looks like giving up sharding, but a queue
// with a global order cannot be sharded: "the next task" is a question about all
// tasks at once, and answering it from shards that do not see each other means
// answering it wrong. The hash tag makes the scripts legal under Cluster instead
// of failing at runtime with CROSSSLOT, and the queue is not the bottleneck --
// a slot takes seconds of VM time to serve, so even a modest Redis outpaces
// hundreds of nodes.
type RedisQueue struct {
	rdb *redis.Client
	log *slog.Logger

	// ns is the hash-tagged key namespace, e.g. "{microvm}". Lua builds
	// per-task keys from it, so it is passed to every script.
	ns string

	defaultAttempts int
	resultTTL       time.Duration
	doorbellTimeout time.Duration

	// closeCtx is cancelled by Close, which is how a worker parked in BLPOP is
	// woken rather than left blocking until its doorbell times out.
	closeCtx  context.Context
	closeStop context.CancelFunc
	closeOnce sync.Once

	reaperDone chan struct{}
}

// RedisConfig configures a RedisQueue.
type RedisConfig struct {
	// Addr is "host:port" or a "redis://" / "rediss://" URL.
	Addr     string
	Password string
	DB       int

	// Prefix namespaces the keys, so one Redis can serve several fleets.
	Prefix string

	// DefaultMaxAttempts applies to tasks that do not set their own.
	DefaultMaxAttempts int

	// ReapInterval is how often this node looks for expired leases. Every node
	// reaps; the script makes that safe.
	ReapInterval time.Duration

	// ResultTTL is how long a finished task's result is readable. Unlike the
	// in-memory queue, which leaks results for the life of the process, this
	// one has to forget: a shared Redis is not the place to keep every result
	// ever produced.
	ResultTTL time.Duration

	// DoorbellTimeout bounds how long a waiting worker blocks before it
	// re-checks the queue itself. See the doorbell comment on Lease.
	DoorbellTimeout time.Duration
}

func (c *RedisConfig) applyDefaults() {
	if c.Prefix == "" {
		c.Prefix = "microvm"
	}
	if c.DefaultMaxAttempts <= 0 {
		c.DefaultMaxAttempts = 2
	}
	if c.ReapInterval <= 0 {
		c.ReapInterval = time.Second
	}
	if c.ResultTTL <= 0 {
		c.ResultTTL = 24 * time.Hour
	}
	if c.DoorbellTimeout <= 0 {
		c.DoorbellTimeout = time.Second
	}
}

// NewRedis connects to Redis and returns a queue backed by it.
//
// It pings before returning: a queue that cannot reach its backend should fail
// at startup, where an operator sees it, rather than at the first submission.
func NewRedis(ctx context.Context, cfg RedisConfig, log *slog.Logger) (*RedisQueue, error) {
	cfg.applyDefaults()

	var opts *redis.Options
	if strings.Contains(cfg.Addr, "://") {
		parsed, err := redis.ParseURL(cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("parse redis URL: %w", err)
		}
		opts = parsed
	} else {
		opts = &redis.Options{Addr: cfg.Addr, Password: cfg.Password, DB: cfg.DB}
	}

	// A blocked worker holds its connection for the whole doorbell wait, so the
	// pool must have room for every slot to wait at once plus the reaper and the
	// ordinary traffic. The default (10 per CPU) is usually enough, but a node
	// with many slots and few cores would otherwise deadlock on connections.
	opts.ReadTimeout = cfg.DoorbellTimeout + 5*time.Second

	rdb := redis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("cannot reach redis at %s: %w", cfg.Addr, err)
	}

	closeCtx, closeStop := context.WithCancel(context.Background())
	q := &RedisQueue{
		rdb:             rdb,
		log:             log,
		ns:              "{" + cfg.Prefix + "}",
		defaultAttempts: cfg.DefaultMaxAttempts,
		resultTTL:       cfg.ResultTTL,
		doorbellTimeout: cfg.DoorbellTimeout,
		closeCtx:        closeCtx,
		closeStop:       closeStop,
		reaperDone:      make(chan struct{}),
	}

	go q.reapExpired(cfg.ReapInterval)
	return q, nil
}

// Key names. All are hash-tagged through ns so Cluster keeps them together.
func (q *RedisQueue) key(name string) string { return q.ns + ":" + name }

func (q *RedisQueue) pendingKey() string         { return q.key("pending") }
func (q *RedisQueue) leasedKey() string          { return q.key("leased") }
func (q *RedisQueue) seqKey() string             { return q.key("seq") }
func (q *RedisQueue) notifyKey() string          { return q.key("notify") }
func (q *RedisQueue) doneKey() string            { return q.key("done") }
func (q *RedisQueue) failedKey() string          { return q.key("failed") }
func (q *RedisQueue) reservationKey() string     { return q.key("reservation") }
func (q *RedisQueue) resultKey(id string) string { return q.key("result:" + id) }

// doorbellCap bounds the wake-up list. It is a doorbell, not a second copy of
// the queue: the sorted set is the truth and every worker re-checks it on every
// pass, so a dropped ring only ever costs one doorbell timeout of latency. An
// uncapped list in a shared Redis, by contrast, grows without limit.
const doorbellCap = 1024

// reapLimit bounds one reaping pass. A script blocks Redis while it runs, so a
// backlog of ten thousand expired leases is reclaimed over several passes
// rather than in one that stalls every other node.
const reapLimit = 128

// storedResult is the result's wire form in Redis.
//
// It exists rather than serialising Result directly because Lua also writes
// results -- a task that runs out of attempts is failed inside the script that
// discovers it -- and Lua cannot construct a Go type. Durations become integer
// milliseconds and times become Unix milliseconds so that both writers can
// produce, and both readers accept, the same JSON.
type storedResult struct {
	ExitCode    *int   `json:"exit_code,omitempty"`
	Stdout      []byte `json:"stdout,omitempty"`
	Stderr      []byte `json:"stderr,omitempty"`
	Error       string `json:"error,omitempty"`
	Attempts    int    `json:"attempts"`
	StartedAt   int64  `json:"started_at,omitempty"`
	FinishedAt  int64  `json:"finished_at,omitempty"`
	ActiveCPUMS int64  `json:"active_cpu_ms,omitempty"`
	MemoryPeak  uint64 `json:"memory_peak,omitempty"`
}

func newStoredResult(r Result) storedResult {
	sr := storedResult{
		ExitCode:    r.ExitCode,
		Stdout:      r.Stdout,
		Stderr:      r.Stderr,
		Error:       r.Error,
		Attempts:    r.Attempts,
		ActiveCPUMS: r.ActiveCPU.Milliseconds(),
		MemoryPeak:  r.MemoryPeak,
	}
	if !r.StartedAt.IsZero() {
		sr.StartedAt = r.StartedAt.UnixMilli()
	}
	if !r.FinishedAt.IsZero() {
		sr.FinishedAt = r.FinishedAt.UnixMilli()
	}
	return sr
}

func (sr storedResult) toResult(taskID string) Result {
	r := Result{
		TaskID:     taskID,
		ExitCode:   sr.ExitCode,
		Stdout:     sr.Stdout,
		Stderr:     sr.Stderr,
		Error:      sr.Error,
		Attempts:   sr.Attempts,
		ActiveCPU:  time.Duration(sr.ActiveCPUMS) * time.Millisecond,
		MemoryPeak: sr.MemoryPeak,
	}
	if sr.StartedAt != 0 {
		r.StartedAt = time.UnixMilli(sr.StartedAt)
	}
	if sr.FinishedAt != 0 {
		r.FinishedAt = time.UnixMilli(sr.FinishedAt)
	}
	return r
}

// Enqueue adds a task.
func (q *RedisQueue) Enqueue(ctx context.Context, task Task) error {
	if err := validateTask(task); err != nil {
		return err
	}
	if q.isClosed() {
		return ErrClosed
	}
	if task.MaxAttempts <= 0 {
		task.MaxAttempts = q.defaultAttempts
	}
	task.EnqueuedAt = time.Now()

	payload, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("marshal task %s: %w", task.ID, err)
	}

	cost := taskCost(task)
	res, err := scriptEnqueue.Run(ctx, q.rdb,
		[]string{q.pendingKey(), q.seqKey(), q.notifyKey()},
		q.ns, task.ID, payload, task.Priority, task.MaxAttempts,
		task.EnqueuedAt.UnixMilli(), doorbellCap, cost.CPU, cost.MemMiB,
	).Text()
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", task.ID, err)
	}
	if res == "DUP" {
		return fmt.Errorf("%w: %s", ErrDuplicate, task.ID)
	}
	return nil
}

// Lease blocks until a task is available, then hands it to exactly one caller.
//
// The wait is a doorbell, not a delivery. Redis has no way to block on "a
// sorted set became non-empty", so an enqueue also pushes to a list that
// workers BLPOP. The list is only a hint: a lease that the reaper returns to
// the queue rings no doorbell, and two workers can wake for one task. So the
// loop never trusts the doorbell -- it re-reads the sorted set on every pass,
// and treats a timed-out wait exactly like a signalled one. The sorted set is
// the truth; the doorbell only decides whether a worker learns of work in a
// millisecond or in a doorbell timeout.
func (q *RedisQueue) Lease(ctx context.Context, leaseFor time.Duration, lessee Lessee) (*Lease, error) {
	if leaseFor <= 0 {
		return nil, errors.New("lease duration must be positive")
	}

	for {
		if q.isClosed() {
			return nil, ErrClosed
		}

		lease, err := q.tryLease(ctx, leaseFor, lessee)
		if err != nil {
			return nil, err
		}
		if lease != nil {
			return lease, nil
		}

		// Nothing fit. The doorbell has a timeout, so even if the wake for a
		// fitting task went to a node it did not fit (and was consumed there),
		// this node re-scans within that timeout rather than sleeping on it.
		if err := q.waitDoorbell(ctx); err != nil {
			return nil, err
		}
	}
}

// leaseScanLimit caps how many pending tasks one lease call inspects for a fit.
// It bounds the Lua script's work so a huge backlog cannot make a single lease
// walk the whole set; a task past the cap simply waits for the front to drain.
const leaseScanLimit = 1000

// tryLease takes the head of the queue if there is one. A nil lease and a nil
// error mean the queue is empty.
func (q *RedisQueue) tryLease(ctx context.Context, leaseFor time.Duration, lessee Lessee) (*Lease, error) {
	token := newToken()
	expiresAt := time.Now().Add(leaseFor)
	now := time.Now()

	res, err := scriptLease.Run(ctx, q.rdb,
		[]string{q.pendingKey(), q.leasedKey(), q.reservationKey()},
		q.ns, expiresAt.UnixMilli(), token,
		lessee.Avail.CPU, lessee.Avail.MemMiB,
		lessee.Total.CPU, lessee.Total.MemMiB,
		lessee.ID, now.UnixMilli(), reservationTTL.Milliseconds(),
		leaseScanLimit,
	).Slice()
	if errors.Is(err, redis.Nil) {
		return nil, nil // empty
	}
	if err != nil {
		return nil, fmt.Errorf("lease: %w", err)
	}
	if len(res) != 2 {
		return nil, fmt.Errorf("lease: script returned %d values, want 2", len(res))
	}

	payload, ok := res[0].(string)
	if !ok {
		return nil, fmt.Errorf("lease: payload is %T, want string", res[0])
	}
	var task Task
	if err := json.Unmarshal([]byte(payload), &task); err != nil {
		return nil, fmt.Errorf("unmarshal leased task: %w", err)
	}

	attempt, err := parseLuaInt(res[1])
	if err != nil {
		return nil, fmt.Errorf("lease: attempt: %w", err)
	}

	return &Lease{
		Task:      task,
		Attempt:   int(attempt),
		ExpiresAt: expiresAt,
		token:     token,
	}, nil
}

// waitDoorbell parks until work may exist, the caller gives up, or Close fires.
func (q *RedisQueue) waitDoorbell(callerCtx context.Context) error {
	// Close must wake a parked worker, so the BLPOP watches both contexts. The
	// caller's own context is kept separate to report its error faithfully: a
	// caller that set a deadline wants DeadlineExceeded, not the Canceled that
	// the merged context would carry.
	ctx, cancel := context.WithCancel(callerCtx)
	defer cancel()
	stop := context.AfterFunc(q.closeCtx, cancel)
	defer stop()

	err := q.rdb.BLPop(ctx, q.doorbellTimeout, q.notifyKey()).Err()
	switch {
	case err == nil, errors.Is(err, redis.Nil):
		// Signalled, or the doorbell timed out. Both mean the same thing: go
		// look at the sorted set.
		return nil
	case q.isClosed():
		return ErrClosed
	case callerCtx.Err() != nil:
		return callerCtx.Err()
	default:
		return fmt.Errorf("waiting for work: %w", err)
	}
}

// Extend pushes a lease's expiry out.
func (q *RedisQueue) Extend(ctx context.Context, lease *Lease, by time.Duration) error {
	expiresAt := time.Now().Add(by)

	ok, err := scriptExtend.Run(ctx, q.rdb,
		[]string{q.leasedKey()},
		q.ns, lease.Task.ID, lease.token, expiresAt.UnixMilli(),
	).Int()
	if err != nil {
		return fmt.Errorf("extend lease on %s: %w", lease.Task.ID, err)
	}
	if ok == 0 {
		// The lease lapsed and the task went to somebody else. Say so plainly:
		// the worker must stop, because whatever it is doing is now duplicate
		// work that will be thrown away.
		return fmt.Errorf("lease on task %s is no longer valid; it was reassigned", lease.Task.ID)
	}
	lease.ExpiresAt = expiresAt
	return nil
}

// Complete records a result and releases the lease.
func (q *RedisQueue) Complete(ctx context.Context, lease *Lease, result Result) error {
	result.TaskID = lease.Task.ID
	result.Attempts = lease.Attempt

	payload, err := json.Marshal(newStoredResult(result))
	if err != nil {
		return fmt.Errorf("marshal result for %s: %w", lease.Task.ID, err)
	}

	isErr := "0"
	if result.Error != "" {
		isErr = "1"
	}

	ok, err := scriptComplete.Run(ctx, q.rdb,
		[]string{q.leasedKey(), q.doneKey(), q.failedKey()},
		q.ns, lease.Task.ID, lease.token, payload,
		int(q.resultTTL.Seconds()), isErr,
	).Int()
	if err != nil {
		return fmt.Errorf("complete %s: %w", lease.Task.ID, err)
	}
	if ok == 0 {
		// A worker that stalled past its expiry finishing a task somebody else
		// now owns. Dropping its result is correct: the current owner's answer
		// is the one that counts.
		return fmt.Errorf("lease on task %s is no longer valid; its result was discarded", lease.Task.ID)
	}
	return nil
}

// Fail releases a lease and retries the task if it has attempts left.
func (q *RedisQueue) Fail(ctx context.Context, lease *Lease, reason string) error {
	outcome, err := scriptFail.Run(ctx, q.rdb,
		[]string{q.pendingKey(), q.leasedKey(), q.seqKey(), q.notifyKey(), q.failedKey()},
		q.ns, lease.Task.ID, lease.token, reason,
		time.Now().UnixMilli(), int(q.resultTTL.Seconds()), doorbellCap,
	).Int()
	if err != nil {
		return fmt.Errorf("fail %s: %w", lease.Task.ID, err)
	}

	switch outcome {
	case outcomeStale:
		return fmt.Errorf("lease on task %s is no longer valid", lease.Task.ID)
	case outcomeRequeued:
		q.log.Info("task requeued", "task", lease.Task.ID, "attempt", lease.Attempt+1, "reason", reason)
	case outcomeFailedForGood:
		q.log.Warn("task failed for good", "task", lease.Task.ID, "attempts", lease.Attempt, "reason", reason)
	}
	return nil
}

// Outcomes returned by the release scripts.
const (
	outcomeStale         = -1 // the token did not match; someone else owns it
	outcomeUnknown       = 0  // no such task
	outcomeRequeued      = 1
	outcomeFailedForGood = 2
)

// reapExpired returns tasks whose workers stopped checking in.
//
// Every node runs this, concurrently, against the same keys -- which is safe
// only because the work happens inside a script. Two nodes reaping the same
// expired lease is not a race but a no-op for the loser: the first script to
// run requeues the task and clears the lease, and the second finds nothing
// expired. That is why there is no leader here, and nothing to elect.
func (q *RedisQueue) reapExpired(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-q.closeCtx.Done():
			close(q.reaperDone)
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(q.closeCtx, 10*time.Second)
			n, err := scriptReap.Run(ctx, q.rdb,
				[]string{q.pendingKey(), q.leasedKey(), q.seqKey(), q.notifyKey(), q.failedKey()},
				q.ns, time.Now().UnixMilli(), int(q.resultTTL.Seconds()), doorbellCap, reapLimit,
			).Int()
			cancel()

			if err != nil {
				if q.isClosed() {
					continue
				}
				q.log.Warn("could not reap expired leases", "err", err)
				continue
			}
			if n > 0 {
				q.log.Info("reclaimed tasks from workers that stopped responding", "count", n)
			}
		}
	}
}

// Result returns a finished task's outcome.
func (q *RedisQueue) Result(ctx context.Context, taskID string) (Result, bool, error) {
	raw, err := q.rdb.Get(ctx, q.resultKey(taskID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("read result for %s: %w", taskID, err)
	}

	var sr storedResult
	if err := json.Unmarshal(raw, &sr); err != nil {
		return Result{}, false, fmt.Errorf("unmarshal result for %s: %w", taskID, err)
	}
	return sr.toResult(taskID), true, nil
}

// Stats reports the queue's depth.
//
// Done and Failed are lifetime counters, not a count of readable results:
// results expire, and a fleet's "how much have we run" should not drop to zero
// because yesterday's results aged out.
func (q *RedisQueue) Stats(ctx context.Context) (Stats, error) {
	res, err := scriptStats.Run(ctx, q.rdb,
		[]string{q.pendingKey(), q.leasedKey(), q.doneKey(), q.failedKey()},
		q.ns,
	).Slice()
	if err != nil {
		return Stats{}, fmt.Errorf("read queue stats: %w", err)
	}
	if len(res) != 5 {
		return Stats{}, fmt.Errorf("stats: script returned %d values, want 5", len(res))
	}

	nums := make([]int64, 5)
	for i, v := range res {
		n, err := parseLuaInt(v)
		if err != nil {
			return Stats{}, fmt.Errorf("stats field %d: %w", i, err)
		}
		nums[i] = n
	}

	s := Stats{
		Pending: int(nums[0]),
		Leased:  int(nums[1]),
		Done:    int(nums[2]),
		Failed:  int(nums[3]),
	}
	if oldestMS := nums[4]; oldestMS > 0 {
		s.OldestPending = time.Since(time.UnixMilli(oldestMS))
	}
	return s, nil
}

// Close stops this node's use of the queue.
//
// It does not empty the queue: the tasks in Redis are the fleet's, not this
// node's, and a node shutting down must leave them for everyone else. What it
// does is release this node's grip -- waiting workers are unblocked, the reaper
// stops, and the connections go back.
func (q *RedisQueue) Close() error {
	var err error
	q.closeOnce.Do(func() {
		q.closeStop()
		<-q.reaperDone // let the reaper finish its pass rather than cut its connection
		err = q.rdb.Close()
	})
	return err
}

func (q *RedisQueue) isClosed() bool { return q.closeCtx.Err() != nil }

// parseLuaInt reads an integer out of a script's reply.
//
// Redis converts a Lua number to a RESP integer, which go-redis surfaces as
// int64 -- but a script that returns a string (as lease does for its attempt
// count, to keep it out of Lua's float arithmetic) arrives as a string. Both
// shapes are accepted here rather than at each call site.
func parseLuaInt(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case string:
		var parsed int64
		if _, err := fmt.Sscanf(n, "%d", &parsed); err != nil {
			return 0, fmt.Errorf("value %q is not an integer", n)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("value is %T, want an integer", v)
	}
}
