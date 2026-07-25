package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
)

// coldRuntime is a runtime with no snapshot support, for asserting that
// suspend/resume is off where the backend cannot freeze a VM. It deliberately
// does not embed the fake, so the Snapshotter methods are not promoted onto it.
type coldRuntime struct{ inner *runtimetest.Runtime }

func (c coldRuntime) Create(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
	return c.inner.Create(ctx, spec)
}
func (c coldRuntime) Close() error { return c.inner.Close() }

// A suspended sandbox can be resumed to a running one under the same id, and run
// commands again -- the whole point of resume-after-stop.
func TestSuspendThenResume(t *testing.T) {
	m, rt := newAdmissionManager(t)

	sb, err := create(t, m, "sb_1", "acme", 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := sb.Suspend(context.Background()); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if sb.State() != StateSuspended {
		t.Fatalf("state = %q, want suspended", sb.State())
	}
	if got := len(rt.Snapshots()); got != 1 {
		t.Fatalf("captured %d snapshots, want 1", got)
	}

	resumed, err := m.Resume(context.Background(), sb)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.State() != StateRunning {
		t.Fatalf("state = %q, want running", resumed.State())
	}
	if resumed.ID() != sb.ID() {
		t.Errorf("resume changed the id: %s -> %s", sb.ID(), resumed.ID())
	}
	if got := rt.Restored(); got != 1 {
		t.Errorf("restored %d VMs, want 1", got)
	}
	// The resumed sandbox is the one the manager now resolves by id.
	if got, _ := m.Get("sb_1"); got != resumed {
		t.Error("Get did not return the resumed sandbox")
	}
	// And it runs commands: the suspended sandbox refused them.
	if err := resumed.StartExec(protocol.ExecRequest{ID: "exe_1", Cmd: "python3"}); err != nil {
		t.Errorf("resumed sandbox refused an exec: %v", err)
	}
}

// The snapshot is reclaimed once the resume is live: leaving it would leave a full
// copy of a guest's RAM on disk that nothing will ever open again.
func TestResumeDiscardsTheSnapshot(t *testing.T) {
	m, rt := newAdmissionManager(t)
	sb, _ := create(t, m, "sb_1", "acme", 0)

	if err := sb.Suspend(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	if got := rt.Discarded(); got != 1 {
		t.Errorf("discarded %d snapshots after resume, want 1", got)
	}
}

// Suspend keeps the tenant slot, so a resume is never refused for capacity -- the
// one operation whose contract is "you'll get this back".
func TestSuspendKeepsTheSlot(t *testing.T) {
	m, _ := newAdmissionManager(t)

	sb, err := create(t, m, "sb_1", "acme", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Suspend(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The slot is still held by the suspended sandbox: a second create is refused.
	if _, err := create(t, m, "sb_2", "acme", 1); err == nil {
		t.Error("a suspended sandbox gave up its slot; a second create should have been refused")
	}

	// And the resume reuses that held slot rather than reserving a new one.
	if _, err := m.Resume(context.Background(), sb); err != nil {
		t.Fatalf("resume was refused despite holding the slot: %v", err)
	}
}

// Deleting a suspended sandbox reclaims its snapshot.
func TestStoppingASuspendedSandboxDiscardsTheSnapshot(t *testing.T) {
	m, rt := newAdmissionManager(t)
	sb, _ := create(t, m, "sb_1", "acme", 0)

	if err := sb.Suspend(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sb.Stop(context.Background(), ReasonStopped); err != nil {
		t.Fatal(err)
	}
	if got := rt.Discarded(); got != 1 {
		t.Errorf("discarded %d snapshots on stop, want 1", got)
	}
	if sb.State() != StateStopped {
		t.Errorf("state = %q, want stopped", sb.State())
	}
}

// A sandbox with an execution in flight cannot be suspended: freezing the VM
// would capture the process mid-run.
func TestSuspendRefusesABusySandbox(t *testing.T) {
	m, rt := newAdmissionManager(t)
	rt.OnExec = func(ctx context.Context, req protocol.ExecRequest, onFrame func(protocol.Frame) error) error {
		<-ctx.Done() // run until the sandbox is torn down
		return ctx.Err()
	}
	sb, _ := create(t, m, "sb_1", "acme", 0)

	if err := sb.StartExec(protocol.ExecRequest{ID: "exe_1", Cmd: "forever"}); err != nil {
		t.Fatal(err)
	}

	if err := sb.Suspend(context.Background()); !errors.Is(err, ErrSandboxBusy) {
		t.Errorf("suspend of a busy sandbox: err = %v, want ErrSandboxBusy", err)
	}
}

// Resume only applies to a suspended sandbox: a running one has nothing to resume.
func TestResumeRefusesARunningSandbox(t *testing.T) {
	m, _ := newAdmissionManager(t)
	sb, _ := create(t, m, "sb_1", "acme", 0)

	if _, err := m.Resume(context.Background(), sb); err == nil {
		t.Error("resumed a running sandbox")
	}
}

// Suspend/resume is off where the backend cannot snapshot, and says so rather than
// pretending.
func TestSuspendUnsupportedWithoutSnapshots(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := coldRuntime{inner: runtimetest.New()}
	m := NewManager(rt, logstore.New(logstore.Config{}), log)
	t.Cleanup(func() { _ = m.Close(context.Background()) })

	sb, err := m.Create(context.Background(), Spec{Spec: runtime.Spec{ID: "sb_1", Image: "python"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Suspend(context.Background()); !errors.Is(err, ErrSnapshotUnsupported) {
		t.Errorf("suspend on a cold runtime: err = %v, want ErrSnapshotUnsupported", err)
	}
}

// A named sandbox keeps its name across a suspend/resume, and get-or-create
// resolves to the resumed VM.
func TestNameSurvivesSuspendResume(t *testing.T) {
	m, _ := newAdmissionManager(t)
	sb, err := named(t, m, "sb_1", "acme", "build")
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Suspend(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumed, err := m.Resume(context.Background(), sb)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := m.GetByName("acme", "build")
	if !ok || got != resumed {
		t.Errorf("GetByName after resume = (%v, %v), want the resumed sandbox", got, ok)
	}
}
