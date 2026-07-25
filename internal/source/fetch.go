package source

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// Fetcher downloads archives from allowlisted hosts. Safe for concurrent use.
type Fetcher struct {
	cfg        Config
	allowHosts []string
	client     *http.Client

	// lookup resolves a hostname. A field rather than a call so the resolved set
	// can be driven from a test: the interesting cases are a name answering with
	// the metadata address and a name answering with a mix, and neither is
	// something to arrange with real DNS.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)

	// permitLoopback and tlsConfig exist for the tests in this package and have
	// no exported setter, deliberately. httptest listens on 127.0.0.1 with a
	// self-signed certificate, both of which the policy refuses, and a test that
	// needs the public network is a test that does not run in CI. The default
	// path -- loopback refused, system roots -- is what the daemon gets, and
	// TestLoopbackRefusedOnTheDialPath asserts it.
	permitLoopback bool
	tlsConfig      *tls.Config
}

// New validates the configuration and returns a Fetcher.
//
// An empty allowlist is not an error: it is the off state, and every fetch is
// refused until an operator names a host.
func New(cfg Config) (*Fetcher, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}

	f := &Fetcher{cfg: cfg}
	for _, h := range cfg.AllowHosts {
		if h = canonicalHost(h); h != "" {
			f.allowHosts = append(f.allowHosts, h)
		}
	}
	f.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}

	f.client = &http.Client{
		CheckRedirect: f.checkRedirect,
		Transport: &http.Transport{
			DialContext: f.dialContext,
			// No proxy, ever, and not the environment's either. A proxy is a host
			// that never saw the allowlist and an address the dialler was never
			// asked to vet: HTTPS_PROXY=http://169.254.169.254:80 in the daemon's
			// environment would otherwise be a complete bypass of this file.
			Proxy: nil,
			// One connection per fetch. A pooled connection outlives the check that
			// approved it, which is the rebinding window again in a slower form.
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 10 * time.Second,
			DialTLSContext:      f.dialTLSContext,
		},
	}
	return f, nil
}

// WithTimeout bounds a whole seed operation with the configured timeout.
//
// Fetch applies the same bound to itself, so a caller who forgets this is still
// covered for the download. What only this covers is fetch *and* expand under
// one deadline, which is the honest limit: an origin that stalls a body and an
// archive that yields one member a second hold a slot the same way.
func (f *Fetcher) WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, f.cfg.Timeout)
}

// Fetch downloads rawURL into a buffered archive. The caller must Close the
// result, which is what releases the buffer.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Archive, error) {
	if len(f.allowHosts) == 0 {
		return nil, notPermitted("no source host is allowlisted")
	}

	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, notPermitted("URL is not parseable")
	}
	if err := f.checkURL(u); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, f.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, notPermitted("URL cannot be requested")
	}
	// Ask for the bytes as they are. Transparent gzip would decompress the body
	// underneath the counting reader, which is the compressed cap measuring the
	// wrong number.
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "microvmd")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The status is quoted because the host is one the operator chose to
		// expose. Nothing from the body or the headers is, ever: that is the
		// caller reading an internal service through us.
		return nil, fetchFailed("origin answered %s", resp.Status)
	}
	// Content-Length is a claim, so it may refuse early but may never permit.
	if resp.ContentLength > f.cfg.MaxBytes {
		return nil, tooLarge("body declares %d bytes, over the %d byte limit",
			resp.ContentLength, f.cfg.MaxBytes)
	}

	return f.buffer(resp.Body)
}

// buffer copies the body into an unlinked temp file.
//
// Unlinked as soon as it is created, so the buffer has no name for the whole
// time it holds attacker-supplied bytes: nothing can be raced onto the path,
// nothing is left behind by a crash between here and Close, and no host path
// derived from this fetch exists to be confused with a member name.
func (f *Fetcher) buffer(body io.Reader) (*Archive, error) {
	tmp, err := os.CreateTemp(f.cfg.TempDir, "microvm-source-*")
	if err != nil {
		return nil, fmt.Errorf("buffer source: %w", err)
	}
	if err := os.Remove(tmp.Name()); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("buffer source: %w", err)
	}

	n, err := io.Copy(tmp, &cappedReader{r: body, cap: f.cfg.MaxBytes, what: "body"})
	if err != nil {
		tmp.Close()
		if isCapExceeded(err) {
			return nil, err
		}
		return nil, transportError(err)
	}
	if n == 0 {
		tmp.Close()
		return nil, fetchFailed("origin returned an empty body")
	}

	return &Archive{buf: tmp, size: n, cfg: f.cfg}, nil
}

// checkRedirect re-runs the URL policy on every hop.
//
// The address check happens on each hop's dial regardless, since it lives in the
// dialler. This is the allowlist and the scheme, which a redirect would
// otherwise be a way around: a host that answers 302 Location: http://internal
// gets to choose where we go next.
func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fetchFailed("more than %d redirects", maxRedirects)
	}
	return f.checkURL(req.URL)
}

func (f *Fetcher) dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := f.dialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		conn.Close()
		return nil, notPermitted("address %q is unusable", addr)
	}

	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if f.tlsConfig != nil {
		cfg = f.tlsConfig.Clone()
		cfg.ServerName = host
	}

	// The certificate is verified against the name in the URL even though the
	// connection was made to a literal address: dialling a vetted IP must not
	// become a way to skip the check that the IP is who it claims to be.
	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// dialContext resolves, vets, and then connects to a vetted address.
//
// The name is never handed to the transport's own dialler. Resolving, checking,
// and then dialling the hostname is the DNS-rebinding hole: the second lookup is
// free to answer with the metadata address, and by then nothing is looking.
func (f *Fetcher) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, notPermitted("address %q is unusable", addr)
	}

	resolved, err := f.resolve(ctx, canonicalHost(host))
	if err != nil {
		return nil, err
	}

	// Drop the addresses that fail the policy rather than failing the whole dial
	// on the first one. A public host with one bad answer in a round-robin set is
	// still reachable, and a host with nothing but bad answers is refused below.
	var permitted []netip.Addr
	for _, a := range resolved {
		if f.addrPermitted(a) == nil {
			permitted = append(permitted, a)
		}
	}
	if len(permitted) == 0 {
		return nil, notPermitted("host %q resolves to no permitted address", host)
	}

	dialer := net.Dialer{
		Timeout: 10 * time.Second,
		// The only check on the syscall path. Everything above it is advisory by
		// comparison: this one sees the sockaddr the kernel is about to connect
		// to, after resolution, so there is no interval in which the answer can
		// change behind it.
		Control: f.control,
	}

	var lastErr error
	for _, a := range permitted {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if err != nil {
			lastErr = err
			continue
		}
		// Third check, and the cheapest: whatever we are actually connected to.
		if remote, ok := netip.ParseAddrPort(conn.RemoteAddr().String()); ok == nil {
			if err := f.addrPermitted(remote.Addr()); err != nil {
				conn.Close()
				return nil, err
			}
		}
		return conn, nil
	}
	return nil, lastErr
}

// resolve turns a host into addresses. An IP literal is used as written, without
// a lookup -- there is nothing to resolve, and the policy applies to it exactly
// the same.
func (f *Fetcher) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	addrs, err := f.lookup(ctx, host)
	if err != nil {
		// Uniform with every other pre-flight refusal. "No such host" and "blocked
		// address" told apart is a caller enumerating the operator's network.
		return nil, notPermitted("host %q could not be resolved", host)
	}
	if len(addrs) == 0 {
		return nil, notPermitted("host %q resolved to nothing", host)
	}
	return addrs, nil
}

// control is net.Dialer.Control: the last thing to run before connect.
func (f *Fetcher) control(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return notPermitted("address %q is unusable", address)
	}
	return f.addrPermitted(ap.Addr())
}

// addrPermitted applies the address policy, with the test-only loopback
// exemption. Every check in this file goes through here so the exemption cannot
// be honoured by one of them and forgotten by another.
func (f *Fetcher) addrPermitted(addr netip.Addr) error {
	if f.permitLoopback && addr.Unmap().IsLoopback() {
		return nil
	}
	return checkAddr(addr)
}

// transportError collapses a failed request. A timeout is named because it is
// the one thing an operator can act on; everything else is deliberately vague
// about whether the host refused, vanished, or was never permitted.
func transportError(err error) error {
	// A decision made by the dialler or the redirect check must keep its class.
	// Both arrive here wrapped in *url.Error, and downgrading a refusal to a fetch
	// failure would answer 502 to what is the caller's own invalid request.
	if own := unwrapSentinel(err); own != nil {
		return own
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return fetchFailed("timed out")
	}
	return fetchFailed("request failed")
}
