package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
	"github.com/pablofdezr/microvm/internal/source"
)

// Seeding is tested through a fake Preparer, which is the whole reason
// source.Preparer is an interface. What is under test here is not fetching --
// internal/source owns that, against a real server -- it is the order Create does
// things in: what has booted when a fetch fails, what is listed when a write
// fails, and whether the writes count as work.

// fakePreparer stands in for the fetcher.
type fakePreparer struct {
	prepared *fakePrepared
	err      error
	calls    int
}

func (f *fakePreparer) Prepare(_ context.Context, req source.Request) (source.Prepared, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	f.prepared.req = req
	return f.prepared, nil
}

// fakePrepared is a seed already on the host: a manifest, and a Write that either
// writes it or fails the way a dying guest would.
type fakePrepared struct {
	man      source.Manifest
	res      source.Result
	req      source.Request
	writeErr error
	// onWrite runs inside Write, while the sandbox is mid-seed. It is how a test
	// observes the state a seed is metered in.
	onWrite func(w source.Writer)
	closed  int
}

func (f *fakePrepared) Manifest() source.Manifest { return f.man }
func (f *fakePrepared) Result() source.Result     { return f.res }
func (f *fakePrepared) Close() error              { f.closed++; return nil }

func (f *fakePrepared) Write(ctx context.Context, w source.Writer) error {
	if f.onWrite != nil {
		f.onWrite(w)
	}
	for _, e := range f.man.Entries {
		if e.Dir {
			if err := w.Mkdir(ctx, e.Path); err != nil {
				return err
			}
			continue
		}
		if err := w.WriteFile(ctx, e.Path, strings.NewReader("seeded\n"), e.Mode); err != nil {
			return err
		}
	}
	return f.writeErr
}

// sampleSeed is a two-file project with one empty directory.
func sampleSeed() *fakePrepared {
	return &fakePrepared{
		man: source.Manifest{
			Entries: []source.Entry{
				{Path: "main.py", Size: 7, Mode: "0644"},
				{Path: "bin/run", Size: 7, Mode: "0755"},
				{Path: "out", Dir: true},
			},
			Files: 2,
			Dirs:  1,
			Bytes: 14,
		},
		res: source.Result{
			Type:        source.TypeTarball,
			URLRedacted: "https://codeload.example.com/acme/widgets.tar.gz",
			Files:       2,
			Bytes:       14,
		},
	}
}

func newSeedManager(t *testing.T, p source.Preparer) (*Manager, *runtimetest.Runtime) {
	t.Helper()
	rt := runtimetest.New()
	logs := logstore.New(logstore.Config{})
	var opts []Option
	if p != nil {
		opts = append(opts, WithSource(p))
	}
	m := NewManager(rt, logs, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m, rt
}

func seedSpec(id string, req *source.Request) Spec {
	return Spec{
		Spec:   runtime.Spec{ID: id, Image: "python"},
		Source: req,
	}
}

func tarballRequest() *source.Request {
	return &source.Request{Type: source.TypeTarball, URL: "https://codeload.example.com/acme/widgets.tar.gz?sig=x"}
}

// The seed is in the guest before Create returns, so the first execution already
// has the project. That is the whole feature.
func TestCreateSeedsTheSandboxBeforeItReturns(t *testing.T) {
	seed := sampleSeed()
	m, rt := newSeedManager(t, &fakePreparer{prepared: seed})

	sb, err := m.Create(context.Background(), seedSpec("sb_seeded", tarballRequest()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	inst, ok := rt.Instance("sb_seeded")
	if !ok {
		t.Fatal("no instance was created")
	}
	if body, ok := inst.File("main.py"); !ok || string(body) != "seeded\n" {
		t.Errorf("main.py in the guest = %q, %v", body, ok)
	}
	// The mode goes in with the file. An executable that arrives unexecutable needs
	// a chmod the caller does not know to run.
	if mode, _ := inst.FileMode("bin/run"); mode != "0755" {
		t.Errorf("bin/run mode = %q, want 0755", mode)
	}
	// An empty directory has to be asked for; nothing else creates it.
	if _, ok := inst.File("out/"); !ok {
		t.Error("the empty directory in the source was not created")
	}

	// Reported, so a sandbox found later can be traced back to the code in it.
	info := sb.Info()
	if info.Source == nil {
		t.Fatal("the sandbox does not report what it was seeded from")
	}
	if info.Source.URLRedacted != seed.res.URLRedacted {
		t.Errorf("url = %q, want the redacted one", info.Source.URLRedacted)
	}
	if seed.closed == 0 {
		t.Error("the prepared source was never closed, so its buffer is still on disk")
	}
}

// A sandbox with no source is untouched by any of this: nothing is prepared, and
// nothing is reported.
func TestCreateWithoutASourceSeedsNothing(t *testing.T) {
	p := &fakePreparer{prepared: sampleSeed()}
	m, _ := newSeedManager(t, p)

	sb, err := m.Create(context.Background(), seedSpec("sb_plain", nil))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.calls != 0 {
		t.Errorf("the fetcher was asked %d times for a sandbox with no source", p.calls)
	}
	if sb.Info().Source != nil {
		t.Error("a sandbox with no source reports one")
	}
}

// The capability is off unless the operator turned it on, and a request for it is
// refused rather than ignored: a create that quietly produced an empty sandbox
// would be the wrong project delivered silently.
func TestASourceIsRefusedWhenTheNodeDoesNotSeed(t *testing.T) {
	m, rt := newSeedManager(t, nil)

	_, err := m.Create(context.Background(), seedSpec("sb_off", tarballRequest()))
	if !errors.Is(err, source.ErrNotPermitted) {
		t.Fatalf("Create = %v, want source.ErrNotPermitted", err)
	}
	if rt.Created() != 0 {
		t.Error("a VM was booted for a source that was never going to be fetched")
	}
}

// A fetch that fails boots nothing at all, which is also the metering answer: the
// sandbox's clock never started, so nobody is billed for someone else's slow web
// server.
func TestAFailedFetchBootsNothing(t *testing.T) {
	p := &fakePreparer{err: source.ErrFetchFailed}
	m, rt := newSeedManager(t, p)

	_, err := m.Create(context.Background(), seedSpec("sb_fetch", tarballRequest()))
	if err == nil {
		t.Fatal("Create succeeded with a failing fetch")
	}

	var seedErr *SeedError
	if !errors.As(err, &seedErr) {
		t.Fatalf("Create = %v, want a *SeedError naming the stage", err)
	}
	if seedErr.Stage != SeedFetch {
		t.Errorf("stage = %q, want %q", seedErr.Stage, SeedFetch)
	}
	if rt.Created() != 0 {
		t.Error("a VM was booted for a fetch that failed")
	}
	if len(m.List()) != 0 {
		t.Error("a sandbox is listed for a create that failed")
	}
}

// An archive this cannot expand is the expand stage, not the fetch stage. The
// distinction is the point of reporting one at all: it sends an operator to the
// archive rather than to the network.
func TestAnUnusableArchiveIsTheExpandStage(t *testing.T) {
	m, _ := newSeedManager(t, &fakePreparer{err: source.ErrInvalidArchive})

	_, err := m.Create(context.Background(), seedSpec("sb_expand", tarballRequest()))
	var seedErr *SeedError
	if !errors.As(err, &seedErr) {
		t.Fatalf("Create = %v, want a *SeedError", err)
	}
	if seedErr.Stage != SeedExpand {
		t.Errorf("stage = %q, want %q", seedErr.Stage, SeedExpand)
	}
}

// The all-or-nothing rule. A write that fails halfway leaves no sandbox behind:
// not returned, not listed, not running, and not charged against the tenant's
// concurrency.
func TestAFailedWriteDestroysTheSandbox(t *testing.T) {
	seed := sampleSeed()
	seed.writeErr = errors.New("the guest went away")
	m, rt := newSeedManager(t, &fakePreparer{prepared: seed})

	sb, err := m.Create(context.Background(), Spec{
		Spec:          runtime.Spec{ID: "sb_halfway", Image: "python"},
		Source:        tarballRequest(),
		Tenant:        "t_a",
		MaxConcurrent: 1,
	})
	if sb != nil {
		t.Fatal("a half-seeded sandbox was handed back")
	}

	var seedErr *SeedError
	if !errors.As(err, &seedErr) {
		t.Fatalf("Create = %v, want a *SeedError", err)
	}
	if seedErr.Stage != SeedWrite {
		t.Errorf("stage = %q, want %q", seedErr.Stage, SeedWrite)
	}

	if len(m.List()) != 0 {
		t.Error("the half-seeded sandbox is listed")
	}
	if _, ok := m.Get("sb_halfway"); ok {
		t.Error("the half-seeded sandbox is retrievable")
	}
	inst, ok := rt.Instance("sb_halfway")
	if !ok {
		t.Fatal("the VM was never created, so this test asserts nothing about tearing it down")
	}
	if _, err := inst.Stats(); err == nil {
		t.Error("the VM is still running after its seed failed")
	}
	// The tenant's slot has to come back, or a node that fails to seed slowly locks
	// a caller out of a cap they never spent.
	if _, err := m.Create(context.Background(), Spec{
		Spec: runtime.Spec{ID: "sb_next", Image: "python"}, Tenant: "t_a", MaxConcurrent: 1,
	}); err != nil {
		t.Errorf("the tenant's concurrency slot was not released: %v", err)
	}
}

// A seed is work, and the idle reclaim has to see it that way: staging a project
// is a run of transfers with nothing executed in between, and a sandbox measured
// only by its execs is idle for all of it.
func TestSeedingCountsAsActivity(t *testing.T) {
	seed := sampleSeed()
	seed.onWrite = func(w source.Writer) {
		sb, ok := w.(*Sandbox)
		if !ok {
			t.Errorf("the writer is %T, not the sandbox itself", w)
			return
		}
		// Backdated to an hour ago, which is the state the reclaim acts on. The
		// transfer in flight is what has to keep it alive.
		sb.mu.Lock()
		sb.lastActive = time.Now().Add(-time.Hour)
		sb.mu.Unlock()

		if err := sb.WriteFile(context.Background(), "probe", strings.NewReader("x"), "0644"); err != nil {
			t.Errorf("write during a seed: %v", err)
		}
		if sb.idleFor(time.Minute) {
			t.Error("a sandbox being seeded looks idle, so the reclaim would kill it mid-seed")
		}
	}

	m, _ := newSeedManager(t, &fakePreparer{prepared: seed})
	sb, err := m.Create(context.Background(), seedSpec("sb_activity", tarballRequest()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// And the clock restarts from the last file rather than from before the first.
	if sb.idleFor(time.Minute) {
		t.Error("the idle clock was not reset by the seed")
	}
}

// A tree too big for the sandbox is refused as a request error before the VM
// boots, with the numbers. The alternative is what this replaces: an ENOSPC from
// somewhere inside a file transfer, or a guest wedged on an allocation stall.
func TestASourceTooBigForTheSandboxIsRefused(t *testing.T) {
	seed := sampleSeed()
	seed.man.Bytes = 500 << 20
	p := &fakePreparer{prepared: seed}
	m, rt := newSeedManager(t, p)

	_, err := m.Create(context.Background(), Spec{
		// 256 MiB of memory, so the writable tmpfs cannot hold 500 MiB whatever the
		// overlay is nominally sized at.
		Spec:   runtime.Spec{ID: "sb_big", Image: "python", MemMiB: 256},
		Source: tarballRequest(),
	})

	var tooLarge *SeedTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Create = %v, want a *SeedTooLargeError", err)
	}
	if tooLarge.LayerMiB != 256 {
		t.Errorf("LayerMiB = %d, want 256: the layer is memory-backed, so memory bounds it", tooLarge.LayerMiB)
	}
	if !strings.Contains(tooLarge.Error(), "500") {
		t.Errorf("the refusal does not say how big the source is: %v", tooLarge)
	}
	if rt.Created() != 0 {
		t.Error("a VM was booted for a source that could never fit in it")
	}
	if seed.closed == 0 {
		t.Error("the prepared source was not closed, so its buffer is still on disk")
	}
}

func TestSeedLimit(t *testing.T) {
	tests := []struct {
		name         string
		mem          int
		disk         int
		wantLayerMiB int
	}{
		{"the default layer", 4096, 0, DefaultOverlayMiB},
		{"a smaller overlay wins", 4096, 64, 64},
		{"memory wins, because the layer is a tmpfs", 128, 512, 128},
		{"the default memory bounds the default layer", DefaultMemMiB, 0, DefaultMemMiB},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := Spec{Spec: runtime.Spec{MemMiB: tc.mem, Limits: runtime.Limits{DiskMiB: tc.disk}}}
			limit, layer := seedLimit(spec)
			if layer != tc.wantLayerMiB {
				t.Errorf("layer = %d MiB, want %d", layer, tc.wantLayerMiB)
			}
			// Never the whole layer: the sandbox has to work afterwards, and
			// everything it installs and writes comes out of the same tmpfs.
			if limit >= int64(layer)<<20 {
				t.Errorf("limit %d leaves the sandbox no room in a %d MiB layer", limit, layer)
			}
		})
	}
}

// The TTL is time to run things in, and seeding is not that: the sandbox does not
// exist for the caller until its source is in. The clock was armed at boot, before
// the fetch was written, so a three-minute seed against ttl_seconds: 300 handed back
// a sandbox with two minutes left and an expires_at that had already elapsed during
// the request.
func TestTheTTLClockStartsAfterTheSeedIsWritten(t *testing.T) {
	const ttl = 30 * time.Second
	const seedTakes = 150 * time.Millisecond

	seed := sampleSeed()
	seed.onWrite = func(source.Writer) { time.Sleep(seedTakes) }
	m, _ := newSeedManager(t, &fakePreparer{prepared: seed})

	sb, err := m.Create(context.Background(), Spec{
		Spec:   runtime.Spec{ID: "sb_ttl", Image: "python"},
		Source: tarballRequest(),
		TTL:    ttl,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Whatever the seed cost, the caller has their whole TTL from here. Measured
	// against the write's own duration rather than a fixed number, so this asserts
	// the property and not the machine it ran on.
	remaining := time.Until(sb.Info().ExpiresAt)
	if remaining < ttl-seedTakes/2 {
		t.Errorf("the sandbox has %s left of a %s ttl, so the seed was charged to the caller",
			remaining.Round(time.Millisecond), ttl)
	}
}

// A ttl shorter than the seed is the caller's number, and answering it with an
// internal error pages an operator for something nothing on this host did wrong.
func TestATTLShorterThanTheSeedIsTheCallersError(t *testing.T) {
	seed := sampleSeed()
	seed.onWrite = func(source.Writer) { time.Sleep(300 * time.Millisecond) }
	m, _ := newSeedManager(t, &fakePreparer{prepared: seed})

	sb, err := m.Create(context.Background(), Spec{
		Spec:   runtime.Spec{ID: "sb_ttl_short", Image: "python"},
		Source: tarballRequest(),
		TTL:    50 * time.Millisecond,
	})
	if sb != nil {
		t.Fatal("a sandbox that had already expired was handed back")
	}

	var expired *SeedExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("Create = %v (%T), want a *SeedExpiredError", err, err)
	}
	if expired.TTL != 50*time.Millisecond {
		t.Errorf("TTL = %s, want the 50ms that was asked for", expired.TTL)
	}
	if len(m.List()) != 0 {
		t.Error("the expired sandbox is listed")
	}
}

// The node's own bound on concurrent fetches. The per-tenant cap is not one --
// -tenant-max-sandboxes defaults to unlimited -- and the node's admission is inside
// rt.Create, which a seed happens before, so without this one token holds as many
// simultaneous downloads onto this host's disk as it cares to open.
func TestConcurrentSeedsAreBoundedNodeWide(t *testing.T) {
	release := make(chan struct{})
	held := make(chan struct{}, maxConcurrentSeeds)
	seed := sampleSeed()
	seed.onWrite = func(source.Writer) {}

	p := &blockingPreparer{prepared: seed, entered: held, release: release}
	m, _ := newSeedManager(t, p)

	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentSeeds; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = m.Create(context.Background(), Spec{
				Spec:   runtime.Spec{ID: fmt.Sprintf("sb_hold_%d", i), Image: "python"},
				Source: tarballRequest(),
			})
		}(i)
	}
	// Every slot held, and each of them inside Prepare.
	for i := 0; i < maxConcurrentSeeds; i++ {
		select {
		case <-held:
		case <-time.After(10 * time.Second):
			t.Fatal("the fetches never started")
		}
	}

	_, err := m.Create(context.Background(), Spec{
		Spec:   runtime.Spec{ID: "sb_one_too_many", Image: "python"},
		Source: tarballRequest(),
	})
	var busy *SeedBusyError
	if !errors.As(err, &busy) {
		t.Errorf("Create = %v, want a *SeedBusyError: %d fetches were already in flight",
			err, maxConcurrentSeeds)
	}

	close(release)
	wg.Wait()

	// And the slots come back, or one burst of creates disables seeding for the
	// daemon's whole life.
	if _, err := m.Create(context.Background(), Spec{
		Spec:   runtime.Spec{ID: "sb_after", Image: "python"},
		Source: tarballRequest(),
	}); err != nil {
		t.Errorf("a seed after the burst was refused: %v", err)
	}
}

// blockingPreparer holds inside Prepare until it is released, which is how a test
// occupies the node's fetch slots.
type blockingPreparer struct {
	prepared *fakePrepared
	entered  chan struct{}
	release  chan struct{}
}

func (b *blockingPreparer) Prepare(ctx context.Context, _ source.Request) (source.Prepared, error) {
	b.entered <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// A fresh one per call: the fakePrepared records what it was asked to do, and
	// eight concurrent creates sharing one would be a data race rather than a test.
	p := sampleSeed()
	p.onWrite = b.prepared.onWrite
	return p, nil
}
