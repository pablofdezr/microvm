//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
// NOTE: the create/load calls exercise Firecracker's snapshot API, which needs
// KVM and a guest kernel that supports snapshot resume. This code compiles and
// its request-building is unit-tested in internal/fcapi, but the end-to-end
// restore -- and in particular the guest-agent reconnect timing that mvm reports
// as fragile -- must be validated on real hardware via the e2e suite.

const (
	snapStateFile = "state"
	snapMemFile   = "mem"

	// restoreReadyTimeout is how long a restored VM has to answer. It is longer
	// than a cold boot's: a guest resumed from a snapshot re-establishes its vsock
	// session slower than one that booted fresh.
	restoreReadyTimeout = 90 * time.Second
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

	dir, err := os.MkdirTemp(r.cfg.SnapshotDir, "snap-*")
	if err != nil {
		return runtime.SnapshotRef{}, fmt.Errorf("snapshot: make dir: %w", err)
	}

	// Firecracker writes the snapshot to paths relative to its chroot, so it
	// lands in the jail; we move it out to SnapshotDir afterwards so it survives
	// the jail's teardown.
	jailRoot := r.jailRoot(fi.jailID)
	api := fcapi.New(fi.apiPath)
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
	}

	// The source VM has served its purpose; it is stopped so the snapshot, not a
	// live VM, is what gets reused.
	_ = fi.Stop(ctx)

	digest, err := digestSnapshot(dir)
	if err != nil {
		os.RemoveAll(dir)
		return runtime.SnapshotRef{}, err
	}
	return runtime.SnapshotRef{Dir: dir, Digest: digest}, nil
}

// Restore boots a fresh VM from a snapshot and reseeds its entropy before
// returning it. Every VM restored from one snapshot begins with an identical
// CSPRNG, so the reseed -- stirring a unique, snapshot-bound token into the
// guest's pool -- is not optional: without it, restored VMs share keys.
func (r *Runtime) Restore(ctx context.Context, spec runtime.Spec, ref runtime.SnapshotRef) (runtime.Instance, error) {
	if r.cfg.SnapshotDir == "" {
		return nil, fmt.Errorf("restore: snapshots are disabled (no SnapshotDir)")
	}

	jailID := newJailID()
	if err := r.checkVsockPath(jailID); err != nil {
		return nil, err
	}
	inst := &instance{
		id:      spec.ID,
		jailID:  jailID,
		runtime: r,
		log:     r.log.With("sandbox", spec.ID, "jail", jailID),
		started: time.Now(),
		// The cgroup the jailer creates, at <slice>/<jail-id>; waitReady polls it
		// for the guest coming up, so it must be set before startForRestore runs.
		group: r.slice.Child(jailID),
	}

	if spec.Network {
		lease, err := r.pool.Allocate()
		if err != nil {
			return nil, err
		}
		inst.lease = &lease
		if err := r.taps.Create(lease); err != nil {
			return nil, fmt.Errorf("restore: create tap: %w", err)
		}
	}

	jailRoot := r.jailRoot(inst.jailID)
	if err := os.MkdirAll(jailRoot, 0o755); err != nil {
		return nil, fmt.Errorf("restore: jail: %w", err)
	}

	// The block devices are not inside a snapshot, only referenced by it, so the
	// kernel, rootfs and the snapshot's own files must all be staged at the paths
	// the memory image expects.
	if err := r.stageFile(r.cfg.KernelPath, filepath.Join(jailRoot, "vmlinux")); err != nil {
		return nil, fmt.Errorf("restore: stage kernel: %w", err)
	}
	if err := r.stageFile(filepath.Join(r.cfg.ImageDir, spec.Image), filepath.Join(jailRoot, "rootfs.ext4")); err != nil {
		return nil, fmt.Errorf("restore: stage rootfs: %w", err)
	}
	for _, name := range []string{snapStateFile, snapMemFile} {
		if err := r.stageFile(filepath.Join(ref.Dir, name), filepath.Join(jailRoot, name)); err != nil {
			return nil, fmt.Errorf("restore: stage %s: %w", name, err)
		}
	}

	if err := chownTree(jailRoot, r.cfg.UID, r.cfg.GID); err != nil {
		return nil, fmt.Errorf("restore: chown jail: %w", err)
	}

	inst.udsPath = filepath.Join(jailRoot, vsockSocketName)
	inst.apiPath = filepath.Join(jailRoot, apiSocketName)
	inst.client = guestclient.New(inst.udsPath)
	if l, err := vsock.Listen(inst.udsPath, protocol.StoragePort, r.cfg.UID, r.cfg.GID); err == nil {
		inst.hostListener = l
	}

	// A fresh, blank VMM with only its API socket: Firecracker refuses to load a
	// snapshot into a VMM that has already started a microVM, so this one starts
	// nothing until the load below.
	if err := r.startForRestore(inst, spec); err != nil {
		return nil, err
	}

	api := fcapi.New(inst.apiPath)
	if err := waitForSocket(ctx, inst.apiPath); err != nil {
		_ = inst.Stop(ctx)
		return nil, fmt.Errorf("restore: API socket never appeared: %w", err)
	}
	if err := api.LoadSnapshot(ctx, snapStateFile, snapMemFile, true); err != nil {
		_ = inst.Stop(ctx)
		return nil, fmt.Errorf("restore: load snapshot: %w\n--- console ---\n%s", err, inst.consoleTail())
	}
	if err := inst.waitReadyWithin(ctx, restoreReadyTimeout); err != nil {
		_ = inst.Stop(ctx)
		return nil, fmt.Errorf("restore: guest never became ready: %w", err)
	}

	if err := reseedGuest(ctx, inst.client, ref.Digest); err != nil {
		// A restore that cannot reseed is a security hazard (shared CSPRNG), so
		// it fails closed rather than handing back a VM that reuses entropy.
		_ = inst.Stop(ctx)
		return nil, fmt.Errorf("restore: reseed entropy: %w", err)
	}

	r.mu.Lock()
	r.insts[inst.id] = inst
	r.mu.Unlock()
	return inst, nil
}

// reseedGuest stirs a fresh, snapshot-bound token into the guest's entropy pool
// so this restored VM's CSPRNG diverges from every other restore of the same
// snapshot. Writing to /dev/urandom mixes the bytes in without needing to credit
// entropy (which would require CAP_SYS_ADMIN); the state change is what matters.
func reseedGuest(ctx context.Context, client *guestclient.Client, digest string) error {
	tok, err := vmgenid.Mint(digest)
	if err != nil {
		return err
	}
	return client.WriteFile(ctx, "/dev/urandom", bytes.NewReader(tok.Value), "0644")
}

// waitForSocket blocks until path exists, so the API client does not dial before
// Firecracker has created its socket.
func waitForSocket(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
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

// digestSnapshot hashes the snapshot's state and memory files, binding a restore
// token to exactly this snapshot so it cannot be replayed against another.
func digestSnapshot(dir string) (string, error) {
	h := sha256.New()
	for _, name := range []string{snapStateFile, snapMemFile} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("digest %s: %w", name, err)
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", fmt.Errorf("digest %s: %w", name, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
