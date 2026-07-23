// Package queue defines the task queue that is the system's source of truth.
//
// The design goal is that one VPS and three hundred run the same code. That is
// achieved by making the queue authoritative and the workers dumb: a node never
// decides what work it should have, it asks for work when it has a free slot.
//
// This is pull-based, and the choice is load-bearing. A push-based scheduler has
// to know how many nodes exist, how loaded each one is, and what to do when one
// dies mid-assignment -- state that is wrong the moment it is written, and a
// component whose failure stops the whole fleet. With pull, a node with a free
// slot takes the next task; a node that dies simply stops asking, and its
// in-flight work returns to the queue when its lease expires. Adding the 300th
// node requires no coordination and no configuration: it just starts pulling.
//
// The Queue interface exists so the in-memory implementation used on a single
// host can be swapped for a Redis or Postgres one across a fleet, without a
// single caller changing.
package queue

import (
	"context"
	"errors"
	"math"
	"time"
)

// ErrClosed is returned once the queue is shut down.
var ErrClosed = errors.New("queue is closed")

// ErrDuplicate means a task with that ID is already in the queue, pending or
// running.
//
// This is what makes a caller-supplied task ID an idempotency key. A client
// whose submission times out cannot know whether it landed, so its only sane
// move is to retry with the same ID -- and that retry must be rejected rather
// than run the work a second time. It is a sentinel because the honest response
// to it is "your task is already queued" (a conflict), which is a different
// answer from "your task is malformed".
var ErrDuplicate = errors.New("a task with this ID is already queued")

// State is where a task is in its life.
type State string

const (
	// StatePending means the task is waiting for a free slot.
	StatePending State = "pending"
	// StateLeased means a worker holds it and is running it.
	StateLeased State = "leased"
	// StateDone means it finished, successfully or not.
	StateDone State = "done"
	// StateFailed means it exhausted its attempts.
	StateFailed State = "failed"
)

// Task is one unit of work: run some code in a sandbox.
type Task struct {
	ID string

	// Image names the language image, e.g. "python".
	Image string

	// Files are written into the sandbox before Cmd runs, keyed by path.
	Files map[string][]byte

	Cmd  string
	Args []string
	Env  map[string]string

	// Timeout bounds the process. The sandbox's own TTL bounds everything else.
	Timeout time.Duration

	// Network enables filtered egress for this task.
	Network bool

	VCPUs    int
	MemMiB   int
	CPUCores float64

	// Priority orders the queue. Higher runs first; equal priorities are FIFO,
	// which is the guarantee callers actually reason about.
	Priority int

	// MaxAttempts is how many times the task may be tried before it is failed
	// for good. Zero uses the queue's default.
	//
	// Retries exist for nodes dying, not for code failing: a task whose process
	// exits non-zero is *done*, not failed. Retrying that would run someone's
	// buggy code repeatedly and bill them for each attempt.
	MaxAttempts int

	// EnqueuedAt is set by the queue.
	EnqueuedAt time.Time
}

// Cost is what this task reserves on a node: the CPU and memory the scheduler
// bin-packs against. Exposed so a node can debit exactly the amount the queue
// used to decide the task fits.
func (t Task) Cost() Resources { return taskCost(t) }

// Resources is a CPU-and-memory amount: what a node has free, or what a task
// costs. It is the unit the scheduler packs in -- a node leases a task only when
// the task's cost fits its free Resources -- which is what keeps a mix of large
// and small tasks from oversubscribing any one box.
type Resources struct {
	// CPU is cores, and fractional is meaningful: a task capped at half a core
	// costs 0.5 here.
	CPU float64

	// MemMiB is memory in mebibytes, and it is the hard dimension. A microVM
	// reserves real RAM, so overcommitting memory is an OOM that kills other
	// tenants' VMs, where overcommitting CPU is only contention.
	MemMiB int
}

// Unbounded fits every task. A node passes it when it caps concurrency by count
// alone and does no resource packing, which reproduces the old behaviour of
// always taking the highest-priority task.
var Unbounded = Resources{CPU: math.MaxFloat64, MemMiB: math.MaxInt}

// Lessee is who is asking for work: a node's identity and its resources, both
// what is free now and what it holds in total. The scheduler needs all three.
//
//   - Avail decides what the node can run right now.
//   - Total decides whether it could run the head task at all once it drains,
//     which is what separates "reserve capacity and wait for it" from "this task
//     is not for me, keep working".
//   - ID owns a reservation, so exactly one node drains for a task too big to
//     place while the rest of the fleet carries on.
type Lessee struct {
	ID    string
	Avail Resources
	Total Resources
}

// reservationTTL is how long a drain claim lives without being refreshed. A node
// draining for the head re-claims on every lease attempt, so this only has to
// outlast the gap between those attempts; its real job is to release the claim
// promptly when the draining node dies, so another can take over rather than the
// head waiting forever on a corpse.
const reservationTTL = 5 * time.Second

// fits reports whether a task costing cost can run in avail.
func (avail Resources) fits(cost Resources) bool {
	return cost.CPU <= avail.CPU && cost.MemMiB <= avail.MemMiB
}

// taskCost is what a task reserves for scheduling. It applies the same fallbacks
// the runtime uses for an unset request, so a task that names no resources packs
// as its real footprint rather than as zero -- which would let unlimited such
// tasks pile onto one node.
func taskCost(t Task) Resources {
	cpu := t.CPUCores
	if cpu <= 0 {
		if t.VCPUs > 0 {
			cpu = float64(t.VCPUs)
		} else {
			cpu = 1
		}
	}
	mem := t.MemMiB
	if mem <= 0 {
		mem = 256
	}
	return Resources{CPU: cpu, MemMiB: mem}
}

// Lease is a claim on a task, held by one worker.
//
// It exists because a worker can die at any moment. Without a lease, its task
// would either be lost (if the queue forgot it) or duplicated (if the queue
// re-issued it while the worker was merely slow). The lease makes the queue's
// question answerable: has this worker checked in recently enough that I should
// still believe it is running?
type Lease struct {
	Task    Task
	Attempt int

	// ExpiresAt is when the queue will consider the worker dead and re-queue the
	// task. Workers must Extend before then for work that legitimately runs long.
	ExpiresAt time.Time

	// token proves this lease is the current one for the task. A worker that
	// pauses past its expiry and comes back must not be able to complete a task
	// that has since been handed to somebody else.
	token string
}

// Result is the outcome of a task.
type Result struct {
	TaskID string

	// ExitCode is the process's status. Nil means it never ran to completion.
	ExitCode *int
	Stdout   []byte
	Stderr   []byte

	// Error describes an infrastructure failure -- no sandbox could be created,
	// the VM died. Distinct from a non-zero exit, which is the code's own doing
	// and not a failure of ours.
	Error string

	// Attempts is how many tries it took.
	Attempts int

	StartedAt  time.Time
	FinishedAt time.Time

	// ActiveCPU and MemoryPeak are what the task actually cost.
	ActiveCPU  time.Duration
	MemoryPeak uint64
}

// validateTask rejects a task no implementation should accept. It lives here so
// both implementations enforce one rule rather than two that drift.
func validateTask(t Task) error {
	if t.ID == "" {
		return errors.New("task ID is required")
	}
	if t.Cmd == "" {
		return errors.New("task Cmd is required")
	}
	return nil
}

// Stats describe the queue's depth.
type Stats struct {
	Pending int
	Leased  int
	Done    int
	Failed  int

	// OldestPending is how long the head of the queue has been waiting. This is
	// the number that says whether the fleet is big enough.
	OldestPending time.Duration
}

// Queue is the source of truth for work.
//
// Implementations must be safe for concurrent use by many workers, and across
// many hosts if the implementation is distributed.
type Queue interface {
	// Enqueue adds a task. It returns an error if the task is invalid or the
	// queue is closed, and ErrDuplicate if a task with the same ID is already
	// pending or leased.
	//
	// Enqueue is idempotent on ID for as long as the task is in the queue: the
	// second submission is refused, not run. Once the task finishes, its ID is
	// free again -- the queue remembers what it is running, not everything it
	// ever ran.
	Enqueue(ctx context.Context, task Task) error

	// Lease blocks until a task the caller can run is available, then hands it to
	// exactly one caller. avail is the node's free resources; the task returned
	// is the highest-priority one whose cost fits it, which is what turns the
	// queue into a resource-aware scheduler without any central component. Pass
	// Unbounded to take the highest-priority task regardless of size. It returns
	// ErrClosed on shutdown, and ctx.Err() if the caller gives up waiting.
	//
	// A node that frees resources should cancel a blocked Lease and re-issue it
	// with the larger avail: a task too big a moment ago may fit now, and the
	// call it is parked in was made against the smaller amount.
	//
	// # Reservation
	//
	// When the head task fits no node right now, backfilling smaller tasks
	// forever would starve it. Instead the first node that could run it (it fits
	// the node's Total) reserves it: that node takes nothing and drains until the
	// head fits, while every other node keeps pulling work it can run. So a big
	// task waits for one node to clear, not for the whole fleet to idle, and it is
	// never overtaken indefinitely by the small tasks behind it. Only the head is
	// reserved; the next big task's turn comes once the head is placed.
	Lease(ctx context.Context, leaseFor time.Duration, lessee Lessee) (*Lease, error)

	// Extend pushes a lease's expiry out, for a task that is legitimately still
	// running. It fails if the lease has already expired and been reissued.
	Extend(ctx context.Context, lease *Lease, by time.Duration) error

	// Complete records a task's result and releases its lease.
	Complete(ctx context.Context, lease *Lease, result Result) error

	// Fail releases a lease without a result. The task is retried if it has
	// attempts left, and failed for good otherwise.
	Fail(ctx context.Context, lease *Lease, reason string) error

	// Result returns a finished task's outcome. The bool reports whether a
	// result exists; the error reports that the queue could not be asked.
	//
	// Those are separate returns because they demand opposite answers. "No
	// result" means the task is still running and the caller should wait; an
	// unreachable queue means nothing at all is known. Collapsing the second
	// into the first would have a caller told "still pending" about a task that
	// finished an hour ago, and wait forever on a backend that is simply down.
	Result(ctx context.Context, taskID string) (Result, bool, error)

	// Stats reports the queue's depth.
	Stats(ctx context.Context) (Stats, error)

	// Close shuts the queue down, unblocking every waiting Lease.
	Close() error
}
