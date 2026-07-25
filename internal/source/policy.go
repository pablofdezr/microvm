package source

import (
	"net/netip"
	"net/url"
	"strings"
)

// blockedPrefixes are the destinations the daemon must never be talked into
// reaching on a caller's behalf.
//
// This is the same list as the blocked4/blocked6 sets in
// internal/netpool/firewall_linux.go, and the parity test in that package fails
// if it stops being. The two exist separately because they act at different
// layers -- nftables filters the guest, this filters the daemon's own socket --
// but they must describe the same set, or a destination closed to a sandbox is
// open to the process that boots it.
//
// The v6 half is not decoration. ::ffff:169.254.169.254 is the metadata service
// written in v6 form, and 64:ff9b::/96 translates straight to a blocked v4
// destination on any host with NAT64.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC1918 private
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT: the ISP's own infrastructure
	netip.MustParsePrefix("127.0.0.0/8"),    // host loopback, and the daemon's own API
	netip.MustParsePrefix("169.254.0.0/16"), // link-local, and cloud metadata at 169.254.169.254
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC1918 private, and where the sandbox pool lives
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("192.168.0.0/16"), // RFC1918 private: the LAN this host sits on
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking range
	netip.MustParsePrefix("224.0.0.0/4"),    // multicast
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved

	netip.MustParsePrefix("::/128"),        // unspecified
	netip.MustParsePrefix("::1/128"),       // loopback
	netip.MustParsePrefix("::ffff:0:0/96"), // IPv4-mapped
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64 well-known prefix
	netip.MustParsePrefix("fc00::/7"),      // unique local
	netip.MustParsePrefix("fe80::/10"),     // link-local
	netip.MustParsePrefix("2001:db8::/32"), // documentation range
	netip.MustParsePrefix("ff00::/8"),      // multicast
}

// BlockedPrefixes returns the daemon-side blocklist. Exported for the parity
// test in internal/netpool, which is the only thing that should read it: the
// decision "may I connect here" is checkAddr's, not a caller's.
func BlockedPrefixes() []netip.Prefix {
	out := make([]netip.Prefix, len(blockedPrefixes))
	copy(out, blockedPrefixes)
	return out
}

// checkAddr decides whether the daemon may open a connection to an address.
//
// Deny by default, twice over: the standard-library predicates and the explicit
// prefix list. They overlap heavily and that is the point -- the predicates
// catch a class the list forgot, the list catches ranges (CGNAT, reserved, the
// documentation prefixes) that no predicate names.
//
// Is4In6 is refused before Unmap rather than after. An address only arrives in
// mapped form because someone wrote it that way, and the mapped form is exactly
// how a blocked v4 target gets tested against the v6 rules and passes.
func checkAddr(addr netip.Addr) error {
	if !addr.IsValid() {
		return notPermitted("invalid address")
	}
	if addr.Is4In6() {
		return notPermitted("address %s is IPv4-mapped", addr)
	}
	if addr.Zone() != "" {
		// A zone only means anything for an address scoped to one link, which is
		// not a destination reached over the public internet.
		return notPermitted("address %s is scoped to an interface", addr)
	}

	a := addr.Unmap()
	switch {
	case a.IsLoopback(),
		a.IsUnspecified(),
		a.IsPrivate(),
		a.IsMulticast(),
		a.IsInterfaceLocalMulticast(),
		a.IsLinkLocalUnicast(),
		a.IsLinkLocalMulticast(),
		!a.IsGlobalUnicast():
		return notPermitted("address %s is not a permitted destination", addr)
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return notPermitted("address %s is in blocked range %s", addr, p)
		}
	}
	return nil
}

// checkURL vets a URL before anything is resolved or dialled. It runs on the
// original URL and again on every redirect hop: an allowlist checked once is an
// allowlist a 302 walks around.
func (f *Fetcher) checkURL(u *url.URL) error {
	// https only. Not http: a seed fetched in clear text is code a path attacker
	// rewrites before it runs. Everything else -- file:, data:, ftp:, gopher: for
	// protocol smuggling, and git's ext:: which is command execution rather than a
	// transport -- fails the same check, including the opaque forms, which have no
	// host at all.
	if u.Scheme != "https" || u.Opaque != "" {
		return notPermitted("scheme %q is not https", u.Scheme)
	}
	if u.User != nil {
		// Credentials belong in an operator-configured credential, not in a URL
		// that lands in ps output, an access log and every error message.
		return notPermitted("URL carries userinfo")
	}

	host := canonicalHost(u.Hostname())
	if host == "" {
		return notPermitted("URL has no host")
	}
	if !f.hostAllowed(host) {
		return notPermitted("host %q is not allowlisted", host)
	}
	return nil
}

// hostAllowed matches a host against the operator's allowlist.
func (f *Fetcher) hostAllowed(host string) bool {
	for _, entry := range f.allowHosts {
		if strings.HasPrefix(entry, ".") {
			if strings.HasSuffix(host, entry) {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}

// canonicalHost normalises a host for comparison. DNS is case-insensitive and
// treats a trailing dot as the same name, so "GitHub.com." must not slip past an
// allowlist entry of "github.com".
func canonicalHost(host string) string {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	// A v6 literal may arrive with brackets depending on how it was taken apart.
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	return host
}
