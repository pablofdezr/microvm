//go:build linux

package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pablofdezr/microvm/internal/cgroup"
	"github.com/pablofdezr/microvm/internal/fcapi"
	"github.com/pablofdezr/microvm/internal/guestclient"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/vmgenid"
	"github.com/pablofdezr/microvm/internal/vsock"
)

// This file implements runtime.Snapshotter for the Firecracker backend. It is
// active only when Config.SnapshotDir is set, in which case VMs boot with the
// control API socket (see start()). The sequence follows tinylabscom/mvm
// (Apache-2.0): pause + PUT /snapshot/create to capture, and a fresh VMM +
// PUT /snapshot/load {resume_vm:true} + entropy reseed to restore.
//
// Validated end to end on arm64 (Raspberry Pi 5, BCM2712/GIC-400, Firecracker
// v1.16.1, guest 6.1.102): a restored guest answers in tens of milliseconds, and
// the warm pool fills by restoring rather than cold-booting. Getting there needed
// one thing that is not in Firecracker's API at all -- on a GICv2 host a snapshot
// silently loses the guest's per-vCPU interrupt-controller state, and the guest
// has to carry it across the snapshot itself. Snapshot arms that carry before
// pausing and Restore stands it down; internal/agent/gic_linux.go documents the
// defect, the measurements, and why the guest is the only thing that can fix it.
//
// # What a restored VM is and is not
//
// It is a fresh VMM, in a fresh jail, with its own vsock socket and its own host
// listener, resumed from a template that was captured from a pristine VM the pool
// itself cold-booted. Every restore rotates the guest's CSPRNG before the VM is
// reachable, and a restore that cannot fails rather than hand back a VM whose
// random numbers another tenant already has.
//
// It is not a general-purpose "resume this sandbox". Three things this path does
// not do, deliberately and visibly:
//
//   - Networking. A snapshot carries the guest's MAC and IP in its memory image
//     and names its host TAP in its device state, so every restore of one template
//     would come back claiming the same address. Restore refuses a spec with
//     Network set rather than producing that collision.
//   - Restore a tenant's sandbox. Templates are captured from VMs that have run no
//     code. Nothing here can snapshot a sandbox that has.
//   - Survive a daemon restart. A SnapshotRef only exists in memory, so templates
//     are captured per run and reclaimed at shutdown and at startup.

const (
	snapStateFile = "state"
	snapMemFile   = "mem"

	// restoreTimeout bounds a whole restore.
	//
	// Every step inside has its own bound, but two of them used to have none: the
	// wait for Firecracker's API socket polled the caller's context, and the reseed
	// was issued on a client that deliberately sets no timeout. From the warm pool
	// the caller's context is the pool's own, which has no deadline -- so a jailer
	// that died on exec, or a guest that answered health and then wedged, blocked
	// the single refill goroutine for the daemon's life. Not slowly: forever, with
	// no log line and no fallback to cold boots for any shape.
	restoreTimeout = 30 * time.Second

	// restoreReadyTimeout is how long a restored VM has to answer.
	//
	// Measured on the hardware above, from PUT /snapshot/load returning to the
	// agent's first health reply, over 36 restores while the host also carried
	// other tenants' work (load average 7-13 on four cores): 4ms fastest, 83ms
	// slowest, ~12ms typical. This is 60x the slowest of those.
	// The margin here is for a host under far worse load, not for a slow guest: a
	// restore answers in tens of milliseconds or it does not answer at all,
	// because what used to make it late (the guest's interrupt-controller state)
	// is settled within microseconds of the resume and nothing about it heals
	// later. This was 90s in the belief that a restored guest reconnects slowly.
	// It does not -- it was broken -- and 90s only made each failure expensive:
	// the warm pool paid it per shape before falling back to cold boots.
	//
	// It is a tight bound on a measured number, which is only safe because failing
	// it is now recoverable: the pool retries the snapshot path for a shape after a
	// cooldown instead of latching it to cold boots for the daemon's life.
	restoreReadyTimeout = 5 * time.Second

	// apiSocketTimeout bounds the wait for Firecracker to create its control
	// socket. A VMM that is going to create it does so in milliseconds; one that is
	// not (a bad binary, a jailer the host rejects) never will, and the wait also
	// ends the moment the process dies.
	apiSocketTimeout = 5 * time.Second

	// armTimeout bounds the arm call before a capture. It is a single round trip to
	// a guest that is already answering, on the pool's own goroutine rather than a
	// caller's, so it can afford to be generous.
	armTimeout = 10 * time.Second

	// standDownTimeout bounds the disarm on the restore path, where latency is the
	// point of the whole exercise. It is deliberately smaller than
	// restoreReadyTimeout: the measured stand-down is 12ms, and a stand-down that
	// takes seconds means a vCPU is not being scheduled, which is a VM to throw
	// away rather than one to keep waiting on.
	standDownTimeout = 3 * time.Second

	// reseedTimeout bounds the CSPRNG rotation. It is an ioctl behind one vsock
	// round trip; anything longer is a guest that has stopped serving.
	reseedTimeout = 5 * time.Second
)

var _ runtime.Snapshotter = (*Runtime)(nil)

// Snapshot pauses inst, writes a full snapshot under SnapshotDir, and stops the
// source VM. The VM must have booted with the API enabled (SnapshotDir set) or
// there is no socket to pause it through.
func (r *Runtime) Snapshot(ctx context.Context, inst runtime.Instance) (runtime.SnapshotRef, error) {
	fi, ok := inst.(*instance)
	if !ok {
		return runtime.SnapshotRef{}, fmt.Errorf("snapshot: not a firecracker instance")
	}
	if fi.apiPath == "" {
		return runtime.SnapshotRef{}, fmt.Errorf("snapshot: VM booted without the control API; set SnapshotDir")
	}

	// The image the guest's page cache and inode state describe. A restore stages
	// whatever stands at this path then, so the template has to be able to tell
	// that it is still the same file.
	rootfsID, err := fileIdentity(fi.rootfs)
	if err != nil {
		return runtime.SnapshotRef{}, fmt.Errorf("snapshot: identify rootfs %s: %w", fi.rootfs, err)
	}

	dir, err := os.MkdirTemp(r.cfg.SnapshotDir, "snap-*")
	if err != nil {
		return runtime.SnapshotRef{}, fmt.Errorf("snapshot: make dir: %w", err)
	}

	// Firecracker writes the snapshot to paths relative to its chroot, so it
	// lands in the jail; we move it out to SnapshotDir afterwards so it survives
	// the jail's teardown.
	jailRoot := r.jailRoot(fi.jailID)
	api := fcapi.New(fi.apiPath)

	// Last thing before the pause, deliberately: arming costs the guest a spinning
	// thread per vCPU, and this call and the pause below are the whole of the
	// window in which the guest is executing and paying for them.
	if err := armGuestForSnapshot(ctx, fi); err != nil {
		os.RemoveAll(dir)
		return runtime.SnapshotRef{}, err
	}

	if err := api.Pause(ctx); err != nil {
		os.RemoveAll(dir)
		return runtime.SnapshotRef{}, fmt.Errorf("snapshot: pause: %w", err)
	}
	if err := api.CreateSnapshot(ctx, snapStateFile, snapMemFile); err != nil {
		os.RemoveAll(dir)
		return runtime.SnapshotRef{}, fmt.Errorf("snapshot: create: %w", err)
	}

	for _, name := range []string{snapStateFile, snapMemFile} {
		if err := os.Rename(filepath.Join(jailRoot, name), filepath.Join(dir, name)); err != nil {
			os.RemoveAll(dir)
			return runtime.SnapshotRef{}, fmt.Errorf("snapshot: collect %s: %w", name, err)
		}
		// Immediately, before anything else can be handed a link to this inode.
		if err := sealTemplateFile(filepath.Join(dir, name)); err != nil {
			os.RemoveAll(dir)
			return runtime.SnapshotRef{}, fmt.Errorf("snapshot: seal %s: %w", name, err)
		}
	}

	// The source VM has served its purpose; it is stopped so the snapshot, not a
	// live VM, is what gets reused.
	_ = fi.Stop(ctx)

	digest, err := digestSnapshot(ctx, dir)
	if err != nil {
		os.RemoveAll(dir)
		return runtime.SnapshotRef{}, err
	}
	return runtime.SnapshotRef{
		Dir:      dir,
		Rootfs:   fi.rootfs,
		RootfsID: rootfsID,
		Digest:   digest,
	}, nil
}

// sealTemplateFile makes a snapshot file readable by the jailed VMMs that will
// restore from it and writable by none of them.
//
// The files arrive owned by the unprivileged uid every jailed Firecracker on the
// host shares, because a jailed Firecracker is what wrote them. A restore then
// hardlinks them into the restoring VM's own jail, so without this the shared
// template inode sits inside each restored sandbox's chroot owned by that
// sandbox's own VMM -- and ownership is write access, since an owner can chmod. A
// VMM that escaped its guest (the event the jailer and that uid exist for) could
// open /mem O_RDWR in its own jail and choose the bytes of the guest RAM, kernel
// text included, that every later restore of that shape boots with. Nothing would
// detect it: no restore re-hashes what it stages.
//
// Root-owned and 0444 closes that. Firecracker only ever reads a snapshot it
// loads -- the memory file is mapped private, which is the whole reason one
// template can serve many restores -- so read-only costs it nothing, and the mode
// travels with the inode into every jail it is linked into.
func sealTemplateFile(path string) error {
	if err := os.Chown(path, 0, 0); err != nil {
		return err
	}
	return os.Chmod(path, 0o444)
}

// Restore boots a fresh VM from a snapshot and reseeds its entropy before
// returning it. Every VM restored from one snapshot begins with an identical
// CSPRNG, so the reseed -- rotating the guest's CSPRNG from a unique token -- is
// not optional: without it, restored VMs share keys.
func (r *Runtime) Restore(ctx context.Context, spec runtime.Spec, ref runtime.SnapshotRef) (runtime.Instance, error) {
	if r.cfg.SnapshotDir == "" {
		return nil, fmt.Errorf("restore: snapshots are disabled (no SnapshotDir)")
	}
	if spec.Network {
		// Not "unimplemented" -- unimplementable from here. The guest's MAC and IP
		// are in the memory image and its host TAP is named in the device state, so
		// a restore comes back as the template: on the template's tap if it still
		// exists (it does not; capture stopped that VM and deleted it), and
		// otherwise on whatever sandbox the netpool has since issued that slot to,
		// with the template's address colliding with the current holder's. Giving a
		// restore its own identity needs the guest to reconfigure itself on resume,
		// which is a feature and not a fix-up.
		return nil, fmt.Errorf("restore: a snapshot cannot be restored into a networked sandbox: " +
			"the guest's MAC and IP live in the memory image, so every restore would claim the same address")
	}

	// One deadline over the whole thing. Every step below also bounds itself, but a
	// step that is bounded only by its own timeout is a step that can be reached
	// with the budget already spent, and the caller (the warm pool's refill
	// goroutine) has no deadline of its own to fall back on.
	ctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()

	jailID := newJailID()
	if err := r.checkVsockPath(jailID); err != nil {
		return nil, err
	}
	inst := &instance{
		id:      spec.ID,
		jailID:  jailID,
		runtime: r,
		log:     r.log.With("sandbox", spec.ID, "jail", jailID),
		// The cgroup the jailer creates, at <slice>/<jail-id>; waitReady polls it
		// for the guest coming up, so it must be set before startForRestore runs.
		group: r.slice.Child(jailID),
	}
	inst.startedNano.Store(time.Now().UnixNano())

	if err := r.restoreInto(ctx, inst, spec, ref); err != nil {
		// Rolled back for the same reason Create rolls back, and it was missing
		// here: eight bare returns after the jail existed leaked, between them, the
		// jail tree, its hardlinks to the shared template, and -- once the listener
		// was open -- a bound host socket in a directory whose VM never existed.
		// context.Background() because the failure is often the deadline above, and
		// a teardown that inherits an expired context does not tear down.
		_ = inst.Stop(context.Background())
		return nil, err
	}

	r.mu.Lock()
	r.insts[inst.id] = inst
	r.mu.Unlock()
	return inst, nil
}

// restoreInto does the work of a restore into an instance the caller will roll
// back on error.
func (r *Runtime) restoreInto(ctx context.Context, inst *instance, spec runtime.Spec, ref runtime.SnapshotRef) error {
	jailRoot := r.jailRoot(inst.jailID)
	if err := os.MkdirAll(jailRoot, 0o755); err != nil {
		return fmt.Errorf("restore: jail: %w", err)
	}

	// The block devices are not inside a snapshot, only referenced by it, so the
	// kernel, rootfs and the snapshot's own files must all be staged at the paths
	// the memory image expects.
	kernelPath := filepath.Join(jailRoot, "vmlinux")
	if err := r.stageFile(r.cfg.KernelPath, kernelPath); err != nil {
		return fmt.Errorf("restore: stage kernel: %w", err)
	}
	rootfs, err := r.rootfsPath(spec.Image)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	// The template describes one particular file, not a name. An operator who
	// rebuilds an image in place -- the documented way to update one -- leaves this
	// template resumable onto a filesystem that is not the one its guest's page
	// cache and open inodes describe, which is EIO and silent corruption behind a
	// health probe that passes. Refuse instead; the pool recaptures.
	if err := checkRootfsUnchanged(ref, rootfs); err != nil {
		return err
	}
	inst.rootfs = rootfs
	rootfsPath := filepath.Join(jailRoot, "rootfs.ext4")
	if err := r.stageFile(rootfs, rootfsPath); err != nil {
		return fmt.Errorf("restore: stage rootfs: %w", err)
	}

	// Skipped for the same reason the snapshot files are staged after the chown,
	// one step down: they are hardlinks to masters the host shares. The reasoning
	// below was applied to the snapshot's own files and not to these, which are
	// shared by every sandbox on the host rather than by every restore of one
	// shape -- the larger blast radius of the two.
	if err := chownTree(jailRoot, r.cfg.UID, r.cfg.GID, kernelPath, rootfsPath); err != nil {
		return fmt.Errorf("restore: chown jail: %w", err)
	}

	// The snapshot goes in *after* the chown, and the order is the point: the
	// staged files are hardlinks to one inode shared by every restore of this
	// shape, so a tree walk over them chowns that inode to this sandbox's VMM --
	// and while it did, every other live VMM restored from the same template could
	// have opened it for writing. Linking afterwards means the inode keeps the
	// root-owned 0444 it was sealed with at capture. The re-seal is belt and
	// braces, two syscalls, and makes the invariant hold however the file arrived
	// (a copy across filesystems, say). Same reasoning as vsock.Listen coming after
	// the chown on the cold path.
	for _, name := range []string{snapStateFile, snapMemFile} {
		dst := filepath.Join(jailRoot, name)
		if err := r.stageFile(filepath.Join(ref.Dir, name), dst); err != nil {
			return fmt.Errorf("restore: stage %s: %w", name, err)
		}
		if err := sealTemplateFile(dst); err != nil {
			return fmt.Errorf("restore: seal %s: %w", name, err)
		}
	}

	inst.udsPath = filepath.Join(jailRoot, vsockSocketName)
	inst.apiPath = filepath.Join(jailRoot, apiSocketName)
	inst.client = guestclient.New(inst.udsPath)
	// Before the VMM starts, for the same reason as the cold path: Firecracker
	// resolves this path when the guest first connects, and a socket that is not
	// there yet is a refused connection rather than a wait.
	if l, err := vsock.Listen(inst.udsPath, protocol.StoragePort, r.cfg.UID, r.cfg.GID); err != nil {
		// Not fatal -- a sandbox without it simply has no storage -- but it used
		// to be silent, which made a restored sandbox that could not reach storage
		// indistinguishable from one that had nothing to say.
		inst.log.Warn("no host listener for this restored sandbox; storage will be unavailable to it", "err", err)
	} else {
		inst.hostListener = l
	}

	// A fresh, blank VMM with only its API socket: Firecracker refuses to load a
	// snapshot into a VMM that has already started a microVM, so this one starts
	// nothing until the load below.
	if err := r.startForRestore(inst, spec); err != nil {
		return err
	}

	api := fcapi.New(inst.apiPath)
	if err := waitForSocket(ctx, inst.apiPath, inst.group); err != nil {
		return fmt.Errorf("restore: API socket never appeared: %w\n--- console ---\n%s", err, inst.consoleTail())
	}
	if err := api.LoadSnapshot(ctx, snapStateFile, snapMemFile, true); err != nil {
		return fmt.Errorf("restore: load snapshot: %w\n--- console ---\n%s", err, inst.consoleTail())
	}
	resumed := time.Now()
	if err := inst.waitReadyWithin(ctx, restoreReadyTimeout); err != nil {
		// The console before the teardown, exactly as the cold path does it: Stop
		// removes the jail, so this is the last moment the guest's own account of
		// itself exists. Not having it here is what made this failure take three
		// investigations -- the timeout alone says only that nothing answered,
		// which is true of every possible cause.
		return fmt.Errorf("restore: guest never became ready: %w\n--- guest console ---\n%s",
			err, inst.consoleTail())
	}

	// Logged because it is the number restoreReadyTimeout is set from, and the
	// one that says whether this host restores healthy guests: it is tens of
	// milliseconds when a restore works and the whole timeout when it does not.
	inst.log.Debug("restored guest answered", "after", time.Since(resumed))

	// The guest resumed inside the spin that repaired its interrupt controller;
	// let it go before the sandbox is handed over, and check that it repaired all
	// of itself and not just the vCPU that answered above. Ahead of the reseed so
	// that runs on a guest whose vCPUs are its own.
	if err := standDownCarry(ctx, inst); err != nil {
		return err
	}

	if err := reseedGuest(ctx, inst.client, ref.Digest); err != nil {
		// A restore that cannot reseed is a security hazard (shared CSPRNG), so
		// it fails closed rather than handing back a VM that reuses entropy.
		return fmt.Errorf("restore: reseed entropy: %w", err)
	}
	return nil
}

// armGuestForSnapshot asks the guest to carry the interrupt-controller state
// that Firecracker's GICv2 snapshot path drops.
//
// A guest that says it needs no carry (any x86 host, or an arm64 host with a
// GICv3) is the normal case and not worth more than a debug line. A guest that
// needs one and cannot provide it fails the snapshot here, rather than producing
// a snapshot whose every restore is a VM that never answers.
func armGuestForSnapshot(ctx context.Context, fi *instance) error {
	ctx, cancel := context.WithTimeout(ctx, armTimeout)
	defer cancel()

	resp, err := fi.client.ArmSnapshot(ctx)
	switch {
	case errors.Is(err, guestclient.ErrSnapshotArmUnsupported):
		// An older image. It restores fine where the host does not need the
		// carry, so this is a warning and not a refusal -- but on a GICv2 host
		// every restore from this snapshot will time out, and that is worth
		// saying once, here, rather than leaving it to be rediscovered.
		fi.log.Warn("guest image predates the snapshot arm route; restores will fail on a host whose "+
			"snapshots lose the guest's interrupt-controller state (any GICv2 arm64 host)", "err", err)
		return nil
	case err != nil:
		return fmt.Errorf("snapshot: arm guest: %w", err)
	case resp.Armed:
		fi.log.Debug("guest armed to carry its interrupt-controller state across the snapshot", "cpus", resp.CPUs)
	default:
		fi.log.Debug("guest needs no interrupt-controller carry", "detail", resp.Detail)
	}
	return nil
}

// standDownCarry closes the carry out on a guest that has come back, and is the
// only check the host has that the whole guest came back.
//
// The readiness probe above is one HTTP request, answered on whichever vCPU the
// guest's scheduler picked; the state a GICv2 snapshot loses is banked per vCPU.
// So one reply proves one vCPU. Arming leaves a pinned spinner per vCPU plus at
// least one unpinned runtime thread on the same vCPUs, which means at least one
// spinner is off-CPU when the host pauses -- and a vCPU whose spinner was off-CPU
// comes back with no timer and no reachable SGIs, so the first TLB shootdown wedges
// the whole guest. The agent's spinners each check their own registers before
// standing down, and this call is what waits for all of them and reads the tally.
//
// It is therefore fatal, where it used to warn. A stand-down that does not account
// for every vCPU is a sandbox that looks healthy and will hang on its first
// concurrent workload, and handing that to a tenant is worse than losing it: the
// pool recaptures or cold-boots, and the tenant gets a VM that works.
func standDownCarry(ctx context.Context, inst *instance) error {
	ctx, cancel := context.WithTimeout(ctx, standDownTimeout)
	defer cancel()

	resp, err := inst.client.DisarmSnapshot(ctx)
	if errors.Is(err, guestclient.ErrSnapshotArmUnsupported) {
		// An image with no carry at all. It restored and answered, which on this
		// host means the host never needed one.
		inst.log.Debug("guest image has no snapshot carry to stand down")
		return nil
	}
	if err != nil {
		return fmt.Errorf("restore: stand down the guest's interrupt-controller carry: %w", err)
	}
	if resp.Armed && resp.Checked < resp.CPUs {
		return fmt.Errorf("restore: the guest accounted for %d of its %d vCPUs' interrupt-controller "+
			"state; the rest have no timer and cannot be signalled, so this VM would wedge on its "+
			"first cross-CPU call", resp.Checked, resp.CPUs)
	}
	inst.log.Debug("restored guest stood down its interrupt-controller carry",
		"cpus", resp.CPUs, "reapplied", resp.Reapplied)
	return nil
}

// reseedGuest rotates the guest's CSPRNG so this restored VM's random numbers
// diverge from every other restore of the same snapshot.
//
// This is a dedicated guest route and not a write to /dev/urandom, and the
// difference is the whole of the security property. Writing to /dev/urandom mixes
// bytes into the kernel's input pool and does not re-derive the key getrandom(2)
// answers from; that key is rotated on a jiffies deadline which a snapshot
// restores identically into every restore, so restores of one template returned
// identical secrets for the same first minute of guest execution -- while this
// function reported success. The agent's route mixes *and* forces the
// re-derivation, and fails if the forcing fails. See internal/agent/reseed.go.
func reseedGuest(ctx context.Context, client *guestclient.Client, digest string) error {
	ctx, cancel := context.WithTimeout(ctx, reseedTimeout)
	defer cancel()

	tok, err := vmgenid.Mint(digest)
	if err != nil {
		return err
	}
	return client.ReseedEntropy(ctx, tok.Value)
}

// Discard deletes a snapshot's files.
//
// It refuses a directory outside SnapshotDir. A SnapshotRef is an in-memory
// struct that travels through the layer above, and "delete whatever path this
// says" is a recursive removal one confused caller away from being aimed at
// something else.
func (r *Runtime) Discard(ctx context.Context, ref runtime.SnapshotRef) error {
	if ref.Dir == "" {
		return nil
	}
	if r.cfg.SnapshotDir == "" {
		return fmt.Errorf("discard snapshot: snapshots are disabled (no SnapshotDir)")
	}
	if !isSnapshotDirEntry(r.cfg.SnapshotDir, ref.Dir) {
		return fmt.Errorf("discard snapshot: %s is not a snapshot under %s", ref.Dir, r.cfg.SnapshotDir)
	}
	if err := os.RemoveAll(ref.Dir); err != nil {
		return fmt.Errorf("discard snapshot %s: %w", ref.Dir, err)
	}
	r.log.Debug("discarded snapshot", "dir", ref.Dir)
	return nil
}

// snapshotDirPrefix names the directories a capture creates, and is what the
// sweep and Discard recognise as theirs.
const snapshotDirPrefix = "snap-"

// isSnapshotDirEntry reports whether dir is an immediate snapshot directory of
// base. Immediate and prefixed, not merely "underneath": a path check that
// accepted anything below base would accept base itself.
func isSnapshotDirEntry(base, dir string) bool {
	parent, name := filepath.Split(filepath.Clean(dir))
	if filepath.Clean(parent) != filepath.Clean(base) {
		return false
	}
	return strings.HasPrefix(name, snapshotDirPrefix)
}

// sweepOrphanedSnapshots removes snapshot directories left by a previous run and
// reports how many, and how many bytes, it reclaimed.
//
// Every snapshot here is unreachable: the only handle to one is a SnapshotRef,
// and those are held in memory by the process that captured them. So a snap-*
// directory present at startup belongs to a run that has ended.
//
// This does mean one SnapshotDir per daemon. Two daemons sharing one would
// reclaim each other's live templates -- which costs correctness nothing (the pool
// recaptures) but is worth knowing, and is why it is the state directory's
// subdirectory rather than somewhere shared.
func sweepOrphanedSnapshots(dir string) (count int, freed int64, err error) {
	if dir == "" {
		return 0, 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), snapshotDirPrefix) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		freed += dirBytes(path)
		if rmErr := os.RemoveAll(path); rmErr != nil {
			errs = append(errs, rmErr)
			continue
		}
		count++
	}
	return count, freed, errors.Join(errs...)
}

// dirBytes sums the sizes of the files in dir, for the log line that says how
// much a sweep got back. Best effort: a number that is slightly wrong is worth
// more here than a failure.
func dirBytes(dir string) int64 {
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

// fileIdentity fingerprints a file so a later check can tell it is the same one.
//
// Device, inode, size and modification time: a rebuilt image is a different inode
// and a replaced one is at minimum a different mtime, and none of it costs a read
// of a file that may be hundreds of megabytes.
func fileIdentity(path string) (string, error) {
	if path == "" {
		return "", errors.New("no path")
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "", err
	}
	mtime := time.Unix(st.Mtim.Unix())
	return fmt.Sprintf("dev=%d,ino=%d,size=%d,mtime=%d", st.Dev, st.Ino, st.Size, mtime.UnixNano()), nil
}

// checkRootfsUnchanged refuses a restore whose image file is not the one its
// template was captured against.
func checkRootfsUnchanged(ref runtime.SnapshotRef, rootfs string) error {
	if ref.RootfsID == "" {
		// A ref from before this was recorded. Nothing to compare, and inventing a
		// pass would be worse than saying so.
		return fmt.Errorf("restore: this snapshot does not record which image file it was captured from")
	}
	now, err := fileIdentity(rootfs)
	if err != nil {
		return fmt.Errorf("restore: identify rootfs %s: %w", rootfs, err)
	}
	if now != ref.RootfsID {
		return fmt.Errorf("restore: the image %s has changed since this snapshot was captured "+
			"(was %s, now %s); a guest resumed onto a different filesystem than the one its page "+
			"cache describes corrupts silently, so this snapshot is no longer usable", rootfs, ref.RootfsID, now)
	}
	return nil
}

// waitForSocket blocks until path exists, so the API client does not dial before
// Firecracker has created its socket.
//
// It watches the VMM's own exit as well as the clock. A jailer that dies on exec
// -- a bad binary, a cgroup layout the host rejects -- never creates the socket,
// and waiting out a timeout for a process that is already gone reports "slow"
// about something that was never coming.
// The liveness signal is the cgroup and NOT inst.exited. With --new-pid-ns the
// jailer must fork -- the init of a new PID namespace has to be a child -- so the
// process we started returns 0 about 30ms in, while the VMM it left behind is
// alive and has not created its socket yet. Reading that as death failed every
// restore on this host: measured, the parent returns in 29ms and Firecracker
// answers on its socket after that. The cgroup holds the actual VMM, so it is the
// only thing here that distinguishes a dead VM from a slow one.
func waitForSocket(ctx context.Context, path string, group *cgroup.Group) error {
	ctx, cancel := context.WithTimeout(ctx, apiSocketTimeout)
	defer cancel()

	// The jailer creates the cgroup and places the VMM in it, so an empty one only
	// means death once it has held something -- the same rule waitReadyWithin
	// applies for the same window.
	var seenPopulated bool

	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}

		if populated, err := group.Populated(); err == nil {
			switch {
			case populated:
				seenPopulated = true
			case seenPopulated:
				// One last look: the VMM could have created the socket and died
				// between the stat above and here.
				if _, err := os.Stat(path); err == nil {
					return nil
				}
				return errors.New("the VMM exited before it created its control socket")
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// startForRestore execs the jailer for a VM that will be restored from a
// snapshot: a blank VMM with only its API socket and no config file, since it
// loads its devices from the snapshot rather than booting them from a config. It
// mirrors start()'s jail and cgroup setup deliberately rather than sharing it,
// so the cold-boot path stays untouched.
func (r *Runtime) startForRestore(inst *instance, spec runtime.Spec) error {
	cores := spec.Limits.CPUCores
	if cores <= 0 {
		cores = r.cfg.DefaultCPUCores
	}
	args := []string{
		"--id", inst.jailID,
		"--exec-file", r.cfg.FirecrackerBin,
		"--uid", fmt.Sprint(r.cfg.UID),
		"--gid", fmt.Sprint(r.cfg.GID),
		"--chroot-base-dir", r.cfg.ChrootBase,
		"--cgroup-version", "2",
		"--parent-cgroup", r.cfg.Slice,
		"--new-pid-ns",
	}
	quota := cgroup.CoresToQuota(cores)
	args = append(args, "--cgroup", fmt.Sprintf("cpu.max=%d 100000", quota.Microseconds()))
	if spec.MemMiB > 0 {
		limit := uint64(spec.MemMiB)*1024*1024 + vmmOverheadBytes
		args = append(args, "--cgroup", fmt.Sprintf("memory.max=%d", limit))
	}
	args = append(args, "--", "--api-sock", apiSocketName)

	cmd := exec.Command(r.cfg.JailerBin, args...)
	logPath := filepath.Join(r.jailRoot(inst.jailID), "console.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("restore: create console log: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("restore: start jailer: %w", err)
	}
	inst.cmd = cmd
	inst.logFile = logFile
	inst.logPath = logPath
	inst.exited = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(inst.exited)
	}()
	return nil
}

// digestSnapshot identifies a snapshot by hashing its state file.
//
// It covers the state and not the memory image, and the honest reason is that
// nothing needs it to. Its only consumer is vmgenid.Mint, which binds it into a
// restore token whose actual uniqueness comes from crypto/rand on every call; no
// restore re-verifies it, and it never goes on a wire. Hashing the memory image
// too made every capture read the guest's entire RAM on the warm pool's single
// refill goroutine, ignoring its context -- 8 GiB at the 30 MB/s floor
// internal/fcapi cites is four and a half minutes of a pool that is not refilling
// -- to strengthen a claim nobody checks.
//
// Tamper-resistance for the snapshot is sealTemplateFile's job, not this one's.
func digestSnapshot(ctx context.Context, dir string) (string, error) {
	f, err := os.Open(filepath.Join(dir, snapStateFile))
	if err != nil {
		return "", fmt.Errorf("digest %s: %w", snapStateFile, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, newCtxReader(ctx, f)); err != nil {
		return "", fmt.Errorf("digest %s: %w", snapStateFile, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ctxReader makes a read cancellable at chunk boundaries, so a hash of a file on
// a slow disk ends when the caller gives up rather than when the file does.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func newCtxReader(ctx context.Context, r io.Reader) io.Reader { return &ctxReader{ctx: ctx, r: r} }

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
