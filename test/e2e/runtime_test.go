//go:build linux

package e2e

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/cgroup"
	"github.com/pablofdezr/microvm/internal/guestclient"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
	fcruntime "github.com/pablofdezr/microvm/internal/runtime/firecracker"
)

// The test bench shares a host with unrelated production services, so the
// ceiling here is deliberately small: whatever these tests do, they cannot take
// more than this from the rest of the box.
const (
	testCeilingCores = 1.0
	testCeilingMem   = 1 << 30 // 1 GiB
)

// runtimePoolCIDR is distinct from the network tests' pool so the two suites do
// not fight over TAP devices if they ever run concurrently.
const runtimePoolCIDR = "172.30.0.0/16"

func newTestRuntime(t *testing.T) *fcruntime.Runtime {
	t.Helper()
	requireRoot(t)
	kernel, rootfs := requireEnv(t)

	// The jail must share a filesystem with the images, or every sandbox copies
	// a few hundred MB instead of hardlinking.
	chrootBase := filepath.Join(filepath.Dir(rootfs), "jailer-test")
	t.Cleanup(func() { _ = os.RemoveAll(chrootBase) })

	uid, gid := unprivilegedIDs(t)

	rt, err := fcruntime.New(fcruntime.Config{
		ChrootBase: chrootBase,
		ImageDir:   filepath.Dir(rootfs),
		KernelPath: kernel,
		Slice:      "microvm-test.slice",
		Ceiling: cgroup.Limits{
			CPU:       cgroup.CoresToQuota(testCeilingCores),
			MemoryMax: testCeilingMem,
		},
		UID:      uid,
		GID:      gid,
		PoolCIDR: netip.MustParsePrefix(runtimePoolCIDR),
	}, testLogger())
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// unprivilegedIDs returns a non-root uid/gid for the VMM to drop to.
func unprivilegedIDs(t *testing.T) (int, int) {
	t.Helper()
	// The tests run under sudo, so SUDO_UID names the human who invoked them.
	if u := os.Getenv("SUDO_UID"); u != "" {
		var uid, gid int
		fmt.Sscanf(u, "%d", &uid)
		fmt.Sscanf(os.Getenv("SUDO_GID"), "%d", &gid)
		if uid > 0 && gid > 0 {
			return uid, gid
		}
	}
	return 65534, 65534 // nobody:nogroup
}

func imageName(t *testing.T) string {
	t.Helper()
	_, rootfs := requireEnv(t)
	return filepath.Base(rootfs)
}

func TestRuntimeCreatesAndRunsSandbox(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	inst, err := rt.Create(ctx, runtime.Spec{
		ID:     "rt-basic",
		Image:  imageName(t),
		VCPUs:  1,
		MemMiB: 256,
		Limits: runtime.Limits{CPUCores: 0.5, DiskMiB: 64},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer inst.Stop(context.Background())

	res, err := guestclient.Collect(ctx, inst.Client(), protocol.ExecRequest{
		ID:   "hello",
		Cmd:  "sh",
		Args: []string{"-c", "echo jailed-and-running; id -u"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exec failed with %d: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "jailed-and-running") {
		t.Errorf("stdout = %q", res.Stdout)
	}
	t.Logf("guest output: %s", strings.TrimSpace(string(res.Stdout)))
}

// The VMM must not be running as root on the host, which is the entire reason
// the jailer is in the picture.
func TestVMMRunsUnprivileged(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	inst, err := rt.Create(ctx, runtime.Spec{
		ID: "rt-jail", Image: imageName(t), VCPUs: 1, MemMiB: 256,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer inst.Stop(context.Background())

	uid, _ := unprivilegedIDs(t)

	// Find the firecracker process on the host and check who owns it.
	out, err := runHost("pgrep", "-u", fmt.Sprint(uid), "firecracker")
	if err != nil || strings.TrimSpace(out) == "" {
		rootOut, _ := runHost("pgrep", "-u", "0", "firecracker")
		if strings.TrimSpace(rootOut) != "" {
			t.Fatal("firecracker is running as root: the jailer did not drop privileges")
		}
		t.Fatalf("no firecracker process found for uid %d: %v", uid, err)
	}
	t.Logf("firecracker running as uid %d, not root", uid)
}

// Metering is the basis of the billing model: CPU actually burned, not
// wall-clock. A sandbox that sleeps must bill almost nothing.
func TestMeteringSeparatesActiveCPUFromIdle(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	inst, err := rt.Create(ctx, runtime.Spec{
		ID: "rt-meter", Image: imageName(t), VCPUs: 1, MemMiB: 256,
		Limits: runtime.Limits{CPUCores: 1},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer inst.Stop(context.Background())

	before, err := inst.Stats()
	if err != nil {
		t.Fatal(err)
	}

	// Sleep, not spin: this is the I/O-wait case the whole model turns on.
	if _, err := guestclient.Collect(ctx, inst.Client(), protocol.ExecRequest{
		ID: "sleep", Cmd: "sleep", Args: []string{"3"},
	}); err != nil {
		t.Fatal(err)
	}

	after, err := inst.Stats()
	if err != nil {
		t.Fatal(err)
	}

	// Measure over the window, not since creation. Stats.ActiveCPU and Idle are
	// cumulative, and booting the VM burns a couple of CPU-seconds across the
	// VMM's threads in well under a second of wall -- so cumulative idle is
	// dominated by that transient for the sandbox's first few seconds. A biller
	// charges the delta between two samples, so that is what to assert on.
	cpuBurned := after.ActiveCPU - before.ActiveCPU
	wallElapsed := after.Wall - before.Wall
	idleGained := wallElapsed - cpuBurned

	t.Logf("over a 3s sleep: wall +%v, active CPU +%v, idle +%v",
		wallElapsed.Round(time.Millisecond),
		cpuBurned.Round(time.Millisecond),
		idleGained.Round(time.Millisecond))

	// The guest kernel ticks a little even when idle, so this is not zero --
	// but it must be nothing like three seconds, or we would be billing for
	// waiting.
	if cpuBurned > 1500*time.Millisecond {
		t.Errorf("sleeping for 3s burned %v of CPU: idle time is being billed", cpuBurned)
	}
	if idleGained < 2*time.Second {
		t.Errorf("a 3s sleep produced only %v of idle: idle accounting looks wrong", idleGained)
	}

	// Now burn CPU deliberately and confirm the meter moves.
	if _, err := guestclient.Collect(ctx, inst.Client(), protocol.ExecRequest{
		ID: "burn", Cmd: "sh", Args: []string{"-c", "end=$(( $(date +%s) + 3 )); while [ $(date +%s) -lt $end ]; do :; done"},
		Timeout: 20 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	burned, err := inst.Stats()
	if err != nil {
		t.Fatal(err)
	}
	spinCPU := burned.ActiveCPU - after.ActiveCPU
	t.Logf("3s of spinning cost %v of active CPU", spinCPU)

	if spinCPU < time.Second {
		t.Errorf("spinning for 3s only registered %v of CPU: the meter is not tracking work", spinCPU)
	}
}

// A sandbox must not be able to exceed its CPU limit, whatever it runs. This is
// the abuse case: a recursive function or a mining loop trying to take the box.
func TestCPULimitIsEnforced(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const limitCores = 0.25

	inst, err := rt.Create(ctx, runtime.Spec{
		ID: "rt-cpulimit", Image: imageName(t), VCPUs: 1, MemMiB: 256,
		Limits: runtime.Limits{CPUCores: limitCores},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer inst.Stop(context.Background())

	before, err := inst.Stats()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()

	// Spin as hard as the guest can for a fixed wall-clock window.
	if _, err := guestclient.Collect(ctx, inst.Client(), protocol.ExecRequest{
		ID: "hog", Cmd: "sh", Args: []string{"-c", "end=$(( $(date +%s) + 5 )); while [ $(date +%s) -lt $end ]; do :; done"},
		Timeout: 30 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	after, err := inst.Stats()
	if err != nil {
		t.Fatal(err)
	}
	wall := time.Since(start)
	burned := after.ActiveCPU - before.ActiveCPU

	ratio := float64(burned) / float64(wall)
	t.Logf("hogging for %v burned %v of CPU (%.2f cores, limit %.2f)", wall, burned, ratio, limitCores)

	// Allow generous slack: the host is oversubscribed and the accounting
	// window is 100ms, so a little overshoot is normal. Anything near a whole
	// core would mean the limit did nothing.
	if ratio > limitCores*2 {
		t.Errorf("sandbox burned %.2f cores against a %.2f core limit: cpu.max is not being enforced",
			ratio, limitCores)
	}
}

// Stopping must release everything: a leaked TAP device or cgroup accumulates
// until the host runs out.
func TestStopReleasesEverything(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	inst, err := rt.Create(ctx, runtime.Spec{
		ID: "rt-cleanup", Image: imageName(t), VCPUs: 1, MemMiB: 256, Network: true,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// Confirm the resources exist while it runs, so their absence afterwards
	// means something.
	taps, _ := runHost("sh", "-c", "ip link show | grep -c fctap || true")
	if strings.TrimSpace(taps) == "0" {
		t.Fatal("no tap device while the sandbox is running")
	}

	if err := inst.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Stop is idempotent: teardown paths call it unconditionally.
	if err := inst.Stop(ctx); err != nil {
		t.Errorf("second stop returned %v, want nil", err)
	}

	tapsAfter, _ := runHost("sh", "-c", "ip link show | grep -c fctap || true")
	if strings.TrimSpace(tapsAfter) != "0" {
		t.Errorf("%s tap devices remain after stop", strings.TrimSpace(tapsAfter))
	}

	if _, err := os.Stat("/sys/fs/cgroup/microvm-test.slice/rt-cleanup"); err == nil {
		t.Error("cgroup remains after stop")
	}
}
