// Package sandbox manages sandbox lifetimes on top of a runtime.
//
// It owns the three things a bare runtime does not: when a sandbox dies, where
// its output goes, and what a caller is told about either. A sandbox that runs
// forever is a bill that grows forever; output that only exists while someone
// is streaming it is output you cannot read after the thing you wanted to debug.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/source"
	"github.com/pablofdezr/microvm/internal/storage"
)

// Defaults applied when a spec leaves them unset. Every sandbox must have a
// finite life: untrusted code with no deadline is a resource leak that pays an
// attacker to wait.
const (
	DefaultTTL         = 15 * time.Minute
	DefaultIdleTimeout = 2 * time.Minute
	DefaultExecTimeout = 5 * time.Minute
	MaxTTL             = 24 * time.Hour
)

// State is where a sandbox is in its life.
type State string

const (
	StateRunning State = "running"
	StateStopped State = "stopped"
)

// StopReason says why a sandbox is no longer running.
//
// The distinction is the whole point of reporting it: "your code failed",
// "you ran out of time", and "you were idle and we reclaimed it" are three
// different things for whoever has to act on it.
type StopReason string

const (
	// ReasonStopped means the caller asked.
	ReasonStopped StopReason = "stopped"
	// ReasonExpired means the TTL elapsed.
	ReasonExpired StopReason = "expired"
	// ReasonIdle means nothing ran for IdleTimeout.
	ReasonIdle StopReason = "idle"
	// ReasonFailed means the VM died on its own -- a crash, or the OOM killer.
	ReasonFailed StopReason = "failed"
)

// Spec describes a sandbox to create.
type Spec struct {
	runtime.Spec

	// TTL is the maximum lifetime. The sandbox is killed when it elapses,
	// whatever it is doing. Zero uses DefaultTTL; it is capped at MaxTTL.
	TTL time.Duration

	// IdleTimeout shuts the sandbox down after this long with nothing running.
	// This is the fluid-compute reclaim: an idle VM bills almost no CPU but
	// still holds memory, so keeping it is only worth it if it gets reused.
	// Zero uses DefaultIdleTimeout; negative disables it.
	IdleTimeout time.Duration

	// Tenant is who the sandbox is attributed to, or empty for nobody. Like
	// Storage it comes from the caller's authenticated identity and never from a
	// request body: it is what MaxConcurrent is counted against, and a tenant a
	// body could choose is a limit a body could dodge.
	//
	// Empty means uncounted, which covers two real cases: a daemon with no auth,
	// and a queued task. A task's submitter is known at enqueue time, so that
	// second case is a limitation rather than a tidy rule: this count is one node's
	// and a task is leased by whichever node has room, so charging it here would
	// refuse work on the node that read the count and admit the same work on the
	// next. Task load is bounded by slots and by the scheduler's resource packing
	// instead. See DEPLOY.md.
	Tenant string

	// MaxConcurrent bounds how many sandboxes Tenant may have running at once.
	// Zero is unlimited. It is carried per create rather than held on the Manager
	// because it belongs to the caller's identity, and two callers on one node do
	// not share one.
	MaxConcurrent int

	// Storage is where this sandbox's files go, or nil to fall back to a
	// per-sandbox namespace. It is set by the layer that knows who the caller is,
	// from the caller's authenticated identity and never from the request body:
	// the prefix names a tenant, and letting a body choose it would let one
	// tenant name another's. See the API's create handler.
	Storage *storage.Mount

	// Source is a project to seed the sandbox's filesystem with before Create
	// returns, or nil for an empty sandbox.
	//
	// Unlike Storage and Tenant this legitimately comes from the request body: it
	// is a URL, and what decides whether it may be fetched is the operator's
	// allowlist rather than the caller's identity. Nothing in it reaches the guest
	// except the files -- see seed.go.
	Source *source.Request

	// Tags are the caller's own labels, for finding this sandbox again. They stay
	// host-side -- nothing about them reaches the guest, which is what separates
	// them from Env: a tag names a sandbox, it does not configure one. They are
	// also reported back, so they are labels and never secrets.
	//
	// Pass them through ValidateTags first. They are held for as long as the
	// sandbox is, so the caps are the whole reason a caller cannot make this
	// map big.
	Tags map[string]string
}

// Tag limits.
//
// A tag is caller-controlled memory the manager holds until the sandbox is
// forgotten, so these bound it: without them a create call is a way to park
// megabytes in the daemon's heap per sandbox and pay for a VM's worth of
// nothing. The lengths are in bytes because that is what is being spent.
const (
	MaxTags          = 10
	MaxTagKeyBytes   = 64
	MaxTagValueBytes = 256
)

// ValidateTags checks a caller's labels against the limits above.
//
// The rule lives beside the map it bounds rather than in the API, because every
// caller of Create pays the memory. The returned error reads as the reason a
// value was refused, so a caller-facing layer can quote it verbatim.
func ValidateTags(tags map[string]string) error {
	if len(tags) > MaxTags {
		return fmt.Errorf("at most %d tags, and this is %d", MaxTags, len(tags))
	}
	for k, v := range tags {
		switch {
		case k == "":
			return errors.New("a tag key cannot be empty")
		case len(k) > MaxTagKeyBytes:
			// Bytes, not characters: a key written in emoji runs out about four
			// times sooner than its length suggests, and the difference is exactly
			// what the cap is spending.
			return fmt.Errorf("the key %q is %d bytes, and a key may be at most %d",
				elide(k), len(k), MaxTagKeyBytes)
		case strings.Contains(k, ":"):
			return fmt.Errorf("the key %q contains \":\", which is where the tag filter splits", elide(k))
		case len(v) > MaxTagValueBytes:
			return fmt.Errorf("the value of %q is %d bytes, and a value may be at most %d",
				elide(k), len(v), MaxTagValueBytes)
		}
	}
	return nil
}

// elide shortens a caller's string for an error message. Quoting the key back is
// how a caller knows which tag was refused, and a key that broke the length
// limit is by definition too long to repeat. It cuts on a rune boundary, so the
// message stays valid UTF-8 for whatever renders it.
func elide(s string) string {
	const max = 32
	if len(s) <= max {
		return s
	}
	for i := range s {
		if i > max {
			return s[:i] + "..."
		}
	}
	return s
}

// Info is a sandbox's status.
//
// It carries the sandbox's shape as well as its state, because a caller reading
// a list needs both and the manager is the only thing that knows either. When
// the shape was not here, listing sandboxes reported every image as empty: the
// one caller who had the image to hand passed it in, and the one who did not
// silently could not.
type Info struct {
	ID        string
	State     State
	Reason    StopReason
	CreatedAt time.Time
	StoppedAt time.Time

	// ExpiresAt is when the TTL will kill it. Extend moves it, so it is read at
	// the moment Info is called and not before.
	ExpiresAt time.Time

	Image   string
	VCPUs   int
	MemMiB  int
	Network bool

	// Storage describes where this sandbox's files go, or nil when the node has
	// no object storage and the sandbox therefore has none.
	Storage *StorageInfo

	// Source is what the sandbox was seeded from, or nil when it was not seeded.
	// Reported so a sandbox found later can be traced back to the code inside it,
	// which is otherwise unknowable from outside the guest. The URL in it is
	// already redacted.
	Source *source.Result

	// Tags are the labels the sandbox was created with, or nil when it was created
	// with none. Reported, unlike Env: they are what a caller filters a list by,
	// so they have to come back out.
	Tags map[string]string

	Stats runtime.Stats
}

// StorageInfo is a sandbox's storage mount, as a caller sees it.
type StorageInfo struct {
	// Namespace is the leaf a caller's files live under -- their tenant when
	// authenticated, the sandbox itself when not. It is what persists across
	// runs, so it is the one storage fact worth returning.
	Namespace string
	ReadOnly  bool

	// MountPath is where the storage appears inside the guest, e.g. "/mnt/storage".
	// A caller sets it at create time and reads it back here to know where its
	// files are, without hardcoding a path the server chose.
	MountPath string

	// MaxBytes is the tenant's total cap, or 0 for unlimited. Reflecting it here
	// lets a caller see the limit their writes are measured against without an
	// admin call they may not be allowed to make.
	MaxBytes int64
	// Policy is what a write does when the cap is reached: "preserve" or "evict".
	// Empty when there is no cap.
	Policy string
}

// Sandbox is one managed sandbox.
type Sandbox struct {
	id   string
	spec Spec
	inst runtime.Instance
	log  *slog.Logger
	mgr  *Manager

	createdAt time.Time

	// source is what this sandbox was seeded from, or nil when it was not.
	//
	// Written before the sandbox is published in the manager's map and never
	// afterwards, so it is read without the lock: every reader got the sandbox out
	// of that map, under the manager's lock, which is after this was set.
	source *source.Result

	// cancelSupervisor stops the goroutine enforcing TTL and idle.
	cancelSupervisor context.CancelFunc

	// ttlNudge wakes the supervisor when the deadline moves, so the one TTL
	// timer is re-armed rather than joined by a second one still armed for the
	// old deadline. Sent to without blocking: a nudge that does not fit is a
	// nudge already pending, and the supervisor re-reads the deadline when it
	// wakes rather than trusting what woke it.
	ttlNudge chan struct{}

	mu sync.Mutex
	// expiresAt is when the TTL kills it. Extend moves it, so it is guarded like
	// everything else here -- it was read without the lock while it was
	// write-once, and reading it that way now is a race.
	expiresAt time.Time
	// running counts execs in flight. The sandbox is idle at zero, and the
	// idle clock only runs then.
	running int
	// lastActive is when the last exec finished, which is when the idle clock
	// starts from.
	lastActive time.Time
	state      State
	reason     StopReason
	stoppedAt  time.Time

	// stopping is set the moment the supervisor decides the TTL is up, which is
	// earlier than the state saying stopped: stop samples the meters -- a cgroup
	// read plus two sysfs reads for the TAP counters -- before it takes this lock,
	// so the gap between the two is filesystem-IO wide rather than instruction
	// wide. Extend refuses in that gap, because granting an hour to a sandbox that
	// dies a millisecond later is the one outcome extension exists to prevent.
	stopping bool

	// storageInfo is the mount this sandbox actually got, set once by
	// serveStorage when storage is served and nil otherwise. It is read back out
	// through Info so a caller can see where their files persist.
	storageInfo *StorageInfo

	// finalStats is sampled just before the VM is killed. After that the cgroup
	// is gone and the meters can never be read again, so a caller asking what
	// their run cost would otherwise get nothing.
	finalStats runtime.Stats

	stopOnce sync.Once
}

func (s *Sandbox) ID() string { return s.id }

// Manager creates and supervises sandboxes.
type Manager struct {
	rt   runtime.Runtime
	logs *logstore.Store
	log  *slog.Logger

	// storage is the object store sandboxes may reach, or nil for none. Nil is
	// the default on purpose: storage costs money, and a deployment that has not
	// said which bucket to spend it in should not be guessing.
	storage storage.Backend

	// source fetches a caller's project, or nil when the node does not seed
	// sandboxes. Nil is the default: the fetch is an outbound request the daemon
	// makes on a caller's behalf, from outside the firewall it installs for its
	// guests, so it exists only where an operator has asked for it.
	source source.Preparer

	// seeding bounds how many sources are being fetched at once, node-wide. Nil
	// when source is nil, and created with it: see maxConcurrentSeeds.
	seeding chan struct{}

	// warm holds pre-booted pristine VMs, or nil when the pool is not configured.
	// It amortizes cold-boot latency without weakening the one-sandbox-per-task
	// rule, since each pooled VM is a distinct VM that has run no code.
	warm *warmPool

	// retention is how long a stopped sandbox is remembered before Sweep forgets
	// it. Zero remembers forever. Set in NewManager and never moved, so it is read
	// without the lock. See reap.go.
	retention time.Duration

	mu        sync.RWMutex
	sandboxes map[string]*Sandbox

	// live counts each tenant's sandboxes that are still running, including the
	// ones still booting. A counter rather than a walk of sandboxes: the walk
	// would have to read every sandbox's state under its own lock while holding
	// this one, and stopped sandboxes stay in the map for the retention window,
	// so most of what it read would not count. An entry is deleted at zero, which
	// is what stops this growing with every tenant that ever called.
	live map[string]int
}

// Option configures a Manager.
type Option func(*Manager)

// WithStorage gives sandboxes a place to put files.
//
// The backend is host-side and holds the credential. Nothing about it crosses
// into a guest: each sandbox reaches it through its own vsock socket, confined
// to its own prefix. See the storage package.
func WithStorage(backend storage.Backend) Option {
	return func(m *Manager) { m.storage = backend }
}

// maxConcurrentSeeds is how many sources this node fetches at once.
//
// A node-wide bound, because the per-tenant one is neither: -tenant-max-sandboxes
// defaults to unlimited, and the node's own admission -- slots, cpu, memory -- is
// inside rt.Create, which a seed happens before. Without this, one token holds N
// concurrent creates that are each downloading to this host's disk before anything
// has asked whether the node has room for a VM, and N is bounded by nothing.
//
// A small number on purpose. A seed is disk and bandwidth, and the node has one of
// each: more fetches at once do not finish sooner, they only multiply what is in
// flight when something goes wrong. Refused rather than queued, with the same 429
// a full node answers, because a queue behind a 60-second fetch is a request
// timeout dressed as progress.
const maxConcurrentSeeds = 8

// WithSource lets a create seed the sandbox from a caller-supplied URL.
//
// Off without it, and that is the default an upgrade keeps: the daemon fetching a
// URL a caller chose is an outbound request made by a process that sits outside
// the firewall it installs for its guests, so it is the operator's decision and
// not a feature that appears on its own. The Preparer carries the allowlist, the
// address policy and the size caps; nothing here second-guesses them.
func WithSource(p source.Preparer) Option {
	return func(m *Manager) {
		m.source = p
		m.seeding = make(chan struct{}, maxConcurrentSeeds)
	}
}

// WithWarmPool pre-boots and maintains a stock of pristine VMs for the given
// shapes, so a task of a matching shape is served without waiting for a cold
// boot. Each pooled VM is a distinct, never-used VM, so handing one to a task
// preserves the one-sandbox-per-task invariant. The pool is only consulted for
// sandboxes with no object storage, since storage binds to a sandbox's own
// prefix at boot and a pooled VM cannot carry it.
func WithWarmPool(specs []WarmSpec) Option {
	return func(m *Manager) { m.warm = newWarmPool(m.rt, m.log, specs, false) }
}

// WithWarmPoolSnapshots is WithWarmPool that fills the pool by restoring from a
// per-shape template snapshot when the runtime supports it (tens of milliseconds
// per VM instead of a cold boot), falling back to cold boots for any shape whose
// snapshot path fails.
func WithWarmPoolSnapshots(specs []WarmSpec) Option {
	return func(m *Manager) { m.warm = newWarmPool(m.rt, m.log, specs, true) }
}

// NewManager returns a Manager over a runtime.
func NewManager(rt runtime.Runtime, logs *logstore.Store, log *slog.Logger, opts ...Option) *Manager {
	m := &Manager{
		rt:        rt,
		logs:      logs,
		log:       log,
		sandboxes: make(map[string]*Sandbox),
		live:      make(map[string]int),
	}
	for _, opt := range opts {
		opt(m)
	}
	// After the options, because the floor is not the operator's to lower.
	m.retention = retentionFloor(m.retention, logs.Retention())
	m.warm.start() // no-op when unconfigured or nil
	return m
}

// StoragePrefix returns where a sandbox's files live in the bucket.
//
// One prefix per sandbox, which is the isolating default rather than the
// convenient one. Files outlive the VM -- the object is in S3, and the caller
// can fetch it long after the sandbox is gone -- but two sandboxes never share
// a namespace, so one cannot read or clobber another's output even if they
// belong to the same caller.
//
// A caller-chosen prefix is the obvious next step and is deliberately not here
// yet: it needs the mount to come from the authenticated token rather than from
// the request body, and getting that backwards would let a caller name someone
// else's prefix. That is an API change, not a default.
func StoragePrefix(sandboxID string) string {
	return path.Join("sandboxes", sandboxID)
}

// TenantPrefix returns where a tenant's files live in the bucket.
//
// One prefix per tenant, shared across every sandbox that tenant creates. This
// is the point of tenant scoping: files written by one run are still there for
// the next, because both resolve to the same place. The tenant comes from the
// caller's token, never from a request, so "the same place" is a place only
// that caller can name.
func TenantPrefix(tenant string) string {
	return path.Join("tenants", tenant)
}

// TenantUsage reports how many bytes a tenant is currently storing.
//
// It reads the object store directly, so it is the real total and not a cached
// guess. It returns 0 and no error when the node has no storage configured: a
// tenant on a storageless node uses nothing, which is true.
func (m *Manager) TenantUsage(ctx context.Context, tenant string) (int64, error) {
	if m.storage == nil {
		return 0, nil
	}
	return storage.UsageBytes(ctx, m.storage, TenantPrefix(tenant))
}

// ConcurrencyLimitError is a create refused because the tenant already has as
// many sandboxes running as it may.
//
// Typed, so the layer facing the caller can say whose limit was reached: "the
// node is full" and "you are full" both stop the same call, and only one of them
// is fixed by waiting for someone else.
type ConcurrencyLimitError struct {
	// Live is how many the tenant has running, counting sandboxes still booting.
	Live int
	// Max is how many it may have at once.
	Max int
}

func (e *ConcurrencyLimitError) Error() string {
	return fmt.Sprintf("this tenant has %d sandboxes running and may have %d at once", e.Live, e.Max)
}

// reserve takes one of the tenant's concurrency slots, or reports the cap.
//
// The count is incremented whether or not there is a limit, because the cheap
// bookkeeping is what makes the limit exact when one is set. An unlimited tenant
// costs one int.
func (m *Manager) reserve(tenant string, limit int) error {
	if tenant == "" {
		// Nobody to charge. Said out loud when a limit was asked for, because this
		// is the one place a cap is silently not applied: a caller that set
		// MaxConcurrent and left Tenant empty gets no limit and, without this, no
		// hint that it asked for one.
		if limit > 0 {
			m.log.Warn("a concurrency limit was set with no tenant to charge it to, so it does not apply",
				"limit", limit)
		}
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if n := m.live[tenant]; limit > 0 && n >= limit {
		return &ConcurrencyLimitError{Live: n, Max: limit}
	}
	m.live[tenant]++
	return nil
}

// release gives a tenant's slot back. Called when a sandbox stops, and when a
// create that reserved one never got a VM.
func (m *Manager) release(tenant string) {
	if tenant == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if n := m.live[tenant]; n > 1 {
		m.live[tenant] = n - 1
		return
	}
	// Deleted rather than left at zero: the map would otherwise hold an entry per
	// tenant that has ever created a sandbox, for the daemon's whole life.
	delete(m.live, tenant)
}

// Create starts a sandbox and begins enforcing its lifetime.
func (m *Manager) Create(ctx context.Context, spec Spec) (*Sandbox, error) {
	spec.applyDefaults()

	// Reserved before the VM boots, not counted after it has. Two creates that
	// both read a count and then raised it would both pass, so the cap would hold
	// everywhere except under the concurrency that makes it matter.
	if err := m.reserve(spec.Tenant, spec.MaxConcurrent); err != nil {
		return nil, err
	}

	// The source is fetched here, before a VM exists, and the slot above is
	// already held while it happens: a fetch is work done for this tenant, and one
	// that cost nothing against their cap would be a way to have the daemon make
	// unbounded outbound requests. See prepareSeed for why it is not done after
	// boot.
	seed, err := m.prepareSeed(ctx, spec)
	if err != nil {
		m.release(spec.Tenant)
		return nil, err
	}
	if seed != nil {
		defer seed.Close()
	}

	// Take the tags rather than borrow them. The caps were checked against this
	// map before the call, and a map the caller still holds can grow afterwards
	// -- which would make the check theatre, since what is kept here is kept for
	// the sandbox's whole life.
	spec.Tags = maps.Clone(spec.Tags)

	// The guest is told about storage on its kernel command line, which the
	// runtime builds inside Create -- so the decision has to be made now, before
	// the VM boots, not later in serveStorage when the socket is wired up. What
	// is known now is enough: whether this node has a bucket at all, and where
	// the mount should appear. serveStorage still owns the socket and the store.
	m.applyStorageBoot(&spec)

	// Serve from the warm pool when a pristine VM of this shape is ready; fall
	// back to a cold boot otherwise. The pool is skipped whenever the sandbox has
	// object storage, because storage is bound to the sandbox's own prefix on the
	// boot command line and a pre-booted VM cannot carry it.
	inst := m.acquireWarm(spec)
	if inst == nil {
		inst, err = m.rt.Create(ctx, spec.Spec)
		if err != nil {
			// Nothing booted, so the slot reserved above is nobody's. Holding it
			// would let a node that is failing to boot VMs slowly lock a tenant out
			// of the cap it never spent.
			m.release(spec.Tenant)
			return nil, err
		}
	}

	now := time.Now()
	sb := &Sandbox{
		id:         spec.ID,
		spec:       spec,
		inst:       inst,
		log:        m.log.With("sandbox", spec.ID),
		mgr:        m,
		createdAt:  now,
		expiresAt:  now.Add(spec.TTL),
		lastActive: now,
		state:      StateRunning,
		ttlNudge:   make(chan struct{}, 1),
	}

	// The supervisor outlives this call, so it gets its own context rather than
	// the caller's: Create's ctx is cancelled the moment it returns.
	supervisorCtx, cancel := context.WithCancel(context.Background())
	sb.cancelSupervisor = cancel
	go sb.supervise(supervisorCtx)

	m.serveStorage(supervisorCtx, sb, inst)

	// The seed goes in before the sandbox is published and before this returns, so
	// there is no moment at which a half-seeded sandbox is listed, retrievable, or
	// executable. A seeding failure destroys the VM: create's contract is a sandbox
	// ready to run commands or an error, and a fourth outcome would be a new state
	// for every caller and a new stop reason for three SDKs.
	if seed != nil {
		started := time.Now()
		err := m.writeSeed(ctx, sb, seed)
		if err != nil && sb.Reason() == ReasonExpired {
			// The write failed because the TTL took the sandbox away underneath it.
			// Reported as the ttl_seconds it is rather than as a write that broke,
			// which carries no failure of source's own and would be answered 500.
			err = &SeedExpiredError{TTL: spec.TTL, Elapsed: time.Since(started)}
		}
		if err == nil {
			// The TTL is time to run things in, and none of it was. The clock was
			// started at boot so a wedged seed still has a supervisor that will take
			// the VM away; restarting it here is what stops the seed's own duration
			// from being charged to the caller, who could otherwise be handed a
			// sandbox with an expires_at that elapsed during their create.
			err = sb.startTTLAfterSeed(spec.TTL, started)
		}
		if err != nil {
			// Its own deadline, and not the caller's: a client that hung up
			// mid-write must not leave a VM behind holding a TAP device and a
			// network slot. Stop releases the tenant's slot, and the sandbox was
			// never in the map, so nothing else has to be unwound.
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
			_ = sb.Stop(stopCtx, ReasonFailed)
			cancel()
			sb.log.Error("sandbox destroyed because seeding failed", "err", err)
			return nil, err
		}
		sb.source = ptrTo(seed.Result())
	}

	m.mu.Lock()
	m.sandboxes[spec.ID] = sb
	m.mu.Unlock()

	sb.log.Info("sandbox created", "ttl", spec.TTL, "idle_timeout", spec.IdleTimeout)
	return sb, nil
}

// ptrTo keeps one copy of a seed's result on the sandbox. A value rather than the
// Prepared it came from, which is closed the moment Create returns.
func ptrTo[T any](v T) *T { return &v }

// startTTLAfterSeed restarts the TTL clock now that the sandbox is the caller's,
// and refuses if the sandbox did not survive the seed.
//
// Both under one lock, deliberately. Checking the state and then setting the
// deadline as two steps leaves the window Create used to have: the TTL fires
// between them, Stop runs, and Create publishes a stopped sandbox and answers 201
// -- the fourth outcome create's contract says cannot exist. claimExpiry sets
// stopping under this same lock, so seeing neither it nor a state change here means
// the supervisor has not claimed the kill and cannot now, because the deadline it
// would re-read has already moved.
func (s *Sandbox) startTTLAfterSeed(ttl time.Duration, started time.Time) error {
	s.mu.Lock()
	if s.state != StateRunning || s.stopping {
		s.mu.Unlock()
		return &SeedExpiredError{TTL: ttl, Elapsed: time.Since(started)}
	}
	s.expiresAt = time.Now().Add(ttl)
	s.mu.Unlock()

	select {
	case s.ttlNudge <- struct{}{}:
	default: // one is already pending, and it will read the deadline set above
	}
	return nil
}

// applyStorageBoot decides, before the VM boots, whether the guest is told to
// mount storage and where. It mirrors serveStorage's choice of mount so the
// path the guest is handed is the path the host will serve under, but it reads
// only what is known this early: the node's backend and the caller's spec.
func (m *Manager) applyStorageBoot(spec *Spec) {
	if m.storage == nil {
		return // no bucket on this node; the guest is told nothing and mounts nothing
	}
	mount := storage.Mount{Prefix: StoragePrefix(spec.ID)}
	if spec.Storage != nil {
		mount = *spec.Storage
	}
	spec.Spec.StorageMount = mount.MountPoint()
	spec.Spec.StorageReadOnly = mount.ReadOnly
}

// acquireWarm returns a pre-booted VM for this spec when the warm pool has a
// compatible one, or nil to fall back to a cold boot. It applies only when the
// sandbox has no object storage: storage binds to the sandbox's own prefix at
// boot, so a VM pre-booted for the pool cannot carry it.
func (m *Manager) acquireWarm(spec Spec) runtime.Instance {
	if m.warm == nil || spec.Spec.StorageMount != "" {
		return nil
	}
	inst := m.warm.checkout(warmKeyOf(spec.Spec))
	if inst != nil {
		m.log.Debug("served from warm pool", "sandbox", spec.ID, "image", spec.Image)
	}
	return inst
}

// serveStorage starts this sandbox's private storage server, if it has one.
//
// The server is per-sandbox and so is the listener it runs on, which is the
// whole of the access control: a request arriving on that socket came from that
// sandbox, because no other sandbox can reach it. This function is the only
// place the two are paired, and pairing them wrongly -- one server for many
// sandboxes, or one listener shared -- would hand every sandbox everyone
// else's files without failing anything.
func (m *Manager) serveStorage(ctx context.Context, sb *Sandbox, inst runtime.Instance) {
	if m.storage == nil {
		return // no bucket configured; sandboxes simply have no storage
	}

	l := inst.HostListener()
	if l == nil {
		// The backend could not offer one. The sandbox still works; it just has
		// nowhere to put files, which beats refusing to start it.
		sb.log.Warn("sandbox has no inbound socket; storage is unavailable to it")
		return
	}

	// A mount from the caller's identity when there is one, a per-sandbox
	// namespace when there is not. The nil case is not a fallback for errors --
	// it is exactly what an unauthenticated daemon should do: isolate every
	// sandbox, since there is no tenant to attribute files to.
	mount := storage.Mount{Prefix: StoragePrefix(sb.id)}
	if sb.spec.Storage != nil {
		mount = *sb.spec.Storage
	}

	// Record what the sandbox got, so Info can report the namespace a caller's
	// files persist under. path.Base turns "tenants/t_abc" into "t_abc" and
	// "sandboxes/sb_x" into "sb_x": the meaningful leaf either way.
	info := &StorageInfo{
		Namespace: path.Base(mount.Prefix),
		ReadOnly:  mount.ReadOnly,
		MountPath: mount.MountPoint(),
	}
	if mount.Tenant.MaxBytes > 0 {
		info.MaxBytes = mount.Tenant.MaxBytes
		info.Policy = string(mount.Tenant.OnFull)
	}
	sb.mu.Lock()
	sb.storageInfo = info
	sb.mu.Unlock()

	store := storage.NewStore(m.storage, mount)

	go func() {
		// Serve returns when the listener closes, which the runtime does on stop.
		// That is the normal end of this goroutine, not a failure.
		if err := storage.Serve(ctx, l, store, sb.log); err != nil {
			sb.log.Error("storage server stopped", "err", err)
		}
	}()
}

// DefaultVCPUs and DefaultMemMiB size a sandbox whose spec does not.
//
// They live here, in the one place every caller passes through, rather than in
// each caller. A zero vCPU count reaches Firecracker as a config error and a
// dead VM, so "every caller remembers to set it" is not a workable contract --
// the API forgot, and the failure surfaced as a boot error naming no field.
const (
	DefaultVCPUs  = 1
	DefaultMemMiB = 256
)

func (s *Spec) applyDefaults() {
	if s.TTL == 0 {
		s.TTL = DefaultTTL
	}
	if s.TTL > MaxTTL {
		s.TTL = MaxTTL
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = DefaultIdleTimeout
	}
	if s.VCPUs <= 0 {
		s.VCPUs = DefaultVCPUs
	}
	if s.MemMiB <= 0 {
		s.MemMiB = DefaultMemMiB
	}
}

// Get returns a sandbox by ID.
func (m *Manager) Get(id string) (*Sandbox, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sb, ok := m.sandboxes[id]
	return sb, ok
}

// List returns every sandbox the manager knows, including stopped ones that
// have not yet been forgotten.
func (m *Manager) List() []*Sandbox {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*Sandbox, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		out = append(out, sb)
	}
	return out
}

// Close stops every sandbox.
func (m *Manager) Close(ctx context.Context) error {
	// Drain the warm pool first, so it stops minting VMs while we are tearing the
	// live ones down.
	m.warm.close(ctx)

	var errs []error
	for _, sb := range m.List() {
		if err := sb.Stop(ctx, ReasonStopped); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// supervise enforces the TTL and the idle timeout. It exits when the sandbox
// stops.
func (s *Sandbox) supervise(ctx context.Context) {
	ttl := time.NewTimer(time.Until(s.deadline()))
	defer ttl.Stop()

	// The idle check polls rather than arming a timer per exec: an exec that
	// finishes resets lastActive, and a tick that reads it is both simpler and
	// impossible to leave armed by mistake.
	idleTick := time.NewTicker(idleCheckInterval)
	defer idleTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-s.ttlNudge:
			// The deadline moved. Re-arm this timer instead of arming another:
			// a second one would still fire at the old deadline and kill the
			// sandbox the extension just bought time for. Reset alone is enough
			// -- a timer's channel holds no stale value to drain.
			ttl.Reset(time.Until(s.deadline()))

		case <-ttl.C:
			// A nudge and this firing can be ready in the same select, which
			// picks either. Re-read the deadline rather than trusting the wakeup,
			// and claim the kill in the same breath: killing a sandbox whose
			// extension was already granted is the one outcome extension exists
			// to prevent.
			remaining, expired := s.claimExpiry()
			if !expired {
				ttl.Reset(remaining)
				continue
			}
			s.log.Info("sandbox reached its ttl",
				"lifetime", time.Since(s.createdAt).Round(time.Second))
			// Its own context: the supervisor's is about to be cancelled by the
			// very stop we are calling.
			stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			_ = s.Stop(stopCtx, ReasonExpired)
			cancel()
			return

		case <-idleTick.C:
			if s.spec.IdleTimeout < 0 {
				continue // idle reclaim disabled
			}
			if !s.idleFor(s.spec.IdleTimeout) {
				continue
			}
			s.log.Info("sandbox idle, reclaiming", "idle_timeout", s.spec.IdleTimeout)
			stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			_ = s.Stop(stopCtx, ReasonIdle)
			cancel()
			return
		}
	}
}

const (
	idleCheckInterval = 5 * time.Second
	stopTimeout       = 30 * time.Second
)

// idleFor reports whether nothing has run for at least d.
func (s *Sandbox) idleFor(d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// An exec in flight means the sandbox is working, however long it has been
	// since the last one finished. Killing a sandbox mid-exec for idleness
	// would be exactly wrong.
	if s.running > 0 {
		return false
	}
	return time.Since(s.lastActive) >= d
}

// deadline is when the TTL will kill the sandbox.
func (s *Sandbox) deadline() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiresAt
}

// claimExpiry reports whether the TTL is up, and claims the kill when it is.
//
// Deciding and claiming are one critical section because Extend moves the
// deadline under this same lock. Read the deadline, decide, then flip the state
// as three separate steps and there is a window in between where Extend still
// sees a running sandbox with a deadline it may move: the caller is told 200 with
// an hour on the clock, and the sandbox they were promised is killed immediately
// afterwards. Claiming here means Extend's own guard refuses everything from this
// moment on, and stop overwrites the claim with StateStopped as it already does.
func (s *Sandbox) claimExpiry() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if remaining := time.Until(s.expiresAt); remaining > 0 {
		return remaining, false
	}
	s.stopping = true
	return 0, true
}

// TTLLimitError is an extension refused for reaching past the sandbox's maximum
// lifetime, with the room that is left.
//
// Refused rather than clamped: a caller told 200 for an hour they did not get
// plans for an hour, and finds out when their work is killed halfway. An error
// they can read is worse news, delivered while they can still act on it.
type TTLLimitError struct {
	// Requested is the extension asked for, counted from now.
	Requested time.Duration
	// Remaining is the largest extension that would be accepted: Max less the
	// time the sandbox has already lived. Zero once the sandbox has lived Max.
	Remaining time.Duration
	// Max is the maximum lifetime, measured from creation.
	Max time.Duration
}

func (e *TTLLimitError) Error() string {
	return fmt.Sprintf("an extension of %s would outlive the maximum lifetime of %s from creation; %s remains",
		e.Requested, e.Max, e.Remaining)
}

// ExpiredError is an extension refused because the deadline had already passed
// and the supervisor had claimed the kill.
//
// Typed because the caller-facing layer cannot work this out by reading the
// state: the state still says running for as long as the kill takes, and that gap
// is exactly the window this error reports. It means the same thing to a caller as
// extending an already-stopped sandbox, which is what they would have got a
// millisecond later.
type ExpiredError struct{ ID string }

func (e *ExpiredError) Error() string {
	return fmt.Sprintf("sandbox %s reached its ttl and is stopping", e.ID)
}

// Extend pushes the TTL deadline out and re-arms the timer enforcing it.
//
// It never brings a deadline forward: the new one is the later of what it
// already was and now+ttl. Two callers heartbeating the same sandbox at
// different intervals would otherwise have the shorter one cut the longer one
// short, and it is what makes a retry of this call harmless.
//
// The deadline is bounded by MaxTTL measured from creation, never from now,
// which is what makes extension buy time and never immortality: a caller that
// heartbeats forever still ends up with a sandbox that dies. Measured from now
// the bound would move with every call and the lifetime hostile code is held to
// would stop existing. Asking past it returns *TTLLimitError.
//
// Extending a stopped sandbox is refused, not quietly accepted -- there is no
// deadline left to move, and a caller heartbeating something already gone
// should learn it here rather than from an exec that has nowhere to run.
//
// The idle timeout is untouched. A long TTL does not keep an idle sandbox alive.
func (s *Sandbox) Extend(ttl time.Duration) (time.Time, error) {
	s.mu.Lock()
	if s.state != StateRunning {
		state, reason := s.state, s.reason
		s.mu.Unlock()
		return time.Time{}, fmt.Errorf("sandbox %s is %s (%s)", s.id, state, reason)
	}
	// Claimed by the supervisor and not yet stopped. Refused rather than granted,
	// because the deadline this would move belongs to a sandbox already on its way
	// out: see claimExpiry.
	if s.stopping {
		s.mu.Unlock()
		return time.Time{}, &ExpiredError{ID: s.id}
	}
	expires, err := extendedDeadline(time.Now(), s.createdAt, s.expiresAt, ttl, MaxTTL)
	if err != nil {
		s.mu.Unlock()
		return time.Time{}, fmt.Errorf("sandbox %s: %w", s.id, err)
	}
	s.expiresAt = expires
	s.mu.Unlock()

	select {
	case s.ttlNudge <- struct{}{}:
	default: // one is already pending, and it will read the deadline set above
	}

	s.log.Info("sandbox ttl extended", "requested", ttl, "expires", expires)
	return expires, nil
}

// extendedDeadline is the deadline an extension produces, or *TTLLimitError when
// the request reaches past the maximum lifetime.
//
// It is separate from Extend because it is the whole of the rule and none of the
// plumbing, and because it takes created and now as arguments rather than
// reading the clock: the bound is measured from creation, and that is only worth
// asserting from partway through a sandbox's life.
func extendedDeadline(now, created, expires time.Time, ttl, maxLifetime time.Duration) (time.Time, error) {
	if ttl <= 0 {
		return time.Time{}, fmt.Errorf("an extension must be positive, got %s", ttl)
	}

	limit := created.Add(maxLifetime)
	wanted := now.Add(ttl)
	if wanted.After(limit) {
		return time.Time{}, &TTLLimitError{
			Requested: ttl,
			Remaining: max(limit.Sub(now), 0).Round(time.Second),
			Max:       maxLifetime,
		}
	}

	// Later of the two, never the shorter: a caller asking for less than the
	// sandbox already has is asking for a floor, not a ceiling.
	if wanted.Before(expires) {
		return expires, nil
	}
	return wanted, nil
}

// Exec runs a command, streaming frames to onFrame and recording everything in
// the log store.
//
// onFrame may be nil, for a caller that wants the output kept but not streamed;
// the record is retrievable afterwards either way. That is the point of storing
// on the host: a caller can fire an exec, disconnect, and still collect the
// result -- even if the sandbox is killed in the meantime.
func (s *Sandbox) Exec(ctx context.Context, req protocol.ExecRequest, onFrame func(protocol.Frame) error) error {
	if req.Timeout == 0 {
		// An exec with no deadline outlives its caller's interest and holds the
		// sandbox open against its idle timeout forever.
		req.Timeout = DefaultExecTimeout
	}

	// The sandbox's own environment underlies every exec; this call's Env wins
	// where they overlap, since it is the more specific of the two.
	req.Env = mergeEnv(s.spec.Env, req.Env)

	if err := s.beginExec(); err != nil {
		return err
	}
	defer s.endExec()

	s.mgr.logs.Begin(req.ID, s.id, req.Cmd, req.Args)

	err := s.inst.Client().Exec(ctx, req, func(f protocol.Frame) error {
		// Record before forwarding: if the caller's handler fails or their
		// connection is gone, the output is still captured.
		s.mgr.logs.Append(req.ID, f)
		if onFrame == nil {
			return nil
		}
		return onFrame(f)
	})

	if err != nil {
		// Distinguish the caller giving up from the sandbox being taken away.
		// Both surface here as a broken stream, but they mean opposite things:
		// one is the caller's own doing, the other is ours.
		if s.State() != StateRunning {
			s.mgr.logs.Finish(req.ID, logstore.StatusVanished)
		} else if ctx.Err() != nil {
			s.mgr.logs.Finish(req.ID, logstore.StatusAborted)
		}
		return err
	}
	return nil
}

// StartExec runs a command in the background and returns as soon as it has
// started.
//
// The execution is tied to the sandbox's life, not to the caller's request.
// That is the whole difference from Exec, and it matters: an exec bound to an
// HTTP request is killed when that connection drops, so a caller whose network
// blinked loses a job that was running perfectly well. Here the request can go
// away and the work continues, to be streamed or collected afterwards.
//
// It returns once the exec is registered, so a caller who immediately asks to
// stream it will find it.
func (s *Sandbox) StartExec(req protocol.ExecRequest) error {
	if req.Timeout == 0 {
		req.Timeout = DefaultExecTimeout
	}
	req.Env = mergeEnv(s.spec.Env, req.Env)

	if err := s.beginExec(); err != nil {
		return err
	}

	// Registered before returning, not inside the goroutine: otherwise this
	// returns an ID that a caller can legitimately race to stream, and lose.
	s.mgr.logs.Begin(req.ID, s.id, req.Cmd, req.Args)

	go func() {
		defer s.endExec()

		// Deliberately not the caller's context. The sandbox's TTL, the idle
		// reclaim and the exec's own timeout already bound this; the caller's
		// connection is not one of the things that should.
		err := s.inst.Client().Exec(context.Background(), req, func(f protocol.Frame) error {
			s.mgr.logs.Append(req.ID, f)
			return nil
		})
		if err == nil {
			return
		}

		// The stream broke without an exit status. The only question worth
		// answering is whose fault it was, because the record is what a caller
		// will read later.
		if s.State() != StateRunning {
			s.mgr.logs.Finish(req.ID, logstore.StatusVanished)
			return
		}
		s.log.Warn("exec ended without an exit status", "exec", req.ID, "err", err)
		s.mgr.logs.Finish(req.ID, logstore.StatusFailed)
	}()

	return nil
}

// StreamExec returns an exec's frames: what it has printed, then what it
// prints next. The channel closes when the exec ends.
func (s *Sandbox) StreamExec(execID string) (<-chan protocol.Frame, bool) {
	return s.mgr.logs.Subscribe(execID)
}

// FinishExec records an ending the process itself could not report -- a caller
// cancelling it, most of all: a SIGKILLed process never gets to say why it died.
//
// It does not override an exec that already reported its own exit. The agent's
// account of how a process ended always beats an inference drawn from outside.
func (s *Sandbox) FinishExec(execID string, status logstore.Status) {
	s.mgr.logs.Finish(execID, status)
}

// mergeEnv layers an exec's own variables over the sandbox's.
//
// Precedence runs specific-over-general the whole way down: exec beats sandbox,
// sandbox beats the image's ENV, and the image beats init's fallbacks. A caller
// overriding PATH for one command must not have to think about what the image
// set.
//
// Neither input is mutated: the sandbox's map is shared by every exec, and
// writing into it would leak one call's variables into the next.
func mergeEnv(sandboxEnv, execEnv map[string]string) map[string]string {
	if len(sandboxEnv) == 0 {
		return execEnv
	}
	if len(execEnv) == 0 {
		// Copy anyway: the caller may hold on to the returned map, and it must
		// not be a handle on the sandbox's own.
		out := make(map[string]string, len(sandboxEnv))
		for k, v := range sandboxEnv {
			out[k] = v
		}
		return out
	}

	out := make(map[string]string, len(sandboxEnv)+len(execEnv))
	for k, v := range sandboxEnv {
		out[k] = v
	}
	for k, v := range execEnv {
		out[k] = v
	}
	return out
}

// beginExec registers an exec in flight, refusing if the sandbox is gone.
func (s *Sandbox) beginExec() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateRunning {
		return fmt.Errorf("sandbox %s is %s (%s)", s.id, s.state, s.reason)
	}
	s.running++
	return nil
}

func (s *Sandbox) endExec() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running--
	// The idle clock starts from the last exec to finish, not the last to start.
	s.lastActive = time.Now()
}

// A transfer counts as activity, exactly like an exec.
//
// Bracketing these with beginExec/endExec is what stops the idle reclaim killing a
// sandbox that is busy: staging a project is minutes of uploads with nothing run
// in between, and a sandbox measured only by its execs is idle for all of it. It
// also resets the idle clock when the transfer ends, so the two minutes start from
// the last file rather than from before the first.

// WriteFile uploads content into the sandbox.
func (s *Sandbox) WriteFile(ctx context.Context, path string, content io.Reader, mode string) error {
	if err := s.beginExec(); err != nil {
		return err
	}
	defer s.endExec()
	return s.inst.Client().WriteFile(ctx, path, content, mode)
}

// ReadFile downloads a file from the sandbox. The caller must close the reader.
//
// The sandbox stays busy until that Close rather than until this returns: the
// transfer is the reading, and a multi-gigabyte artifact takes longer to stream
// than the idle timeout allows. Returning here and calling it finished would have
// the VM reclaimed out from under the response body.
func (s *Sandbox) ReadFile(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := s.beginExec(); err != nil {
		return nil, err
	}
	rc, err := s.inst.Client().ReadFile(ctx, path)
	if err != nil {
		s.endExec()
		return nil, err
	}
	return &activeRead{ReadCloser: rc, sb: s}, nil
}

// activeRead holds the sandbox busy for as long as its caller is still reading.
type activeRead struct {
	io.ReadCloser
	sb   *Sandbox
	once sync.Once
}

func (a *activeRead) Close() error {
	// Once: a second Close would decrement the in-flight count twice, and a count
	// below zero is a sandbox nothing can ever reclaim.
	a.once.Do(a.sb.endExec)
	return a.ReadCloser.Close()
}

// Mkdir creates a directory inside the sandbox.
func (s *Sandbox) Mkdir(ctx context.Context, path string) error {
	if err := s.beginExec(); err != nil {
		return err
	}
	defer s.endExec()
	return s.inst.Client().Mkdir(ctx, path)
}

// Signal delivers a signal to a running exec's process group.
func (s *Sandbox) Signal(ctx context.Context, execID, signal string) error {
	if err := s.requireRunning(); err != nil {
		return err
	}
	return s.inst.Client().Signal(ctx, execID, signal)
}

// requireRunning rejects work aimed at a sandbox that is gone, with a reason
// rather than whatever obscure transport error the dead VM would produce.
func (s *Sandbox) requireRunning() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateRunning {
		return fmt.Errorf("sandbox %s is %s (%s)", s.id, s.state, s.reason)
	}
	return nil
}

// Logs returns the record for one exec, whether or not the sandbox still exists.
func (s *Sandbox) Logs(execID string) (logstore.Record, bool) {
	return s.mgr.logs.Get(execID)
}

// AllLogs returns every exec this sandbox has run.
func (s *Sandbox) AllLogs() []logstore.Record {
	return s.mgr.logs.ListSandbox(s.id)
}

// Info reports the sandbox's status and meters.
func (s *Sandbox) Info() Info {
	s.mu.Lock()
	state, reason, stoppedAt, final := s.state, s.reason, s.stoppedAt, s.finalStats
	storageInfo := s.storageInfo
	expiresAt := s.expiresAt
	s.mu.Unlock()

	info := Info{
		ID:        s.id,
		State:     state,
		Reason:    reason,
		CreatedAt: s.createdAt,
		StoppedAt: stoppedAt,
		ExpiresAt: expiresAt,
		Image:     s.spec.Image,
		VCPUs:     s.spec.VCPUs,
		MemMiB:    s.spec.MemMiB,
		Network:   s.spec.Network,
		Storage:   storageInfo,
		Source:    s.source,
		Tags:      s.Tags(),
	}

	if state == StateRunning {
		if stats, err := s.inst.Stats(); err == nil {
			info.Stats = stats
		}
		return info
	}

	// Stopped: the cgroup is gone, so serve the snapshot taken before the kill.
	// Reading live meters here would fail, and reporting zeros would tell a
	// caller their run was free.
	info.Stats = final
	return info
}

// Stop shuts the sandbox down. It is idempotent, and the first reason wins:
// a TTL expiry followed by a caller's stop is still an expiry.
func (s *Sandbox) Stop(ctx context.Context, reason StopReason) error {
	var err error
	s.stopOnce.Do(func() {
		err = s.stop(ctx, reason)
	})
	return err
}

func (s *Sandbox) stop(ctx context.Context, reason StopReason) error {
	// Sample the meters while they still exist. Once the VM is killed its
	// cgroup goes with it and the final cost is unrecoverable.
	final, statsErr := s.inst.Stats()

	s.mu.Lock()
	s.state = StateStopped
	s.reason = reason
	s.stoppedAt = time.Now()
	if statsErr == nil {
		s.finalStats = final
	}
	s.mu.Unlock()

	// The tenant's slot goes back here, not when the record is forgotten: the
	// sandbox stays listed for the retention window, and a caller who deleted one
	// must be able to create the next immediately. Stop runs once (stopOnce), so
	// this cannot double-release. Called with s.mu released, since it takes the
	// manager's lock and nothing else in this package holds the two together.
	s.mgr.release(s.spec.Tenant)

	// Any exec still streaming is about to have its VM pulled out from under
	// it. Mark those records now so a caller polling for a result is told the
	// sandbox vanished rather than waiting for an exit that will never come.
	s.mgr.logs.SandboxGone(s.id)

	s.cancelSupervisor()

	err := s.inst.Stop(ctx)

	s.log.Info("sandbox stopped",
		"reason", reason,
		"lifetime", time.Since(s.createdAt).Round(time.Millisecond),
		"active_cpu", final.ActiveCPU.Round(time.Millisecond),
		"idle", final.Idle.Round(time.Millisecond),
		"mem_peak_mb", final.MemoryPeak/(1024*1024))

	return err
}

// State returns the sandbox's current state.
func (s *Sandbox) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Reason returns why the sandbox stopped, or empty while it runs.
func (s *Sandbox) Reason() StopReason {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// Tenant is who the sandbox is attributed to, or empty for nobody.
//
// Fixed at creation and never from a request body, so no lock, like Tags. It is
// what a caller-facing layer compares an authenticated identity against before it
// hands the sandbox over: a sandbox ID is not a capability, and a sandbox with no
// tenant belongs to no caller.
func (s *Sandbox) Tenant() string { return s.spec.Tenant }

// Tags returns the sandbox's labels, or nil when it has none.
//
// A copy, and no lock: tags are fixed at creation, so there is nothing to guard,
// but the map behind them is the one every tag filter reads. Handing it out
// would let anyone holding the result relabel the sandbox for everybody.
func (s *Sandbox) Tags() map[string]string {
	if len(s.spec.Tags) == 0 {
		return nil
	}
	return maps.Clone(s.spec.Tags)
}
