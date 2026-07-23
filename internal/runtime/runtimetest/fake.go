// Package runtimetest provides a runtime that needs no virtual machine.
//
// It exists because the layers above runtime.Runtime -- sandbox lifetimes, the
// log store, the whole HTTP API -- contain most of the logic and none of the
// virtualisation. Before this, testing any of it meant booting Firecracker,
// which meant KVM, which meant that in practice it was tested on one Raspberry
// Pi by hand. Everything here is a real implementation of the port; only the
// thing on the far side of it is pretend.
package runtimetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
)

// Runtime is a runtime.Runtime that creates fake sandboxes.
type Runtime struct {
	// CreateErr, when set, makes every Create fail. This is how a test reaches
	// the node-is-full path without filling a node.
	CreateErr error

	// CreateDelay simulates a slow boot.
	CreateDelay time.Duration

	// OnExec decides what a command does. Nil runs Script, or exits 0 silently.
	OnExec func(ctx context.Context, req protocol.ExecRequest, onFrame func(protocol.Frame) error) error

	// Script maps a command to canned output, for the common case where a test
	// just needs a program that prints something.
	Script map[string]Output

	// Stats is what every instance reports.
	Stats runtime.Stats

	mu        sync.Mutex
	instances map[string]*Instance
	created   int
	// specs records every spec Create was handed, so a test can assert what the
	// manager decided before boot -- the storage kernel-cmdline flags among it.
	specs []runtime.Spec
}

// Output is a scripted command's result.
type Output struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// Delay is how long the command "runs" before its output appears. Use it to
	// test anything that has to happen while a command is still going.
	Delay time.Duration
	// Block makes the command run until its context is cancelled, for testing
	// cancellation and timeouts.
	Block bool
}

// New returns a fake runtime.
func New() *Runtime {
	return &Runtime{
		instances: make(map[string]*Instance),
		Script:    make(map[string]Output),
		Stats: runtime.Stats{
			ActiveCPU:     100 * time.Millisecond,
			Wall:          time.Second,
			Idle:          900 * time.Millisecond,
			MemoryCurrent: 32 << 20,
			MemoryPeak:    48 << 20,
		},
	}
}

func (r *Runtime) Create(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
	if r.CreateDelay > 0 {
		select {
		case <-time.After(r.CreateDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.CreateErr != nil {
		return nil, r.CreateErr
	}

	inst := &Instance{
		id:       spec.ID,
		rt:       r,
		files:    make(map[string][]byte),
		stopped:  make(chan struct{}),
		listener: NewListener(),
	}

	r.mu.Lock()
	r.instances[spec.ID] = inst
	r.created++
	r.specs = append(r.specs, spec)
	r.mu.Unlock()

	return inst, nil
}

func (r *Runtime) Close() error { return nil }

// Created reports how many sandboxes were asked for.
func (r *Runtime) Created() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.created
}

// LastSpec returns the most recent spec Create was called with. It is how a
// test inspects the boot-time decisions the manager made -- for storage, the
// mount path and read-only flag the guest is told about on its command line.
func (r *Runtime) LastSpec() (runtime.Spec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.specs) == 0 {
		return runtime.Spec{}, false
	}
	return r.specs[len(r.specs)-1], true
}

// Instance is a fake running sandbox.
type Instance struct {
	id string
	rt *Runtime

	// listener is this sandbox's inbound socket, and it is per-instance for the
	// same reason the real one is: sharing it across sandboxes would silently
	// destroy the isolation that makes the socket an identity.
	listener *Listener

	mu      sync.Mutex
	files   map[string][]byte
	stopped chan struct{}
	isDown  bool

	// signals records what was sent to which exec, so a test can assert that
	// cancelling actually signalled rather than merely returning 200.
	signals []SignalRecord
}

// SignalsSent returns the signals delivered to this sandbox.
func (i *Instance) SignalsSent() []SignalRecord {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]SignalRecord(nil), i.signals...)
}

// SignalRecord is one delivered signal.
type SignalRecord struct {
	ExecID string
	Signal string
}

func (i *Instance) ID() string { return i.id }

func (i *Instance) Client() runtime.GuestClient { return &client{inst: i} }

// HostListener returns this sandbox's inbound socket.
//
// Concrete *Listener rather than net.Listener, so a test can Dial it as the
// guest would. It still satisfies the interface.
func (i *Instance) HostListener() net.Listener { return i.listener }

// Guest returns the same listener as the caller-facing type, for dialling in.
func (i *Instance) Guest() *Listener { return i.listener }

func (i *Instance) Stats() (runtime.Stats, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.isDown {
		// A dead sandbox's cgroup is gone, and reading it fails. Faking that is
		// the point: code that assumes stats are always available is code that
		// breaks the first time a VM dies.
		return runtime.Stats{}, fmt.Errorf("sandbox %s is gone", i.id)
	}
	return i.rt.Stats, nil
}

func (i *Instance) Stop(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.isDown {
		return nil // idempotent, like the real one
	}
	i.isDown = true
	close(i.stopped)
	// The real instance closes its listener on stop, which is what ends the
	// Accept loop serving it. A fake that left the listener open would let a
	// goroutine leak pass every test.
	i.listener.Close()
	return nil
}

// client implements runtime.GuestClient against the fake instance.
type client struct{ inst *Instance }

func (c *client) Exec(ctx context.Context, req protocol.ExecRequest, onFrame func(protocol.Frame) error) error {
	c.inst.mu.Lock()
	down := c.inst.isDown
	stopped := c.inst.stopped
	c.inst.mu.Unlock()

	if down {
		return fmt.Errorf("sandbox %s is gone", c.inst.id)
	}

	if c.inst.rt.OnExec != nil {
		return c.inst.rt.OnExec(ctx, req, onFrame)
	}

	out, scripted := c.inst.rt.Script[req.Cmd]
	if !scripted {
		out = Output{}
	}

	if err := emit(onFrame, protocol.Frame{Type: protocol.FrameStarted, PID: 42}); err != nil {
		return err
	}

	if out.Delay > 0 {
		select {
		case <-time.After(out.Delay):
		case <-ctx.Done():
			return ctx.Err()
		case <-stopped:
			return fmt.Errorf("sandbox %s went away", c.inst.id)
		}
	}

	if out.Stdout != "" {
		if err := emit(onFrame, protocol.Frame{Type: protocol.FrameStdout, Data: []byte(out.Stdout)}); err != nil {
			return err
		}
	}
	if out.Stderr != "" {
		if err := emit(onFrame, protocol.Frame{Type: protocol.FrameStderr, Data: []byte(out.Stderr)}); err != nil {
			return err
		}
	}

	if out.Block {
		// Runs until something stops it, which is how a test exercises a
		// timeout, a cancel, or a sandbox pulled out from underneath.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopped:
			return fmt.Errorf("sandbox %s went away", c.inst.id)
		}
	}

	code := out.ExitCode
	return emit(onFrame, protocol.Frame{Type: protocol.FrameExit, ExitCode: &code})
}

func emit(onFrame func(protocol.Frame) error, f protocol.Frame) error {
	if onFrame == nil {
		return nil
	}
	return onFrame(f)
}

func (c *client) Signal(ctx context.Context, execID, signal string) error {
	c.inst.mu.Lock()
	defer c.inst.mu.Unlock()
	if c.inst.isDown {
		return fmt.Errorf("sandbox %s is gone", c.inst.id)
	}
	c.inst.signals = append(c.inst.signals, SignalRecord{ExecID: execID, Signal: signal})
	return nil
}

func (c *client) WriteFile(ctx context.Context, path string, content io.Reader, mode string) error {
	c.inst.mu.Lock()
	down := c.inst.isDown
	c.inst.mu.Unlock()
	if down {
		return fmt.Errorf("sandbox %s is gone", c.inst.id)
	}

	body, err := io.ReadAll(content)
	if err != nil {
		return err
	}

	c.inst.mu.Lock()
	defer c.inst.mu.Unlock()
	c.inst.files[path] = body
	return nil
}

func (c *client) ReadFile(ctx context.Context, path string) (io.ReadCloser, error) {
	c.inst.mu.Lock()
	defer c.inst.mu.Unlock()
	if c.inst.isDown {
		return nil, fmt.Errorf("sandbox %s is gone", c.inst.id)
	}
	body, ok := c.inst.files[path]
	if !ok {
		return nil, fmt.Errorf("no such file: %s", path)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (c *client) Mkdir(ctx context.Context, path string) error {
	c.inst.mu.Lock()
	defer c.inst.mu.Unlock()
	if c.inst.isDown {
		return fmt.Errorf("sandbox %s is gone", c.inst.id)
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	c.inst.files[path] = nil
	return nil
}

// File returns what a test wrote into the sandbox.
func (i *Instance) File(path string) ([]byte, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	body, ok := i.files[path]
	return body, ok
}

// Instance returns a created sandbox by ID.
func (r *Runtime) Instance(id string) (*Instance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[id]
	return inst, ok
}

// Compile-time proof that the fake is a real implementation of the ports. If it
// drifts from them, this file stops building rather than the tests quietly
// testing something the production code no longer does.
var (
	_ runtime.Runtime     = (*Runtime)(nil)
	_ runtime.Instance    = (*Instance)(nil)
	_ runtime.GuestClient = (*client)(nil)
)
