package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pablofdezr/microvm/internal/source"
)

// Seeding a sandbox from a caller-supplied source.
//
// The work is split across Create in three steps, and the order is the whole
// design: fetch before the VM exists, check the fit before it boots, write after
// it answers. What follows is why each boundary is where it is.

// A sandbox is a source.Writer already. The guest file endpoints carry a mode and
// count as activity, which is exactly what a seed needs, so seeding adds nothing
// to the guest agent and nothing to the rootfs images -- a new guest endpoint
// would have to be rebuilt into every image before a seed worked anywhere.
var _ source.Writer = (*Sandbox)(nil)

// SeedStage is which part of a seed failed.
//
// Reported because the three fail for unrelated reasons and are fixed by
// different people: a fetch is the caller's URL or the operator's allowlist, an
// expand is the archive's contents, and a write is this host or the guest. "Seed
// failed" sends an operator to the wrong place.
type SeedStage string

const (
	// SeedFetch is everything up to having the bytes on this host: the allowlist,
	// the address policy, the origin, a git clone.
	SeedFetch SeedStage = "fetch"
	// SeedExpand is reading what arrived: a member this cannot write, or a tree
	// past a cap.
	SeedExpand SeedStage = "expand"
	// SeedWrite is putting the tree into the guest.
	SeedWrite SeedStage = "write"
)

// SeedError is a seed that failed, naming the stage.
//
// It wraps the cause rather than replacing it, so the layer facing the caller
// still sees source's own sentinels and can answer 400 for a refusal and 502 for
// an origin that misbehaved.
type SeedError struct {
	Stage SeedStage
	Err   error
}

func (e *SeedError) Error() string {
	return fmt.Sprintf("seeding this sandbox failed at the %s stage: %v", e.Stage, e.Err)
}

func (e *SeedError) Unwrap() error { return e.Err }

// SeedTooLargeError is a source that will not fit in the sandbox it was asked
// for.
//
// Typed and refused before the VM boots, because the alternative is what this
// replaces: the writes go in, the guest's tmpfs fills, and the caller gets an
// ENOSPC from somewhere inside a file transfer -- or worse, a VM wedged on an
// allocation stall. Neither names the two fields that would fix it.
type SeedTooLargeError struct {
	// Bytes is what the source expands to.
	Bytes int64
	// Limit is the most a seed may write into this sandbox.
	Limit int64
	// LayerMiB is the writable layer the limit was derived from, and MemMiB and
	// DiskMiB are the two numbers that set it.
	LayerMiB int
	MemMiB   int
	DiskMiB  int
}

func (e *SeedTooLargeError) Error() string {
	return fmt.Sprintf(
		"this source expands to %d MiB and at most %d MiB may be seeded into a sandbox whose writable layer is %d MiB",
		mib(e.Bytes), mib(e.Limit), e.LayerMiB)
}

func mib(bytes int64) int64 { return (bytes + (1 << 20) - 1) >> 20 }

// SeedBusyError is a seed refused because this node is already fetching as many
// as it will.
//
// Its own type so the layer facing the caller can answer capacity rather than a
// bad request: nothing is wrong with the URL, and retrying shortly is exactly the
// right move. See maxConcurrentSeeds.
type SeedBusyError struct {
	Max int
}

func (e *SeedBusyError) Error() string {
	return fmt.Sprintf("this node is already fetching %d sources, which is as many as it will at once", e.Max)
}

// SeedExpiredError is a sandbox whose ttl_seconds ran out while its source was
// being written into it.
//
// Typed, because the alternative is what it replaces: the write fails against a
// sandbox the TTL has already stopped, no failure of source's own is inside the
// error, and the caller is told 500 for something they asked for. The seed's own
// duration is not charged to the TTL -- Create restarts that clock once the tree is
// in -- so reaching this means the seed took longer than the whole lifetime
// requested, which is the caller's number to raise.
type SeedExpiredError struct {
	// TTL is what was asked for and Elapsed is how long seeding took.
	TTL     time.Duration
	Elapsed time.Duration
}

func (e *SeedExpiredError) Error() string {
	return fmt.Sprintf("seeding this sandbox took %s and its ttl of %s expired first",
		e.Elapsed.Round(time.Millisecond), e.TTL)
}

// DefaultOverlayMiB is the writable layer a sandbox gets when its spec does not
// set one.
//
// It mirrors defaultUpperSize in internal/agent's init, which is the code that
// actually mounts the tmpfs. The two cannot be one constant -- that one lives in
// a Linux-only file inside the guest agent -- so they are kept equal by hand, and
// the direction of the risk is worth knowing: too small here refuses a seed that
// would have fitted, too large here lets one through to fail inside the guest,
// which is the failure this exists to prevent.
const DefaultOverlayMiB = 512

// seedShareOfLayerPercent is how much of the writable layer a seed may claim.
//
// Not all of it, because the sandbox has to work afterwards: whatever it installs,
// builds and writes comes out of the same tmpfs. A seed that exactly filled the
// layer would pass a whole-layer check and fail the first command, which is the
// mystery ENOSPC one step later.
const seedShareOfLayerPercent = 75

// seedLimit is the most a seed may write into a sandbox of this shape.
//
// Two numbers bound the writable layer and the smaller wins. The first is the
// layer's own size, which -disk-mib sets. The second is memory, because the layer
// is a tmpfs the guest stacks over its read-only image, so its pages *are* guest
// memory: a 256 MiB sandbox cannot hold a 400 MiB tree however large its overlay
// is nominally configured to be.
func seedLimit(spec Spec) (limit int64, layerMiB int) {
	layerMiB = DefaultOverlayMiB
	if spec.Limits.DiskMiB > 0 {
		layerMiB = spec.Limits.DiskMiB
	}
	if spec.MemMiB > 0 && spec.MemMiB < layerMiB {
		layerMiB = spec.MemMiB
	}
	return (int64(layerMiB) << 20) * seedShareOfLayerPercent / 100, layerMiB
}

// prepareSeed fetches and validates a sandbox's source, before any VM exists.
//
// Before, deliberately, and this is the metering decision. Stats are cumulative
// from boot -- Wall is how long the sandbox has existed and Idle is Wall minus
// ActiveCPU, see runtime.Stats -- so a seed fetched after boot would show up as
// wall-clock time on a sandbox that had not yet been handed any work: the caller
// billed for somebody else's slow web server. Fetching first means the clock
// starts when the VM does. It costs the overlap with boot, which is a few hundred
// milliseconds against a download measured in seconds, and it buys a create that
// fails on a bad URL without having booted anything at all.
//
// The writes that follow are metered as activity, which is right for the opposite
// reason: they are this host and this guest doing real work, and counting them is
// what stops the idle reclaim taking the VM away mid-seed.
func (m *Manager) prepareSeed(ctx context.Context, spec Spec) (source.Prepared, error) {
	if spec.Source == nil {
		return nil, nil
	}
	if m.source == nil {
		// The capability is off on this node. Refused in the same words as an
		// unallowlisted host, because a caller can act on neither and telling them
		// apart only maps the operator's configuration.
		return nil, &SeedError{Stage: SeedFetch, Err: fmt.Errorf(
			"%w: fetching a source is not enabled on this node", source.ErrNotPermitted)}
	}

	// The node's own bound on concurrent fetches, and the only one there is until
	// rt.Create is reached. See maxConcurrentSeeds.
	select {
	case m.seeding <- struct{}{}:
		defer func() { <-m.seeding }()
	default:
		return nil, &SeedError{Stage: SeedFetch, Err: &SeedBusyError{Max: maxConcurrentSeeds}}
	}

	prepared, err := m.source.Prepare(ctx, *spec.Source)
	if err != nil {
		m.log.Warn("sandbox source could not be prepared",
			"sandbox", spec.ID, "type", spec.Source.Type,
			// Redacted: a presigned URL carries its signature in the query string,
			// and this line goes to a log.
			"url", source.Redact(spec.Source.URL),
			"stage", stageOf(err), "err", err)
		return nil, &SeedError{Stage: stageOf(err), Err: err}
	}

	// The fit, now that the expanded size is known and while there is still
	// nothing to tear down.
	if limit, layerMiB := seedLimit(spec); prepared.Manifest().Bytes > limit {
		err := &SeedTooLargeError{
			Bytes:    prepared.Manifest().Bytes,
			Limit:    limit,
			LayerMiB: layerMiB,
			MemMiB:   spec.MemMiB,
			DiskMiB:  spec.Limits.DiskMiB,
		}
		prepared.Close()
		return nil, err
	}
	return prepared, nil
}

// stageOf maps one of source's failure classes onto the stage it happened in.
//
// The package does not report a stage itself, and it should not have to: the
// sentinels already say which half of the work went wrong. A refusal and a bad
// origin are the fetch; a cap and a member this cannot write are the expansion,
// whichever moment they were noticed in.
func stageOf(err error) SeedStage {
	switch {
	case errors.Is(err, source.ErrTooLarge), errors.Is(err, source.ErrInvalidArchive):
		return SeedExpand
	default:
		return SeedFetch
	}
}

// writeSeed puts a prepared source into a running sandbox.
//
// The sandbox is the Writer, so every file goes in over the same guest endpoint
// an upload uses: it carries the mode, and it counts as activity, so a seed of a
// thousand files cannot be reclaimed as idle halfway through.
func (m *Manager) writeSeed(ctx context.Context, sb *Sandbox, prepared source.Prepared) error {
	man := prepared.Manifest()
	if err := prepared.Write(ctx, sb); err != nil {
		return &SeedError{Stage: SeedWrite, Err: err}
	}

	res := prepared.Result()
	sb.log.Info("sandbox seeded",
		"type", res.Type, "url", res.URLRedacted, "commit", res.Commit,
		"files", man.Files, "bytes", man.Bytes)
	return nil
}
