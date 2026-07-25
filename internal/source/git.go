package source

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The git half. It shells out to git rather than speaking the protocol, because
// a shallow, authenticated clone of one ref is a great deal of protocol and git
// already has all of it right.
//
// Shelling out is also where a credential leaks, so three rules hold and each of
// them is load-bearing:
//
//   - The secret never enters argv. /proc/<pid>/cmdline is world-readable, so an
//     unprivileged process anywhere on the host can read the arguments of a root
//     process; /proc/<pid>/environ is readable by the owner alone. That is the
//     whole reason for the askpass indirection.
//   - The secret never enters the URL. git quotes the remote back in its own
//     error messages, and those reach a log.
//   - git's environment is built from nothing rather than inherited. An
//     https_proxy in the daemon's environment would route the clone through a
//     host that never saw the allowlist and that no pin binds -- the same hole
//     Transport.Proxy = nil closes on the tarball path -- and a GIT_* or a
//     ~/.gitconfig insteadOf rule can rewrite the URL after it was checked.
//
// The address policy is enforced too, which git would otherwise decide for
// itself: see pinsFor.

// gitDir is the checkout's own metadata, which is never written into a guest.
const gitDir = ".git"

// defaultGitUsername is who a bare token authenticates as. It is what GitHub
// expects for a PAT over HTTPS and what every other forge ignores; an operator
// whose host needs a real username writes "username:secret" in the credential
// file instead.
const defaultGitUsername = "x-access-token"

// maxGitPins caps how many resolved addresses are pinned. A round-robin answer
// with a hundred entries is a hundred arguments, and git tries them in order
// anyway.
const maxGitPins = 4

// gitWaitDelay bounds how long a killed git may keep its output pipes open.
//
// It is what makes the deadline real. Killing git does not kill git-remote-https,
// which is the process holding the socket, and that grandchild inherits the
// pipes -- so Wait would sit in its output-copying goroutines until curl's own
// connect timeout, minutes past a deadline that had already fired. WaitDelay
// abandons the I/O instead.
const gitWaitDelay = 2 * time.Second

// maxGitStderr bounds how much of git's complaint is kept. Enough for the line
// that says what went wrong, not enough for a hostile remote to write a log file
// for us.
const maxGitStderr = 4 << 10

// Environment variables the askpass helper reads. The helper holds no secret --
// it holds these names.
const (
	envGitUsername = "MICROVM_GIT_USERNAME"
	envGitPassword = "MICROVM_GIT_PASSWORD"
)

// askpassScript answers git's credential prompts from the environment.
//
// LC_ALL=C is set for the run, so the prompt git passes as $1 is the English one
// this matches on. A prompt that matches neither case falls through to the
// password, which is the only prompt a clone reaches with credential.username
// already configured.
const askpassScript = `#!/bin/sh
# Written by microvmd for one clone. It holds no secret: the values are in the
# environment, which only this user can read, and never in argv, which anyone can.
case "$1" in
*sername*) printf %s "$MICROVM_GIT_USERNAME" ;;
*) printf %s "$MICROVM_GIT_PASSWORD" ;;
esac
`

func (s *Seeder) prepareGit(ctx context.Context, req Request) (Prepared, error) {
	if s.gitPath == "" {
		// This host has no git. Refused as "not permitted" like an unallowlisted
		// host, because it is the same kind of fact: what this daemon will fetch is
		// the operator's decision, and a caller cannot act on either.
		return nil, notPermitted("git sources are unavailable on this host")
	}

	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil {
		return nil, notPermitted("URL is not parseable")
	}
	// Scheme, userinfo and the allowlist, exactly as the tarball path checks them.
	// https only is also what makes it safe to hand the URL to git as an argument
	// without a "--" guard: it cannot begin with a dash.
	if err := s.fetcher.checkURL(u); err != nil {
		return nil, err
	}

	cred, err := s.credential(req.CredentialRef, u)
	if err != nil {
		return nil, err
	}
	pins, err := s.pinsFor(ctx, u)
	if err != nil {
		return nil, err
	}

	// The parent holds the askpass helper and git's HOME; the checkout is a
	// subdirectory of it, so nothing the helper needs can be mistaken for part of
	// the project.
	tmp, err := os.MkdirTemp(s.cfg.TempDir, "microvm-git-*")
	if err != nil {
		return nil, fmt.Errorf("prepare a git checkout: %w", err)
	}
	done := false
	defer func() {
		if !done {
			os.RemoveAll(tmp)
		}
	}()

	g := &gitRun{
		git:      s.gitPath,
		tmp:      tmp,
		checkout: filepath.Join(tmp, "repo"),
		askpass:  filepath.Join(tmp, "askpass"),
		cred:     cred,
		pins:     pins,
		insecure: s.insecure,
		limit:    s.cfg.MaxBytes + s.cfg.MaxExpandedBytes,
	}
	if cred.set {
		if err := os.WriteFile(g.askpass, []byte(askpassScript), 0o700); err != nil {
			return nil, fmt.Errorf("prepare a git credential: %w", err)
		}
	}

	if err := g.clone(ctx, u, req); err != nil {
		return nil, err
	}
	commit, err := g.head(ctx)
	if err != nil {
		return nil, err
	}
	man, err := walkCheckout(g.checkout, s.cfg)
	if err != nil {
		return nil, err
	}

	done = true
	return &preparedDir{
		root: g.checkout,
		tmp:  tmp,
		man:  *man,
		res: Result{
			Type:          TypeGit,
			URLRedacted:   Redact(req.URL),
			Ref:           req.Ref,
			Commit:        commit,
			CredentialRef: cred.name,
		},
	}, nil
}

// gitCredential is one resolved operator credential.
type gitCredential struct {
	set      bool
	name     string
	username string
	secret   string
	// raw is the value as the operator wrote it, kept only so scrub can remove
	// that form too: a secret holding a colon is split into a username and a
	// password, and the half before the colon is not a secret this would otherwise
	// recognise on its way into an error.
	raw string
}

// credential resolves a caller's credential_ref against the operator's map, for
// the URL it is about to be spent on.
//
// One refusal for three different facts -- no such name, a name with nothing
// behind it, and a name that does not cover this URL -- because the differences
// are the operator's configuration and a caller who could tell them apart could
// map it: which names exist, and which repositories each one reaches. The URL
// check is the security boundary and not a convenience: see Credential.
func (s *Seeder) credential(ref string, u *url.URL) (gitCredential, error) {
	if ref == "" {
		return gitCredential{}, nil
	}
	refused := notPermitted("credential %q may not be used for this URL", ref)

	cred, ok := s.cfg.Credentials[ref]
	if !ok || cred.Secret == "" {
		return gitCredential{}, refused
	}
	if !credentialCovers(cred.URLPrefix, u) {
		return gitCredential{}, refused
	}

	c := gitCredential{
		set: true, name: ref, username: defaultGitUsername,
		secret: cred.Secret, raw: cred.Secret,
	}
	if user, pass, found := strings.Cut(cred.Secret, ":"); found {
		c.username, c.secret = user, pass
	}
	return c, nil
}

// credentialCovers reports whether a credential's prefix reaches a URL.
//
// Host equality and then the path, at a component boundary: a prefix of
// "/acme" must not cover "/acmecorp/anything", which is the whole difference
// between pinning a credential to an organisation and pinning it to a name that
// organisation happens to start with. A prefix already validated by
// ParseCredentialPrefix, so an unparseable one here refuses rather than matches.
func credentialCovers(prefix string, u *url.URL) bool {
	p, err := ParseCredentialPrefix(prefix)
	if err != nil {
		return false
	}
	if authority(p) != authority(u) {
		return false
	}
	path, want := u.EscapedPath(), p.EscapedPath()
	switch {
	case want == "" || want == "/":
		return true
	case path == want:
		return true
	case strings.HasSuffix(want, "/"):
		return strings.HasPrefix(path, want)
	default:
		return strings.HasPrefix(path, want+"/")
	}
}

// authority is a URL's host and port with the default filled in, so a prefix
// written without ":443" still covers a URL that spells it out.
func authority(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return canonicalHost(u.Hostname()) + ":" + port
}

// ParseCredentialPrefix validates the URL prefix a credential is bound to.
//
// Exported because the operator's flag is parsed where the flags are, and a
// prefix that would match nothing should stop the daemon at startup rather than
// refuse a create hours later. The rules are the ones checkURL applies to a
// request: https, a host, and no credential smuggled into the userinfo. A query
// or a fragment is refused too -- neither takes part in the match, so one here is
// an operator believing something that is not true.
func ParseCredentialPrefix(prefix string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(prefix))
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL", prefix)
	}
	switch {
	case u.Scheme != "https" || u.Opaque != "":
		return nil, fmt.Errorf("%q is not an https URL", prefix)
	case u.User != nil:
		return nil, fmt.Errorf("%q carries userinfo, and a credential is what this flag is for", prefix)
	case canonicalHost(u.Hostname()) == "":
		return nil, fmt.Errorf("%q names no host", prefix)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, fmt.Errorf("%q has a query or a fragment, which take no part in the match", prefix)
	}
	return u, nil
}

// pinsFor vets where the URL's host resolves and pins git to the addresses that
// pass.
//
// This is the address half of the policy, which the tarball path enforces inside
// its own dialler and git would otherwise decide for itself -- the daemon runs
// outside the firewall it installs for guests, so "git resolves it" means "git
// may connect to 169.254.169.254 for a caller". Every permitted address is handed
// over as a pre-resolved answer (CURLOPT_RESOLVE, which git exposes as
// http.curloptResolve), so no lookup happens inside git at all: a host whose DNS
// answers with a blocked address, or rebinds to one a moment later, gets no
// socket. The hostname is still what TLS is verified against, because a pin
// replaces the lookup and nothing else.
func (s *Seeder) pinsFor(ctx context.Context, u *url.URL) ([]string, error) {
	host := canonicalHost(u.Hostname())
	addrs, err := s.fetcher.resolve(ctx, host)
	if err != nil {
		return nil, err
	}

	port := u.Port()
	if port == "" {
		port = "443"
	}

	var pins []string
	for _, a := range addrs {
		if s.fetcher.addrPermitted(a) != nil {
			continue
		}
		literal := a.String()
		if a.Unmap().Is6() {
			// curl's resolve syntax brackets a v6 literal, since the entry is
			// colon-separated and so is the address.
			literal = "[" + literal + "]"
		}
		pins = append(pins, host+":"+port+":"+literal)
		if len(pins) == maxGitPins {
			break
		}
	}
	if len(pins) == 0 {
		return nil, notPermitted("host %q resolves to no permitted address", host)
	}
	return pins, nil
}

// gitRun is one checkout's worth of git invocations.
type gitRun struct {
	git      string
	tmp      string
	checkout string
	askpass  string
	cred     gitCredential
	pins     []string
	insecure bool

	// limit is the most this clone may write under tmp, and overLimit records that
	// it did. See watchDisk.
	limit     int64
	overLimit atomic.Bool
}

// How the clone's footprint on the host is bounded. git has no option for it: no
// --max-bytes, and neither --depth 1 nor a filter says anything about how large
// the one commit it fetches is.
const (
	// diskCheckInterval is how often the clone is measured. Often enough that a
	// gigabit link cannot put much past the limit before the next look, rarely
	// enough that walking a checkout of the allowed size costs nothing next to the
	// clone itself.
	diskCheckInterval = 500 * time.Millisecond
	// diskCheckFirst is the first look, brought forward so a tiny limit is not
	// approximately no limit for half a second.
	diskCheckFirst = 50 * time.Millisecond
)

// clone fetches exactly one ref, shallowly.
//
// A full commit SHA cannot be a --branch, so it takes the other route: clone
// names a ref and fetch names an object. Both end with a working tree at the
// requested commit and no history worth speaking of.
func (g *gitRun) clone(ctx context.Context, u *url.URL, req Request) error {
	// Bounded from here to the last invocation, which is the window in which git
	// writes: the packfile, then the working tree it expands to.
	ctx, stopWatching := g.watchDisk(ctx)
	defer stopWatching()

	depth := req.Depth
	if depth < 1 {
		// The sandbox needs a working tree; the commits behind it are bytes
		// nothing in there will read.
		depth = 1
	}
	remote := u.String()

	if isCommitSHA(req.Ref) {
		if _, err := g.run(ctx, g.tmp, "init", "--quiet", g.checkout); err != nil {
			return err
		}
		if _, err := g.run(ctx, g.checkout, "remote", "add", "origin", remote); err != nil {
			return err
		}
		if _, err := g.run(ctx, g.checkout, "fetch", "--quiet",
			"--depth", strconv.Itoa(depth), "origin", req.Ref); err != nil {
			return err
		}
		_, err := g.run(ctx, g.checkout, "checkout", "--quiet", "FETCH_HEAD")
		return err
	}

	args := []string{"clone", "--quiet",
		"--depth", strconv.Itoa(depth),
		"--single-branch", "--no-tags",
		// A .gitmodules pointing at file:// or ext:: is somebody else's URL
		// evaluated on our host. The protocol allowlist below refuses both, and
		// not recursing means the question never arises.
		"--no-recurse-submodules",
	}
	if req.Ref != "" {
		args = append(args, "--branch", req.Ref)
	}
	args = append(args, remote, g.checkout)

	_, err := g.run(ctx, g.tmp, args...)
	return err
}

// watchDisk cancels the clone when it has written more to this host than it may.
//
// This is the git half of -source-max-bytes and -source-max-expanded-bytes, and
// without it neither applies here at all: those caps live in the HTTP fetcher's
// counting reader and in the walk over an archive's headers, and a clone goes
// through neither. What bounded it before was -source-timeout alone -- sixty
// seconds of a fast link, written into a directory that is a tmpfs on a good many
// distributions, and only then measured by walkCheckout and refused. Measured
// while it happens rather than after, because "the caps were satisfied" is worth
// nothing once the disk is gone.
//
// The limit covers the whole temporary directory, so it is the sum of the two
// caps: the packfile is what the compressed cap bounds on the tarball path, and
// the tree it expands to is what the expanded cap bounds. walkCheckout still
// applies the expanded cap exactly, on the tree alone.
func (g *gitRun) watchDisk(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if g.limit <= 0 {
		return ctx, cancel
	}

	stop := make(chan struct{})
	go func() {
		timer := time.NewTimer(diskCheckFirst)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-timer.C:
				if dirSize(g.tmp) > g.limit {
					// Recorded before the cancel, so run cannot read the two in the
					// other order and report a timeout for a cap.
					g.overLimit.Store(true)
					cancel()
					return
				}
				timer.Reset(diskCheckInterval)
			}
		}
	}()
	return ctx, func() {
		close(stop)
		cancel()
	}
}

// dirSize is what a directory tree occupies, as far as this needs to know.
//
// Apparent size rather than blocks, and errors are ignored: git is writing
// underneath this walk, so a file that vanishes between readdir and stat is
// normal, and the next tick sees the tree as it is by then.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// head is the commit that was actually checked out. A branch moves and a tag can
// be moved under you; this is the only answer that means the same thing
// tomorrow.
func (g *gitRun) head(ctx context.Context) (string, error) {
	out, err := g.run(ctx, g.checkout, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// run invokes git with the safety settings and a built-from-nothing environment.
func (g *gitRun) run(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, g.git, append(g.settings(), args...)...)
	cmd.Dir = cwd
	cmd.Env = g.environ()
	cmd.WaitDelay = gitWaitDelay
	// Killing git does not kill git-remote-https, which is the process holding the
	// socket and doing the downloading. See reapWithProcess.
	reapWithProcess(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &cappedWriter{w: &stdout, remaining: maxGitStderr}
	cmd.Stderr = &cappedWriter{w: &stderr, remaining: maxGitStderr}

	if err := cmd.Run(); err != nil {
		if g.overLimit.Load() {
			return "", tooLarge("the clone wrote more than %d bytes to this host", g.limit)
		}
		if ctx.Err() != nil {
			return "", fetchFailed("git %s timed out", args[0])
		}
		// The detail is the origin's, which the operator chose to allow, so it is
		// safe to quote -- and it is the only thing that tells "no such ref" from
		// "authentication failed". Scrubbed all the same: the secret is not in argv
		// and not in the URL, and a defence that costs one string replacement is
		// worth having anyway.
		return "", fetchFailed("git %s failed: %s", args[0], g.scrub(lastLine(stderr.String())))
	}
	return stdout.String(), nil
}

// settings are the -c overrides every invocation carries. None of them is a
// secret, which is why they may live in argv at all.
func (g *gitRun) settings() []string {
	args := []string{
		// https and nothing else. git's ext:: is command execution rather than a
		// transport, and file:// would read the host's own disk.
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		// A redirect is the remote choosing where we go next, and the allowlist
		// never saw the new host. The tarball path re-checks every hop; git cannot
		// be made to, so it does not get to follow one.
		"-c", "http.followRedirects=false",
		// No helper is consulted. One configured on this host would hand its
		// credential to whatever repository a caller named.
		"-c", "credential.helper=",
		"-c", "advice.detachedHead=false",
	}
	for _, pin := range g.pins {
		args = append(args, "-c", "http.curloptResolve="+pin)
	}
	if g.cred.set {
		// With the username known, the askpass helper is asked for the password
		// alone.
		args = append(args, "-c", "credential.username="+g.cred.username)
	}
	if g.insecure {
		args = append(args, "-c", "http.sslVerify=false")
	}
	return args
}

// environ is git's whole environment. Nothing is inherited: see the file
// comment.
func (g *gitRun) environ() []string {
	env := []string{
		// git finds its own subcommands through its exec-path, not this. It is here
		// for the askpass helper's interpreter.
		"PATH=/usr/local/bin:/usr/bin:/bin",
		// Not the daemon's home: a ~/.gitconfig with an insteadOf rule would
		// rewrite the URL after the allowlist approved it.
		"HOME=" + g.tmp,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		// A prompt would block on a terminal nobody is watching until the deadline
		// expires.
		"GIT_TERMINAL_PROMPT=0",
		// The askpass helper matches on git's own prompt text.
		"LC_ALL=C",
	}
	if g.cred.set {
		env = append(env,
			"GIT_ASKPASS="+g.askpass,
			envGitUsername+"="+g.cred.username,
			envGitPassword+"="+g.cred.secret)
	}
	return env
}

// scrub removes the credential from anything on its way to a caller or a log.
//
// Both forms, because "username:token" is split before it gets here and the half
// in front of the colon is then not a string this would recognise -- so a secret
// that happens to contain one would have had its first half quoted verbatim into a
// 502.
func (g *gitRun) scrub(s string) string {
	if !g.cred.set {
		return s
	}
	s = strings.ReplaceAll(s, g.cred.raw, "[redacted]")
	return strings.ReplaceAll(s, g.cred.secret, "[redacted]")
}

// lastLine is the part of git's complaint worth repeating: its progress and
// advice come first and the reason comes last.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return "no output"
}

// isCommitSHA reports whether ref is a full object name, which is the one kind
// of ref clone --branch cannot take. An abbreviated SHA is deliberately not one:
// a remote cannot be asked for a prefix, so treating it as a branch name and
// letting git say there is no such ref is the honest answer.
func isCommitSHA(ref string) bool {
	if len(ref) != 40 && len(ref) != 64 {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		hex := c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		if !hex {
			return false
		}
	}
	return true
}

// cappedWriter keeps the first n bytes and silently drops the rest. Unlike
// cappedReader it does not fail at its cap: this bounds what we keep of git's
// chatter, and a remote that talks too much has not done anything wrong.
type cappedWriter struct {
	w         io.Writer
	remaining int
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.remaining <= 0 {
		return len(p), nil
	}
	keep := p
	if len(keep) > c.remaining {
		keep = keep[:c.remaining]
	}
	n, err := c.w.Write(keep)
	c.remaining -= n
	if err != nil {
		return n, err
	}
	return len(p), nil
}

// walkCheckout validates a checkout and reports what it would write.
//
// The same rules as an archive member and the same sentinels, because the
// destination is the same one: no link, no device or socket, no .git, and the
// operator's caps on count and size. It runs before anything is written, which is
// what lets the git path make the tarball path's promise -- nothing reaches the
// sandbox until the whole tree is known good.
func walkCheckout(root string, cfg Config) (*Manifest, error) {
	man := &Manifest{}

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Through the same validation an archive member gets. Nothing here can
		// contain ".." -- these names came from readdir -- and running them through
		// it anyway means one function decides what a destination path may be.
		name, err := memberPath(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		if name == "" {
			return nil
		}

		if d.Name() == gitDir {
			// The history is not the project: nothing in the sandbox will read it, it
			// is most of the bytes, and it holds the remote the clone used. Skipped
			// whether it is a directory or the file a linked worktree leaves -- both
			// are metadata, and only one of them is a directory.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			man.Dirs++
			man.Entries = append(man.Entries, Entry{Path: name, Dir: true})
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			// Refused rather than skipped, exactly as in an archive: the guest file
			// endpoints cannot express a link, and a checkout silently missing one is
			// a broken project somebody debugs for an hour.
			return invalidArchive("%q is %s, which a source may not contain", elide(name), kindOf(info.Mode()))
		}

		man.Files++
		if man.Files > cfg.MaxFiles {
			return tooLarge("the checkout holds more than %d files", cfg.MaxFiles)
		}
		if info.Size() > cfg.MaxFileBytes {
			return tooLarge("%q is %d bytes, over the %d byte per-file limit",
				elide(name), info.Size(), cfg.MaxFileBytes)
		}
		man.Bytes += info.Size()
		if man.Bytes > cfg.MaxExpandedBytes {
			return tooLarge("the checkout is larger than the %d byte limit", cfg.MaxExpandedBytes)
		}

		mode := "0644"
		if info.Mode().Perm()&0o111 != 0 {
			mode = "0755"
		}
		man.Entries = append(man.Entries, Entry{Path: name, Size: info.Size(), Mode: mode})
		return nil
	})
	if err != nil {
		if unwrapSentinel(err) != nil {
			return nil, err
		}
		return nil, fmt.Errorf("read the git checkout: %w", err)
	}
	if len(man.Entries) == 0 {
		return nil, invalidArchive("the checkout holds no files or directories")
	}
	return man, nil
}

// kindOf names what a non-regular file is, for the refusal that quotes it.
func kindOf(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "a symlink"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&fs.ModeDevice != 0:
		return "a device"
	default:
		return "not a regular file"
	}
}

// preparedDir is a validated checkout on the host's disk, not yet written.
type preparedDir struct {
	root string
	tmp  string
	man  Manifest
	res  Result
}

func (p *preparedDir) Manifest() Manifest { return p.man }

func (p *preparedDir) Result() Result {
	res := p.res
	res.Files, res.Dirs, res.Bytes = p.man.Files, p.man.Dirs, p.man.Bytes
	return res
}

// Close removes the whole checkout, credential helper and all.
func (p *preparedDir) Close() error { return os.RemoveAll(p.tmp) }

// Write copies the checkout into the destination in the order it was validated,
// so a write that fails halfway has written a prefix of a known list.
func (p *preparedDir) Write(ctx context.Context, w Writer) error {
	standalone := standaloneDirs(p.man.Entries)

	for _, e := range p.man.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Dir {
			// Every other directory is created by the write of the file inside it.
			if !standalone[e.Path] {
				continue
			}
			if err := w.Mkdir(ctx, e.Path); err != nil {
				return fmt.Errorf("create %s: %w", e.Path, err)
			}
			continue
		}

		f, err := os.Open(filepath.Join(p.root, filepath.FromSlash(e.Path)))
		if err != nil {
			return fmt.Errorf("read %s: %w", e.Path, err)
		}
		// Bounded by the size the manifest was validated at, which is the number the
		// destination's own limits were checked against.
		err = w.WriteFile(ctx, e.Path, io.LimitReader(f, e.Size), e.Mode)
		f.Close()
		if err != nil {
			return fmt.Errorf("write %s: %w", e.Path, err)
		}
	}
	return nil
}
