// Package runtime defines what a sandbox backend must provide.
//
// The interface exists so the Firecracker backend is not the only possible one:
// a gVisor backend suits a host without KVM, and a plain-container backend makes
// the system developable on a laptop. Callers above this line never learn which
// is in use.
package runtime

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/pablofdezr/microvm/internal/guestclient"
	"github.com/pablofdezr/microvm/internal/protocol"
)

// Runtime creates sandboxes.
type Runtime interface {
	// Create starts a sandbox and returns once its agent is reachable.
	Create(ctx context.Context, spec Spec) (Instance, error)

	// Close releases host-wide resources the runtime set up.
	Close() error
}

// Spec describes a sandbox to create.
type Spec struct {
	// ID is unique per sandbox and appears in the jail path and cgroup name, so
	// it must be filesystem-safe.
	ID string

	// Image names the rootfs to boot, e.g. "python".
	Image string

	VCPUs  int
	MemMiB int

	// Network enables egress. Nil means the sandbox has loopback only, which is
	// the safer default for untrusted code that has no reason to phone home.
	Network bool

	// Env is injected into every exec in this sandbox.
	//
	// It is applied by the host on each exec rather than baked into the guest.
	// The alternative -- writing it into the VM at boot -- would leave the
	// values sitting in the guest's filesystem for anything inside to read long
	// after the exec that needed them, and these are usually credentials.
	Env map[string]string

	// Limits bound this sandbox. They are additionally capped by the runtime's
	// host-wide ceiling, which no spec can raise.
	Limits Limits

	// StorageMount, when non-empty, is the guest path at which the sandbox mounts
	// its object storage as a filesystem. Empty means no storage. The runtime
	// only relays this onto the kernel command line -- the meaning lives in the
	// guest agent and the storage package -- and it belongs in this neutral spec
	// because the runtime already owns both the vsock storage port and the
	// cmdline. Because it lands on a space-separated command line, the runtime
	// must treat it as untrusted and refuse anything that could inject a second
	// kernel parameter.
	StorageMount string

	// StorageReadOnly mounts the storage filesystem read-only in the guest. It is
	// a courtesy that lets the guest fail writes fast; the host enforces the same
	// thing regardless.
	StorageReadOnly bool
}

// Limits are the per-sandbox resource ceilings.
type Limits struct {
	// CPUCores is how many cores' worth of time the sandbox may use. It may be
	// fractional. Zero means the runtime's default.
	CPUCores float64

	// DiskMiB caps the writable layer. The overlay is RAM-backed, so this is
	// really a slice of the memory allowance rather than disk.
	DiskMiB int

	// NetworkBps caps egress and ingress bandwidth, in bytes per second.
	//
	// CPU and memory limits do not touch this: a sandbox pinned to a quarter of
	// a core can still saturate the host's uplink, which is how a hostile one
	// turns a rate limit into somebody else's DDoS. Zero means unlimited.
	NetworkBps int64

	// DiskBps and DiskIOPS cap the VM's block device. A sandbox that thrashes
	// the disk degrades every other tenant on the host, and on a shared box,
	// everything else too. Zero means unlimited.
	DiskBps  int64
	DiskIOPS int64
}

// Stats is a sandbox's resource consumption.
type Stats struct {
	// ActiveCPU is CPU time actually burned. This is the billable number: a
	// sandbox waiting on the network consumes none of it.
	ActiveCPU time.Duration

	// Wall is how long the sandbox has existed.
	Wall time.Duration

	// Idle is Wall minus ActiveCPU: time the sandbox existed but did no work.
	// It is what makes usage-based pricing possible rather than charging for
	// the whole lifetime.
	Idle time.Duration

	MemoryCurrent uint64
	MemoryPeak    uint64

	// NetworkRxBytes and NetworkTxBytes are cumulative transfer, from the
	// GUEST's point of view: Tx is what the sandbox sent out. A backend measuring
	// them on a host-side device has to turn them round, because a host receives
	// what the guest sends -- get it backwards and a sandbox's egress, the number
	// a bandwidth cap and an abuse report are about, lands in the field nobody
	// reads.
	//
	// Nil when the sandbox has no network device at all. That is not zero:
	// "nothing to count" and "counted nothing" are different claims, and only one
	// of them can be billed.
	NetworkRxBytes *uint64
	NetworkTxBytes *uint64
}

// Instance is a running sandbox.
type Instance interface {
	// ID returns the sandbox's identifier.
	ID() string

	// Client talks to the agent inside the guest.
	Client() GuestClient

	// HostListener accepts connections the guest makes *to* the host, and is
	// how a sandbox reaches a host-side service such as object storage.
	//
	// It returns a listener rather than taking a handler because the runtime has
	// no business knowing what gets served on it: the runtime owns the socket's
	// path and permissions, which is a backend detail, and the layer above owns
	// the protocol, which is not.
	//
	// The listener belongs to exactly one sandbox, and that is the whole security
	// model of anything served on it. A connection arriving here came from this
	// sandbox because no other sandbox can reach this socket -- identity is the
	// path, established before the VM booted, rather than anything the guest
	// claims. Never serve two sandboxes from one listener.
	//
	// It returns nil when the backend has no way to offer one, so callers must
	// check. A sandbox with no host listener is a sandbox with no storage, not a
	// broken one.
	HostListener() net.Listener

	// Stats samples the sandbox's meters.
	Stats() (Stats, error)

	// Stop shuts the sandbox down and releases everything it held. It is
	// idempotent, so teardown paths can run unconditionally.
	Stop(ctx context.Context) error
}

// MeterAdopter is an optional Instance capability: restarting the wall-clock
// meter at the moment a task takes the sandbox over.
//
// It exists because a pre-booted sandbox exists before anybody owns it. Stats
// derives Wall from when the VM was minted and Idle from Wall minus CPU burned,
// so a VM that sat in the warm pool for ten minutes reported ten minutes of wall
// and ten minutes of idle against the first tenant to be handed it -- next to a
// created_at stamped at checkout, so the billable numbers were larger than the
// sandbox's whole apparent life. Pool residency is the host's cost, not a
// tenant's.
//
// It is optional rather than part of Instance so a backend with no pool, and every
// test fake, is not made to implement it. Callers type-assert.
type MeterAdopter interface {
	// AdoptMeter restarts the wall-clock and idle meters from now. It is called
	// once, as the sandbox leaves the pool and before any caller can see it.
	AdoptMeter()
}

// GuestClient is what the layers above can ask of the code inside a sandbox.
//
// It is an interface rather than the concrete vsock client for two reasons, and
// the second one is why it is worth the indirection.
//
// The first is the ordinary one: the transport is a detail. Today it is HTTP
// over AF_VSOCK; a container backend would use a pipe, and neither belongs in
// the vocabulary of a package that manages lifetimes.
//
// The second is that returning the concrete client made everything above this
// line untestable. A fake Instance was impossible to write -- Client() had to
// hand back a real *guestclient.Client, which needs a real socket, which needs
// a real VM -- so the sandbox manager and the whole HTTP API could only be
// exercised against KVM. That is a lot of logic reachable only from a
// Raspberry Pi, and it is exactly the logic a unit test should own.
type GuestClient interface {
	// Exec runs a command, calling onFrame for each chunk of output.
	Exec(ctx context.Context, req protocol.ExecRequest, onFrame func(protocol.Frame) error) error

	// Signal delivers a signal to a running exec's process group.
	Signal(ctx context.Context, execID, signal string) error

	// Resize changes a running tty exec's window. It is a no-op-shaped error
	// (guestclient.ErrNotTTY) against an exec with no pseudo-terminal.
	Resize(ctx context.Context, execID string, rows, cols uint16) error

	// WriteFile uploads content into the guest.
	WriteFile(ctx context.Context, path string, content io.Reader, mode string) error

	// ReadFile downloads a file. The caller closes the reader.
	ReadFile(ctx context.Context, path string) (io.ReadCloser, error)

	// Mkdir creates a directory and its parents inside the guest. An existing
	// directory is success; a non-directory in the way is
	// guestclient.ErrNotDirectory.
	Mkdir(ctx context.Context, path string) error
}

// guestclient.Client is the production implementation. Asserting it here means
// a change to either side is a compile error in this package rather than a
// surprise at the one call site that happens to run first.
var _ GuestClient = (*guestclient.Client)(nil)

// SnapshotRef identifies a saved VM snapshot on disk.
type SnapshotRef struct {
	// Dir holds the snapshot's state and memory files.
	Dir string

	// Image, VCPUs and MemMiB are the shape the snapshot was frozen from. A
	// restore only reuses a snapshot into a request of the same shape, since the
	// memory image encodes them.
	Image  string
	VCPUs  int
	MemMiB int

	// Rootfs is the image file the snapshot was captured against, and RootfsID
	// identifies that exact file. A snapshot does not contain its block devices,
	// only references to them, so a restore stages whatever stands at that path
	// today -- and a guest resumed with the page cache and inode state of one
	// filesystem over a different one is silent corruption behind a health probe
	// that passes. The pair is what lets a restore refuse an image that has been
	// rebuilt underneath its template.
	Rootfs   string
	RootfsID string

	// Digest identifies the snapshot. It is bound into the per-restore entropy
	// token (see internal/vmgenid) so a token minted for one snapshot is not
	// reusable against another.
	//
	// It is NOT an integrity check and nothing re-verifies it: no restore
	// re-hashes the files it stages. Tamper-resistance for the snapshot comes from
	// the files themselves being read-only and root-owned, so the unprivileged VMM
	// that maps them cannot write them.
	Digest string
}

// Snapshotter is an optional capability a runtime may implement: freezing a
// running VM to disk and booting fresh VMs from that image. It is a separate
// interface, not part of Runtime, so a backend without it (the container or
// gVisor backends, say) is not forced to fake one -- callers type-assert for it.
//
// Restore is what makes a warm pool cheap: a snapshot loads in tens of
// milliseconds where a cold boot takes hundreds. But a snapshot is a copy of
// RAM, so every VM restored from one starts with identical state, including its
// CSPRNG. Restore therefore MUST reseed the guest's entropy before it returns a
// usable VM, or restored VMs share keys. The digest on SnapshotRef exists for
// exactly that reseed (see internal/vmgenid).
type Snapshotter interface {
	// Snapshot pauses inst, writes a snapshot, and leaves inst stopped. The
	// snapshot must be taken from a VM that has run no untrusted code, so every
	// restore begins from a clean, pristine state.
	Snapshot(ctx context.Context, inst Instance) (SnapshotRef, error)

	// Restore boots a fresh VM from ref under spec's identity, reseeds its
	// entropy, and returns it ready to use.
	Restore(ctx context.Context, spec Spec, ref SnapshotRef) (Instance, error)

	// Discard deletes a snapshot's files. It is idempotent.
	//
	// Someone has to: a snapshot is a full copy of a guest's RAM, so a daemon that
	// captures one template per shape per run and never reclaims them fills the
	// disk it shares with the log store and the object cache -- and leaves a
	// complete guest memory image at rest for as long as the box lives. There is no
	// owner but the caller that asked for the capture, because a SnapshotRef is the
	// only handle that exists.
	Discard(ctx context.Context, ref SnapshotRef) error
}
