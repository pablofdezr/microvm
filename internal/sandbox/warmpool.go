package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pablofdezr/microvm/internal/runtime"
)

// WarmSpec configures how many pristine VMs to keep pre-booted for one shape.
// A Manager built WithWarmPool holds that stock so a task of a matching shape
// skips the cold boot entirely.
type WarmSpec struct {
	Image   string
	VCPUs   int
	MemMiB  int
	Network bool
	// Count is how many booted-but-unused VMs to keep ready for this shape.
	Count int
}

// warmKey identifies a boot-compatible VM shape. It deliberately excludes the
// sandbox ID (the guest sees it only as a cosmetic hostname) and the environment
// (the host applies that on each exec, never baking it into the VM), so one
// pre-booted VM serves any task of the same shape. Every field is comparable, so
// it keys a map directly.
type warmKey struct {
	image   string
	vcpus   int
	memMiB  int
	network bool
	limits  runtime.Limits
}

func warmKeyOf(s runtime.Spec) warmKey {
	return warmKey{
		image:   s.Image,
		vcpus:   s.VCPUs,
		memMiB:  s.MemMiB,
		network: s.Network,
		limits:  s.Limits,
	}
}

// warmPool keeps a stock of pristine, pre-booted VMs so a task can skip the cold
// boot. Every VM in it is a *distinct* real microVM that has run no code, so
// handing one to a task keeps the one-sandbox-per-task invariant intact -- and,
// unlike a single snapshot restored many times, these never share a network
// identity or any runtime state, so there is no MAC/IP collision or CSPRNG-reuse
// hazard to fix up afterwards. A VM is handed out at most once; the pool refills
// in the background up to each shape's target.
type warmPool struct {
	rt  runtime.Runtime
	log *slog.Logger

	targets map[warmKey]int          // desired ready count per shape
	specs   map[warmKey]runtime.Spec // boot template per shape

	// snap, when set, fills the pool by restoring from a per-shape template
	// snapshot instead of cold-booting each VM -- tens of milliseconds instead of
	// hundreds. A snapshot failure for a shape falls the shape back to cold boots.
	snap runtime.Snapshotter

	seq  atomic.Uint64
	hits atomic.Uint64

	mu        sync.Mutex
	ready     map[warmKey][]runtime.Instance
	templates map[warmKey]runtime.SnapshotRef // shape -> its template snapshot
	snapFails map[warmKey]*snapFailure        // shapes whose snapshot path failed

	kick   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	closeOnce sync.Once
}

func newWarmPool(rt runtime.Runtime, log *slog.Logger, specs []WarmSpec, useSnapshots bool) *warmPool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &warmPool{
		rt:        rt,
		log:       log,
		targets:   make(map[warmKey]int),
		specs:     make(map[warmKey]runtime.Spec),
		ready:     make(map[warmKey][]runtime.Instance),
		templates: make(map[warmKey]runtime.SnapshotRef),
		snapFails: make(map[warmKey]*snapFailure),
		kick:      make(chan struct{}, 1),
		ctx:       ctx,
		cancel:    cancel,
	}
	// Restore-from-snapshot only when asked for it AND the backend can do it, so
	// a cold-boot pool (and its tests) behaves exactly as before.
	if useSnapshots {
		if s, ok := rt.(runtime.Snapshotter); ok {
			p.snap = s
		}
	}
	for _, ws := range specs {
		if ws.Count <= 0 || ws.Image == "" {
			continue
		}
		tmpl := runtime.Spec{
			Image:   ws.Image,
			VCPUs:   ws.VCPUs,
			MemMiB:  ws.MemMiB,
			Network: ws.Network,
		}
		k := warmKeyOf(tmpl)
		p.targets[k] += ws.Count
		p.specs[k] = tmpl
	}
	return p
}

// start launches the background refill loop. It is a no-op when nothing is
// configured, so a Manager with an empty warm pool costs nothing at all.
func (p *warmPool) start() {
	if p == nil || len(p.targets) == 0 {
		return
	}
	p.wg.Add(1)
	go p.run()
}

func (p *warmPool) run() {
	defer p.wg.Done()
	for {
		if p.ctx.Err() != nil {
			return
		}
		if p.refillOne() {
			continue // minted one; there may be more to do, so loop right away
		}
		// Every shape is at target. Wait for a checkout to drain one, or poll
		// slowly in case a mint failed and should be retried.
		select {
		case <-p.ctx.Done():
			return
		case <-p.kick:
		case <-time.After(3 * time.Second):
		}
	}
}

// refillOne tops up one VM for the shape furthest below target, or returns false
// when every shape is stocked. It restores from a template snapshot when that is
// available and working for the shape, and cold-boots otherwise.
func (p *warmPool) refillOne() bool {
	k, spec, want := p.pickDeficit()
	if !want {
		return false
	}
	// A networked shape is never a snapshot shape: a snapshot carries the guest's
	// MAC and IP in its memory image, so every restore of one template would claim
	// the same address. The runtime refuses it; not asking is how the pool avoids
	// one guaranteed failure and cooldown per networked shape.
	if p.snap != nil && !k.network && !p.isColdOnly(k) {
		if p.refillViaSnapshot(k, spec) {
			return true
		}
		// No VM came of it. If the shape has just fallen back to cold boots, do
		// that now: leaving it short until the next poll would spend three seconds
		// with an empty pool on the way to the boot we already know we want.
		if !p.isColdOnly(k) {
			return false
		}
	}
	return p.refillViaColdBoot(k, spec)
}

func (p *warmPool) refillViaColdBoot(k warmKey, spec runtime.Spec) bool {
	// A fresh id per warm VM; the guest sees it only as a hostname, which the
	// task that adopts the VM supersedes in every way that matters.
	spec.ID = fmt.Sprintf("warm-%d", p.seq.Add(1))

	inst, err := p.rt.Create(p.ctx, spec)
	if err != nil {
		if p.ctx.Err() != nil {
			return false // shutting down; not a real failure
		}
		// Don't hot-loop on a failing mint (capacity, say) -- back off and let
		// the caller's next checkout, or the poll, retry.
		p.log.Warn("warm pool could not pre-boot a VM", "image", spec.Image, "err", err)
		p.backoff()
		return false
	}

	p.mu.Lock()
	p.ready[k] = append(p.ready[k], inst)
	n := len(p.ready[k])
	p.mu.Unlock()
	p.log.Debug("warm VM ready", "image", spec.Image, "ready", n, "target", p.targets[k])
	return true
}

// refillViaSnapshot captures a per-shape template once (by cold-booting a
// pristine VM and snapshotting it), then fills the pool by restoring from it --
// each restore reseeds its entropy, so the restored VMs do not share a CSPRNG.
// Any snapshot failure marks the shape cold-only, so the worst case is slower
// warm VMs, never none.
func (p *warmPool) refillViaSnapshot(k warmKey, spec runtime.Spec) bool {
	p.mu.Lock()
	ref, haveTemplate := p.templates[k]
	p.mu.Unlock()

	if !haveTemplate {
		spec.ID = fmt.Sprintf("warm-tmpl-%d", p.seq.Add(1))
		inst, err := p.rt.Create(p.ctx, spec)
		if err != nil {
			return p.snapshotFailed(k, "boot template", err)
		}
		ref, err = p.snap.Snapshot(p.ctx, inst) // consumes the template VM
		if err != nil {
			_ = inst.Stop(p.ctx)
			return p.snapshotFailed(k, "capture template", err)
		}
		p.mu.Lock()
		p.templates[k] = ref
		p.mu.Unlock()
		p.log.Debug("warm pool captured a template snapshot", "image", spec.Image)
		return true // progress; the loop restores from it next
	}

	spec.ID = fmt.Sprintf("warm-%d", p.seq.Add(1))
	inst, err := p.snap.Restore(p.ctx, spec, ref)
	if err != nil {
		return p.snapshotFailed(k, "restore", err)
	}
	p.mu.Lock()
	delete(p.snapFails, k) // a working restore clears the shape's record
	p.ready[k] = append(p.ready[k], inst)
	n := len(p.ready[k])
	p.mu.Unlock()
	p.log.Debug("warm VM restored", "image", spec.Image, "ready", n, "target", p.targets[k])
	return true
}

// snapFailure is a shape's record of the snapshot path failing.
//
// It records a count and a retry time rather than a flag, because "this host
// cannot do snapshots" and "this one restore was slow" are different facts and the
// flag could not tell them apart. It latched on the first failure and nothing
// cleared it, so a single slow restore -- a memory image whose page cache had been
// reclaimed, on an SD card -- disabled restores for that shape for the daemon's
// whole life, silently, on a code path whose entire purpose is to be faster than
// the alternative.
type snapFailure struct {
	n     int
	until time.Time
}

const (
	// snapRetryBase is the first cooldown after a failure, doubling each time.
	snapRetryBase = 30 * time.Second
	// snapRetryMax caps it: a shape that keeps failing should keep being retried,
	// just rarely.
	snapRetryMax = 10 * time.Minute
	// snapGiveUpAfter is how many consecutive failures make it permanent. By this
	// point it is not one slow restore, it is a host that cannot do this -- no KVM
	// snapshot support, a guest image without the agent routes -- and retrying
	// costs a cold boot and a full memory dump every time.
	snapGiveUpAfter = 5
)

// snapshotFailed records a snapshot-path failure for a shape and falls it back to
// cold boots: a snapshot problem degrades to slower warm VMs, never to none.
//
// It also throws the shape's template away. The template is a candidate cause --
// captured against an image that has since been rebuilt, captured from a guest
// that did not carry its interrupt-controller state -- and one that would
// otherwise be retried unchanged forever while occupying a full copy of a guest's
// RAM on disk.
func (p *warmPool) snapshotFailed(k warmKey, stage string, err error) bool {
	if p.ctx.Err() != nil {
		return false
	}

	p.mu.Lock()
	f := p.snapFails[k]
	if f == nil {
		f = &snapFailure{}
		p.snapFails[k] = f
	}
	f.n++
	cooldown := snapRetryBase << min(f.n-1, 8)
	if cooldown > snapRetryMax {
		cooldown = snapRetryMax
	}
	f.until = time.Now().Add(cooldown)
	stale, hadTemplate := p.templates[k]
	delete(p.templates, k)
	// Copied out under the lock: everything below runs outside it, and reaching
	// back into f there would be reading a value another goroutine may be changing.
	failures := f.n
	permanent := failures >= snapGiveUpAfter
	p.mu.Unlock()

	if permanent {
		p.log.Warn("warm pool snapshot path failed too often; cold-booting this shape from now on",
			"stage", stage, "failures", failures, "err", err)
	} else {
		p.log.Warn("warm pool snapshot path failed; cold-booting this shape and retrying snapshots later",
			"stage", stage, "failures", failures, "retry_in", cooldown, "err", err)
	}

	if hadTemplate {
		if derr := p.snap.Discard(p.ctx, stale); derr != nil {
			p.log.Warn("could not reclaim the failed shape's template snapshot", "err", derr)
		}
	}
	return false
}

// isColdOnly reports whether a shape should skip the snapshot path right now.
func (p *warmPool) isColdOnly(k warmKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.snapFails[k]
	if f == nil {
		return false
	}
	if f.n >= snapGiveUpAfter {
		return true
	}
	return time.Now().Before(f.until)
}

func (p *warmPool) backoff() {
	select {
	case <-p.ctx.Done():
	case <-time.After(3 * time.Second):
	}
}

// readyCount is how many VMs of a shape are waiting to be handed out.
func (p *warmPool) readyCount(k warmKey) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ready[k])
}

func (p *warmPool) pickDeficit() (warmKey, runtime.Spec, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, target := range p.targets {
		if len(p.ready[k]) < target {
			return k, p.specs[k], true
		}
	}
	return warmKey{}, runtime.Spec{}, false
}

// checkout hands out a ready VM for the requested shape, or nil when none is
// stocked. The VM is removed from the pool so it is never handed out twice, and
// the refill loop is nudged to top the shape back up.
func (p *warmPool) checkout(k warmKey) runtime.Instance {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	insts := p.ready[k]
	if len(insts) == 0 {
		p.mu.Unlock()
		return nil
	}
	inst := insts[len(insts)-1]
	p.ready[k] = insts[:len(insts)-1]
	p.mu.Unlock()

	// The meters start now, not when the VM was minted. This is the moment the
	// sandbox stops being the host's readiness and becomes somebody's work, and it
	// is the one place both fill paths pass through -- without it a VM that waited
	// ten minutes in the pool billed its first tenant ten minutes of wall and ten
	// of idle, next to a created_at stamped here.
	if a, ok := inst.(runtime.MeterAdopter); ok {
		a.AdoptMeter()
	}

	p.hits.Add(1)
	select {
	case p.kick <- struct{}{}:
	default:
	}
	return inst
}

// close stops refilling and tears down every VM still waiting in the pool. It is
// safe to call more than once and on a nil pool.
func (p *warmPool) close(ctx context.Context) {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		p.cancel()
		p.wg.Wait()

		p.mu.Lock()
		var leftover []runtime.Instance
		for k, insts := range p.ready {
			leftover = append(leftover, insts...)
			p.ready[k] = nil
		}
		var templates []runtime.SnapshotRef
		for k, ref := range p.templates {
			templates = append(templates, ref)
			delete(p.templates, k)
		}
		p.mu.Unlock()

		for _, inst := range leftover {
			_ = inst.Stop(ctx)
		}

		// The templates go too. Each is a full copy of a guest's RAM -- half a
		// gigabyte for the smallest shape this pool is worth using on -- and the only
		// handle to one is the ref just deleted above, so leaving them behind leaves
		// files nothing will ever open again, plus a complete guest memory image at
		// rest for as long as the box lives. The startup sweep is the backstop for a
		// daemon that is killed rather than closed; this is the ordinary path.
		for _, ref := range templates {
			if err := p.snap.Discard(ctx, ref); err != nil {
				p.log.Warn("could not reclaim a template snapshot on shutdown", "dir", ref.Dir, "err", err)
			}
		}
	})
}
