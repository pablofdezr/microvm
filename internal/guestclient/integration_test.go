package guestclient_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/guestclient"
	"github.com/pablofdezr/microvm/internal/protocol"
)

// These tests drive a real guest over a real vsock socket. They are skipped
// unless MICROVM_VSOCK_UDS points at the host-side socket of a running VM,
// because they need KVM, a booted microVM and an agent inside it -- none of
// which exist on a developer laptop.
//
// Run them with:
//
//	MICROVM_VSOCK_UDS=/path/to/v.sock go test ./internal/guestclient/ -v
func testClient(t *testing.T) *guestclient.Client {
	t.Helper()

	uds := os.Getenv("MICROVM_VSOCK_UDS")
	if uds == "" {
		t.Skip("MICROVM_VSOCK_UDS not set; skipping guest integration test")
	}
	return guestclient.New(uds)
}

func TestGuestHealth(t *testing.T) {
	c := testClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("guest not ready: %v", err)
	}

	health, err := c.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !health.OK {
		t.Error("guest reports not ok")
	}
	t.Logf("agent version %s, uptime %dms", health.Version, health.UptimeMS)
}

func TestGuestExecCollect(t *testing.T) {
	c := testClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("guest not ready: %v", err)
	}

	res, err := c.ExecCollect(ctx, protocol.ExecRequest{
		ID:   "it-collect",
		Cmd:  "sh",
		Args: []string{"-c", "echo out; echo err >&2; exit 5"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := string(res.Stdout); got != "out\n" {
		t.Errorf("stdout = %q, want %q", got, "out\n")
	}
	if got := string(res.Stderr); got != "err\n" {
		t.Errorf("stderr = %q, want %q", got, "err\n")
	}
	// A wrong exit code here is what the PID 1 reaper race used to produce, so
	// this assertion is load-bearing rather than decorative.
	if res.ExitCode != 5 {
		t.Errorf("exit code = %d, want 5", res.ExitCode)
	}
}

// The guest must be a real VM, not a container sharing the host kernel.
func TestGuestIsIsolatedVM(t *testing.T) {
	c := testClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("guest not ready: %v", err)
	}

	res, err := c.ExecCollect(ctx, protocol.ExecRequest{
		ID:   "it-isolation",
		Cmd:  "sh",
		Args: []string{"-c", "cat /proc/sys/kernel/random/boot_id; uname -r; nproc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("probe failed with %d: %s", res.ExitCode, res.Stderr)
	}
	t.Logf("guest boot_id/kernel/nproc:\n%s", res.Stdout)

	// The guest runs its own kernel, so its boot id cannot match the host's.
	hostBootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Skip("cannot read host boot_id")
	}
	if strings.Contains(string(res.Stdout), strings.TrimSpace(string(hostBootID))) {
		t.Error("guest boot_id matches the host's: this is not an isolated VM")
	}
}

func TestGuestStreamsIncrementally(t *testing.T) {
	c := testClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("guest not ready: %v", err)
	}

	start := time.Now()
	var firstAt time.Duration

	err := c.Exec(ctx, protocol.ExecRequest{
		ID:   "it-stream",
		Cmd:  "sh",
		Args: []string{"-c", "echo first; sleep 1; echo second"},
	}, func(f protocol.Frame) error {
		if f.Type == protocol.FrameStdout && strings.Contains(string(f.Data), "first") && firstAt == 0 {
			firstAt = time.Since(start)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if firstAt == 0 {
		t.Fatal("never saw the first chunk")
	}
	// Buffering would hold "first" until the process exited a second later.
	if firstAt > 700*time.Millisecond {
		t.Errorf("first chunk took %v: output is being buffered, not streamed", firstAt)
	}
	t.Logf("first chunk after %v", firstAt)
}

// Cancelling the context must kill the process in the guest, not merely
// abandon the stream.
func TestGuestAbortKillsProcess(t *testing.T) {
	c := testClient(t)
	setup, cancelSetup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelSetup()

	if err := c.WaitReady(setup); err != nil {
		t.Fatalf("guest not ready: %v", err)
	}

	const marker = "/tmp/abort-probe-alive"
	_ = mustExec(t, c, setup, "rm -f "+marker)

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})

	go func() {
		_ = c.Exec(ctx, protocol.ExecRequest{
			ID:  "it-abort",
			Cmd: "sh",
			// Touches the marker continuously; if it survives the abort the
			// file keeps coming back after we delete it.
			Args: []string{"-c", "echo up; while true; do touch " + marker + "; sleep 0.1; done"},
		}, func(f protocol.Frame) error {
			if f.Type == protocol.FrameStdout && strings.Contains(string(f.Data), "up") {
				close(started)
			}
			return nil
		})
	}()

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("process never started")
	}

	cancel()
	time.Sleep(500 * time.Millisecond)

	// Delete the marker, then see whether anything recreates it.
	_ = mustExec(t, c, setup, "rm -f "+marker)
	time.Sleep(700 * time.Millisecond)

	res := mustExec(t, c, setup, "test -e "+marker+" && echo ALIVE || echo GONE")
	if strings.Contains(string(res.Stdout), "ALIVE") {
		t.Error("process survived the abort: it is still running inside the guest")
	}
}

func mustExec(t *testing.T, c *guestclient.Client, ctx context.Context, script string) guestclient.Result {
	t.Helper()
	res, err := c.ExecCollect(ctx, protocol.ExecRequest{
		ID:   "probe-" + strings.NewReplacer(" ", "-", "/", "_").Replace(script)[:min(20, len(script))],
		Cmd:  "sh",
		Args: []string{"-c", script},
	})
	if err != nil {
		t.Fatalf("probe %q failed: %v", script, err)
	}
	return res
}
