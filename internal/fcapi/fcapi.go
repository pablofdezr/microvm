// Package fcapi is a minimal client for the Firecracker HTTP API over its Unix
// domain socket.
//
// The daemon boots VMs in config-file mode with the API off (`--no-api`), which
// is all a plain cold boot needs. Pause, snapshot and resume, though, exist only
// on the API -- so a snapshot-backed warm pool boots its template with an API
// socket and drives it through this client. Keeping the client in its own,
// platform-neutral package means its request-building can be unit-tested over a
// real Unix socket without Firecracker or KVM.
package fcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to one Firecracker VMM over its API socket.
type Client struct {
	http *http.Client
}

// Timeouts are per call rather than one cap on the client, because the calls are
// not alike.
//
// Everything here is a control round trip that answers in milliseconds -- except
// the snapshot write, whose duration is the guest's entire memory divided by the
// disk's write throughput, and which the VMM performs from inside a cgroup capped
// at roughly the guest's memory size, so the page cache it dirties is reclaimed
// as it goes and the write runs well below the disk's raw speed. Measured on a
// Pi 5 writing to an SD card with other tenants doing I/O, a 512 MiB guest took
// over 30 seconds. A single 30s client timeout -- which is what this was --
// therefore cut the capture short and reported it as a snapshot failure, after
// which the warm pool marked the shape cold-only for the daemon's whole life. A
// bigger guest would never have captured at all.
const (
	controlTimeout = 30 * time.Second

	// snapshotWriteTimeout covers an 8 GiB guest at ~30 MB/s, which is the floor
	// this has been seen to run at.
	snapshotWriteTimeout = 5 * time.Minute
)

// New returns a client bound to the API socket at path.
func New(socketPath string) *Client {
	return &Client{
		http: &http.Client{
			// No client-wide Timeout: it is a hard cap that cannot tell a
			// millisecond control call from a memory dump. Each call below sets
			// its own, and a caller's context still bounds them all.
			Transport: &http.Transport{
				// Every request dials the same VMM socket; the host in the URL is
				// ignored, so it is a fixed placeholder.
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// Pause and Resume flip the VM's run state. A snapshot must be taken while
// paused; a restored VM is resumed to start running.
func (c *Client) Pause(ctx context.Context) error  { return c.patchVMState(ctx, "Paused") }
func (c *Client) Resume(ctx context.Context) error { return c.patchVMState(ctx, "Resumed") }

func (c *Client) patchVMState(ctx context.Context, state string) error {
	return c.do(ctx, controlTimeout, http.MethodPatch, "/vm", map[string]string{"state": state})
}

// CreateSnapshot writes a full snapshot: statePath gets the VM state and memPath
// the guest memory. The VM must be Paused first.
func (c *Client) CreateSnapshot(ctx context.Context, statePath, memPath string) error {
	return c.do(ctx, snapshotWriteTimeout, http.MethodPut, "/snapshot/create", map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": statePath,
		"mem_file_path": memPath,
	})
}

// LoadSnapshot restores a VM from a snapshot, resuming it in the same call when
// resume is true. It is issued against a fresh VMM that has never started a
// microVM -- Firecracker refuses a load once one has.
func (c *Client) LoadSnapshot(ctx context.Context, statePath, memPath string, resume bool) error {
	// A load is not a read of the memory file: Firecracker maps it, so this
	// returns in milliseconds however large the guest is (6ms measured for a
	// 512 MiB guest). It is a control call.
	return c.do(ctx, controlTimeout, http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": statePath,
		"mem_backend":   map[string]string{"backend_type": "File", "backend_path": memPath},
		"resume_vm":     resume,
	})
}

func (c *Client) do(ctx context.Context, timeout time.Duration, method, path string, body any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fcapi %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("fcapi %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}
