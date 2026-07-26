//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/cgroup"
	"github.com/pablofdezr/microvm/internal/runtime"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A restore stages whatever file stands at the image path today, but the guest it
// resumes has the page cache and open inodes of the file the template was captured
// from. Rebuilding an image in place is the documented way to update one, so this
// is a thing operators do -- and a guest resumed onto a different filesystem
// corrupts silently behind a health probe that passes.
func TestRestoreRefusesARebuiltImage(t *testing.T) {
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "python.ext4")
	if err := os.WriteFile(rootfs, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := fileIdentity(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	ref := runtime.SnapshotRef{Rootfs: rootfs, RootfsID: id}

	if err := checkRootfsUnchanged(ref, rootfs); err != nil {
		t.Fatalf("the unchanged image was refused: %v", err)
	}

	// Rebuilt in place: a new inode at the same path, which is what a build script
	// that writes a temp file and renames it produces.
	rebuilt := filepath.Join(dir, "python.ext4.new")
	if err := os.WriteFile(rebuilt, []byte("rebuilt, and bigger"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rebuilt, rootfs); err != nil {
		t.Fatal(err)
	}

	err = checkRootfsUnchanged(ref, rootfs)
	if err == nil {
		t.Fatal("a rebuilt image was accepted: this restore would resume a guest onto a filesystem that is not its own")
	}
	if !strings.Contains(err.Error(), "has changed") {
		t.Errorf("error %q does not say what happened", err)
	}
}

// A ref from before the binding existed cannot be checked, and pretending it
// passes is the failure mode the check exists to prevent.
func TestRestoreRefusesASnapshotWithNoImageBinding(t *testing.T) {
	if err := checkRootfsUnchanged(runtime.SnapshotRef{}, "/nonexistent"); err == nil {
		t.Fatal("a snapshot with no recorded image was accepted")
	}
}

// TestRestoreRefusesANetworkedSpec used to live here, asserting that a restore
// refused any spec with Network set: a snapshot carries the guest's MAC and IP in
// its memory image and its host TAP in its device state, so a plain restore came
// back claiming the template's address.
//
// It is gone because the behaviour it guarded was deliberately replaced rather
// than kept -- a networked restore now takes a fresh netpool slot, remaps the
// interface onto the new TAP with Firecracker's network_overrides, and re-addresses
// the guest over vsock (see Restore and restoreInto). The test outlived the refusal
// it was written for by one commit, and since the early refusal was what returned
// before Restore touched anything, removing it left the test running on into a
// Runtime built by struct literal -- panicking on a nil cgroup slice that New
// always sets, so it failed for a reason unrelated to what it claimed to check.
//
// What replaced it is not unit-testable here: the override, the TAP and the guest
// reconfiguration only run against a real Firecracker on a KVM host. The host-side
// suspend/resume lifecycle is covered against the runtime fake instead.

// The snapshot files are read by every restore and writable by none of them.
//
// They arrive owned by the unprivileged uid every jailed Firecracker shares,
// because a jailed Firecracker wrote them, and a restore hardlinks them into each
// restored sandbox's own chroot. Ownership is write access -- an owner can chmod --
// so without this a VMM that escaped its guest could choose the bytes of the guest
// RAM, kernel text included, that every later restore of that shape boots with.
func TestSealTemplateFileLeavesItReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem")
	if err := os.WriteFile(path, []byte("guest ram"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := sealTemplateFile(path); err != nil {
		if os.Geteuid() != 0 {
			t.Skipf("chown to root needs root: %v", err)
		}
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o222 != 0 {
		t.Errorf("mode is %o: the shared template inode is writable", mode)
	}
	if mode := info.Mode().Perm(); mode&0o444 != 0o444 {
		t.Errorf("mode is %o: a jailed VMM could not read the template it must map", mode)
	}
}

// Discard must not be a "remove whatever this path says". A SnapshotRef is an
// in-memory struct travelling through the layer above, and a recursive removal
// aimed by one is one confused caller away from being aimed elsewhere.
func TestDiscardOnlyRemovesItsOwnSnapshots(t *testing.T) {
	base := t.TempDir()
	r := &Runtime{cfg: Config{SnapshotDir: base}, log: discardLog()}

	mine := filepath.Join(base, "snap-abc")
	if err := os.MkdirAll(mine, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()

	for _, dir := range []string{elsewhere, base, filepath.Join(base, "not-a-snapshot")} {
		if err := r.Discard(context.Background(), runtime.SnapshotRef{Dir: dir}); err == nil {
			t.Errorf("Discard accepted %s", dir)
		}
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Errorf("Discard removed a directory that was not a snapshot: %v", err)
	}

	if err := r.Discard(context.Background(), runtime.SnapshotRef{Dir: mine}); err != nil {
		t.Fatalf("Discard on its own snapshot: %v", err)
	}
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Error("the snapshot survived Discard")
	}
	// Idempotent: a shutdown path that discards twice must not report a failure.
	if err := r.Discard(context.Background(), runtime.SnapshotRef{Dir: mine}); err != nil {
		t.Errorf("second Discard: %v", err)
	}
}

// Snapshots from a previous run are unreachable -- the only handle to one is a
// SnapshotRef, and those live in memory -- so they are reclaimed at startup.
// Without this a daemon restarted twenty times leaves twenty full copies of a
// guest's RAM that no code path will ever open again.
func TestSweepOrphanedSnapshots(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"snap-one", "snap-two"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, name, "mem"), make([]byte, 128), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Not ours, and not to be touched.
	if err := os.MkdirAll(filepath.Join(base, "keep-me"), 0o700); err != nil {
		t.Fatal(err)
	}

	n, freed, err := sweepOrphanedSnapshots(base)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("swept %d snapshots, want 2", n)
	}
	if freed != 256 {
		t.Errorf("reported %d bytes reclaimed, want 256", freed)
	}
	if _, err := os.Stat(filepath.Join(base, "keep-me")); err != nil {
		t.Errorf("the sweep removed something that was not a snapshot: %v", err)
	}

	// A missing directory is not an error: snapshots may simply be off.
	if _, _, err := sweepOrphanedSnapshots(filepath.Join(base, "nope")); err != nil {
		t.Errorf("sweeping a missing dir: %v", err)
	}
	if _, _, err := sweepOrphanedSnapshots(""); err != nil {
		t.Errorf("sweeping with snapshots disabled: %v", err)
	}
}

// The wait for Firecracker's control socket ends when the VMM dies, not when the
// clock runs out. A jailer that dies on exec never creates the socket, and this
// used to poll the caller's context -- which, from the warm pool, has no deadline
// at all: the single refill goroutine blocked for the daemon's life, with no log
// line and no fallback to cold boots for any shape.
func TestWaitForSocketEndsWhenTheVMMDies(t *testing.T) {
	dir := t.TempDir()
	group := fakeCgroup(t, dir, true)

	done := make(chan error, 1)
	go func() {
		done <- waitForSocket(context.Background(), filepath.Join(dir, "never"), group)
	}()

	// Held a process, then lost it: that is death, and only that.
	time.Sleep(30 * time.Millisecond)
	writeCgroupEvents(t, dir, false)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("reported success for a socket that never appeared")
		}
		if !strings.Contains(err.Error(), "exited") {
			t.Errorf("error %q does not name the cause", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait outlived the process it was waiting on")
	}
}

// The regression that broke every restore: --new-pid-ns makes the jailer fork, so
// the process we started returns ~30ms in while the VMM it left behind is still
// coming up. This used to watch that exit and call it death, which no timeout and
// no retry could recover from -- the warm pool cold-booted every shape forever.
// An empty cgroup before the VMM has ever been in it is a VM that has not started,
// not one that has died.
func TestWaitForSocketWaitsThroughTheStartupWindow(t *testing.T) {
	dir := t.TempDir()
	group := fakeCgroup(t, dir, false) // not populated yet: the jailer has only forked
	path := filepath.Join(dir, "fc-api.sock")

	done := make(chan error, 1)
	go func() { done <- waitForSocket(context.Background(), path, group) }()

	// The VMM appears late, exactly as it does on hardware.
	time.Sleep(60 * time.Millisecond)
	writeCgroupEvents(t, dir, true)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("gave up on a VM that was still starting: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never noticed the socket")
	}
}

// fakeCgroup returns a Group rooted at dir, so the socket wait can be tested
// without a real cgroup hierarchy.
func fakeCgroup(t *testing.T, dir string, populated bool) *cgroup.Group {
	t.Helper()
	writeCgroupEvents(t, dir, populated)
	return (&cgroup.Group{}).Child(dir)
}

func writeCgroupEvents(t *testing.T, dir string, populated bool) {
	t.Helper()
	v := 0
	if populated {
		v = 1
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.events"),
		[]byte(fmt.Sprintf("populated %d\nfrozen 0\n", v)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A socket that appears is found even if the process has already exited: the
// stat and the exit can land in either order.
func TestWaitForSocketFindsALateSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fc-api.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	group := fakeCgroup(t, dir, false)

	if err := waitForSocket(context.Background(), path, group); err != nil {
		t.Fatalf("waitForSocket: %v", err)
	}
}

// The identity hashing a snapshot must respect its caller's context: it used to
// read the guest's entire memory image on the warm pool's single refill goroutine,
// ignoring cancellation -- minutes of a pool that is not refilling, to strengthen a
// claim nothing checks.
func TestDigestSnapshotHonoursCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, snapStateFile), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := digestSnapshot(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// And it still works when nobody cancels.
	got, err := digestSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Error("no digest")
	}
}

// The stand-down is the host's only check that the *whole* guest came back: the
// readiness probe is one HTTP reply, answered on one vCPU, while the state a GICv2
// snapshot loses is banked per vCPU. So the timeout for it must be tighter than
// the one for readiness -- a stand-down measured at 12ms that takes seconds means a
// vCPU is not being scheduled, which is a VM to discard rather than wait on.
func TestStandDownIsBoundedTighterThanReadiness(t *testing.T) {
	if standDownTimeout >= restoreReadyTimeout {
		t.Errorf("standDownTimeout %v must be under restoreReadyTimeout %v, or a stalled "+
			"stand-down dominates the latency this whole path exists to reduce",
			standDownTimeout, restoreReadyTimeout)
	}
	// And every step has to fit inside the one deadline over the whole restore, or
	// a step can be reached with the budget already spent.
	total := apiSocketTimeout + restoreReadyTimeout + standDownTimeout + reseedTimeout
	if total >= restoreTimeout {
		t.Errorf("the restore steps sum to %v, which does not fit in restoreTimeout %v", total, restoreTimeout)
	}
}
