package source

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Type is which fetcher a source needs. The wire values, so the API hands one
// straight over and nothing translates between two spellings of "tarball".
type Type string

const (
	TypeTarball Type = "tarball"
	TypeGit     Type = "git"
)

// Request is one seed, as a caller asked for it.
//
// The per-type fields are validated for shape before they arrive -- a request
// that sets `ref` on a tarball is refused by the API naming the field, rather
// than having it quietly dropped -- so what is left to decide here is whether
// the fetch is permitted at all.
type Request struct {
	Type Type
	URL  string

	// StripComponents belongs to a tarball. See Options.
	StripComponents int

	// Ref, Depth and CredentialRef belong to a git clone. Ref is a branch, a tag
	// or a full commit SHA, and empty means the remote's default branch.
	Ref           string
	Depth         int
	CredentialRef string
}

// Result is what a seed turned out to be, in the terms the API reports back.
//
// The URL is redacted in here rather than by whoever reports it. A presigned URL
// is a bearer credential in its query string, and a field that arrives already
// safe cannot be published unsafely by the next caller who forgets.
type Result struct {
	Type        Type
	URLRedacted string

	// Ref is what was asked for and Commit is what was resolved. Both empty for a
	// tarball. Keeping them separate is the point: a branch names a moving target,
	// and only the commit says the same thing tomorrow.
	Ref    string
	Commit string

	// CredentialRef is the operator credential used, by name. Never the secret --
	// it never was one, it is the name of one.
	CredentialRef string

	Files int
	Dirs  int
	Bytes int64
}

// Prepared is a source fetched onto the host and validated, with nothing written
// anywhere yet.
//
// Two phases rather than one because the caller has a decision to take in
// between. A sandbox's writable layer is smaller than this package's own caps,
// and the honest place to refuse a tree that will not fit is before the VM that
// cannot hold it has booted -- so Manifest is readable before Write is called.
//
// Close releases the buffer or the checkout, and must be called on every path.
type Prepared interface {
	// Manifest is what Write would produce, validated in full.
	Manifest() Manifest
	// Write puts the whole tree into w, or returns an error having written a
	// prefix of it. Validation is over by now: what can still fail here is the
	// destination.
	Write(ctx context.Context, w Writer) error
	// Result describes the seed for reporting.
	Result() Result
	Close() error
}

// Preparer is this package as the layer above uses it.
//
// An interface because the implementation needs the network and an operator's
// allowlist, while what sits on top of it -- the order a create does things in,
// what it tears down when a seed fails, how the seed is metered -- needs neither
// and is exactly the part a test has to be able to reach.
type Preparer interface {
	Prepare(ctx context.Context, req Request) (Prepared, error)
}

// Seeder prepares seeds of either type. Safe for concurrent use.
type Seeder struct {
	cfg     Config
	fetcher *Fetcher
	gitPath string

	// gitNote says why gitPath is empty, for the operator. Never for a caller: a
	// git source is refused in the same words as an unallowlisted host, because a
	// caller can act on neither.
	gitNote string

	// insecure relaxes the loopback refusal and TLS verification, for the tests
	// in this package and nothing else -- there is no exported setter, exactly as
	// in fetch.go. A git server a test can run listens on 127.0.0.1 with a
	// self-signed certificate, and the daemon refuses both.
	insecure bool
}

var _ Preparer = (*Seeder)(nil)

// NewSeeder validates the configuration and returns a Seeder.
//
// An empty allowlist is not an error: it is the off state, and every seed is
// refused until an operator names a host.
func NewSeeder(cfg Config) (*Seeder, error) {
	f, err := New(cfg)
	if err != nil {
		return nil, err
	}
	for name, cred := range cfg.Credentials {
		// The prefix a credential is bound to, checked where the operator is
		// watching. A prefix that matches nothing would otherwise be a credential
		// that silently never resolves.
		if _, err := ParseCredentialPrefix(cred.URLPrefix); err != nil {
			return nil, fmt.Errorf("credential %q: %w", name, err)
		}
	}

	// f.cfg, not cfg: the defaults were applied to the fetcher's copy, and the
	// caps this package enforces on a git checkout have to be the same numbers.
	s := &Seeder{cfg: f.cfg, fetcher: f, gitPath: cfg.GitPath}
	if s.gitPath == "" {
		// Resolved once, at startup. A host with no git cannot serve a git source,
		// and the operator should learn that from a log line at boot rather than
		// from a caller's failed create.
		s.gitPath, _ = exec.LookPath("git")
	}
	if s.gitPath == "" {
		s.gitNote = "no git on PATH"
		return s, nil
	}
	if err := gitPinsAddresses(s.gitPath); err != nil {
		// Refused rather than run without the pins. The pin is the address half of
		// the policy on this path, and a git that ignores it does its own DNS --
		// which is the rebinding window the tarball path was built to close: a public
		// answer for the pre-flight check, the metadata address for git's own lookup
		// a moment later. There is no safe degraded mode, so there is no degraded
		// mode.
		s.gitPath, s.gitNote = "", err.Error()
	}
	return s, nil
}

// gitVersionForResolvePins is the first git that understands
// http.curloptResolve, which is how pinsFor binds git to an address the policy
// approved.
//
// The floor exists because an unrecognised -c http.* key is accepted and ignored:
// on an older git the pins do nothing and nothing says so. Debian 11 ships 2.30.2,
// which is exactly the wrong side of it.
var gitVersionForResolvePins = [2]int{2, 31}

// gitPinsAddresses reports whether this git will honour the address pins.
func gitPinsAddresses(gitPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitVersionTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, gitPath, "--version").Output()
	if err != nil {
		return fmt.Errorf("%s --version failed, so it cannot be checked for the "+
			"http.curloptResolve support the address policy needs", gitPath)
	}
	major, minor, ok := parseGitVersion(string(out))
	if !ok {
		return fmt.Errorf("%s --version is unreadable (%q), so it cannot be checked for the "+
			"http.curloptResolve support the address policy needs", gitPath, strings.TrimSpace(string(out)))
	}
	if major < gitVersionForResolvePins[0] ||
		major == gitVersionForResolvePins[0] && minor < gitVersionForResolvePins[1] {
		return fmt.Errorf("git %d.%d is older than %d.%d and ignores http.curloptResolve, "+
			"so a clone would do its own DNS and reach an address the policy refused",
			major, minor, gitVersionForResolvePins[0], gitVersionForResolvePins[1])
	}
	return nil
}

// gitVersionTimeout bounds the one subprocess this package runs at startup. A git
// that does not answer `--version` promptly is a git nothing should wait for.
const gitVersionTimeout = 5 * time.Second

// parseGitVersion reads the major and minor out of `git --version` output, which
// is "git version 2.39.5 (Apple Git-154)" and variations on it.
func parseGitVersion(out string) (major, minor int, ok bool) {
	fields := strings.Fields(out)
	for _, f := range fields {
		parts := strings.SplitN(f, ".", 3)
		if len(parts) < 2 {
			continue
		}
		major, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		minor, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		return major, minor, true
	}
	return 0, 0, false
}

// GitPath is the git binary this Seeder will use, or empty when a git source is
// refused on this host.
func (s *Seeder) GitPath() string { return s.gitPath }

// GitNote says why GitPath is empty, for a startup log line. Empty when git is
// usable.
func (s *Seeder) GitNote() string { return s.gitNote }

// Prepare fetches a source onto the host and validates it, writing nothing.
func (s *Seeder) Prepare(ctx context.Context, req Request) (Prepared, error) {
	if len(s.fetcher.allowHosts) == 0 {
		return nil, notPermitted("no source host is allowlisted")
	}

	// One deadline over the whole preparation and none over the Write that
	// follows, because that is where the line between the two actually falls:
	// everything a third party controls happens in here, and once this returns the
	// seed is bytes on the host's own disk with nothing left to wait for.
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	switch req.Type {
	case TypeTarball:
		return s.prepareTarball(ctx, req)
	case TypeGit:
		return s.prepareGit(ctx, req)
	default:
		return nil, notPermitted("source type %q is not supported", req.Type)
	}
}

func (s *Seeder) prepareTarball(ctx context.Context, req Request) (Prepared, error) {
	arc, err := s.fetcher.Fetch(ctx, req.URL)
	if err != nil {
		return nil, err
	}

	opts := Options{StripComponents: req.StripComponents}
	man, err := arc.Validate(opts)
	if err != nil {
		arc.Close()
		return nil, err
	}
	return &preparedArchive{
		arc:  arc,
		opts: opts,
		man:  *man,
		res:  Result{Type: TypeTarball, URLRedacted: Redact(req.URL)},
	}, nil
}

// preparedArchive is a buffered tarball, validated and not yet expanded.
type preparedArchive struct {
	arc  *Archive
	opts Options
	man  Manifest
	res  Result
}

func (p *preparedArchive) Manifest() Manifest { return p.man }

func (p *preparedArchive) Write(ctx context.Context, w Writer) error {
	_, err := p.arc.Expand(ctx, p.opts, w)
	return err
}

func (p *preparedArchive) Result() Result {
	res := p.res
	res.Files, res.Dirs, res.Bytes = p.man.Files, p.man.Dirs, p.man.Bytes
	return res
}

func (p *preparedArchive) Close() error { return p.arc.Close() }
