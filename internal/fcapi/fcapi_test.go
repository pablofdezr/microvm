package fcapi

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
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
	dir := t.TempDir()
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
	if err := c.LoadSnapshot(ctx, "/snap/state", "/snap/mem", true); err != nil {
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
	if reqs[3].body["state"] != "Resumed" {
		t.Errorf("resume request wrong: %+v", reqs[3])
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

	if err := New(sock).LoadSnapshot(context.Background(), "/s", "/m", true); err == nil {
		t.Fatal("expected an error on a 400 response")
	}
}

func TestDialFailsWhenNoSocket(t *testing.T) {
	if err := New("/nonexistent/fc.sock").Pause(context.Background()); err == nil {
		t.Fatal("expected an error dialing a missing socket")
	}
}
