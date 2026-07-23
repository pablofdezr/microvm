// Package cgroup manages the cgroup v2 hierarchy that meters and bounds
// sandboxes.
//
// The layout is two levels deep on purpose:
//
//	/sys/fs/cgroup/microvm.slice/          <- ceiling for ALL sandboxes together
//	    ├── <sandbox-id-1>/                <- this sandbox's limits and meter
//	    └── <sandbox-id-2>/
//
// The per-sandbox limits are what a caller asks for. The slice ceiling is what
// protects everything else on the host: no matter how many sandboxes run, or
// whether a per-sandbox limit was computed wrongly, the whole subtree cannot
// exceed it. A single level would make correctness of the host depend on
// getting every individual limit right, which is a bet that eventually loses.
//
// Metering reads cpu.stat's usage_usec, which counts CPU actually consumed
// rather than wall-clock: a sandbox blocked on I/O bills nothing while it waits.
package cgroup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Root is where the unified cgroup v2 hierarchy is mounted.
const Root = "/sys/fs/cgroup"

// Unlimited is the value cgroup v2 uses for "no limit".
const Unlimited = "max"

// defaultPeriod is the CPU accounting window. cpu.max is expressed as
// "<quota> <period>" in microseconds, so a quota equal to the period means one
// core's worth of time.
const defaultPeriod = 100_000 * time.Microsecond

// Limits bounds a cgroup's resource consumption. A zero field means the limit
// is left alone, which for a fresh cgroup means unlimited.
type Limits struct {
	// CPU is how much CPU time per period the cgroup may consume. One core is
	// CPUPeriod worth of quota; two cores is twice that.
	CPU time.Duration
	// CPUPeriod defaults to 100ms when zero.
	CPUPeriod time.Duration

	// MemoryMax is a hard ceiling. Exceeding it triggers the OOM killer inside
	// the cgroup rather than on the host at large, which is the entire point:
	// a sandbox's memory bomb kills the sandbox, not a neighbour.
	MemoryMax uint64

	// PidsMax caps the number of processes. On the host side this bounds the
	// VMM's threads, not the guest's processes -- a fork bomb inside a guest is
	// already contained by the VM's own fixed vCPU and memory allocation.
	PidsMax int
}

// Stats is a point-in-time reading of a cgroup's meters.
type Stats struct {
	// ActiveCPU is CPU time actually consumed. Time spent blocked on I/O or
	// idle never lands here, which is what makes it the right thing to bill.
	ActiveCPU time.Duration
	UserCPU   time.Duration
	SystemCPU time.Duration

	// MemoryCurrent is resident bytes right now.
	MemoryCurrent uint64
	// MemoryPeak is the high-water mark over the cgroup's life. This is the
	// honest number for billing: current is whatever happened to be resident at
	// the instant it was sampled.
	MemoryPeak uint64
}

// Group is one cgroup directory.
type Group struct {
	path string
}

// Path returns the group's absolute path.
func (g *Group) Path() string { return g.path }

// EnsureSlice creates the parent slice and applies the ceiling for every
// sandbox on the host.
//
// It also enables the controllers that children need. A controller absent from
// a parent's cgroup.subtree_control means the child's cpu.max and memory.max
// files do not exist at all, and writes to limits that do not exist fail
// quietly enough to look like they worked.
func EnsureSlice(name string, ceiling Limits) (*Group, error) {
	if err := requireV2(); err != nil {
		return nil, err
	}

	path := filepath.Join(Root, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create slice %s: %w", path, err)
	}

	g := &Group{path: path}

	// Enable controllers for children. Doing this on our own slice touches
	// nothing else: the host's other cgroups are siblings, and systemd's tree
	// is left exactly as it was.
	if err := g.enableControllers("cpu", "memory", "io", "pids"); err != nil {
		return nil, err
	}

	if err := g.Apply(ceiling); err != nil {
		return nil, fmt.Errorf("apply slice ceiling: %w", err)
	}
	return g, nil
}

// Child returns the group a sandbox's VMM will be placed in. The jailer creates
// the directory itself, so this only names it.
func (g *Group) Child(id string) *Group {
	return &Group{path: filepath.Join(g.path, id)}
}

// Exists reports whether the cgroup directory is present. A sandbox whose VMM
// has exited leaves the directory behind until it is removed.
func (g *Group) Exists() bool {
	_, err := os.Stat(g.path)
	return err == nil
}

// Apply writes the limits. Fields left zero are not touched.
func (g *Group) Apply(l Limits) error {
	if l.CPU > 0 {
		period := l.CPUPeriod
		if period <= 0 {
			period = defaultPeriod
		}
		quota := l.CPU.Microseconds()
		value := fmt.Sprintf("%d %d", quota, period.Microseconds())
		if err := g.write("cpu.max", value); err != nil {
			return err
		}
	}

	if l.MemoryMax > 0 {
		if err := g.write("memory.max", strconv.FormatUint(l.MemoryMax, 10)); err != nil {
			return err
		}
		// Without this, the kernel may swap a sandbox out instead of enforcing
		// the limit, which turns a memory cap into a disk-thrashing cap and
		// drags every other tenant down with it.
		if err := g.write("memory.swap.max", "0"); err != nil {
			// Not fatal: a host with swap accounting disabled has no swap to
			// leak into anyway.
			_ = err
		}
	}

	if l.PidsMax > 0 {
		if err := g.write("pids.max", strconv.Itoa(l.PidsMax)); err != nil {
			return err
		}
	}
	return nil
}

// Stats reads the cgroup's meters.
func (g *Group) Stats() (Stats, error) {
	var s Stats

	cpu, err := g.readKeyed("cpu.stat")
	if err != nil {
		return s, err
	}
	// usage_usec is the number the whole billing model rests on.
	s.ActiveCPU = time.Duration(cpu["usage_usec"]) * time.Microsecond
	s.UserCPU = time.Duration(cpu["user_usec"]) * time.Microsecond
	s.SystemCPU = time.Duration(cpu["system_usec"]) * time.Microsecond

	// Memory files are absent when the memory controller is not enabled on the
	// parent. Report zero rather than failing: CPU metering still works, and a
	// missing meter should not take a sandbox down.
	if v, err := g.readUint("memory.current"); err == nil {
		s.MemoryCurrent = v
	}
	if v, err := g.readUint("memory.peak"); err == nil {
		s.MemoryPeak = v
	}
	return s, nil
}

// AddProcess moves a process into the cgroup. Unused when the jailer places the
// VMM for us, but needed by any runtime backend that does not.
func (g *Group) AddProcess(pid int) error {
	return g.write("cgroup.procs", strconv.Itoa(pid))
}

// Kill terminates every process in the cgroup, including any the caller does
// not have a handle on.
//
// This is the only reliable way to stop a sandbox. Killing the process we
// started is not enough: the jailer clones into a new PID namespace, so the pid
// we hold is not the VMM that ends up running, and anything that forked along
// the way is invisible to us entirely. The cgroup, by contrast, is the exact
// boundary of "everything this sandbox is running" -- the kernel maintains that
// membership itself, so nothing can hide from it.
func (g *Group) Kill() error {
	// cgroup.kill exists since Linux 5.14. On anything older the file is
	// missing and the caller has to fall back to signalling pids.
	return g.write("cgroup.kill", "1")
}

// WaitEmpty blocks until no processes remain in the cgroup.
//
// Kill is asynchronous: it delivers SIGKILL, and the kernel still has to tear
// each process down. Removing the cgroup before that finishes fails with EBUSY,
// so teardown has to wait for the membership to actually drain.
func (g *Group) WaitEmpty(ctx context.Context) error {
	const pollInterval = 5 * time.Millisecond

	for {
		populated, err := g.populated()
		if err != nil {
			// The cgroup is gone, which is a stronger form of empty.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !populated {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("cgroup %s still has processes: %w", g.path, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// Populated reports whether the cgroup or any descendant holds a process.
//
// This is the authoritative answer to "is this sandbox still running". A pid is
// not: the jailer clones, so the process we launched is not the VMM that ends
// up running, and watching it exit says nothing about the VM.
func (g *Group) Populated() (bool, error) {
	return g.populated()
}

func (g *Group) populated() (bool, error) {
	events, err := g.readKeyed("cgroup.events")
	if err != nil {
		return false, err
	}
	return events["populated"] == 1, nil
}

// Delete removes the cgroup directory.
//
// A cgroup with live processes cannot be removed, so this is only valid once
// the VMM has exited. Leaving them behind leaks a directory per sandbox and
// eventually makes the hierarchy unwieldy.
func (g *Group) Delete() error {
	// rmdir, not RemoveAll: the kernel synthesises the files inside and rejects
	// unlinking them, so a recursive delete fails on the first one.
	if err := os.Remove(g.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove cgroup %s: %w", g.path, err)
	}
	return nil
}

func (g *Group) enableControllers(names ...string) error {
	available, err := g.read("cgroup.controllers")
	if err != nil {
		return fmt.Errorf("read available controllers: %w", err)
	}

	var enable []string
	for _, n := range names {
		// Asking for a controller the kernel does not offer makes the whole
		// write fail, taking the available ones down with it.
		if !strings.Contains(available, n) {
			continue
		}
		enable = append(enable, "+"+n)
	}
	if len(enable) == 0 {
		return fmt.Errorf("none of the required controllers are available (have: %s)", available)
	}

	if err := g.write("cgroup.subtree_control", strings.Join(enable, " ")); err != nil {
		return fmt.Errorf("enable controllers %v: %w", enable, err)
	}
	return nil
}

func (g *Group) write(file, value string) error {
	path := filepath.Join(g.path, file)
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s to %s: %w", value, path, err)
	}
	return nil
}

func (g *Group) read(file string) (string, error) {
	b, err := os.ReadFile(filepath.Join(g.path, file))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (g *Group) readUint(file string) (uint64, error) {
	s, err := g.read(file)
	if err != nil {
		return 0, err
	}
	if s == Unlimited {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// readKeyed parses the "key value" per line format used by cpu.stat and friends.
func (g *Group) readKeyed(file string) (map[string]uint64, error) {
	raw, err := g.read(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}

	out := make(map[string]uint64)
	for _, line := range strings.Split(raw, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found {
			continue
		}
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			continue
		}
		out[key] = n
	}
	return out, nil
}

// requireV2 verifies the unified hierarchy is mounted. On a cgroup v1 host every
// path here silently refers to something else, so fail early and clearly.
func requireV2() error {
	// cgroup2's magic number. statfs is the reliable check: the mount's
	// filesystem *name* is reported inconsistently across distributions.
	const cgroup2Magic = 0x63677270

	magic, err := fsMagic(Root)
	if err != nil {
		return fmt.Errorf("stat %s: %w", Root, err)
	}
	if magic != cgroup2Magic {
		return fmt.Errorf("%s is not a cgroup v2 unified hierarchy (magic %#x); "+
			"boot with systemd.unified_cgroup_hierarchy=1", Root, magic)
	}
	return nil
}

// CoresToQuota converts a core count to the CPU duration per default period.
// 1.0 cores over a 100ms period is 100ms of quota.
func CoresToQuota(cores float64) time.Duration {
	return time.Duration(float64(defaultPeriod) * cores)
}
