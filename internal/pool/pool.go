// Package pool runs queued tasks on a fixed number of sandbox slots.
//
// A pool is one node's share of the fleet. It knows only two things: how many
// sandboxes it can run at once, and where the queue is. It never learns how
// many other nodes exist, and no one tells it what to run -- each slot pulls
// the next task when it is free.
//
// That is what makes one VPS and three hundred the same program. Capacity is
// the sum of the slots that happen to be pulling; adding a node adds capacity
// with no reconfiguration anywhere, and losing one removes it without anything
// having to notice. The queue's leases handle the rest: a node that dies stops
// extending, and its work returns to the queue on its own.
package pool

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/queue"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

// Config configures a pool.
type Config struct {
	// Slots caps how many sandboxes this node runs at once, whatever their size.
	// It is the ceiling on VM count -- the fixed per-VM overhead (boot, kernel,
	// agent) is real even for a tiny task -- and it applies on top of the CPU and
	// memory budgets below.
	Slots int

	// CPU and MemMiB are the node's schedulable budget: the pool never commits
	// more than this to running tasks at once, so a task is leased only when its
	// cost fits what is free. This is what lets a fleet mix large and small tasks
	// without any one box oversubscribing its RAM. Zero on either means "do not
	// bound this dimension", which reduces scheduling to the slot count alone --
	// the right default for a box dedicated to one task size, and wrong for a
	// shared or heterogeneous one, where both should be set.
	CPU    float64
	MemMiB int

	// NodeID identifies this node when it reserves a task too big to place right
	// now: the reservation is owned by one node so the rest of the fleet keeps
	// working while it drains. Empty means one is generated; it only has to be
	// unique among live nodes, so a fresh random value per process is enough -- a
	// restarted node simply re-reserves.
	NodeID string

	// LeaseDuration is how long a slot claims a task before it must check in
	// again. Too short and a slow task is stolen from a healthy worker; too long
	// and a dead node's task sits unnoticed. It should comfortably exceed the
	// heartbeat interval, not the task's runtime -- long tasks extend.
	LeaseDuration time.Duration

	// HeartbeatInterval is how often a running slot extends its lease. It must
	// be well under LeaseDuration, or a healthy worker loses its task to a
	// timing race.
	HeartbeatInterval time.Duration

	// DefaultTaskTimeout bounds a task whose own Timeout is zero.
	DefaultTaskTimeout time.Duration
}

func (c *Config) applyDefaults() error {
	if c.Slots <= 0 {
		return errors.New("Slots must be positive")
	}
	if c.NodeID == "" {
		c.NodeID = randomNodeID()
	}
	// An unset budget means "unbounded on this dimension", so a node configured
	// only with slots behaves exactly as it did before scheduling by resource:
	// it takes the highest-priority task regardless of size, up to Slots at once.
	if c.CPU <= 0 {
		c.CPU = math.MaxFloat64
	}
	if c.MemMiB <= 0 {
		c.MemMiB = math.MaxInt
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 60 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = c.LeaseDuration / 3
	}
	if c.HeartbeatInterval >= c.LeaseDuration {
		// Not a matter of taste: a heartbeat at or past the expiry means the
		// lease always lapses first, and every long task gets duplicated onto
		// another node.
		return fmt.Errorf("HeartbeatInterval (%v) must be well under LeaseDuration (%v)",
			c.HeartbeatInterval, c.LeaseDuration)
	}
	if c.DefaultTaskTimeout <= 0 {
		c.DefaultTaskTimeout = 5 * time.Minute
	}
	return nil
}

// Pool pulls tasks from a queue and runs them in sandboxes.
//
// One goroutine does the leasing -- the scheduler -- and each task it accepts
// runs in its own goroutine. The scheduler owns the resource accounting, which
// is why it is single: "does this task fit?" and "commit its cost" have to be
// one indivisible step, or two tasks each see the same free memory and both get
// admitted. It leases the amount that is actually free, so the queue only ever
// hands back a task this node can run right now.
type Pool struct {
	cfg      Config
	q        queue.Queue
	mgr      *sandbox.Manager
	log      *slog.Logger
	capacity queue.Resources
	nodeID   string

	wg     sync.WaitGroup
	cancel context.CancelFunc

	// busy counts tasks currently running, for observability.
	busy atomic.Int64

	// mu guards the live accounting and the in-flight lease's cancel.
	mu        sync.Mutex
	committed queue.Resources
	running   int
	// leaseCancel breaks the scheduler out of a blocked Lease when resources
	// free up, so it can re-ask with the larger amount -- a task too big a moment
	// ago may fit now, and the parked call was made against the smaller free.
	leaseCancel context.CancelFunc

	// wake nudges the scheduler to re-evaluate after a task finishes.
	wake chan struct{}

	// executeFn runs one task, defaulting to execute. It is a field so tests can
	// substitute a stub: what this package owns is the scheduling contract --
	// never exceed the budget, retry infrastructure failures but not bad exits,
	// keep a long task's lease alive -- and none of that needs a hypervisor to
	// be true. The real path is covered by the e2e suite against actual VMs.
	executeFn func(ctx context.Context, task queue.Task, started time.Time) (queue.Result, error)
}

// New returns a pool. Call Start to begin pulling.
func New(cfg Config, q queue.Queue, mgr *sandbox.Manager, log *slog.Logger) (*Pool, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	p := &Pool{
		cfg:      cfg,
		q:        q,
		mgr:      mgr,
		log:      log,
		capacity: queue.Resources{CPU: cfg.CPU, MemMiB: cfg.MemMiB},
		nodeID:   cfg.NodeID,
		wake:     make(chan struct{}, 1),
	}
	p.executeFn = p.execute
	return p, nil
}

// Start launches the scheduler. It returns immediately; Stop waits for it and
// every task it started.
func (p *Pool) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.schedule(ctx)
	}()
	p.log.Info("pool started",
		"slots", p.cfg.Slots, "cpu", capacityString(p.cfg.CPU),
		"mem_mib", capacityString(float64(p.cfg.MemMiB)), "lease", p.cfg.LeaseDuration)
}

// Stop stops pulling and waits for in-flight tasks to finish.
func (p *Pool) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	p.log.Info("pool stopped")
}

// Busy is how many tasks are currently running.
func (p *Pool) Busy() int { return int(p.busy.Load()) }

// Slots is the pool's max concurrent VMs.
func (p *Pool) Slots() int { return p.cfg.Slots }

// schedule is the whole scheduling loop: while there is room, lease a task that
// fits the free budget and run it; when there is none, wait for a task to finish
// or a new one to arrive.
//
// It never learns what else runs on the node or the fleet -- it asks the queue
// for work sized to what is free here, and the queue answers with something that
// fits or nothing. That is what keeps the queue, not any central scheduler, the
// thing that decides where work goes, even now that the decision accounts for
// resources.
func (p *Pool) schedule(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		p.mu.Lock()
		full := p.running >= p.cfg.Slots
		avail := sub(p.capacity, p.committed)
		p.mu.Unlock()

		if full {
			// Every VM slot is busy; nothing to do until one frees.
			select {
			case <-ctx.Done():
				return
			case <-p.wake:
			}
			continue
		}

		leaseCtx, cancel := context.WithCancel(ctx)
		p.mu.Lock()
		p.leaseCancel = cancel
		p.mu.Unlock()

		lease, err := p.q.Lease(leaseCtx, p.cfg.LeaseDuration, queue.Lessee{
			ID:    p.nodeID,
			Avail: avail,
			Total: p.capacity,
		})

		p.mu.Lock()
		p.leaseCancel = nil
		p.mu.Unlock()
		cancel()

		if lease != nil {
			p.mu.Lock()
			p.committed = add(p.committed, lease.Task.Cost())
			p.running++
			p.mu.Unlock()
			p.wg.Add(1)
			go p.runOne(ctx, lease)
			continue
		}

		// No task. Distinguish shutdown from "a completion cancelled the lease so
		// I can re-ask with more free" from an unexpected failure.
		if errors.Is(err, queue.ErrClosed) || ctx.Err() != nil {
			return
		}
		if errors.Is(err, context.Canceled) {
			continue // a task freed resources; loop and lease against the larger free
		}
		p.log.Error("leasing failed", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// runOne runs a leased task and then releases its resources, waking the
// scheduler so the room it freed can be reused at once.
func (p *Pool) runOne(ctx context.Context, lease *queue.Lease) {
	defer p.wg.Done()
	p.busy.Add(1)
	p.runTask(ctx, p.log, lease)
	p.busy.Add(-1)

	p.mu.Lock()
	p.committed = sub(p.committed, lease.Task.Cost())
	p.running--
	// Break any lease the scheduler is parked in, so it re-asks with this task's
	// resources added back to the free pool rather than sleeping on the smaller
	// amount it last asked for.
	if p.leaseCancel != nil {
		p.leaseCancel()
		p.leaseCancel = nil
	}
	p.mu.Unlock()

	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func add(a, b queue.Resources) queue.Resources {
	return queue.Resources{CPU: a.CPU + b.CPU, MemMiB: a.MemMiB + b.MemMiB}
}

func sub(a, b queue.Resources) queue.Resources {
	return queue.Resources{CPU: a.CPU - b.CPU, MemMiB: a.MemMiB - b.MemMiB}
}

// randomNodeID returns a fresh identifier for reservation ownership. It only has
// to be unique among live nodes, not stable across restarts, so random is right.
func randomNodeID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic and near-impossible; a fixed value
		// is a safe last resort here because uniqueness only affects reservation
		// efficiency, never correctness.
		return "node-fallback"
	}
	return "node-" + hex.EncodeToString(b[:])
}

// capacityString renders an unbounded budget as "unbounded" rather than a
// meaningless max-int, for the startup log.
func capacityString(v float64) string {
	if v >= math.MaxFloat64 || v >= float64(math.MaxInt) {
		return "unbounded"
	}
	return fmt.Sprintf("%g", v)
}

// runTask executes one leased task and reports its outcome.
func (p *Pool) runTask(ctx context.Context, log *slog.Logger, lease *queue.Lease) {
	task := lease.Task
	log = log.With("task", task.ID, "attempt", lease.Attempt)
	started := time.Now()

	// Keep the lease alive while the task runs, so a legitimately slow task is
	// not mistaken for a dead node.
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go p.heartbeat(heartbeatCtx, log, lease)

	result, err := p.executeFn(ctx, task, started)
	if err != nil {
		// Infrastructure failed us: no sandbox, or the VM died. Fail the lease
		// so another node retries -- this is exactly what retries are for.
		log.Error("task failed on infrastructure", "err", err)
		if ferr := p.q.Fail(context.Background(), lease, err.Error()); ferr != nil {
			log.Warn("could not fail the lease", "err", ferr)
		}
		return
	}

	// The task ran. A non-zero exit is the code's own doing, not a failure of
	// ours, so it completes rather than retries: re-running someone's broken
	// program would bill them again for the same answer.
	if err := p.q.Complete(context.Background(), lease, result); err != nil {
		log.Warn("could not record the result", "err", err)
		return
	}
	log.Info("task done",
		"exit", exitCodeString(result.ExitCode),
		"wall", time.Since(started).Round(time.Millisecond),
		"active_cpu", result.ActiveCPU.Round(time.Millisecond))
}

// execute creates a sandbox, runs the task in it, and collects the result.
func (p *Pool) execute(ctx context.Context, task queue.Task, started time.Time) (queue.Result, error) {
	timeout := task.Timeout
	if timeout <= 0 {
		timeout = p.cfg.DefaultTaskTimeout
	}

	spec := sandbox.Spec{
		Spec: runtime.Spec{
			// One sandbox per task, named after it. Tasks never share a VM: a
			// sandbox that has run untrusted code cannot be handed to the next
			// tenant, whatever it left behind.
			ID:      "task-" + task.ID,
			Image:   task.Image,
			VCPUs:   orDefault(task.VCPUs, 1),
			MemMiB:  orDefault(task.MemMiB, 256),
			Network: task.Network,
			Limits: runtime.Limits{
				CPUCores: task.CPUCores,
			},
		},
		// The sandbox outlives the process by a margin, so a task that finishes
		// normally is never racing its own VM's TTL.
		TTL: timeout + time.Minute,
		// Nothing else will use this VM, so idle reclaim would only fight the
		// explicit teardown below.
		IdleTimeout: -1,
	}

	sb, err := p.mgr.Create(ctx, spec)
	if err != nil {
		return queue.Result{}, fmt.Errorf("create sandbox: %w", err)
	}
	// The sandbox dies with the task, always: leaking one holds a slot's worth
	// of memory for nothing.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sb.Stop(stopCtx, sandbox.ReasonStopped)
	}()

	for path, content := range task.Files {
		if err := sb.WriteFile(ctx, path, bytes.NewReader(content), ""); err != nil {
			return queue.Result{}, fmt.Errorf("upload %s: %w", path, err)
		}
	}

	execID := "task-" + task.ID
	err = sb.Exec(ctx, protocol.ExecRequest{
		ID:      execID,
		Cmd:     task.Cmd,
		Args:    task.Args,
		Env:     task.Env,
		Timeout: timeout,
	}, nil)

	// Read the record before the deferred Stop tears the sandbox down: it holds
	// the output whether the exec succeeded, failed, or was killed.
	rec, ok := sb.Logs(execID)
	info := sb.Info()

	if err != nil {
		// A dead VM is infrastructure's fault and worth retrying. A process that
		// merely exited badly is not, and the record tells them apart.
		if ok && rec.Status != logstore.StatusVanished {
			return buildResult(task.ID, rec, info, started), nil
		}
		return queue.Result{}, fmt.Errorf("exec: %w", err)
	}
	if !ok {
		return queue.Result{}, errors.New("no record for the exec: the sandbox produced nothing")
	}

	return buildResult(task.ID, rec, info, started), nil
}

func buildResult(taskID string, rec logstore.Record, info sandbox.Info, started time.Time) queue.Result {
	return queue.Result{
		TaskID:     taskID,
		ExitCode:   rec.ExitCode,
		Stdout:     rec.Stdout,
		Stderr:     rec.Stderr,
		Error:      rec.Error,
		StartedAt:  started,
		FinishedAt: time.Now(),
		ActiveCPU:  info.Stats.ActiveCPU,
		MemoryPeak: info.Stats.MemoryPeak,
	}
}

// heartbeat extends the lease until the task finishes.
func (p *Pool) heartbeat(ctx context.Context, log *slog.Logger, lease *queue.Lease) {
	ticker := time.NewTicker(p.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.q.Extend(ctx, lease, p.cfg.LeaseDuration); err != nil {
				// The lease is gone: the queue decided this node was dead and
				// gave the task away. Whatever we are still running is now
				// duplicate work whose result will be discarded, but stopping
				// it is the task context's job, not the heartbeat's.
				log.Warn("lost the lease while running", "err", err)
				return
			}
		}
	}
}

func orDefault(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func exitCodeString(code *int) string {
	if code == nil {
		return "none"
	}
	return fmt.Sprint(*code)
}
