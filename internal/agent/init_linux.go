//go:build linux

package agent

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// The guest boots with /dev/vda mounted read-only. That base image is shared by
// every VM on the host, so a single copy of each language image stays in the
// host page cache no matter how many sandboxes are running. To make the root
// writable anyway, init stacks a tmpfs over it with overlayfs and pivots into
// the result: writes land in RAM, cost nothing to discard, and vanish with the
// VM. A sandbox that needs real disk gets a scratch drive mounted separately.
const (
	overlayDir = "/overlay"
	newRoot    = "/overlay/newroot"
	oldRoot    = "/oldroot"
)

// defaultUpperSize caps the writable layer. Without a limit a runaway process
// could fill the tmpfs and hang the guest with an unkillable page-alloc stall
// instead of a clean ENOSPC.
const defaultUpperSize = "512m"

type mount struct {
	source string
	target string
	fstype string
	flags  uintptr
	data   string
	// optional mounts are skipped when the kernel lacks the filesystem, rather
	// than aborting the boot.
	optional bool
}

// InitGuest brings the guest up: early mounts, a writable root, the standard
// pseudo-filesystems, networking and DNS. It runs before anything is served.
func InitGuest(log *slog.Logger) error {
	// These come first and in this order for a reason: the kernel hands PID 1 a
	// bare root with nothing mounted, so /proc does not exist yet -- and the
	// boot parameters are read *from* /proc/cmdline. Reading the command line
	// before this point fails with ENOENT and, since we are init, exiting on
	// that error panics the kernel.
	if err := doMounts(log, []mount{
		{source: "proc", target: "/proc", fstype: "proc"},
		{source: "sysfs", target: "/sys", fstype: "sysfs"},
		{source: "devtmpfs", target: "/dev", fstype: "devtmpfs"},
	}); err != nil {
		return err
	}

	cmdline, err := readCmdline()
	if err != nil {
		return fmt.Errorf("read cmdline: %w", err)
	}

	if err := setupOverlayRoot(cmdline.get("microvm.upper_size", defaultUpperSize)); err != nil {
		return fmt.Errorf("overlay root: %w", err)
	}

	// After pivot_root the earlier mounts live under the detached old root, so
	// the pseudo-filesystems are mounted afresh in the new namespace.
	if err := doMounts(log, []mount{
		{source: "proc", target: "/proc", fstype: "proc"},
		{source: "sysfs", target: "/sys", fstype: "sysfs"},
		{source: "devtmpfs", target: "/dev", fstype: "devtmpfs"},
		{source: "devpts", target: "/dev/pts", fstype: "devpts", data: "gid=5,mode=620"},
		{source: "tmpfs", target: "/dev/shm", fstype: "tmpfs", data: "mode=1777"},
		{source: "tmpfs", target: "/tmp", fstype: "tmpfs", data: "mode=1777"},
		{source: "tmpfs", target: "/run", fstype: "tmpfs", data: "mode=755"},
		// cgroups let a caller inspect its own limits; not worth failing on.
		{source: "cgroup2", target: "/sys/fs/cgroup", fstype: "cgroup2", optional: true},
	}); err != nil {
		return err
	}

	if err := mountScratch(log, cmdline); err != nil {
		return fmt.Errorf("scratch drive: %w", err)
	}

	if err := setupNetwork(log, cmdline); err != nil {
		// A sandbox with no network is degraded but still useful: code that
		// needs no dependencies runs fine. Boot rather than strand the VM.
		log.Error("network setup failed, continuing without it", "err", err)
	}

	if hostname := cmdline.get("microvm.hostname", "sandbox"); hostname != "" {
		if err := unix.Sethostname([]byte(hostname)); err != nil {
			log.Warn("sethostname failed", "err", err)
		}
	}

	setupEnvironment(log, cmdline)

	// Storage comes up last: it needs /dev (for /dev/fuse) and the pivoted root
	// (so the mountpoint is writable), both of which the steps above established.
	// It is best-effort by design -- see mountStorage.
	mountStorage(log, cmdline)

	return nil
}

// imageEnvPath is where the image build writes the environment its Dockerfile
// declared. See images/packer/pack.sh.
const imageEnvPath = "/etc/microvm/environment"

// setupEnvironment gives the VM its environment.
//
// The kernel execs init with an essentially empty environment: no PATH, no
// HOME, nothing. Every process the agent spawns inherits that, so without this
// even `sh` fails to resolve and *all* execs die with "executable file not
// found in $PATH". Nobody else will set these -- there is no shell, no login
// and no service manager in the boot path, only us.
//
// The image's own environment is loaded first and wins over our defaults. It
// carries the things that make a language image work at all: rustc lives on the
// PATH that rust:slim sets, GOCACHE has to point somewhere writable, Python's
// unbuffered mode is what makes its output stream. Those are declared as ENV in
// the Dockerfile, which `docker export` discards -- so the build materialises
// them into a file and this is where they come back.
func setupEnvironment(log *slog.Logger, cmdline kernelCmdline) {
	loaded := loadImageEnv(log)

	defaults := map[string]string{
		"PATH":   "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME":   "/root",
		"USER":   "root",
		"SHELL":  "/bin/sh",
		"TERM":   "xterm-256color",
		"LANG":   "C.UTF-8",
		"TMPDIR": "/tmp",
	}

	// The kernel command line beats everything: it is how the host overrides a
	// specific sandbox.
	if p := cmdline.get("microvm.path", ""); p != "" {
		defaults["PATH"] = p
		_ = os.Setenv("PATH", p)
	}

	for k, v := range defaults {
		// Only fill gaps: a value already set came from the image or the host on
		// purpose, and outranks our generic fallback.
		if _, ok := os.LookupEnv(k); ok {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			log.Warn("setenv failed", "key", k, "err", err)
		}
	}

	log.Info("environment ready", "from_image", loaded, "path", os.Getenv("PATH"))
}

// loadImageEnv applies the environment captured at image build time, returning
// how many variables it set.
func loadImageEnv(log *slog.Logger) int {
	raw, err := os.ReadFile(imageEnvPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("could not read image environment", "path", imageEnvPath, "err", err)
		}
		// An image built before this existed, or the minimal base image. The
		// defaults below still give a working shell.
		return 0
	}

	var count int
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || key == "" {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			log.Warn("setenv from image failed", "key", key, "err", err)
			continue
		}
		count++
	}
	return count
}

// setupOverlayRoot stacks a tmpfs over the read-only root and pivots into it.
func setupOverlayRoot(upperSize string) error {
	if err := os.MkdirAll(overlayDir, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", overlayDir, "tmpfs", 0, "size="+upperSize+",mode=755"); err != nil {
		return fmt.Errorf("mount overlay tmpfs: %w", err)
	}

	for _, dir := range []string{overlayDir + "/upper", overlayDir + "/work", newRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	// lowerdir=/ does not pull in the tmpfs at /overlay: overlayfs does not
	// traverse mount points, so it sees only the empty directory beneath it.
	opts := fmt.Sprintf("lowerdir=/,upperdir=%s/upper,workdir=%s/work", overlayDir, overlayDir)
	if err := unix.Mount("overlay", newRoot, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlayfs: %w", err)
	}

	if err := os.MkdirAll(newRoot+oldRoot, 0o755); err != nil {
		return err
	}
	if err := unix.PivotRoot(newRoot, newRoot+oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}

	// Detach the old root lazily. The tmpfs backing upper/work now lives under
	// it, but the overlay mount holds a reference to that superblock, so the
	// writable layer survives the detach.
	if err := unix.Unmount(oldRoot, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	return os.Remove(oldRoot)
}

// mountScratch mounts the optional second drive at the requested path, for
// sandboxes whose writes are too large for the RAM-backed upper layer.
func mountScratch(log *slog.Logger, cmdline kernelCmdline) error {
	dev := cmdline.get("microvm.scratch_dev", "")
	if dev == "" {
		return nil
	}
	target := cmdline.get("microvm.scratch_path", "/workspace")

	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if err := unix.Mount(dev, target, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s at %s: %w", dev, target, err)
	}
	log.Info("mounted scratch drive", "dev", dev, "path", target)
	return nil
}

func doMounts(log *slog.Logger, mounts []mount) error {
	for _, m := range mounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", m.target, err)
		}
		err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data)
		if err == nil {
			continue
		}
		// Re-mounting an already-mounted pseudo-fs is harmless and happens when
		// an image ships its own early mounts.
		if err == unix.EBUSY {
			continue
		}
		if m.optional {
			log.Warn("skipping optional mount", "target", m.target, "err", err)
			continue
		}
		return fmt.Errorf("mount %s at %s: %w", m.fstype, m.target, err)
	}
	return nil
}

// kernelCmdline holds the key=value pairs passed on the boot command line,
// which is how the host parameterises each VM without a config file.
type kernelCmdline map[string]string

func readCmdline() (kernelCmdline, error) {
	raw, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, err
	}
	return parseCmdline(string(raw)), nil
}

func parseCmdline(raw string) kernelCmdline {
	kv := make(kernelCmdline)
	for _, field := range strings.Fields(raw) {
		name, value, found := strings.Cut(field, "=")
		if !found {
			kv[name] = ""
			continue
		}
		kv[name] = value
	}
	return kv
}

func (c kernelCmdline) get(key, fallback string) string {
	if v, ok := c[key]; ok && v != "" {
		return v
	}
	return fallback
}
