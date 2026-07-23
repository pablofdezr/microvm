//go:build linux

// Package e2e drives the whole stack against real microVMs: real KVM, real
// Firecracker, real TAP devices, real nftables.
//
// These tests are the only place the security claims are actually checked. A
// unit test can assert that a rule was rendered; only a booted guest can prove
// the packet does not get out. They need root (TAP and nftables are privileged)
// and are skipped otherwise.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/guestclient"
	"github.com/pablofdezr/microvm/internal/netpool"
	"github.com/pablofdezr/microvm/internal/protocol"
)

// poolCIDR is deliberately not the daemon's default, so a test run cannot
// disturb sandboxes belonging to a real daemon on the same host.
const poolCIDR = "172.31.0.0/16"

const testTable = "microvm-e2e"

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root for TAP devices and nftables; run with sudo")
	}
}

func requireEnv(t *testing.T) (kernel, rootfs string) {
	t.Helper()
	kernel = os.Getenv("MICROVM_TEST_KERNEL")
	rootfs = os.Getenv("MICROVM_TEST_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set MICROVM_TEST_KERNEL and MICROVM_TEST_ROOTFS to run e2e tests")
	}
	if _, err := exec.LookPath("firecracker"); err != nil {
		t.Skip("firecracker not on PATH")
	}
	return kernel, rootfs
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// vm is a booted microVM under test.
type vm struct {
	client  *guestclient.Client
	udsPath string
	cmd     *exec.Cmd
	logPath string
}

// bootVM brings up a guest with networking and waits for its agent.
func bootVM(t *testing.T, lease netpool.Lease) *vm {
	t.Helper()
	kernel, rootfs := requireEnv(t)

	dir := t.TempDir()
	// Firecracker binds the vsock socket itself, and refuses a path that is
	// already occupied.
	udsPath := filepath.Join(dir, "v.sock")
	logPath := filepath.Join(dir, "boot.log")

	// The rootfs is attached read-only and shared: the guest stacks a tmpfs over
	// it, so concurrent VMs never write to the same image.
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro "+
			"microvm.ip=%s microvm.gw=%s microvm.dns=1.1.1.1 microvm.hostname=e2e microvm.upper_size=64m",
		lease.GuestCIDR(), lease.HostIP)

	cfg := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": kernel,
			"boot_args":         bootArgs,
		},
		"machine-config": map[string]any{"vcpu_count": 1, "mem_size_mib": 256},
		"drives": []any{map[string]any{
			"drive_id":       "rootfs",
			"path_on_host":   rootfs,
			"is_root_device": true,
			"is_read_only":   true,
		}},
		"network-interfaces": []any{map[string]any{
			"iface_id":      "eth0",
			"guest_mac":     lease.MAC.String(),
			"host_dev_name": lease.TapName,
		}},
		"vsock": map[string]any{"guest_cid": protocol.GuestCID, "uds_path": udsPath},
	}

	cfgPath := filepath.Join(dir, "vm.json")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("firecracker", "--no-api", "--config-file", cfgPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// The guest console is on stdin/stdout; leaving stdin attached to the test
	// process makes Firecracker consume it.
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		t.Fatalf("start firecracker: %v", err)
	}

	v := &vm{udsPath: udsPath, cmd: cmd, logPath: logPath}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		logFile.Close()
		if t.Failed() {
			// The console log is the only record of a guest that failed to boot.
			if b, err := os.ReadFile(logPath); err == nil {
				t.Logf("guest console:\n%s", b)
			}
		}
	})

	v.client = guestclient.New(udsPath)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := v.client.WaitReady(ctx); err != nil {
		t.Fatalf("guest never became ready: %v", err)
	}
	return v
}

// setupNetwork installs the firewall and a TAP device, returning the lease.
func setupNetwork(t *testing.T) netpool.Lease {
	t.Helper()

	fw, err := netpool.NewFirewall(netpool.FirewallConfig{
		PoolCIDR: netip.MustParsePrefix(poolCIDR),
		Table:    testTable,
	}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := fw.Install(); err != nil {
		t.Fatalf("install firewall: %v", err)
	}
	t.Cleanup(func() { _ = fw.Remove() })

	pool, err := netpool.New(netip.MustParsePrefix(poolCIDR))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := pool.Allocate()
	if err != nil {
		t.Fatal(err)
	}

	taps := netpool.NewTapManager(testLogger(), 0, 0)
	if err := taps.Create(lease); err != nil {
		t.Fatalf("create tap: %v", err)
	}
	t.Cleanup(func() { _ = taps.Delete(lease.TapName) })

	return lease
}

// canReach reports whether the guest can open a TCP connection to host:port.
//
// bash's /dev/tcp is used rather than curl or nc so that the guest image needs
// no networking tools: adding them would bloat every language image to serve a
// test. A blocked destination shows up as a timeout, so the probe is given a
// hard deadline and a refusal is treated as "reachable" -- a RST means packets
// got there and something answered, which is exactly what must not happen.
func canReach(t *testing.T, v *vm, host string, port int) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	script := fmt.Sprintf(
		`timeout 4 bash -c 'exec 3<>/dev/tcp/%s/%d' 2>&1 && echo REACHED || echo BLOCKED`,
		host, port)

	res, err := v.client.ExecCollect(ctx, protocol.ExecRequest{
		ID:      fmt.Sprintf("probe-%s-%d", strings.ReplaceAll(host, ".", "-"), port),
		Cmd:     "bash",
		Args:    []string{"-c", script},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("probe %s:%d failed to run: %v", host, port, err)
	}

	out := string(res.Stdout)
	switch {
	case strings.Contains(out, "REACHED"):
		return true
	case strings.Contains(out, "BLOCKED"):
		return false
	default:
		t.Fatalf("probe %s:%d gave no verdict: stdout=%q stderr=%q", host, port, out, res.Stderr)
		return false
	}
}

// TestSandboxEgressIsFiltered is the security test this whole layer exists for.
//
// It asserts the two halves of the requirement together: a sandbox can install
// dependencies from the internet, and it cannot touch anything private. Testing
// only the first would pass with no firewall at all.
func TestSandboxEgressIsFiltered(t *testing.T) {
	requireRoot(t)
	lease := setupNetwork(t)
	v := bootVM(t, lease)

	t.Run("public internet is reachable", func(t *testing.T) {
		// Without this, "everything is blocked" would look like a pass while
		// making the product useless: no pip install, no npm install.
		if !canReach(t, v, "1.1.1.1", 80) {
			t.Error("sandbox cannot reach the public internet; dependency installs would fail")
		}
	})

	// Each of these is a way a hostile sandbox could pivot off the host.
	blocked := []struct {
		name string
		host string
		port int
		why  string
	}{
		{"cloud metadata", "169.254.169.254", 80,
			"metadata services hand out credentials to anything that asks"},
		{"link-local", "169.254.0.1", 80,
			"link-local addressing bypasses routing"},
		{"private 10/8", "10.0.0.1", 80,
			"RFC1918 space may hold other infrastructure"},
		{"private 192.168/16 router", "192.168.1.1", 80,
			"this is the LAN the host sits on: the router must be unreachable"},
		{"private 192.168/16 host", "192.168.1.128", 22,
			"the host's own SSH must be unreachable from inside a sandbox"},
		{"its own gateway", "172.31.0.1", 22,
			"the gateway is a next hop, not a service the guest may connect to"},
	}

	for _, tc := range blocked {
		t.Run(tc.name+" is blocked", func(t *testing.T) {
			if canReach(t, v, tc.host, tc.port) {
				t.Errorf("sandbox reached %s:%d -- %s", tc.host, tc.port, tc.why)
			}
		})
	}
}

// Two sandboxes must not be able to see each other, even though both are in the
// same pool network.
func TestSandboxesCannotReachEachOther(t *testing.T) {
	requireRoot(t)

	fw, err := netpool.NewFirewall(netpool.FirewallConfig{
		PoolCIDR: netip.MustParsePrefix(poolCIDR),
		Table:    testTable,
	}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := fw.Install(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fw.Remove() })

	pool, err := netpool.New(netip.MustParsePrefix(poolCIDR))
	if err != nil {
		t.Fatal(err)
	}
	taps := netpool.NewTapManager(testLogger(), 0, 0)

	leaseA, err := pool.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	leaseB, err := pool.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []netpool.Lease{leaseA, leaseB} {
		if err := taps.Create(l); err != nil {
			t.Fatalf("create tap %s: %v", l.TapName, err)
		}
		t.Cleanup(func() { _ = taps.Delete(l.TapName) })
	}

	vmA := bootVM(t, leaseA)
	_ = bootVM(t, leaseB)

	// B's address is in the pool, which sits inside RFC1918 and is therefore
	// covered by the same blocked set that protects the LAN. The /30 topology
	// means the packet must route through the host to have any chance at all.
	if canReach(t, vmA, leaseB.GuestIP.String(), 5000) {
		t.Errorf("sandbox A reached sandbox B at %s: tenants are not isolated", leaseB.GuestIP)
	}
}

// The firewall must survive a daemon restart, and reinstalling must not stack
// duplicate rules or leave a window with no filtering.
func TestFirewallInstallIsIdempotent(t *testing.T) {
	requireRoot(t)

	fw, err := netpool.NewFirewall(netpool.FirewallConfig{
		PoolCIDR: netip.MustParsePrefix(poolCIDR),
		Table:    testTable,
	}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fw.Remove() })

	for i := 0; i < 3; i++ {
		if err := fw.Install(); err != nil {
			t.Fatalf("install %d: %v", i+1, err)
		}
	}

	dump, err := netpool.DumpRuleset(testTable)
	if err != nil {
		t.Fatal(err)
	}
	// Three installs must leave exactly one copy of the egress rule.
	if got := strings.Count(dump, "@blocked4"); got != 1 {
		t.Errorf("blocked4 rule appears %d times after 3 installs, want 1:\n%s", got, dump)
	}
}
