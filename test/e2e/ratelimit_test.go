//go:build linux

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

// A rate limiter that Firecracker merely accepts is worth nothing. This drives
// real I/O through a capped device and measures what actually gets through --
// the only way to tell a working limiter from a config it ignored.
//
// The disk is the right thing to measure rather than the network, even though
// the network is what matters for abuse: reading the VM's own rootfs is local,
// deterministic, and does not depend on a third party still serving the same
// file next year. Both devices are limited by the same token bucket code, so
// this exercises the machinery either way.
func TestDiskRateLimitIsEnforced(t *testing.T) {
	image := "python-" + arch() + ".ext4"
	requireImage(t, image)

	mgr := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// 4 MB/s: slow enough that reading 24MB takes ~6s and is unmistakable,
	// fast enough that the test does not drag.
	const capBps = 4 * 1024 * 1024
	const readMB = 24

	sb, err := mgr.Create(ctx, sandbox.Spec{
		Spec: runtime.Spec{
			ID: "rl-disk", Image: image, VCPUs: 1, MemMiB: 256,
			Limits: runtime.Limits{CPUCores: 1, DiskBps: capBps},
		},
		TTL:         3 * time.Minute,
		IdleTimeout: -1,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer sb.Stop(context.Background(), sandbox.ReasonStopped)

	// iflag=direct bypasses the guest's page cache, so this measures the
	// virtual device rather than how much of the image the guest already had in
	// memory. Without it a cached read returns at RAM speed and the limiter
	// looks broken when it is simply not involved.
	script := fmt.Sprintf(
		`start=$(date +%%s.%%N); dd if=/dev/vda of=/dev/null bs=1M count=%d iflag=direct 2>/dev/null; `+
			`end=$(date +%%s.%%N); echo "$start $end"`, readMB)

	err = sb.Exec(ctx, protocol.ExecRequest{
		ID: "read", Cmd: "sh", Args: []string{"-c", script},
		Timeout: 120 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	rec, _ := sb.Logs("read")
	if rec.ExitCode == nil || *rec.ExitCode != 0 {
		t.Fatalf("the read failed with %s: %s", formatExitCode(rec.ExitCode), rec.Stderr)
	}

	var start, end float64
	if _, err := fmt.Sscan(string(rec.Stdout), &start, &end); err != nil {
		t.Fatalf("could not parse %q: %v", rec.Stdout, err)
	}

	elapsed := end - start
	rate := float64(readMB<<20) / elapsed
	t.Logf("read %dMB in %.2fs = %.0f B/s (%.1f MB/s) against a %d B/s (%.0f MB/s) cap",
		readMB, elapsed, rate, rate/(1<<20), capBps, float64(capBps)/(1<<20))

	// Generous slack: the one-time burst lets the first second through ungated,
	// so a short read legitimately averages above the cap. What must not happen
	// is the read running at full device speed.
	if rate > capBps*3 {
		t.Errorf("read at %.1f MB/s against a %.0f MB/s cap: the rate limiter is not being enforced",
			rate/(1<<20), float64(capBps)/(1<<20))
	}
}

// The network limiter, measured against the public internet.
//
// Skipped rather than failed when the endpoint misbehaves: a third party's
// outage is not a defect in this code, and a test that fails for someone else's
// reasons trains you to ignore it.
func TestNetworkRateLimitIsEnforced(t *testing.T) {
	image := "python-" + arch() + ".ext4"
	requireImage(t, image)

	mgr := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// 200 KB/s: slow enough that a 1MB transfer takes ~5s and is unmistakable,
	// fast enough that the test does not take a minute.
	const capBps = 200 * 1024

	sb, err := mgr.Create(ctx, sandbox.Spec{
		Spec: runtime.Spec{
			ID: "rl-net", Image: image, VCPUs: 1, MemMiB: 256,
			Network: true,
			Limits:  runtime.Limits{CPUCores: 1, NetworkBps: capBps},
		},
		TTL:         3 * time.Minute,
		IdleTimeout: -1,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer sb.Stop(context.Background(), sandbox.ReasonStopped)

	// Try several public endpoints: any one of them can move, change size, or
	// start rejecting requests, and none of that is a bug here.
	script := `
import time, urllib.request
urls = [
    "http://speedtest.tele2.net/1MB.zip",
    "https://proof.ovh.net/files/1Mb.dat",
    "https://speed.cloudflare.com/__down?bytes=1048576",
]
for url in urls:
    try:
        start = time.monotonic()
        with urllib.request.urlopen(url, timeout=45) as r:
            n = len(r.read())
        if n < 100000:
            continue
        print("%d %.3f" % (n, time.monotonic() - start))
        break
    except Exception:
        continue
else:
    raise SystemExit("no endpoint served a usable file")
`

	err = sb.Exec(ctx, protocol.ExecRequest{
		ID: "download", Cmd: "python3", Args: []string{"-c", script},
		Timeout: 120 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	rec, _ := sb.Logs("download")
	if rec.ExitCode == nil || *rec.ExitCode != 0 {
		t.Skipf("the download did not complete (network may be unavailable): %s", rec.Stderr)
	}

	var got int
	var elapsed float64
	if _, err := fmt.Sscan(string(rec.Stdout), &got, &elapsed); err != nil {
		t.Fatalf("could not parse %q: %v", rec.Stdout, err)
	}

	rate := float64(got) / elapsed
	t.Logf("downloaded %d bytes in %.2fs = %.0f B/s against a %d B/s cap",
		got, elapsed, rate, capBps)

	// Generous slack: the limiter's one-time burst lets the first second
	// through ungated, so a 1MiB transfer legitimately averages above the cap.
	// What must not happen is the transfer running at full line rate, which on
	// this link is many megabytes a second.
	if rate > capBps*4 {
		t.Errorf("traffic flowed at %.0f B/s against a %d B/s cap: the rate limiter is not being enforced",
			rate, capBps)
	}
}

// The limiter must not be so eager that it strangles an unlimited sandbox: a
// zero-size token bucket means "no tokens, ever", and would stop the device
// dead rather than leave it alone.
func TestNoRateLimitMeansNoLimit(t *testing.T) {
	image := "python-" + arch() + ".ext4"
	requireImage(t, image)

	mgr := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sb, err := mgr.Create(ctx, sandbox.Spec{
		Spec: runtime.Spec{
			ID: "rl-none", Image: image, VCPUs: 1, MemMiB: 256,
			Network: true,
			// No NetworkBps at all.
			Limits: runtime.Limits{CPUCores: 1},
		},
		TTL:         2 * time.Minute,
		IdleTimeout: -1,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer sb.Stop(context.Background(), sandbox.ReasonStopped)

	// It must still be able to talk at all.
	err = sb.Exec(ctx, protocol.ExecRequest{
		ID:  "reach",
		Cmd: "bash",
		Args: []string{"-c",
			`timeout 10 bash -c 'exec 3<>/dev/tcp/1.1.1.1/80' && echo REACHED || echo BLOCKED`},
		Timeout: 30 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	rec, _ := sb.Logs("reach")
	if !strings.Contains(string(rec.Stdout), "REACHED") {
		t.Errorf("an unlimited sandbox could not reach the network: stdout=%q stderr=%q",
			rec.Stdout, rec.Stderr)
	}
}
