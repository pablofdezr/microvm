//go:build linux

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// gicCarrier makes a Firecracker snapshot restorable on a GICv2 host.
//
// # The problem it solves
//
// Firecracker's GICv2 snapshot support does not save the guest's *banked*
// interrupt-controller state: in src/vmm/src/arch/aarch64/gic/gicv2 every
// distributor register is read and written from INTID 32 upward with the target
// vCPU field left at zero (mpidr_mask() returns 0), on the assumption that
// INTIDs 0-31 are RAZ/WI. That holds for a physical distributor. It does not
// hold for KVM's vGIC, where those registers are per-vCPU and are reached by
// putting the vCPU in the KVM_DEV_ARM_VGIC_GRP_DIST_REGS attribute. The CPU
// interface registers go the same way, so only vCPU 0's survive.
//
// Measured on a Raspberry Pi 5 (BCM2712/GIC-400, Firecracker v1.16.1, guest
// 6.1.102), reading the guest's own registers either side of a restore:
//
//	template   GICD_ISENABLER0=0x0800007f  GICC_CTLR=1 PMR=0xf0   (both vCPUs)
//	restored   GICD_ISENABLER0=0x0000ffff  GICC_CTLR=1 PMR=0xf0   (vCPU 0)
//	restored   GICD_ISENABLER0=0x0000ffff  GICC_CTLR=0 PMR=0x00   (vCPU 1)
//
// Bit 27 is gone on every vCPU and the secondary vCPU's interface is switched
// off entirely; 0xffff is KVM's reset mask, i.e. state nobody restored. INTID 27
// is the ARM64 virtual timer, the only clockevent device an arm64 microVM has,
// and the restored guest showed it pending-but-disabled forever: GICD_ISPENDR0
// stuck at 0x08000000, the guest's arch_timer count frozen at the value it had
// when the snapshot was taken, every SPI (virtio, serial) still working. So the
// guest runs userspace at full speed with a correct monotonic clock and nothing
// that waits on time ever completes again -- the vsock listener accepts the
// host's connection in under a millisecond and the agent never gets far enough
// to write a reply. This is invisible upstream because Firecracker's aarch64 CI
// runs on GICv3 hosts, where the equivalent state *is* saved per vCPU.
//
// # How it solves it
//
// The guest is the only thing that can write its own banked registers, so it
// carries them across the snapshot itself: capture the three registers per vCPU,
// then reapply them the instant the VM resumes.
//
// "The instant it resumes" is the whole difficulty, and it is why this spins. A
// vCPU that was idle when the snapshot was taken is parked in WFI, and nothing
// can wake it: waking it needs an interrupt, its timer PPI is disabled, and an
// IPI would need the SGIs that are equally disabled -- a secondary vCPU with its
// CPU interface off cannot be reached at all. There is no signal to wait for and
// no lever on the host side (verified: load with resume_vm:false then Resume, or
// resume then Pause/Resume, both leave the timer dead). The only state from which
// a vCPU is guaranteed to execute at resume is one where it was already
// executing when the snapshot was taken. So arming pins one thread per vCPU and
// each spins on its own registers, the host snapshots, and every vCPU comes back
// mid-loop and repairs itself in its first iteration.
//
// A spinner stops itself the moment it has repaired its vCPU and seen the repair
// hold, so the whole cost of the carry is the milliseconds between arming and the
// pause (during which the guest is executing) plus microseconds after the resume.
// The host's disarm and armBudget are backstops for a host that turns out not to
// lose the state at all, where no repair is ever needed and so no spinner has
// anything to notice.
//
// A write can only ever enable an interrupt (GICD_ISENABLER0 is write-1-to-set)
// and every value written is one this same vCPU published moments earlier, so on
// a host that restores the state correctly the whole thing is a no-op loop.
type gicCarrier struct {
	log *slog.Logger
	// dtRoot is where to look for the guest's device tree. It is a field rather
	// than a constant so a test can point it at an empty tree: arming reaches
	// straight into a live interrupt controller through /dev/mem, which is the
	// last thing a unit test running on somebody's host should be able to do by
	// accident.
	dtRoot string

	mu   sync.Mutex
	sess *gicSession
}

// gicSession is one armed carry: the mapped registers and the threads spinning
// on them.
type gicSession struct {
	stop atomic.Bool
	wg   sync.WaitGroup
	// checked counts the vCPUs that compared their live state against what was
	// carried before standing down. It is the fact the host cannot observe for
	// itself: a readiness reply proves one vCPU works, and this state is banked
	// per vCPU, so "every vCPU was looked at" is only knowable in here. A carry
	// that stands down with checked < cpus is a guest with a possibly dead vCPU,
	// and the host throws it away.
	checked atomic.Int64
	// reapplied counts the vCPUs that found their state missing and put it back,
	// which is the difference between a host that needs this and one that does not.
	reapplied atomic.Int64
	// done is closed once every spinner has exited and the process-wide settings
	// arming changed have been put back.
	done chan struct{}

	dist, cpuif mmio
	cpus        int

	prevGC    int
	prevProcs int
}

// armBudget caps how long a carry spins when nothing ever tells it to stop: a
// daemon that dies between the restore and the disarm, or a host that does not
// lose the state, so no spinner sees anything to repair.
//
// It is measured in guest time, which does not advance while the VM is paused or
// while its snapshot sits on disk, so it bounds executing time and not wall clock.
// The window it has to cover is the milliseconds between arming and the pause;
// the slack is for a loaded host that is slow to get from one to the other, since
// a carry that expires early costs a snapshot (the restore fails and the pool
// cold-boots) while one that lingers only costs CPU.
const armBudget = 20 * time.Second

func newGICCarrier(log *slog.Logger) *gicCarrier {
	return &gicCarrier{log: log, dtRoot: deviceTreeRoot}
}

// arm captures each vCPU's banked interrupt-controller state and starts the
// threads that will put it back when the VM resumes.
//
// It is idempotent: a guest restored from an armed snapshot is itself armed, and
// snapshotting it again must not stack a second carry on top.
func (c *gicCarrier) arm(ctx context.Context) (protocol.SnapshotArmResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if sess := c.sess; sess != nil {
		select {
		case <-sess.done:
			// Spent: its spinners stood themselves down after repairing the
			// vCPUs, and its register mappings are gone with them. Arming again
			// has to start over rather than report this one as live.
			c.sess = nil
		default:
			return protocol.SnapshotArmResponse{Armed: true, CPUs: sess.cpus}, nil
		}
	}

	gic, reason, err := findGICv2(c.dtRoot)
	if err != nil {
		return protocol.SnapshotArmResponse{}, err
	}
	if gic == nil {
		return protocol.SnapshotArmResponse{Armed: false, Detail: reason}, nil
	}

	cpus, err := onlineCPUs()
	if err != nil {
		return protocol.SnapshotArmResponse{}, err
	}

	dist, cpuif, err := mapGIC(gic)
	if err != nil {
		return protocol.SnapshotArmResponse{}, err
	}

	sess := &gicSession{done: make(chan struct{}), dist: dist, cpuif: cpuif, cpus: len(cpus)}

	// Two process-wide settings, both restored when the carry ends.
	//
	// GC off: a garbage collection stops the world, and a stopped spinner is one
	// that is not running when the snapshot is taken -- the one state this cannot
	// recover from. The armed window allocates nothing, so nothing is lost.
	//
	// One more P than there are vCPUs: the spinners occupy a P each, and the
	// request that stands them down needs one to be answered on.
	sess.prevGC = debug.SetGCPercent(-1)
	sess.prevProcs = runtime.GOMAXPROCS(len(cpus) + 1)

	armed := make(chan error, len(cpus))
	for _, cpu := range cpus {
		sess.wg.Add(1)
		go sess.spin(cpu, armed)
	}

	var failed []error
	for range cpus {
		if err := <-armed; err != nil {
			failed = append(failed, err)
		}
	}
	if len(failed) > 0 {
		// A carry that misses a vCPU is worse than none: the snapshot would
		// restore into a guest that is half alive, which is harder to diagnose
		// than one that never answers. Tear down and let the host decide.
		sess.stop.Store(true)
		sess.reap()
		return protocol.SnapshotArmResponse{}, fmt.Errorf("arm the interrupt-controller carry: %w", failed[0])
	}

	go sess.reap()
	c.sess = sess
	c.log.Info("armed the interrupt-controller carry for a snapshot", "cpus", len(cpus))
	return protocol.SnapshotArmResponse{Armed: true, CPUs: len(cpus)}, nil
}

// disarm stops the carry and waits for it to finish, so the caller knows the
// guest's vCPUs are its own again. Standing down a guest that was never armed is
// success: the host disarms unconditionally after a restore, and a snapshot taken
// on a host that needed no carry restores into a guest with nothing to stand down.
//
// The session is only forgotten once the wait has succeeded. Clearing it first
// looked tidier and was wrong twice over: a retry after a timed-out wait would
// answer "nothing to stand down" while spinners were still running -- telling the
// host the guest's vCPUs were its own when they might not be -- and a later arm()
// would then skip the spent-session probe and record the *armed* GOMAXPROCS and
// GC settings as the ones to restore afterwards.
func (c *gicCarrier) disarm(ctx context.Context) (protocol.SnapshotDisarmResponse, error) {
	c.mu.Lock()
	sess := c.sess
	c.mu.Unlock()

	if sess == nil {
		return protocol.SnapshotDisarmResponse{Armed: false}, nil
	}

	sess.stop.Store(true)
	select {
	case <-sess.done:
	case <-ctx.Done():
		// Left in place on purpose: the spinners are still going to exit (they
		// have seen stop, and armBudget bounds them regardless), reap is already
		// waiting on them, and a retry must be able to wait again rather than be
		// told there was nothing here.
		return protocol.SnapshotDisarmResponse{}, fmt.Errorf(
			"waiting for the interrupt-controller carry to stand down on %d vCPUs (%d accounted for): %w",
			sess.cpus, sess.checked.Load(), ctx.Err())
	}

	c.mu.Lock()
	if c.sess == sess {
		c.sess = nil
	}
	c.mu.Unlock()

	resp := protocol.SnapshotDisarmResponse{
		Armed:     true,
		CPUs:      sess.cpus,
		Checked:   int(sess.checked.Load()),
		Reapplied: int(sess.reapplied.Load()),
	}
	// Whether the state had actually been lost is the one fact worth logging here:
	// it is the difference between "this host needs the carry" and "this host never
	// did", and it is only knowable from inside the guest.
	c.log.Info("stood down the interrupt-controller carry",
		"cpus", resp.CPUs, "checked", resp.Checked, "reapplied", resp.Reapplied)
	return resp, nil
}

// spin pins itself to one vCPU, captures that vCPU's banked state, and holds it
// until it has put it back after a resume -- or until the carry is stood down, on
// a host where there turns out to be nothing to put back.
func (s *gicSession) spin(cpu int, armed chan<- error) {
	defer s.wg.Done()

	// The thread must not move: the registers it captures and repairs are the
	// ones belonging to the vCPU it is executing on.
	runtime.LockOSThread()

	// The pinning must not outlive the carry. Go reuses OS threads, and a process
	// forked later on a thread still confined to one vCPU inherits that
	// confinement -- which showed up as `nproc` reporting 1 inside a restored
	// two-vCPU sandbox, with every command the sandbox ever ran silently pinned to
	// a single core. So the mask goes back before the thread does, and if it
	// cannot be put back the goroutine returns while still locked, which makes the
	// runtime destroy the thread instead of handing it out again.
	var original unix.CPUSet
	if err := unix.SchedGetaffinity(0, &original); err != nil {
		armed <- fmt.Errorf("read this thread's cpu affinity: %w", err)
		runtime.UnlockOSThread()
		return
	}
	defer func() {
		if err := unix.SchedSetaffinity(0, &original); err != nil {
			return // deliberately still locked: this thread must not be reused
		}
		runtime.UnlockOSThread()
	}()

	if err := pinThread(cpu); err != nil {
		armed <- err
		return
	}
	if on, err := currentCPU(); err != nil {
		armed <- fmt.Errorf("confirm this thread runs on cpu %d: %w", cpu, err)
		return
	} else if on != cpu {
		armed <- fmt.Errorf("pinned to cpu %d but running on cpu %d", cpu, on)
		return
	}

	saved := captureBanked(s.dist, s.cpuif)
	if saved.ctlr == 0 {
		// Nothing enabled the CPU interface, so this reading cannot be the state
		// a healthy vCPU should be restored to.
		armed <- fmt.Errorf("cpu %d: GIC CPU interface reads as disabled (CTLR=0), refusing to carry that", cpu)
		return
	}
	armed <- nil

	// A monotonic deadline, which is what survives a snapshot: Firecracker
	// restores the guest's virtual counter, so guest time picks up where it left
	// off however long the snapshot spent on disk.
	deadline := time.Now().Add(armBudget)

	// repaired is per-vCPU on purpose. Every vCPU loses its own copy of this
	// state, so one vCPU having repaired itself says nothing about another.
	repaired := false

	// counted makes this vCPU's one contribution to s.checked, however the loop
	// below ends. The host reads that count and refuses a guest whose vCPUs do not
	// all appear in it.
	counted := false

	for {
		// The health check comes before the stop check, and that ordering is the
		// whole guarantee this loop offers.
		//
		// A spinner is not necessarily on-CPU when the host pauses the VM -- arming
		// leaves N pinned spinners and at least one unpinned runtime thread (the
		// one answering the arm request) on N vCPUs, so at least one spinner is
		// preempted in the arm-to-pause window by construction. That spinner is
		// scheduled again some time after the resume, possibly after the host has
		// already seen a reply from another vCPU and disarmed. Testing stop first
		// meant it exited without ever looking at its registers, and its vCPU stayed
		// dead for the sandbox's life: no timer, no reachable SGIs, and the guest
		// wedged on the first TLB shootdown. Checking first costs one register read.
		healthy := saved.healthy(s.dist, s.cpuif)
		if !counted {
			counted = true
			s.checked.Add(1)
		}
		if !healthy {
			// The first iteration after a resume on a GICv2 host. Everything in
			// this guest that waits on time is stalled until this store lands.
			saved.reapply(s.dist, s.cpuif)
			if !repaired {
				s.reapplied.Add(1)
			}
			repaired = true
			continue // confirm it took effect before standing down
		}
		if repaired {
			// Delivered, and there is nothing to carry a second time: a VM is
			// restored once, and a restored VM that gets snapshotted again arms
			// afresh.
			//
			// Stopping here rather than waiting to be told is what keeps the cost
			// of the carry at microseconds. Spinning on is not free the way it
			// looks: the sandbox's cgroup caps it at its vCPU count, so vCPUs at
			// full tilt leave no quota for the VMM's own device threads, and the
			// host's vsock traffic starves behind them. Measured with the spin
			// held until the host asked it to stop: a disarm that takes 12ms on an
			// uncapped VM took over 10 seconds and timed out.
			return
		}
		if s.stop.Load() {
			// Nothing to put back and the host is done with us: this is the
			// platform that never needed the carry. The check above has already
			// counted this vCPU, so the host's tally still adds up.
			return
		}
		if time.Now().After(deadline) {
			return
		}
	}
}

// reap waits for every spinner and undoes what arming changed.
func (s *gicSession) reap() {
	s.wg.Wait()
	runtime.GOMAXPROCS(s.prevProcs)
	debug.SetGCPercent(s.prevGC)
	// Safe only here: no spinner is left to touch the mapping.
	if err := unix.Munmap(s.dist); err == nil {
		s.dist = nil
	}
	if err := unix.Munmap(s.cpuif); err == nil {
		s.cpuif = nil
	}
	close(s.done)
}

// mapGIC maps the two GICv2 register windows into this process.
//
// /dev/mem is the only way for a userspace process to reach them, and it is not
// a new privilege in a microVM guest: the agent is PID 1 and every exec already
// runs as root inside the VM, whose isolation boundary is the VM itself.
func mapGIC(gic *gicv2) (dist, cpuif mmio, err error) {
	fd, err := unix.Open("/dev/mem", unix.O_RDWR|unix.O_SYNC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/mem to reach the GIC: %w", err)
	}
	// The mappings outlive the descriptor, so it is closed either way.
	defer unix.Close(fd)

	dist, err = mapRegion(fd, gic.distBase, gic.distSize)
	if err != nil {
		return nil, nil, fmt.Errorf("map GIC distributor at %#x: %w", gic.distBase, err)
	}
	cpuif, err = mapRegion(fd, gic.cpuBase, gic.cpuSize)
	if err != nil {
		_ = unix.Munmap(dist)
		return nil, nil, fmt.Errorf("map GIC CPU interface at %#x: %w", gic.cpuBase, err)
	}
	return dist, cpuif, nil
}

func mapRegion(fd int, base, size uint64) (mmio, error) {
	page := uint64(os.Getpagesize())
	if base%page != 0 {
		return nil, fmt.Errorf("%#x is not page aligned", base)
	}
	// Round up: a device window may be smaller than a page, and mmap counts in
	// pages either way.
	length := int((size + page - 1) / page * page)
	if length == 0 {
		length = int(page)
	}
	b, err := unix.Mmap(fd, int64(base), length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	return mmio(b), nil
}

// pinThread confines the calling thread to one CPU.
func pinThread(cpu int) error {
	var set unix.CPUSet
	set.Zero()
	set.Set(cpu)
	// pid 0 is this thread, not the process: each spinner pins only itself.
	if err := unix.SchedSetaffinity(0, &set); err != nil {
		return fmt.Errorf("pin a thread to cpu %d: %w", cpu, err)
	}
	return nil
}

// currentCPU reports which CPU the calling thread is on.
//
// sched_setaffinity migrates the caller before it returns, but "before it
// returns" is a promise about the kernel's implementation and this carry is
// worthless if it is ever wrong, so it is checked rather than assumed.
func currentCPU() (int, error) {
	raw, err := os.ReadFile("/proc/thread-self/stat")
	if err != nil {
		return 0, err
	}
	// The second field is the executable name in parentheses and may itself
	// contain spaces and parentheses, so fields are counted from the last ')'.
	nameEnd := strings.LastIndexByte(string(raw), ')')
	if nameEnd < 0 {
		return 0, fmt.Errorf("unparseable /proc/thread-self/stat")
	}
	fields := strings.Fields(string(raw[nameEnd+1:]))
	// Field 39 of the whole line ("processor") is field 37 after the name.
	const processorField = 37
	if len(fields) < processorField {
		return 0, fmt.Errorf("/proc/thread-self/stat has %d fields after the name, want %d", len(fields), processorField)
	}
	return strconv.Atoi(fields[processorField-1])
}

// onlineCPUs lists the guest's online CPUs.
//
// Every one of them needs its own spinner: the state is banked per vCPU, and a
// vCPU nobody repairs is a vCPU with no timer.
func onlineCPUs() ([]int, error) {
	raw, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return nil, fmt.Errorf("read the online CPU list: %w", err)
	}
	cpus, err := parseCPUList(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, err
	}
	if len(cpus) == 0 {
		return nil, fmt.Errorf("the online CPU list is empty")
	}
	return cpus, nil
}
