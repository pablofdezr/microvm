//go:build linux

package netpool

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/vishvananda/netlink"
)

// TapManager creates and destroys the host-side TAP devices that back each
// sandbox's virtual NIC.
type TapManager struct {
	log   *slog.Logger
	owner int
	group int
}

// NewTapManager returns a manager.
//
// Creating TAP devices requires CAP_NET_ADMIN, so the daemon must hold it; the
// sandboxes never do. owner and group are the ids the VMM will have dropped to
// by the time it opens the device -- see Create for why that matters. Pass 0 to
// leave the device owned by root, which is only correct when the VMM also runs
// as root.
func NewTapManager(log *slog.Logger, owner, group int) *TapManager {
	return &TapManager{log: log, owner: owner, group: group}
}

// Create brings up the TAP device for a lease, addressed as the guest's gateway.
//
// The device is created before Firecracker starts, because Firecracker attaches
// to an existing interface rather than making one.
func (m *TapManager) Create(lease Lease) error {
	// A device left behind by a crashed daemon would otherwise make this fail
	// with EEXIST, and worse, could carry stale addresses or routes.
	if err := m.Delete(lease.TapName); err != nil {
		return fmt.Errorf("clear stale tap %s: %w", lease.TapName, err)
	}

	link := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: lease.TapName},
		Mode:      netlink.TUNTAP_MODE_TAP,

		// These flags must match exactly what Firecracker passes to TUNSETIFF
		// when it opens the device (IFF_TAP|IFF_NO_PI|IFF_VNET_HDR). The kernel
		// rejects an open whose IFF_MULTI_QUEUE bit disagrees with how the
		// device was created, with a bare EINVAL that surfaces from Firecracker
		// as "Invalid TUN/TAP Backend" and says nothing about which flag.
		//
		// Setting them explicitly is not optional tidiness: leaving Flags zero
		// makes netlink pick a default based on Queues, and any non-zero Queues
		// selects TUNTAP_MULTI_QUEUE_DEFAULTS -- which adds IFF_MULTI_QUEUE and
		// breaks every boot.
		Flags: netlink.TUNTAP_VNET_HDR | netlink.TUNTAP_NO_PI,
		// Queues 0 keeps netlink on its single-queue path. A sandbox has one
		// vCPU's worth of traffic, so multiqueue would cost memory per VM and
		// buy no throughput even if Firecracker asked for it.
		Queues: 0,

		// The device is created here by a privileged daemon, but it is opened
		// later by a Firecracker that the jailer has already dropped to an
		// unprivileged uid. Attaching to a TAP needs either CAP_NET_ADMIN or
		// ownership of the device, and the jailed VMM has neither unless we
		// grant it here -- the open fails with a bare EPERM that Firecracker
		// reports as "Invalid TUN/TAP Backend", naming no permission at all.
		//
		// This is the cost of dropping privileges properly, and the fix belongs
		// here rather than by giving the VMM back CAP_NET_ADMIN.
		Owner: uint32(m.owner),
		Group: uint32(m.group),
	}

	if err := netlink.LinkAdd(link); err != nil {
		return fmt.Errorf("create tap %s: %w", lease.TapName, err)
	}

	addr, err := netlink.ParseAddr(lease.HostCIDR())
	if err != nil {
		// Undo the half-built device rather than leave an unaddressed TAP that
		// the firewall matches but nothing routes.
		_ = m.Delete(lease.TapName)
		return fmt.Errorf("parse host address %s: %w", lease.HostCIDR(), err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		_ = m.Delete(lease.TapName)
		return fmt.Errorf("address tap %s with %s: %w", lease.TapName, lease.HostCIDR(), err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		_ = m.Delete(lease.TapName)
		return fmt.Errorf("bring up tap %s: %w", lease.TapName, err)
	}

	m.log.Debug("tap created", "name", lease.TapName, "host", lease.HostIP, "guest", lease.GuestIP)
	return nil
}

// Delete removes a TAP device. A device that does not exist is not an error, so
// teardown paths can run unconditionally.
func (m *TapManager) Delete(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("look up %s: %w", name, err)
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("delete %s: %w", name, err)
	}
	m.log.Debug("tap deleted", "name", name)
	return nil
}

// CleanupOrphans removes every sandbox TAP device on the host.
//
// Called at daemon start: a crash leaves TAP devices behind, and those devices
// hold addresses from the pool. Reclaiming them keeps a restart from colliding
// with its own past.
func (m *TapManager) CleanupOrphans() (int, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return 0, fmt.Errorf("list links: %w", err)
	}

	var removed int
	for _, link := range links {
		name := link.Attrs().Name
		if !isSandboxTap(name) {
			continue
		}
		if err := m.Delete(name); err != nil {
			// Keep going: one stuck device should not block the daemon from
			// reclaiming the rest.
			m.log.Warn("could not remove orphaned tap", "name", name, "err", err)
			continue
		}
		removed++
	}

	if removed > 0 {
		m.log.Info("removed orphaned taps from a previous run", "count", removed)
	}
	return removed, nil
}
