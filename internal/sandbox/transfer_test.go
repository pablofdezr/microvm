package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/runtime"
)

// A transfer is work, and the idle clock has to know it.
//
// Only execs used to count, so a caller staging a project -- minutes of uploads
// with nothing run in between, which is exactly what the batch route is for -- was
// idle by the manager's reckoning for all of it, and the reclaim killed the VM
// mid-batch. The caller then got a 409 having had a prefix of their files written.

func newTransferSandbox(t *testing.T, id string) *Sandbox {
	t.Helper()
	sb, err := newTTLManager(t).Create(context.Background(),
		Spec{Spec: runtime.Spec{ID: id, Image: "python"}})
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

// backdate makes the sandbox look untouched for an hour, which is the state the
// reclaim acts on.
func backdate(sb *Sandbox) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.lastActive = time.Now().Add(-time.Hour)
}

func TestATransferResetsTheIdleClock(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		transfer func(*Sandbox) error
	}{
		{"an upload", func(sb *Sandbox) error {
			return sb.WriteFile(ctx, "/app/main.py", strings.NewReader("x = 1\n"), "0644")
		}},
		{"a directory", func(sb *Sandbox) error {
			return sb.Mkdir(ctx, "/app/out")
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sb := newTransferSandbox(t, "sb_"+strings.ReplaceAll(tc.name, " ", "_"))

			backdate(sb)
			if !sb.idleFor(time.Minute) {
				t.Fatal("a backdated sandbox does not look idle; the rest of this test asserts nothing")
			}

			if err := tc.transfer(sb); err != nil {
				t.Fatal(err)
			}
			if sb.idleFor(time.Minute) {
				t.Error("the sandbox still looks idle for an hour after a transfer, so the reclaim would kill it mid-batch")
			}
		})
	}
}

// A download is the one transfer whose end is not the end of the call: the caller
// holds the reader, and a multi-gigabyte artifact takes longer to stream than the
// idle timeout allows. So the sandbox is busy until Close, not until ReadFile
// returns -- otherwise the VM is reclaimed out from under the response body.
func TestAnOpenDownloadIsNotIdle(t *testing.T) {
	ctx := context.Background()
	sb := newTransferSandbox(t, "sb_download")

	if err := sb.WriteFile(ctx, "/app/artifact.bin", strings.NewReader("bytes"), "0644"); err != nil {
		t.Fatal(err)
	}

	rc, err := sb.ReadFile(ctx, "/app/artifact.bin")
	if err != nil {
		t.Fatal(err)
	}

	backdate(sb)
	if sb.idleFor(time.Minute) {
		t.Error("a sandbox with a download still open looks idle: the reclaim would pull the VM out mid-stream")
	}

	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if !sb.idleFor(0) {
		t.Error("the sandbox is still busy after its reader was closed, so nothing will ever reclaim it")
	}

	// Closing twice must not decrement past zero: a count below it is a sandbox no
	// reclaim can ever touch again.
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if !sb.idleFor(0) {
		t.Error("a second Close left the sandbox counted as busy")
	}
}

// A transfer aimed at a sandbox that is gone is refused with the reason, not with
// whatever the dead VM's transport says -- and it must not leave the sandbox
// counted as busy on the way out.
func TestTransfersOnAStoppedSandboxAreRefused(t *testing.T) {
	ctx := context.Background()
	sb := newTransferSandbox(t, "sb_stopped_transfer")
	if err := sb.Stop(ctx, ReasonStopped); err != nil {
		t.Fatal(err)
	}

	if err := sb.WriteFile(ctx, "/app/x", strings.NewReader("x"), "0644"); err == nil {
		t.Error("an upload into a stopped sandbox was accepted")
	}
	if _, err := sb.ReadFile(ctx, "/app/x"); err == nil {
		t.Error("a download from a stopped sandbox was accepted")
	}
	if err := sb.Mkdir(ctx, "/app/out"); err == nil {
		t.Error("a directory in a stopped sandbox was accepted")
	}

	sb.mu.Lock()
	running := sb.running
	sb.mu.Unlock()
	if running != 0 {
		t.Errorf("%d transfers are counted as in flight after being refused, want 0", running)
	}
}
