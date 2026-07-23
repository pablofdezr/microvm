//go:build linux

// Package firecracker runs sandboxes as Firecracker microVMs, always behind the
// jailer.
//
// The jailer is not optional here. The code inside a sandbox is assumed hostile,
// and the guest kernel is the first boundary; the jailer is the second, for the
// case where the first fails. It chroots the VMM, drops it to an unprivileged
// uid, puts it in a PID namespace and a cgroup, and leaves it with a seccomp
// filter allowing roughly forty syscalls. A VMM escape then lands in an empty
// chroot owned by nobody rather than on a root process.
package firecracker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/pablofdezr/microvm/internal/cgroup"
	"github.com/pablofdezr/microvm/internal/guestclient"
	"github.com/pablofdezr/microvm/internal/netpool"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/vsock"
)

// Config configures the runtime.
type Config struct {
	// JailerBin and FirecrackerBin default to looking them up on PATH.
	JailerBin      string
	FirecrackerBin string

	// ChrootBase is where jails are built. It must live on the same filesystem
	// as the images, because they are hardlinked into each jail rather than
	// copied.
	ChrootBase string

	// ImageDir holds the kernel and the per-language rootfs images.
	ImageDir string

	// KernelPath is the guest kernel, shared by every sandbox.
	KernelPath string

	// Slice is the cgroup slice holding every sandbox, e.g. "microvm.slice".
	Slice string

	// Ceiling bounds every sandbox *together*. No individual spec can exceed it,
	// which is what keeps sandboxes from starving whatever else runs on the
	// host.
	Ceiling cgroup.Limits

	// UID and GID are what the VMM drops to. Must not be root.
	UID int
	GID int

	// PoolCIDR is the network the sandboxes are addressed from.
	PoolCIDR netip.Prefix

	// DefaultCPUCores applies when a spec does not ask for a specific amount.
	DefaultCPUCores float64

	// DefaultNetworkBps caps bandwidth for sandboxes that do not ask for a
	// specific limit.
	//
	// A default matters here in a way it does not for CPU. Nothing else bounds
	// network: a sandbox pinned to a fraction of a core can still push the
	// host's uplink flat, and the first anyone knows is the abuse complaint.
	// Zero means unlimited, which is only right on a box whose uplink is
	// nobody else's problem.
	DefaultNetworkBps int64

	// DefaultDiskBps and DefaultDiskIOPS apply likewise to the block device.
	DefaultDiskBps  int64
	DefaultDiskIOPS int64
}

// Runtime creates Firecracker-backed sandboxes.
type Runtime struct {
	cfg   Config
	log   *slog.Logger
	slice *cgroup.Group

	pool     *netpool.Pool
	taps     *netpool.TapManager
	firewall *netpool.Firewall

	mu    sync.Mutex
	insts map[string]*instance
}

// New prepares the host: cgroup slice, firewall, and reclamation of anything a
// previous run left behind.
func New(cfg Config, log *slog.Logger) (*Runtime, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	slice, err := cgroup.EnsureSlice(cfg.Slice, cfg.Ceiling)
	if err != nil {
		return nil, fmt.Errorf("prepare cgroup slice: %w", err)
	}

	pool, err := netpool.New(cfg.PoolCIDR)
	if err != nil {
		return nil, err
	}

	firewall, err := netpool.NewFirewall(netpool.FirewallConfig{PoolCIDR: cfg.PoolCIDR}, log)
	if err != nil {
		return nil, err
	}
	if err := firewall.Install(); err != nil {
		return nil, fmt.Errorf("install firewall: %w", err)
	}

	taps := netpool.NewTapManager(log, cfg.UID, cfg.GID)
	// A crash leaves TAP devices holding addresses from the pool; reclaim them
	// before handing any out again.
	if _, err := taps.CleanupOrphans(); err != nil {
		log.Warn("could not reclaim orphaned taps", "err", err)
	}

	r := &Runtime{
		cfg:      cfg,
		log:      log,
		slice:    slice,
		pool:     pool,
		taps:     taps,
		firewall: firewall,
		insts:    make(map[string]*instance),
	}
	log.Info("firecracker runtime ready",
		"slice", cfg.Slice, "pool", cfg.PoolCIDR, "chroot_base", cfg.ChrootBase)
	return r, nil
}

func (c *Config) validate() error {
	if c.UID == 0 || c.GID == 0 {
		// The whole point of the jailer is that the VMM is not root. Running it
		// as root would leave the guest kernel as the only boundary.
		return errors.New("uid and gid must be non-root")
	}
	if c.KernelPath == "" {
		return errors.New("kernel path is required")
	}
	if c.ImageDir == "" {
		return errors.New("image dir is required")
	}
	if c.Slice == "" {
		c.Slice = "microvm.slice"
	}
	if c.ChrootBase == "" {
		c.ChrootBase = "/srv/jailer"
	}
	if c.DefaultCPUCores == 0 {
		c.DefaultCPUCores = 1
	}

	if c.JailerBin == "" {
		path, err := exec.LookPath("jailer")
		if err != nil {
			return fmt.Errorf("jailer not found on PATH: %w", err)
		}
		c.JailerBin = path
	}
	if c.FirecrackerBin == "" {
		path, err := exec.LookPath("firecracker")
		if err != nil {
			return fmt.Errorf("firecracker not found on PATH: %w", err)
		}
		c.FirecrackerBin = path
	}
	return nil
}

// jailRoot is where the jailer builds a sandbox's chroot. The layout is the
// jailer's, not ours: <base>/<exec-file-basename>/<jail-id>/root.
func (r *Runtime) jailRoot(jailID string) string {
	return filepath.Join(r.cfg.ChrootBase, filepath.Base(r.cfg.FirecrackerBin), jailID, "root")
}

// jailIDBytes gives 96 bits of jail identifier: short enough to keep the vsock
// path well inside its limit, wide enough that a collision between concurrent
// sandboxes is not a thing that happens.
const jailIDBytes = 6

// newJailID returns the identifier used for a sandbox's jail directory and
// cgroup.
//
// It is deliberately *not* the caller's sandbox ID. That ID ends up in the
// vsock socket's path, and a Unix socket path cannot exceed 108 bytes -- a
// limit a caller-chosen task ID would blow through without trying. The failure
// when it does is a bare EINVAL from connect(), which says nothing about paths,
// lengths, or sockets, and sends you looking at the guest instead. Generating
// the jail name ourselves removes the caller's input from the equation.
func newJailID() string {
	var b [jailIDBytes]byte
	// crypto/rand cannot fail on any platform we run on, and a collision would
	// mean two sandboxes sharing a chroot.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// checkVsockPath rejects a jail whose vsock sockets would not fit in sun_path.
//
// A backstop for the one part of the path we do not control: an operator's
// -chroot-base. Failing here names the problem; failing at connect() time gives
// you EINVAL and a long afternoon.
//
// It measures the *longest* socket in the jail, not the base one. The host-side
// listeners are the base path plus "_<port>", so checking v.sock alone would
// pass a chroot-base that leaves v.sock_5001 five bytes over -- and the sandbox
// would boot fine, serve execs fine, and fail only when the guest first touched
// storage.
func (r *Runtime) checkVsockPath(jailID string) error {
	// sun_path is 108 bytes on Linux, including the NUL terminator.
	const sunPathMax = 107

	path := vsock.HostListenerPath(
		filepath.Join(r.jailRoot(jailID), vsockSocketName), protocol.StoragePort)
	if len(path) > sunPathMax {
		return fmt.Errorf(
			"the vsock socket path would be %d bytes, over the %d byte limit for a unix socket: %s\n"+
				"use a shorter -chroot-base",
			len(path), sunPathMax, path)
	}
	return nil
}

// Create boots a sandbox and waits for its agent.
func (r *Runtime) Create(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
	if spec.ID == "" {
		return nil, errors.New("spec.ID is required")
	}

	// The jail name is generated, not the caller's: see newJailID.
	jailID := newJailID()
	if err := r.checkVsockPath(jailID); err != nil {
		return nil, err
	}

	inst := &instance{
		id:      spec.ID,
		jailID:  jailID,
		runtime: r,
		log:     r.log.With("sandbox", spec.ID, "jail", jailID),
		started: time.Now(),
		// The cgroup sits at <slice>/<jail-id>: the jailer names it after the
		// --id it was given, which the probe on the real host confirmed.
		group: r.slice.Child(jailID),
	}

	if err := r.setup(ctx, inst, spec); err != nil {
		// Roll back whatever got as far as existing, or the failure leaks a TAP
		// device and a network slot on every retry.
		_ = inst.Stop(context.Background())
		return nil, err
	}

	r.mu.Lock()
	r.insts[spec.ID] = inst
	r.mu.Unlock()

	return inst, nil
}

func (r *Runtime) setup(ctx context.Context, inst *instance, spec runtime.Spec) error {
	rootfs := filepath.Join(r.cfg.ImageDir, spec.Image)
	if _, err := os.Stat(rootfs); err != nil {
		return fmt.Errorf("rootfs for image %q: %w", spec.Image, err)
	}

	if spec.Network {
		lease, err := r.pool.Allocate()
		if err != nil {
			return err
		}
		inst.lease = &lease
		if err := r.taps.Create(lease); err != nil {
			return fmt.Errorf("create tap: %w", err)
		}
	}

	// The jailer chroots the VMM, so everything it opens must already be inside
	// the jail. It creates the jail directory itself during exec, which is too
	// late for us to place files -- so build the tree first and let the jailer
	// adopt it.
	jailRoot := r.jailRoot(inst.jailID)
	if err := os.MkdirAll(jailRoot, 0o755); err != nil {
		return fmt.Errorf("create jail root: %w", err)
	}

	if err := r.stageFile(r.cfg.KernelPath, filepath.Join(jailRoot, "vmlinux")); err != nil {
		return fmt.Errorf("stage kernel: %w", err)
	}
	if err := r.stageFile(rootfs, filepath.Join(jailRoot, "rootfs.ext4")); err != nil {
		return fmt.Errorf("stage rootfs: %w", err)
	}

	cfgJSON, err := r.vmConfig(spec, inst.lease)
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(jailRoot, "vm.json")
	if err := os.WriteFile(cfgPath, cfgJSON, 0o644); err != nil {
		return fmt.Errorf("write vm config: %w", err)
	}

	// The jailed VMM must be able to read what we staged and write its socket.
	if err := chownTree(jailRoot, r.cfg.UID, r.cfg.GID); err != nil {
		return fmt.Errorf("chown jail: %w", err)
	}

	// The vsock socket is created by Firecracker inside the jail, so the host
	// path to it goes through the jail root.
	inst.udsPath = filepath.Join(jailRoot, vsockSocketName)
	inst.client = guestclient.New(inst.udsPath)

	// The inbound listener is opened *before* the VMM starts, and the order is
	// the whole point. Firecracker resolves this path when the guest first
	// connects, and a socket that is not there yet is a refused connection, not
	// a wait -- so a listener opened after boot leaves a window in which the
	// guest's storage calls fail for no reason it could ever report. Opening it
	// first costs nothing: nothing can connect until there is a VM.
	//
	// It is created after the chown above rather than before, so that the tree
	// walk cannot undo the tighter mode Listen sets on it.
	l, err := vsock.Listen(inst.udsPath, protocol.StoragePort, r.cfg.UID, r.cfg.GID)
	if err != nil {
		// Not fatal. A sandbox with no inbound socket has no storage, and that is
		// a worse sandbox rather than a broken one -- most never call out at all.
		// Failing the boot here would take down every sandbox on the host for a
		// feature most of them do not use.
		inst.log.Warn("no host listener for this sandbox; storage will be unavailable to it", "err", err)
	} else {
		inst.hostListener = l
	}

	if err := r.start(inst, spec); err != nil {
		return err
	}

	if err := inst.waitReady(ctx); err != nil {
		// The bare timeout says only that nothing answered, which is true of
		// every possible cause. The console holds the actual reason -- a VMM
		// that refused to start, a kernel panic, an init failure -- and the
		// caller is about to roll the jail back and destroy it, so attach it
		// here or lose it.
		return fmt.Errorf("sandbox never became ready: %w\n--- guest console ---\n%s",
			err, inst.consoleTail())
	}
	return nil
}

// bootTimeout bounds how long a sandbox may take to answer.
//
// A guest that boots at all does so in well under a second; this is generous
// enough that a loaded host does not trip it, and short enough that a caller is
// not left waiting on a VM that is never coming.
const bootTimeout = 30 * time.Second

// vsockSocketName is where Firecracker puts the guest's vsock socket, relative
// to the jail root.
const vsockSocketName = "v.sock"

// stageFile makes a host file available inside the jail.
//
// A hardlink rather than a copy: the rootfs images are hundreds of megabytes
// and a copy per sandbox would blow out both disk and boot time. It also keeps
// one inode shared across every VM using that image, so the host page cache
// holds a single copy no matter how many sandboxes run -- the same reason the
// guest stacks a tmpfs over a read-only base.
//
// Hardlinking requires the jail and the images to share a filesystem; the
// fallback exists because that is a deployment detail, not a guarantee.
func (r *Runtime) stageFile(src, dst string) error {
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := os.Link(src, dst); err == nil {
		return nil
	}

	// EXDEV: different filesystems. Copy instead, and say so -- a silent copy
	// per sandbox is a performance cliff worth knowing about.
	r.log.Warn("image is on a different filesystem from the jail; copying instead of linking",
		"src", src, "hint", "put ChrootBase and ImageDir on one filesystem")
	return copyFile(src, dst)
}

// vmConfig renders the Firecracker machine configuration.
//
// Every path in it is relative to the jail root, because the VMM reads this
// after it has already chrooted.
func (r *Runtime) vmConfig(spec runtime.Spec, lease *netpool.Lease) ([]byte, error) {
	// Fill the rate limits from the runtime's defaults before rendering. A spec
	// that names no limit must not mean "no limit": that is how one sandbox
	// takes the host's uplink.
	if spec.Limits.NetworkBps <= 0 {
		spec.Limits.NetworkBps = r.cfg.DefaultNetworkBps
	}
	if spec.Limits.DiskBps <= 0 {
		spec.Limits.DiskBps = r.cfg.DefaultDiskBps
	}
	if spec.Limits.DiskIOPS <= 0 {
		spec.Limits.DiskIOPS = r.cfg.DefaultDiskIOPS
	}

	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro" +
		" microvm.hostname=" + spec.ID

	if spec.Limits.DiskMiB > 0 {
		bootArgs += fmt.Sprintf(" microvm.upper_size=%dm", spec.Limits.DiskMiB)
	}

	// Storage, when the sandbox has it, is announced to the guest agent here. The
	// mount path arrives on a space-separated kernel command line, so a value
	// with whitespace in it would not extend the path -- it would inject a second
	// kernel parameter (init=, say). safeCmdlineValue is the gate: a rejected
	// path drops storage for this VM rather than booting a compromised command
	// line. Callers upstream validate too; this is the layer that must hold even
	// if one did not.
	if spec.StorageMount != "" {
		if safeCmdlineValue(spec.StorageMount) {
			mode := "rw"
			if spec.StorageReadOnly {
				mode = "ro"
			}
			bootArgs += fmt.Sprintf(" microvm.storage=%s microvm.storage_path=%s", mode, spec.StorageMount)
		} else {
			r.log.Error("refusing unsafe storage mount path on kernel cmdline; sandbox will have no storage",
				"sandbox", spec.ID, "path", spec.StorageMount)
		}
	}

	rootDrive := map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   "rootfs.ext4",
		"is_root_device": true,
		// Read-only and shared between every sandbox on this image. The
		// guest makes it writable with an overlay of its own.
		"is_read_only": true,
	}
	if rl := newRateLimiter(spec.Limits.DiskBps, spec.Limits.DiskIOPS); rl != nil {
		rootDrive["rate_limiter"] = rl
	}

	cfg := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": "vmlinux",
			"boot_args":         bootArgs,
		},
		"machine-config": map[string]any{
			"vcpu_count":   spec.VCPUs,
			"mem_size_mib": spec.MemMiB,
		},
		"drives": []any{rootDrive},
		"vsock": map[string]any{
			"guest_cid": protocol.GuestCID,
			"uds_path":  vsockSocketName,
		},
	}

	if lease != nil {
		iface := map[string]any{
			"iface_id":      "eth0",
			"guest_mac":     lease.MAC.String(),
			"host_dev_name": lease.TapName,
		}
		// Both directions: capping only egress still lets a sandbox pull the
		// host's downlink flat, and capping only ingress still lets it flood
		// somebody else.
		if rl := newRateLimiter(spec.Limits.NetworkBps, 0); rl != nil {
			iface["rx_rate_limiter"] = rl
			iface["tx_rate_limiter"] = newRateLimiter(spec.Limits.NetworkBps, 0)
		}
		cfg["network-interfaces"] = []any{iface}
		// The guest configures itself from these: there is no DHCP server, and
		// relying on one would be another moving part inside the trust boundary.
		bootArgs += fmt.Sprintf(" microvm.ip=%s microvm.gw=%s microvm.dns=1.1.1.1",
			lease.GuestCIDR(), lease.HostIP)
		cfg["boot-source"].(map[string]any)["boot_args"] = bootArgs
	}

	return json.MarshalIndent(cfg, "", "  ")
}

// safeCmdlineValue reports whether v can be placed on the kernel command line
// without changing its structure. The command line is space-separated
// key=value tokens, so the danger is not exotic: a single space turns one value
// into a value plus a new parameter. The allowed set is therefore narrow on
// purpose -- an absolute path made of the characters a mount point actually
// needs -- and everything else is refused rather than escaped, because there is
// no escaping on a kernel command line to lean on.
func safeCmdlineValue(v string) bool {
	if v == "" || v[0] != '/' || len(v) > 256 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

// start execs the jailer, which sets up the sandbox and execs Firecracker.
func (r *Runtime) start(inst *instance, spec runtime.Spec) error {
	cores := spec.Limits.CPUCores
	if cores <= 0 {
		cores = r.cfg.DefaultCPUCores
	}

	args := []string{
		"--id", inst.jailID,
		"--exec-file", r.cfg.FirecrackerBin,
		"--uid", fmt.Sprint(r.cfg.UID),
		"--gid", fmt.Sprint(r.cfg.GID),
		"--chroot-base-dir", r.cfg.ChrootBase,
		"--cgroup-version", "2",
		"--parent-cgroup", r.cfg.Slice,
		// A PID namespace means the VMM cannot see or signal anything else on
		// the host, even if it breaks out of its own process.
		"--new-pid-ns",
	}

	// Per-sandbox limits, applied by the jailer as it creates the cgroup. These
	// sit under the slice ceiling, which bounds them all together regardless of
	// what any single sandbox asks for.
	quota := cgroup.CoresToQuota(cores)
	args = append(args,
		"--cgroup", fmt.Sprintf("cpu.max=%d 100000", quota.Microseconds()))

	if spec.MemMiB > 0 {
		// Headroom over the guest's RAM: the VMM itself needs memory beyond
		// what it hands the guest, and a limit set exactly at the guest size
		// would OOM the VMM as soon as the guest touched all its pages.
		limit := uint64(spec.MemMiB)*1024*1024 + vmmOverheadBytes
		args = append(args, "--cgroup", fmt.Sprintf("memory.max=%d", limit))
	}

	args = append(args, "--", "--no-api", "--config-file", "vm.json")

	cmd := exec.Command(r.cfg.JailerBin, args...)

	logPath := filepath.Join(r.jailRoot(inst.jailID), "console.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("create console log: %w", err)
	}
	// The guest console is the only diagnostic for a VM that fails before its
	// agent is up, which is exactly when things go wrong.
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil

	// A jailer that outlives us is a sandbox nobody is metering or will ever
	// stop. Tie its life to ours.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start jailer: %w", err)
	}

	inst.cmd = cmd
	inst.logFile = logFile
	inst.logPath = logPath
	inst.exited = make(chan struct{})

	// This goroutine is the sole owner of cmd.Wait: it reaps the process so a
	// crashed VMM does not linger as a zombie, and closing exited is how
	// everything else learns the process is gone without waiting on it too.
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(inst.exited)
	}()

	return nil
}

// vmmOverheadBytes is how much the VMM needs above the guest's own RAM.
// Firecracker's own footprint is a few MB; the rest is slack for the guest's
// page tables and device buffers.
const vmmOverheadBytes = 64 * 1024 * 1024

// Close tears down host-wide state. Sandboxes must already be stopped.
func (r *Runtime) Close() error {
	r.mu.Lock()
	insts := make([]*instance, 0, len(r.insts))
	for _, i := range r.insts {
		insts = append(insts, i)
	}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, i := range insts {
		if err := i.Stop(ctx); err != nil {
			r.log.Warn("stopping sandbox during shutdown", "sandbox", i.id, "err", err)
		}
	}

	// The firewall comes down last: removing it while a guest still runs would
	// leave that guest with unfiltered egress for as long as it lived.
	return r.firewall.Remove()
}

func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, uid, gid)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := out.ReadFrom(in); err != nil {
		return err
	}
	return out.Close()
}
