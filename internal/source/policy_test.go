package source

import (
	"errors"
	"net/netip"
	"net/url"
	"testing"
)

// Every row here is an entry in the threat model. The metadata address is the
// one that matters most: the daemon reaching it on a caller's behalf, with the
// caller reading the answer out of their own sandbox, is the host's cloud
// identity stolen.
func TestCheckAddr(t *testing.T) {
	tests := []struct {
		name   string
		addr   string
		permit bool
	}{
		{"cloud metadata", "169.254.169.254", false},
		{"cloud metadata in v6 form", "::ffff:169.254.169.254", false},
		{"link-local", "169.254.1.1", false},
		{"loopback", "127.0.0.1", false},
		{"loopback elsewhere in the range", "127.13.9.2", false},
		{"the daemon's own API", "127.0.0.1", false},
		{"v6 loopback", "::1", false},
		{"rfc1918 ten", "10.1.2.3", false},
		{"rfc1918 the sandbox pool", "172.20.0.7", false},
		{"rfc1918 the LAN", "192.168.1.1", false},
		{"cgnat", "100.64.0.1", false},
		{"this network", "0.1.2.3", false},
		{"unspecified v4", "0.0.0.0", false},
		{"unspecified v6", "::", false},
		{"ietf protocol assignments", "192.0.0.8", false},
		{"benchmarking", "198.18.0.1", false},
		{"multicast v4", "224.0.0.1", false},
		{"multicast v6", "ff02::1", false},
		{"reserved", "240.0.0.1", false},
		{"broadcast", "255.255.255.255", false},
		{"unique local v6", "fd00::1", false},
		{"link-local v6", "fe80::1", false},
		{"nat64 of the metadata address", "64:ff9b::a9fe:a9fe", false},
		{"documentation v6", "2001:db8::1", false},
		{"v4-mapped public address", "::ffff:8.8.8.8", false},
		{"interface-local multicast", "ff01::1", false},

		{"a public v4 address", "8.8.8.8", true},
		{"another public v4 address", "93.184.216.34", true},
		{"a public v6 address", "2606:2800:220:1:248:1893:25c8:1946", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("parse %s: %v", tc.addr, err)
			}

			err = checkAddr(addr)
			if tc.permit && err != nil {
				t.Fatalf("checkAddr(%s) refused a permitted address: %v", tc.addr, err)
			}
			if !tc.permit {
				if err == nil {
					t.Fatalf("checkAddr(%s) permitted a blocked address", tc.addr)
				}
				if !errors.Is(err, ErrNotPermitted) {
					t.Fatalf("checkAddr(%s) returned %v, want ErrNotPermitted", tc.addr, err)
				}
			}
		})
	}
}

// A v4-mapped address must be tested as the v4 address it is. Unmapping after
// the v6 rules have already run is how ::ffff:169.254.169.254 reaches the
// metadata service.
func TestCheckAddrRefusesEveryMappedForm(t *testing.T) {
	for _, s := range []string{"::ffff:169.254.169.254", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:8.8.8.8"} {
		addr := netip.MustParseAddr(s)
		if !addr.Is4In6() {
			t.Fatalf("%s is not parsed as v4-mapped; the test no longer tests anything", s)
		}
		if err := checkAddr(addr); err == nil {
			t.Errorf("checkAddr(%s) permitted a v4-mapped address", s)
		}
	}
}

func TestCheckAddrRefusesInvalidAndScoped(t *testing.T) {
	if err := checkAddr(netip.Addr{}); err == nil {
		t.Error("the zero address was permitted")
	}
	if err := checkAddr(netip.MustParseAddr("2606:2800:220::1%eth0")); err == nil {
		t.Error("an interface-scoped address was permitted")
	}
}

func TestHostAllowlist(t *testing.T) {
	tests := []struct {
		name  string
		allow []string
		host  string
		want  bool
	}{
		{"empty allowlist allows nothing", nil, "github.com", false},
		{"empty allowlist allows nothing public either", nil, "example.com", false},
		{"exact match", []string{"github.com"}, "github.com", true},
		{"exact match is not a suffix match", []string{"github.com"}, "evilgithub.com", false},
		{"exact match does not cover subdomains", []string{"github.com"}, "codeload.github.com", false},
		{"suffix entry covers a subdomain", []string{".githubusercontent.com"}, "objects.githubusercontent.com", true},
		{"suffix entry covers a deep subdomain", []string{".githubusercontent.com"}, "a.b.githubusercontent.com", true},
		{"suffix entry does not cover the bare domain", []string{".githubusercontent.com"}, "githubusercontent.com", false},
		{"suffix entry is not a substring match", []string{".example.com"}, "notexample.com", false},
		{"case is ignored", []string{"github.com"}, "GitHub.com", true},
		{"a trailing dot is the same name", []string{"github.com"}, "github.com.", true},
		{"one of several entries", []string{"github.com", ".githubusercontent.com"}, "codeload.githubusercontent.com", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := New(Config{AllowHosts: tc.allow})
			if err != nil {
				t.Fatal(err)
			}
			if got := f.hostAllowed(canonicalHost(tc.host)); got != tc.want {
				t.Errorf("hostAllowed(%q) with allowlist %v = %v, want %v",
					tc.host, tc.allow, got, tc.want)
			}
		})
	}
}

func TestCheckURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"https on an allowlisted host", "https://github.com/o/r/archive/main.tar.gz", true},
		{"https with a query", "https://github.com/x?sig=abc", true},
		{"https on a non-default port", "https://github.com:8443/x", true},

		{"plain http", "http://github.com/x", false},
		{"file", "file:///etc/shadow", false},
		{"gopher, for protocol smuggling", "gopher://github.com/x", false},
		{"ftp", "ftp://github.com/x", false},
		{"data", "data:text/plain,hello", false},
		{"git's ext transport, which is command execution", "ext::sh -c cat%20/etc/shadow", false},
		{"no scheme", "github.com/x", false},
		{"no host", "https:///x", false},
		{"a host that is not allowlisted", "https://evil.example/x", false},
		{"the metadata address directly", "https://169.254.169.254/latest/meta-data/", false},
		{"userinfo, which is a credential in a URL", "https://token@github.com/x", false},
	}

	f, err := New(Config{AllowHosts: []string{"github.com"}})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.url)
			if err != nil {
				if tc.want {
					t.Fatalf("parse %q: %v", tc.url, err)
				}
				return
			}

			err = f.checkURL(u)
			if tc.want && err != nil {
				t.Fatalf("checkURL(%q) refused: %v", tc.url, err)
			}
			if !tc.want {
				if err == nil {
					t.Fatalf("checkURL(%q) permitted it", tc.url)
				}
				if !errors.Is(err, ErrNotPermitted) {
					t.Fatalf("checkURL(%q) returned %v, want ErrNotPermitted", tc.url, err)
				}
			}
		})
	}
}

// A presigned URL is a bearer credential in its query string, so nothing that
// leaves this package may carry one.
func TestRedact(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://github.com/o/r/archive/main.tar.gz", "https://github.com/o/r/archive/main.tar.gz"},
		{"https://bucket.s3.amazonaws.com/x.tgz?X-Amz-Signature=deadbeef", "https://bucket.s3.amazonaws.com/x.tgz"},
		{"https://token:x@github.com/o/r.git", "https://github.com/o/r.git"},
		{"https://github.com/x#frag", "https://github.com/x"},
		{"https://github.com/x?", "https://github.com/x"},
		{"not a url", ""},
		{"", ""},
	}

	for _, tc := range tests {
		if got := Redact(tc.in); got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
