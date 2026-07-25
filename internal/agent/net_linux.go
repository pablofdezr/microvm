//go:build linux

package agent

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/vishvananda/netlink"
)

// The host allocates each sandbox a /30 on its own TAP device and passes the
// addressing on the kernel command line. Configuring it here in Go rather than
// relying on the kernel's ip= autoconfiguration keeps the guest working with a
// stock kernel that was not built with CONFIG_IP_PNP.
func setupNetwork(log *slog.Logger, cmdline kernelCmdline) error {
	if err := bringUpLoopback(); err != nil {
		// Loopback matters even with no external network: plenty of runtimes
		// and test suites bind to 127.0.0.1.
		return fmt.Errorf("loopback: %w", err)
	}

	cidr := cmdline.get("microvm.ip", "")
	if cidr == "" {
		log.Info("no microvm.ip on cmdline, sandbox has loopback only")
		return nil
	}

	iface := cmdline.get("microvm.iface", "eth0")
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("find %s: %w", iface, err)
	}

	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return fmt.Errorf("parse microvm.ip %q: %w", cidr, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("add %s to %s: %w", cidr, iface, err)
	}

	if mtu := cmdline.get("microvm.mtu", ""); mtu != "" {
		n, err := strconv.Atoi(mtu)
		if err != nil {
			return fmt.Errorf("parse microvm.mtu %q: %w", mtu, err)
		}
		if err := netlink.LinkSetMTU(link, n); err != nil {
			return fmt.Errorf("set mtu on %s: %w", iface, err)
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up %s: %w", iface, err)
	}

	if gw := cmdline.get("microvm.gw", ""); gw != "" {
		gwIP := net.ParseIP(gw)
		if gwIP == nil {
			return fmt.Errorf("parse microvm.gw %q", gw)
		}
		// The default route must be added after the link is up, or the kernel
		// rejects it as unreachable.
		route := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Gw:        gwIP,
			Dst:       nil, // default
		}
		if err := netlink.RouteAdd(route); err != nil {
			return fmt.Errorf("add default route via %s: %w", gw, err)
		}
	}

	if err := writeResolvConf(cmdline.get("microvm.dns", "1.1.1.1,8.8.8.8")); err != nil {
		return fmt.Errorf("resolv.conf: %w", err)
	}

	log.Info("network configured", "iface", iface, "addr", cidr, "gw", cmdline.get("microvm.gw", ""))
	return nil
}

// reconfigureNetwork re-addresses an interface after a snapshot restore.
//
// The snapshot restored the template's address onto this interface, so before the
// new address goes on, the old one and its default route come off -- otherwise the
// guest would answer on both, or reject the new default route as conflicting. Done
// over vsock, which does not depend on the address being right, so it works from
// the wrong address to the right one. It is setupNetwork's counterpart: that one
// configures from the kernel command line at boot, this one from the host after a
// resume, and the steps are the same because the end state is.
func reconfigureNetwork(log *slog.Logger, req protocol.NetConfigRequest) error {
	iface := req.Iface
	if iface == "" {
		iface = "eth0"
	}
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("find %s: %w", iface, err)
	}

	// Off first: the memory image brought the template's address and default route
	// back, and leaving them would collide with what goes on below.
	if err := flushAddresses(link); err != nil {
		return fmt.Errorf("flush addresses on %s: %w", iface, err)
	}
	if err := flushDefaultRoutes(link); err != nil {
		return fmt.Errorf("flush default routes on %s: %w", iface, err)
	}

	if req.IP != "" {
		addr, err := netlink.ParseAddr(req.IP)
		if err != nil {
			return fmt.Errorf("parse ip %q: %w", req.IP, err)
		}
		if err := netlink.AddrAdd(link, addr); err != nil {
			return fmt.Errorf("add %s to %s: %w", req.IP, iface, err)
		}
	}

	if req.MTU > 0 {
		if err := netlink.LinkSetMTU(link, req.MTU); err != nil {
			return fmt.Errorf("set mtu on %s: %w", iface, err)
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up %s: %w", iface, err)
	}

	if req.Gateway != "" {
		gwIP := net.ParseIP(req.Gateway)
		if gwIP == nil {
			return fmt.Errorf("parse gateway %q", req.Gateway)
		}
		// After the link is up, or the kernel rejects the route as unreachable --
		// the same ordering setupNetwork relies on.
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Gw: gwIP, Dst: nil}
		if err := netlink.RouteAdd(route); err != nil {
			return fmt.Errorf("add default route via %s: %w", req.Gateway, err)
		}
	}

	if req.DNS != "" {
		if err := writeResolvConf(req.DNS); err != nil {
			return fmt.Errorf("resolv.conf: %w", err)
		}
	}

	log.Info("network reconfigured after restore", "iface", iface, "addr", req.IP, "gw", req.Gateway)
	return nil
}

// flushAddresses removes every IPv4 address from a link, leaving link-local ones
// the kernel manages. It is what clears the template's address before the
// restore's own goes on.
func flushAddresses(link netlink.Link) error {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for i := range addrs {
		if addrs[i].IP.IsLinkLocalUnicast() {
			continue
		}
		if err := netlink.AddrDel(link, &addrs[i]); err != nil {
			return err
		}
	}
	return nil
}

// flushDefaultRoutes removes the link's default routes, so the restore's gateway
// does not collide with the template's.
func flushDefaultRoutes(link netlink.Link) error {
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for i := range routes {
		if routes[i].Dst != nil {
			continue // not a default route
		}
		if err := netlink.RouteDel(&routes[i]); err != nil {
			return err
		}
	}
	return nil
}

func bringUpLoopback() error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	return netlink.LinkSetUp(lo)
}

func writeResolvConf(servers string) error {
	var b strings.Builder
	for _, s := range strings.Split(servers, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if net.ParseIP(s) == nil {
			return fmt.Errorf("invalid nameserver %q", s)
		}
		fmt.Fprintf(&b, "nameserver %s\n", s)
	}
	if b.Len() == 0 {
		return nil
	}
	// The root is an overlay at this point, so this write lands in the tmpfs
	// upper layer and never touches the shared base image.
	return os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0o644)
}
