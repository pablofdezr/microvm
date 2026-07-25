// Package source fetches a project archive from a caller-supplied URL and
// expands it, so a sandbox can be seeded in one request instead of one request
// per file.
//
// The fetch is the dangerous half, and it is the reason this package exists as
// a package. "Fetch this URL for me" hands a caller an outbound request made by
// the daemon, and the daemon runs *outside* the firewall it installs for its
// guests: internal/netpool blocks RFC1918 and 169.254.169.254 for a sandbox and
// protects nothing here. Since the caller can read the result out of their own
// sandbox afterwards, an unfiltered fetcher is a full SSRF read primitive --
// the host's LAN, the daemon's own API on loopback, and the cloud metadata
// service that holds the host's identity.
//
// So the address filter is the feature and the tar reader is the plumbing. Two
// independent checks stand in front of every connection: the hostname is
// resolved here and each resolved address is vetted before any dial, and the
// vetted address is then re-checked inside net.Dialer.Control, which runs after
// resolution and before connect on the syscall path itself. The first check
// alone has a DNS-rebinding window -- the transport's own second lookup can
// answer differently -- and the second alone would give up the ability to skip
// one bad address in an otherwise fine answer. Together there is no window.
//
// Expansion never builds a host path from a member name. The body is buffered
// into one unlinked temp file, walked once to validate every member and again to
// stream each member to its destination, so host-side path traversal is not
// mitigated here -- it is absent.
//
// Nothing in here imports the daemon. Every case in the threat model is
// reachable from a unit test, and a control exercised only by a live deployment
// is a control nobody has tested.
package source

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The four failure classes, as sentinels, because the layers above owe them
// different answers and the difference is a security boundary:
//
//   - ErrNotPermitted is every refusal decided before the request leaves this
//     host. The caller is told one thing in one wording, because "blocked
//     address", "not allowlisted" and "no such host" are otherwise a port
//     scanner. The detail is wrapped for the log, never for the reply.
//   - ErrFetchFailed is the origin answering badly once it is allowlisted. Here
//     detail is safe: the reachable set is the operator's choice.
//   - ErrTooLarge is a cap the operator set. Separate from a fetch failure
//     because it is the archive that is wrong, not the origin.
//   - ErrInvalidArchive is a member this package refuses to expand.
var (
	ErrNotPermitted   = errors.New("source not permitted")
	ErrFetchFailed    = errors.New("source fetch failed")
	ErrTooLarge       = errors.New("source too large")
	ErrInvalidArchive = errors.New("source archive rejected")
)

func notPermitted(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotPermitted, fmt.Sprintf(format, args...))
}

func fetchFailed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrFetchFailed, fmt.Sprintf(format, args...))
}

func tooLarge(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrTooLarge, fmt.Sprintf(format, args...))
}

func invalidArchive(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArchive, fmt.Sprintf(format, args...))
}

// Config configures a Fetcher.
//
// A zero cap means the package default, not "unbounded". An unset limit that
// disables the limit is the one default nobody wants: it turns a mis-wired flag
// into an unmetered download onto the host's disk.
type Config struct {
	// AllowHosts is the operator's allowlist. An entry is either an exact host
	// ("codeload.github.com") or a leading-dot suffix (".githubusercontent.com")
	// matching subdomains of it but not the bare domain itself -- an operator who
	// wants both names says both.
	//
	// Empty allows nothing, so the capability is off until a host is named. That
	// is deliberate: a fetcher whose only control is the address blocklist can
	// still reach every public host on the internet, which is not a feature an
	// operator should get by upgrading.
	AllowHosts []string

	// MaxBytes caps the compressed body, counted as it arrives. Content-Length is
	// a claim and is only ever used to refuse early, never to permit.
	MaxBytes int64

	// MaxExpandedBytes caps the total expanded size, so a 10 KB archive cannot
	// become 100 GB. Enforced on member headers during the validating walk, which
	// trips before anything is written rather than while it is.
	MaxExpandedBytes int64

	// MaxFileBytes caps a single member. Set it to the destination's own per-file
	// limit: without it, an archive under the total cap can still contain one
	// member the destination will refuse, which fails a write halfway through an
	// expansion that had already validated.
	MaxFileBytes int64

	// MaxFiles caps the member count. Separate from the byte caps because a 10 KB
	// tar of a million empty files passes both of them.
	MaxFiles int

	// Timeout bounds the fetch. Use WithTimeout to bound fetch and expand
	// together, which is the limit that matters: a hung origin and an archive
	// that yields a member a second hold a slot equally well.
	Timeout time.Duration

	// TempDir is where the body is buffered. Empty means the system default.
	TempDir string

	// Credentials maps a credential_ref to the credential it names, for a private
	// git repository. The caller sends the name; the secret never leaves this host,
	// never enters the URL, and never enters the guest.
	//
	// Empty resolves nothing, so every credential_ref is refused. Same rule as the
	// allowlist: a capability an operator did not configure is off.
	Credentials map[string]Credential

	// GitPath is the git binary to use. Empty asks PATH once, when the Seeder is
	// built -- a host with no git cannot serve a git source, and the operator
	// should learn that from a line at startup rather than from a caller's create.
	GitPath string
}

// Credential is one operator credential and the repositories it may be spent on.
//
// The scope is not optional, and it is the whole reason this is a struct rather
// than a string. A credential resolved by name alone is a confused deputy: every
// caller shares the operator's map, so any of them could name any credential on
// any allowlisted URL. That is two holes, not one. It spends the operator's token
// on repositories the caller was never given -- git authenticates the clone and
// the working tree lands in the caller's own sandbox, which they can read -- and it
// hands the token itself to whatever server answers, because git sends its
// credential to any host that challenges for one, and an allowlist entry is a
// suffix a caller may be able to choose a name under.
//
// Binding the credential to a prefix closes both, and it closes the enumeration
// oracle with them: a name that exists but does not cover this URL is refused in
// the same words as a name that does not exist, so neither the names nor the
// repositories behind them can be probed.
type Credential struct {
	// URLPrefix is the only place this credential may be sent. It is an https URL
	// naming a host and as much of a path as the operator wants to pin
	// ("https://github.com/acme-corp/"), and it matches a request URL on the same
	// host whose path is that path or below it -- at a component boundary, so
	// ".../acme" does not cover ".../acmecorp".
	URLPrefix string

	// Secret is the token, or "username:token" for a forge that wants a real
	// username.
	Secret string
}

// Package defaults. Conservative on purpose: they are what a caller gets when a
// flag was never wired up, so they should refuse a hostile archive, not admit
// one.
const (
	defaultMaxBytes         = 64 << 20  // 64 MiB compressed
	defaultMaxExpandedBytes = 256 << 20 // 256 MiB expanded
	defaultMaxFileBytes     = 64 << 20  // 64 MiB per member
	defaultMaxFiles         = 20_000
	defaultTimeout          = 60 * time.Second
)

// maxRedirects caps a redirect chain. A constant, not a flag: Go's default of
// 10 is a knob nobody needs, a loop hits the cap, and the whole operation sits
// under Timeout anyway.
const maxRedirects = 5

// MaxStripComponents mirrors the spec's bound on strip_components. Past this it
// is not a checkout layout, it is a typo that silently empties an archive.
//
// Exported because the API refuses an out-of-range value as a bad parameter,
// which is a better answer than the 502 an archive rejection becomes -- and a
// second copy of the number would eventually disagree with this one.
const MaxStripComponents = 8

func (c *Config) applyDefaults() error {
	if c.MaxBytes < 0 || c.MaxExpandedBytes < 0 || c.MaxFileBytes < 0 || c.MaxFiles < 0 || c.Timeout < 0 {
		return errors.New("source limits must not be negative")
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = defaultMaxBytes
	}
	if c.MaxExpandedBytes == 0 {
		c.MaxExpandedBytes = defaultMaxExpandedBytes
	}
	if c.MaxFileBytes == 0 {
		c.MaxFileBytes = defaultMaxFileBytes
	}
	if c.MaxFiles == 0 {
		c.MaxFiles = defaultMaxFiles
	}
	if c.Timeout == 0 {
		c.Timeout = defaultTimeout
	}
	return nil
}

// Redact strips a URL down to what is safe to store and hand back.
//
// Userinfo and the query both go. A presigned URL *is* a bearer credential in
// its query string, so echoing one into a log line, an error, or a sandbox's
// stored source is publishing it. An unparseable URL redacts to nothing rather
// than to itself.
func Redact(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}
