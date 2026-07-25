package fcapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// capturedReq records what the stub Firecracker received.
type capturedReq struct {
	method string
	path   string
	body   map[string]any
}

// stubVMM serves the Firecracker API over a Unix socket, recording requests and
// replying 204 (as Firecracker does on success).
func stubVMM(t *testing.T) (socket string, seen func() []capturedReq) {
	t.Helper()
	// A short path, not t.TempDir(): the sun.sun_path limit is ~104 bytes and a
	// long test name in t.TempDir() overruns it as "bind: invalid argument".
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "fc.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var reqs []capturedReq
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		mu.Lock()
		reqs = append(reqs, capturedReq{method: r.Method, path: r.URL.Path, body: parsed})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	return sock, func() []capturedReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedReq(nil), reqs...)
	}
}

func TestPauseResumeCreateLoad(t *testing.T) {
	sock, seen := stubVMM(t)
	c := New(sock)
	ctx := context.Background()

	if err := c.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := c.CreateSnapshot(ctx, "/snap/state", "/snap/mem"); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if err := c.LoadSnapshot(ctx, "/snap/state", "/snap/mem", true, nil); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if err := c.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	reqs := seen()
	if len(reqs) != 4 {
		t.Fatalf("got %d requests, want 4", len(reqs))
	}

	if reqs[0].method != http.MethodPatch || reqs[0].path != "/vm" || reqs[0].body["state"] != "Paused" {
		t.Errorf("pause request wrong: %+v", reqs[0])
	}
	if reqs[1].method != http.MethodPut || reqs[1].path != "/snapshot/create" ||
		reqs[1].body["snapshot_type"] != "Full" || reqs[1].body["mem_file_path"] != "/snap/mem" {
		t.Errorf("create request wrong: %+v", reqs[1])
	}
	if reqs[2].path != "/snapshot/load" || reqs[2].body["resume_vm"] != true {
		t.Errorf("load request wrong: %+v", reqs[2])
	}
	mb, _ := reqs[2].body["mem_backend"].(map[string]any)
	if mb["backend_type"] != "File" || mb["backend_path"] != "/snap/mem" {
		t.Errorf("load mem_backend wrong: %+v", reqs[2].body["mem_backend"])
	}
	// A load with no overrides must not send the field at all: Firecracker rejects
	// an empty network_overrides, and the warm pool's every restore is unnetworked.
	if _, present := reqs[2].body["network_overrides"]; present {
		t.Errorf("load sent network_overrides for an unnetworked restore: %+v", reqs[2].body)
	}
	if reqs[3].body["state"] != "Resumed" {
		t.Errorf("resume request wrong: %+v", reqs[3])
	}
}

// A networked restore remaps the snapshot's interface onto a fresh host TAP, so
// the restored VM does not come back on the template's -- which is gone, or is
// somebody else's slot by now.
func TestLoadSnapshotSendsNetworkOverrides(t *testing.T) {
	sock, seen := stubVMM(t)
	err := New(sock).LoadSnapshot(context.Background(), "/s", "/m", true,
		[]NetOverride{{IfaceID: "eth0", HostDevName: "fc-tap42"}})
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}

	reqs := seen()
	ovr, ok := reqs[0].body["network_overrides"].([]any)
	if !ok || len(ovr) != 1 {
		t.Fatalf("network_overrides missing or wrong shape: %+v", reqs[0].body)
	}
	first, _ := ovr[0].(map[string]any)
	if first["iface_id"] != "eth0" || first["host_dev_name"] != "fc-tap42" {
		t.Errorf("override wrong: %+v", first)
	}
}

func TestErrorStatusIsReported(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "fc.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"fault_message":"cannot load: microVM already started"}`))
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	if err := New(sock).LoadSnapshot(context.Background(), "/s", "/m", true, nil); err == nil {
		t.Fatal("expected an error on a 400 response")
	}
}

func TestDialFailsWhenNoSocket(t *testing.T) {
	if err := New("/nonexistent/fc.sock").Pause(context.Background()); err == nil {
		t.Fatal("expected an error dialing a missing socket")
	}
}

// A snapshot write outlives the control timeout, because it takes as long as the
// guest's memory takes to reach the disk. A single client-wide timeout could not
// express that, and the 30s one this replaced turned a slow capture into a
// permanent "snapshots do not work on this host".
func TestSnapshotWriteOutlivesControl(t *testing.T) {
	if snapshotWriteTimeout <= controlTimeout {
		t.Fatalf("snapshotWriteTimeout %v must exceed controlTimeout %v", snapshotWriteTimeout, controlTimeout)
	}

	// A short socket path: the sun.sun_path limit is ~104 bytes and a test name
	// is part of t.TempDir().
	dir, err := os.MkdirTemp("", "fcapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "fc.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	// A VMM that answers only after longer than a control call is allowed to run.
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(controlTimeout + time.Second):
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	// The caller's deadline is what fires, not a client-wide cap -- which is also
	// how the test proves the point in 150ms instead of 30s.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := New(sock).CreateSnapshot(ctx, "state", "mem"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
}
