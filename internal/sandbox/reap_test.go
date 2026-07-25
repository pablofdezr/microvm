package sandbox

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
)

// newReapManager returns a manager that forgets stopped sandboxes after
// retention, over a log store that keeps output for logRetention -- the two
// windows the floor is about.
func newReapManager(t *testing.T, retention, logRetention time.Duration) *Manager {
	t.Helper()
	logs := logstore.New(logstore.Config{Retention: logRetention})
	m := NewManager(runtimetest.New(), logs, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithRetention(retention))
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

// newReapSandbox creates one sandbox and stops it, since a stopped one is the
// only kind a sweep looks at.
func newReapSandbox(t *testing.T, m *Manager, id string) *Sandbox {
	t.Helper()
	sb, err := m.Create(context.Background(), Spec{Spec: runtime.Spec{ID: id, Image: "python"}})
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

// The leak this exists to close: nothing used to leave the map, so a daemon that
// never restarts held every sandbox it ever ran and served them all on every page.
func TestSweepForgetsAStoppedSandbox(t *testing.T) {
	m := newReapManager(t, 50*time.Millisecond, 0)

	sb := newReapSandbox(t, m, "sb_gone")
	if err := sb.Stop(context.Background(), ReasonStopped); err != nil {
		t.Fatal(err)
	}

	time.Sleep(80 * time.Millisecond)

	if dropped := m.Sweep(); dropped != 1 {
		t.Errorf("dropped %d sandboxes, want 1", dropped)
	}
	if _, ok := m.Get("sb_gone"); ok {
		t.Error("a stopped sandbox outlived its retention: this is the leak")
	}
	if got := m.List(); len(got) != 0 {
		t.Errorf("List returned %d sandboxes, want 0: a forgotten sandbox is still paged", len(got))
	}
}

// Inside the window the record is the point: it is where the final metering is
// served from, sampled just before the kill and unrecoverable after it.
func TestSweepKeepsAStoppedSandboxInsideItsWindow(t *testing.T) {
	m := newReapManager(t, time.Hour, 0)

	sb := newReapSandbox(t, m, "sb_recent")
	if err := sb.Stop(context.Background(), ReasonStopped); err != nil {
		t.Fatal(err)
	}

	if dropped := m.Sweep(); dropped != 0 {
		t.Fatalf("dropped %d sandboxes inside their window, want 0", dropped)
	}
	if _, ok := m.Get("sb_recent"); !ok {
		t.Fatal("a sandbox stopped a moment ago was already forgotten")
	}

	info := sb.Info()
	if info.State != StateStopped {
		t.Errorf("state = %s, want %s", info.State, StateStopped)
	}
	// The runtime's numbers, still being reported after the VM is gone. A zero here
	// would tell whoever bills for this that the run was free.
	if info.Stats.ActiveCPU == 0 || info.Stats.MemoryPeak == 0 {
		t.Errorf("final stats after the kill: active_cpu=%s mem_peak=%d, want the sampled values",
			info.Stats.ActiveCPU, info.Stats.MemoryPeak)
	}
}

// Age is not what decides. A running sandbox is a live VM and this map is the
// only handle on it, so sweeping one would leave nothing able to stop it.
func TestSweepNeverForgetsARunningSandbox(t *testing.T) {
	m := newReapManager(t, 50*time.Millisecond, 0)

	newReapSandbox(t, m, "sb_live")
	time.Sleep(80 * time.Millisecond)

	if dropped := m.Sweep(); dropped != 0 {
		t.Errorf("dropped %d running sandboxes, want 0", dropped)
	}
	if _, ok := m.Get("sb_live"); !ok {
		t.Error("swept a running sandbox: nothing can stop it now")
	}
}

// No window means nothing is forgotten, which is what every existing deployment
// has: the reaper must not appear on upgrade.
func TestNothingIsForgottenWithoutARetentionWindow(t *testing.T) {
	logs := logstore.New(logstore.Config{Retention: time.Hour})
	m := NewManager(runtimetest.New(), logs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = m.Close(context.Background()) })

	if window := m.Retention(); window != 0 {
		t.Errorf("retention = %s with no option set, want 0: the log window switched a reaper on", window)
	}

	sb := newReapSandbox(t, m, "sb_kept")
	if err := sb.Stop(context.Background(), ReasonStopped); err != nil {
		t.Fatal(err)
	}
	if dropped := m.Sweep(); dropped != 0 {
		t.Errorf("dropped %d sandboxes with no window configured, want 0", dropped)
	}
	if _, ok := m.Get("sb_kept"); !ok {
		t.Error("a sandbox was forgotten by a node that never asked for a reaper")
	}
}

// The two windows are one decision, and this is the half an operator can get
// backwards. Configured separately, -sandbox-retention 5m against -log-retention
// 1h would delete the metering for output that is still on the host.
func TestRetentionIsNeverShorterThanTheLogWindow(t *testing.T) {
	tests := []struct {
		name         string
		want         time.Duration
		logRetention time.Duration
		effective    time.Duration
	}{
		{
			name:         "raised to the log window",
			want:         5 * time.Minute,
			logRetention: time.Hour,
			effective:    time.Hour,
		},
		{
			name:         "a longer window is left alone",
			want:         24 * time.Hour,
			logRetention: time.Hour,
			effective:    24 * time.Hour,
		},
		{
			name:         "equal windows are the floor exactly",
			want:         time.Hour,
			logRetention: time.Hour,
			effective:    time.Hour,
		},
		{
			// Logs kept forever are not a floor of forever: the sandbox window is
			// what was asked for, and a reaper that never ran would be the leak.
			name:         "logs kept forever do not pin the sandbox window",
			want:         time.Hour,
			logRetention: 0,
			effective:    time.Hour,
		},
		{
			name:         "no window stays no window",
			want:         0,
			logRetention: time.Hour,
			effective:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := retentionFloor(tc.want, tc.logRetention); got != tc.effective {
				t.Errorf("retentionFloor(%s, %s) = %s, want %s", tc.want, tc.logRetention, got, tc.effective)
			}
			// And through the manager, which is where the rule has to hold: the
			// floor is applied after the options, so no caller can lower it.
			m := newReapManager(t, tc.want, tc.logRetention)
			if got := m.Retention(); got != tc.effective {
				t.Errorf("manager retention = %s, want %s", got, tc.effective)
			}
		})
	}
}

// The floor's other half, stated as behaviour: a sandbox stopped inside the log
// window is still there, so the exec output the host is still holding is still
// reachable -- every execution route resolves its sandbox first.
func TestASandboxOutlivesItsExecRecords(t *testing.T) {
	m := newReapManager(t, time.Millisecond, time.Hour)

	sb := newReapSandbox(t, m, "sb_logs")
	if err := sb.Stop(context.Background(), ReasonStopped); err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)

	if dropped := m.Sweep(); dropped != 0 {
		t.Errorf("dropped %d sandboxes past a 1ms window while their logs are kept for an hour, want 0", dropped)
	}
	if _, ok := m.Get("sb_logs"); !ok {
		t.Error("the sandbox went before its exec output: the output is now unreachable")
	}
}

// A forgotten sandbox takes its exec records with it.
//
// The two windows mean opposite things at zero: no sandbox window forgets nothing,
// and no *log* window keeps output forever. So a node with -log-retention 0 and a
// sandbox window set used to drop the sandbox and keep every one of its records --
// reachable through no route, since an exec is only reached through its sandbox,
// and dropped by no sweep, since the store had no window. That is the leak the
// reaper was added to close, plus a 404 on output the host was still holding.
func TestForgettingASandboxDropsItsExecRecords(t *testing.T) {
	// Logs kept forever, which is what makes the sandbox window the only thing that
	// can ever release them.
	m := newReapManager(t, 50*time.Millisecond, 0)

	sb := newReapSandbox(t, m, "sb_records")
	if err := sb.Exec(context.Background(), protocol.ExecRequest{ID: "exe_kept", Cmd: "python3"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := sb.Logs("exe_kept"); !ok {
		t.Fatal("the exec left no record; the rest of this test asserts nothing")
	}
	if err := sb.Stop(context.Background(), ReasonStopped); err != nil {
		t.Fatal(err)
	}

	time.Sleep(80 * time.Millisecond)
	if dropped := m.Sweep(); dropped != 1 {
		t.Fatalf("dropped %d sandboxes, want 1", dropped)
	}

	if _, ok := m.logs.Get("exe_kept"); ok {
		t.Error("the exec record outlived the sandbox that was the only way to reach it: this is the leak")
	}
	if recs := m.logs.ListSandbox("sb_records"); len(recs) != 0 {
		t.Errorf("the store still lists %d records for a forgotten sandbox", len(recs))
	}
}
