//go:build linux

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

func newTestManager(t *testing.T) *sandbox.Manager {
	t.Helper()
	rt := newTestRuntime(t)
	logs := logstore.New(logstore.Config{})
	mgr := sandbox.NewManager(rt, logs, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = mgr.Close(ctx)
	})
	return mgr
}

func baseSpec(t *testing.T, id string) sandbox.Spec {
	t.Helper()
	return sandbox.Spec{
		Spec: runtime.Spec{
			ID: id, Image: imageName(t), VCPUs: 1, MemMiB: 256,
			Limits: runtime.Limits{CPUCores: 0.5},
		},
	}
}

// A sandbox must die when its TTL elapses, whatever it is doing. This is the
// "max 5 seconds" case: untrusted code does not get to decide how long it runs.
func TestSandboxTTLShutsDownTheVM(t *testing.T) {
	mgr := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	spec := baseSpec(t, "lc-ttl")
	spec.TTL = 5 * time.Second
	spec.IdleTimeout = -1 // isolate the TTL: no idle reclaim to confuse the result

	sb, err := mgr.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Start something that would otherwise run far past the TTL.
	go func() {
		_ = sb.Exec(context.Background(), protocol.ExecRequest{
			ID: "forever", Cmd: "sh",
			Args:    []string{"-c", "echo starting; sleep 300"},
			Timeout: 300 * time.Second,
		}, nil)
	}()

	start := time.Now()
	deadline := time.After(30 * time.Second)
	for sb.State() == sandbox.StateRunning {
		select {
		case <-deadline:
			t.Fatal("sandbox outlived its TTL by 25s: the deadline is not being enforced")
		case <-time.After(200 * time.Millisecond):
		}
	}
	elapsed := time.Since(start)

	if sb.Reason() != sandbox.ReasonExpired {
		t.Errorf("reason = %q, want %q", sb.Reason(), sandbox.ReasonExpired)
	}
	// The supervisor checks on a timer, so allow slack -- but it must be near
	// 5s, not 30.
	if elapsed > 12*time.Second {
		t.Errorf("took %v to enforce a 5s TTL", elapsed)
	}
	t.Logf("5s TTL enforced after %v, reason=%s", elapsed.Round(time.Millisecond), sb.Reason())

	// A stopped sandbox must refuse new work rather than fail obscurely.
	err = sb.Exec(ctx, protocol.ExecRequest{ID: "after", Cmd: "echo"}, nil)
	if err == nil {
		t.Error("exec on an expired sandbox succeeded")
	}
}

// The output of a killed sandbox must still be readable. This is the case the
// host-side log store exists for: the VM is gone, and its logs are exactly what
// you need to understand why.
func TestLogsSurviveTTLKill(t *testing.T) {
	mgr := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	spec := baseSpec(t, "lc-logs")
	spec.TTL = 6 * time.Second
	spec.IdleTimeout = -1

	sb, err := mgr.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Nothing is streaming this: the caller fires it and walks away.
	go func() {
		_ = sb.Exec(context.Background(), protocol.ExecRequest{
			ID: "chatty", Cmd: "sh",
			Args:    []string{"-c", "echo LINEA-UNO; echo LINEA-DOS; sleep 300"},
			Timeout: 300 * time.Second,
		}, nil)
	}()

	// Let it produce output, then let the TTL take the VM.
	time.Sleep(2 * time.Second)
	for sb.State() == sandbox.StateRunning {
		time.Sleep(200 * time.Millisecond)
	}

	rec, ok := sb.Logs("chatty")
	if !ok {
		t.Fatal("no record for the exec: its output died with the VM")
	}

	out := string(rec.Stdout)
	if !strings.Contains(out, "LINEA-UNO") || !strings.Contains(out, "LINEA-DOS") {
		t.Errorf("stdout = %q, want the output produced before the kill", out)
	}
	// The status must say the VM was taken away, not that the code failed.
	if rec.Status != logstore.StatusVanished {
		t.Errorf("status = %q, want %q", rec.Status, logstore.StatusVanished)
	}
	t.Logf("after the VM was killed, its logs still read: %q (status=%s)",
		strings.TrimSpace(out), rec.Status)
}

// Idle reclaim: a sandbox nobody is using should not be held forever.
func TestSandboxIdleTimeoutReclaims(t *testing.T) {
	mgr := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	spec := baseSpec(t, "lc-idle")
	spec.TTL = 10 * time.Minute // far away: the idle path is what is under test
	spec.IdleTimeout = 6 * time.Second

	sb, err := mgr.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Do a little work, then go quiet.
	if err := sb.Exec(ctx, protocol.ExecRequest{
		ID: "quick", Cmd: "echo", Args: []string{"working"},
	}, nil); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	deadline := time.After(45 * time.Second)
	for sb.State() == sandbox.StateRunning {
		select {
		case <-deadline:
			t.Fatal("idle sandbox was never reclaimed")
		case <-time.After(200 * time.Millisecond):
		}
	}

	if sb.Reason() != sandbox.ReasonIdle {
		t.Errorf("reason = %q, want %q", sb.Reason(), sandbox.ReasonIdle)
	}
	t.Logf("idle sandbox reclaimed after %v", time.Since(start).Round(time.Second))
}

// A busy sandbox must never be reclaimed for idleness: killing a VM mid-exec
// because the last one finished a while ago would be exactly backwards.
func TestBusySandboxIsNotReclaimedAsIdle(t *testing.T) {
	mgr := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	spec := baseSpec(t, "lc-busy")
	spec.TTL = 10 * time.Minute
	spec.IdleTimeout = 5 * time.Second

	sb, err := mgr.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// One exec that runs well past the idle timeout.
	done := make(chan error, 1)
	go func() {
		done <- sb.Exec(context.Background(), protocol.ExecRequest{
			ID: "long", Cmd: "sh", Args: []string{"-c", "sleep 12; echo survived"},
			Timeout: 60 * time.Second,
		}, nil)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exec failed: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("exec never returned")
	}

	if sb.State() != sandbox.StateRunning {
		t.Errorf("sandbox was reclaimed as idle (%s) while an exec was running", sb.Reason())
	}

	rec, _ := sb.Logs("long")
	if !strings.Contains(string(rec.Stdout), "survived") {
		t.Errorf("exec did not complete: stdout = %q", rec.Stdout)
	}
}

// Stopping on demand: the caller asked, so the reason must say so.
func TestExplicitStopReportsItsReason(t *testing.T) {
	mgr := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb, err := mgr.Create(ctx, baseSpec(t, "lc-stop"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := sb.Stop(ctx, sandbox.ReasonStopped); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if sb.State() != sandbox.StateStopped {
		t.Errorf("state = %s, want %s", sb.State(), sandbox.StateStopped)
	}
	if sb.Reason() != sandbox.ReasonStopped {
		t.Errorf("reason = %q, want %q", sb.Reason(), sandbox.ReasonStopped)
	}
}

// Meters must remain readable after the VM is gone: "what did this run cost?"
// is a question asked after the fact, and the cgroup no longer exists then.
func TestFinalStatsSurviveTheVM(t *testing.T) {
	mgr := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sb, err := mgr.Create(ctx, baseSpec(t, "lc-stats"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := sb.Exec(ctx, protocol.ExecRequest{
		ID: "burn", Cmd: "sh",
		Args:    []string{"-c", "end=$(( $(date +%s) + 2 )); while [ $(date +%s) -lt $end ]; do :; done"},
		Timeout: 30 * time.Second,
	}, nil); err != nil {
		t.Fatal(err)
	}

	live := sb.Info()
	if live.Stats.ActiveCPU == 0 {
		t.Fatal("no CPU recorded while running")
	}

	if err := sb.Stop(ctx, sandbox.ReasonStopped); err != nil {
		t.Fatal(err)
	}

	after := sb.Info()
	if after.Stats.ActiveCPU == 0 {
		t.Error("active CPU reads as zero once stopped: the run looks free, and the bill would be wrong")
	}
	t.Logf("after the VM was destroyed, its final cost still reads: cpu=%v idle=%v peak=%dMB",
		after.Stats.ActiveCPU.Round(time.Millisecond),
		after.Stats.Idle.Round(time.Millisecond),
		after.Stats.MemoryPeak/(1024*1024))
}
