package queue

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

// testRedisAddr is the server every Redis test shares, or "" if there is none.
var testRedisAddr string

// TestMain starts a Redis for the suite.
//
// It starts its own rather than using a developer's: these tests write keys,
// and a suite that can silently attach to a real Redis is a suite that can
// silently write to production. The server here listens on a random port with
// its own data directory and no persistence, and dies with the test binary.
func TestMain(m *testing.M) {
	addr, stop, err := startTestRedis()
	if err != nil {
		// Not fatal. The Redis tests skip, the rest of the package still runs,
		// and CI without redis-server stays useful.
		fmt.Fprintf(os.Stderr, "redis tests will skip: %v\n", err)
	} else {
		testRedisAddr = addr
		defer stop()
	}

	code := m.Run()
	if stop != nil {
		stop()
	}
	os.Exit(code)
}

func startTestRedis() (addr string, stop func(), err error) {
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		return "", nil, fmt.Errorf("redis-server is not on PATH")
	}

	port, err := freePort()
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "microvm-redis-test-")
	if err != nil {
		return "", nil, err
	}

	cmd := exec.Command(bin,
		"--port", fmt.Sprint(port),
		"--bind", "127.0.0.1",
		"--dir", dir,
		"--save", "", // no snapshots: this data is worthless the moment the test ends
		"--appendonly", "no",
	)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("start redis-server: %w", err)
	}

	stop = func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		os.RemoveAll(dir)
	}

	addr = fmt.Sprintf("127.0.0.1:%d", port)
	if err := waitForRedis(addr, 10*time.Second); err != nil {
		stop()
		return "", nil, err
	}
	return addr, stop, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitForRedis(addr string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("redis at %s did not come up within %v", addr, within)
}

// prefixCounter keeps every queue in the suite in its own keyspace. Sharing one
// server is fine; sharing a prefix would make one test's leftovers into another
// test's mystery failure.
var prefixCounter atomic.Int64

func newRedisForTest(t *testing.T, cfg testConfig) Queue {
	t.Helper()
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}

	prefix := fmt.Sprintf("t%d", prefixCounter.Add(1))

	q, err := NewRedis(context.Background(), RedisConfig{
		Addr:               testRedisAddr,
		Prefix:             prefix,
		DefaultMaxAttempts: cfg.DefaultMaxAttempts,
		ReapInterval:       cfg.ReapInterval,
		// Short, so a test that waits on the doorbell backstop does not spend a
		// second doing it.
		DoorbellTimeout: 100 * time.Millisecond,
		ResultTTL:       time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect to test redis: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

// TestRedisQueue runs the Queue contract against the Redis implementation.
//
// This is the test that matters for the fleet. Passing it is what makes
// swapping MemoryQueue for RedisQueue a configuration change rather than a
// leap of faith.
func TestRedisQueue(t *testing.T) {
	runConformance(t, newRedisForTest)
}

// Everything below is specific to Redis being shared and remote, so there is
// nothing for the in-memory queue to answer.

// The whole reason Redis exists here: two nodes, one queue. Each node is a
// separate RedisQueue -- separate connections, separate reapers, no shared
// memory -- exactly as two hosts would be, and a task must still reach exactly
// one of them.
func TestRedisTwoNodesShareOneQueue(t *testing.T) {
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}
	prefix := fmt.Sprintf("fleet%d", prefixCounter.Add(1))
	ctx := context.Background()

	newNode := func() Queue {
		q, err := NewRedis(ctx, RedisConfig{
			Addr: testRedisAddr, Prefix: prefix,
			DoorbellTimeout: 100 * time.Millisecond, ResultTTL: time.Hour,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("node: %v", err)
		}
		t.Cleanup(func() { _ = q.Close() })
		return q
	}

	nodeA, nodeB := newNode(), newNode()

	// Submitted through one node.
	const n = 20
	for i := 0; i < n; i++ {
		mustEnqueue(t, nodeA, task(fmt.Sprintf("shared-%d", i)))
	}

	// Drained by both, with no coordination between them.
	seen := map[string]int{}
	for i := 0; i < n; i++ {
		q := nodeA
		if i%2 == 1 {
			q = nodeB
		}
		l := mustLease(t, q, time.Minute)
		seen[l.Task.ID]++
		if err := q.Complete(ctx, l, Result{}); err != nil {
			t.Fatalf("complete via node: %v", err)
		}
	}

	if len(seen) != n {
		t.Errorf("saw %d distinct tasks across both nodes, want %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("task %s ran %d times across the fleet", id, count)
		}
	}

	// A result written by one node must be readable from the other: that is the
	// difference between a queue and a node-local map.
	if _, ok, err := nodeB.Result(ctx, "shared-0"); err != nil || !ok {
		t.Errorf("node B cannot read a result node A stored: ok=%v err=%v", ok, err)
	}
}

// A node dying must not strand its work. Here node A leases and is then killed
// outright -- no Fail, no Close, exactly like a host losing power -- and node B
// has to pick the task up on its own.
func TestRedisWorkSurvivesANodeDying(t *testing.T) {
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}
	prefix := fmt.Sprintf("death%d", prefixCounter.Add(1))
	ctx := context.Background()

	newNode := func() Queue {
		q, err := NewRedis(ctx, RedisConfig{
			Addr: testRedisAddr, Prefix: prefix,
			ReapInterval: 50 * time.Millisecond, DoorbellTimeout: 100 * time.Millisecond,
			ResultTTL: time.Hour,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("node: %v", err)
		}
		return q
	}

	nodeA := newNode()
	nodeB := newNode()
	t.Cleanup(func() { _ = nodeB.Close() })

	mustEnqueue(t, nodeA, task("orphan"))

	l := mustLease(t, nodeA, 200*time.Millisecond)
	if l.Task.ID != "orphan" {
		t.Fatal("unexpected task")
	}

	// Node A is gone. It never completes, never extends, never says goodbye.
	_ = nodeA.Close()

	// Node B's reaper must notice the lapsed lease and re-queue the work.
	leaseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	recovered, err := nodeB.Lease(leaseCtx, time.Minute, Lessee{Avail: Unbounded, Total: Unbounded})
	if err != nil {
		t.Fatalf("work was stranded when its node died: %v", err)
	}
	if recovered.Task.ID != "orphan" {
		t.Errorf("got %s, want orphan", recovered.Task.ID)
	}
	if recovered.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", recovered.Attempt)
	}
}

// The queue is the source of truth, so it must outlive the process that filled
// it. This is the property a map in memory can never have, and the reason the
// interface exists at all.
func TestRedisQueueSurvivesTheProcessThatFilledIt(t *testing.T) {
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}
	prefix := fmt.Sprintf("restart%d", prefixCounter.Add(1))
	ctx := context.Background()

	cfg := RedisConfig{
		Addr: testRedisAddr, Prefix: prefix,
		DoorbellTimeout: 100 * time.Millisecond, ResultTTL: time.Hour,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	before, err := NewRedis(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	mustEnqueue(t, before, task("durable-1"), task("durable-2"))
	_ = before.Close() // the daemon restarts

	after, err := NewRedis(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = after.Close() })

	stats, err := after.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 2 {
		t.Fatalf("pending after restart = %d, want 2: the queue did not survive", stats.Pending)
	}
	// And in the order they went in.
	if got := mustLease(t, after, time.Minute).Task.ID; got != "durable-1" {
		t.Errorf("first after restart = %s, want durable-1", got)
	}
}

// The doorbell is a hint, not the queue. If a wake-up is ever missed -- and it
// will be, because the reaper requeues without ringing it -- a worker must
// still find the task on its next pass. Here the doorbell is drained behind the
// worker's back to force exactly that.
func TestRedisDoorbellIsOnlyAHint(t *testing.T) {
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}
	prefix := fmt.Sprintf("doorbell%d", prefixCounter.Add(1))
	ctx := context.Background()

	q, err := NewRedis(ctx, RedisConfig{
		Addr: testRedisAddr, Prefix: prefix,
		DoorbellTimeout: 100 * time.Millisecond, ResultTTL: time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })

	mustEnqueue(t, q, task("quiet"))

	// Steal every wake-up the enqueue rang.
	if err := q.rdb.Del(ctx, q.notifyKey()).Err(); err != nil {
		t.Fatal(err)
	}

	// The worker must still find the task, on the backstop poll alone.
	leaseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	l, err := q.Lease(leaseCtx, time.Minute, Lessee{Avail: Unbounded, Total: Unbounded})
	if err != nil {
		t.Fatalf("a task with no doorbell was never found: %v", err)
	}
	if l.Task.ID != "quiet" {
		t.Errorf("got %s, want quiet", l.Task.ID)
	}
}

// Results must not accumulate forever in a Redis somebody else is also using.
func TestRedisResultsExpire(t *testing.T) {
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}
	prefix := fmt.Sprintf("ttl%d", prefixCounter.Add(1))
	ctx := context.Background()

	q, err := NewRedis(ctx, RedisConfig{
		Addr: testRedisAddr, Prefix: prefix,
		DoorbellTimeout: 100 * time.Millisecond,
		ResultTTL:       time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })

	mustEnqueue(t, q, task("expiring"))
	l := mustLease(t, q, time.Minute)
	if err := q.Complete(ctx, l, Result{}); err != nil {
		t.Fatal(err)
	}

	ttl, err := q.rdb.TTL(ctx, q.resultKey("expiring")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 {
		t.Fatalf("result TTL = %v: results would accumulate forever", ttl)
	}
	if ttl > time.Hour {
		t.Errorf("result TTL = %v, want at most 1h", ttl)
	}
}

// A finished task must leave nothing behind but its result. The task hash is
// what makes an ID unique while queued, so a leaked one would make that ID
// permanently unusable -- and a leaked pending member would hand a worker a
// task that no longer exists.
func TestRedisFinishedTaskLeavesNoState(t *testing.T) {
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}
	prefix := fmt.Sprintf("leak%d", prefixCounter.Add(1))
	ctx := context.Background()

	q, err := NewRedis(ctx, RedisConfig{
		Addr: testRedisAddr, Prefix: prefix,
		DoorbellTimeout: 100 * time.Millisecond, ResultTTL: time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })

	mustEnqueue(t, q, task("tidy"))
	l := mustLease(t, q, time.Minute)
	if err := q.Complete(ctx, l, Result{}); err != nil {
		t.Fatal(err)
	}

	keys, err := q.rdb.Keys(ctx, q.ns+":*").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k == q.key("task:tidy") {
			t.Errorf("the task hash outlived the task; its ID can never be reused")
		}
	}

	if n, _ := q.rdb.ZCard(ctx, q.pendingKey()).Result(); n != 0 {
		t.Errorf("pending set has %d leftover members", n)
	}
	if n, _ := q.rdb.ZCard(ctx, q.leasedKey()).Result(); n != 0 {
		t.Errorf("leased set has %d leftover members", n)
	}
}

// A worker parked on the doorbell holds a connection. If the pool is smaller
// than the number of slots, workers block waiting for connections rather than
// for work, and the node quietly stops pulling. This pins the pool against a
// node with many slots.
func TestRedisManyWorkersCanWaitAtOnce(t *testing.T) {
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}
	prefix := fmt.Sprintf("pool%d", prefixCounter.Add(1))
	ctx := context.Background()

	q, err := NewRedis(ctx, RedisConfig{
		Addr: testRedisAddr, Prefix: prefix,
		DoorbellTimeout: time.Second, ResultTTL: time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })

	const workers = 64
	leased := make(chan string, workers)
	for i := 0; i < workers; i++ {
		go func() {
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			l, err := q.Lease(wctx, time.Minute, Lessee{Avail: Unbounded, Total: Unbounded})
			if err != nil {
				return
			}
			leased <- l.Task.ID
		}()
	}

	// Give every worker time to park on the doorbell before any work exists.
	time.Sleep(500 * time.Millisecond)

	for i := 0; i < workers; i++ {
		mustEnqueue(t, q, task(fmt.Sprintf("w%d", i)))
	}

	got := map[string]bool{}
	deadline := time.After(10 * time.Second)
	for i := 0; i < workers; i++ {
		select {
		case id := <-leased:
			if got[id] {
				t.Errorf("task %s went to two workers", id)
			}
			got[id] = true
		case <-deadline:
			t.Fatalf("only %d of %d workers were served; the connection pool is "+
				"too small for a node with this many slots", i, workers)
		}
	}
}

// A sanity check on the test harness itself: a unique prefix must actually
// isolate two queues sharing one server, or every test above is suspect.
func TestRedisPrefixIsolatesQueues(t *testing.T) {
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}
	ctx := context.Background()

	a := newRedisForTest(t, testConfig{})
	b := newRedisForTest(t, testConfig{})

	mustEnqueue(t, a, task("only-in-a"))

	stats, err := b.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 0 {
		t.Errorf("queue B sees %d tasks from queue A: the prefixes do not isolate",
			stats.Pending)
	}
}
