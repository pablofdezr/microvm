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
	"path"
	"sync"
	"time"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
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

	// Storage is where this sandbox's files go, or nil to fall back to a
	// per-sandbox namespace. It is set by the layer that knows who the caller is,
	// from the caller's authenticated identity and never from the request body:
	// the prefix names a tenant, and letting a body choose it would let one
	// tenant name another's. See the API's create handler.
	Storage *storage.Mount
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

	// ExpiresAt is when the TTL will kill it.
	ExpiresAt time.Time

	Image   string
	VCPUs   int
	MemMiB  int
	Network bool

	// Storage describes where this sandbox's files go, or nil when the node has
	// no object storage and the sandbox therefore has none.
	Storage *StorageInfo

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
	expiresAt time.Time

	// cancelSupervisor stops the goroutine enforcing TTL and idle.
	cancelSupervisor context.CancelFunc

	mu sync.Mutex
	// running counts execs in flight. The sandbox is idle at zero, and the
	// idle clock only runs then.
	running int
	// lastActive is when the last exec finished, which is when the idle clock
	// starts from.
	lastActive time.Time
	state      State
	reason     StopReason
	stoppedAt  time.Time

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

	// warm holds pre-booted pristine VMs, or nil when the pool is not configured.
	// It amortizes cold-boot latency without weakening the one-sandbox-per-task
	// rule, since each pooled VM is a distinct VM that has run no code.
	warm *warmPool

	mu        sync.RWMutex
	sandboxes map[string]*Sandbox
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
	}
	for _, opt := range opts {
		opt(m)
	}
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

// Create starts a sandbox and begins enforcing its lifetime.
func (m *Manager) Create(ctx context.Context, spec Spec) (*Sandbox, error) {
	spec.applyDefaults()

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
		var err error
		inst, err = m.rt.Create(ctx, spec.Spec)
		if err != nil {
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
	}

	// The supervisor outlives this call, so it gets its own context rather than
	// the caller's: Create's ctx is cancelled the moment it returns.
	supervisorCtx, cancel := context.WithCancel(context.Background())
	sb.cancelSupervisor = cancel
	go sb.supervise(supervisorCtx)

	m.serveStorage(supervisorCtx, sb, inst)

	m.mu.Lock()
	m.sandboxes[spec.ID] = sb
	m.mu.Unlock()

	sb.log.Info("sandbox created", "ttl", spec.TTL, "idle_timeout", spec.IdleTimeout)
	return sb, nil
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
	ttl := time.NewTimer(time.Until(s.expiresAt))
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

		case <-ttl.C:
			s.log.Info("sandbox reached its ttl", "ttl", s.spec.TTL)
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

// WriteFile uploads content into the sandbox.
func (s *Sandbox) WriteFile(ctx context.Context, path string, content io.Reader, mode string) error {
	if err := s.requireRunning(); err != nil {
		return err
	}
	return s.inst.Client().WriteFile(ctx, path, content, mode)
}

// ReadFile downloads a file from the sandbox. The caller must close the reader.
func (s *Sandbox) ReadFile(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := s.requireRunning(); err != nil {
		return nil, err
	}
	return s.inst.Client().ReadFile(ctx, path)
}

// Mkdir creates a directory inside the sandbox.
func (s *Sandbox) Mkdir(ctx context.Context, path string) error {
	if err := s.requireRunning(); err != nil {
		return err
	}
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
	s.mu.Unlock()

	info := Info{
		ID:        s.id,
		State:     state,
		Reason:    reason,
		CreatedAt: s.createdAt,
		StoppedAt: stoppedAt,
		ExpiresAt: s.expiresAt,
		Image:     s.spec.Image,
		VCPUs:     s.spec.VCPUs,
		MemMiB:    s.spec.MemMiB,
		Network:   s.spec.Network,
		Storage:   storageInfo,
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
