//go:build linux

package agent

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"

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
