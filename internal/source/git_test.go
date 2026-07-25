package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The git path is tested against a real git talking to a real server, because
// every interesting thing about it is a property of git's behaviour and not of
// ours: that a shallow clone leaves a working tree, that a credential reaches the
// remote through the askpass helper, that .git is not part of the project. A fake
// git would assert our idea of git.
//
// The server is git's own CGI backend over TLS on loopback, which the daemon's
// policy refuses on both counts -- hence the same unexported test exemptions
// fetch.go uses, and hence TestGitRefusesAnUnallowlistedHost asserting the
// default answer.

const (
	testGitUser  = "ci-bot"
	testGitToken = "s3cr3t-token-value"
)

// gitOrSkip finds git, or skips: a host without it serves no git source, which is
// a supported configuration rather than a broken one.
func gitOrSkip(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH, so the git source path cannot be exercised here")
	}
	return git
}

// gitBackendOrSkip finds git-http-backend, which is what makes a real clone
// possible without a network.
func gitBackendOrSkip(t *testing.T, git string) string {
	t.Helper()
	out, err := exec.Command(git, "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path failed: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("no git-http-backend at %s", backend)
	}
	return backend
}

// mustGit runs one git command in the test's own fixture, with none of the
// production safety flags: this is building a repository, not fetching one.
func mustGit(t *testing.T, git, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(git, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitRepo is a served repository and the commit at its tip.
type gitRepo struct {
	url    string
	commit string
	// prefix is a URL prefix that covers this repository, for binding a credential
	// to it.
	prefix string
	// requests counts what the server was asked for, so a test can tell a clone
	// that happened from one that was refused before a socket was opened.
	requests *int
}

// newGitRepo builds a repository with an executable file, a nested directory and
// a .git of its own, then serves it over HTTPS. requireAuth turns the server into
// a private remote.
func newGitRepo(t *testing.T, git string, requireAuth bool) gitRepo {
	t.Helper()
	backend := gitBackendOrSkip(t, git)

	root := t.TempDir()
	work := filepath.Join(root, "work")

	mustGit(t, git, root, "init", "--quiet", work)
	mustGit(t, git, work, "checkout", "--quiet", "-b", "main")
	writeFixture(t, filepath.Join(work, "main.go"), "package main\n", 0o644)
	writeFixture(t, filepath.Join(work, "run.sh"), "#!/bin/sh\necho hi\n", 0o755)
	writeFixture(t, filepath.Join(work, "src", "lib.go"), "package lib\n", 0o644)
	mustGit(t, git, work, "add", ".")
	mustGit(t, git, work, "commit", "--quiet", "-m", "first")
	commit := mustGit(t, git, work, "rev-parse", "HEAD")

	// A bare copy to serve: git refuses to fetch a non-bare repository's checked-out
	// branch over http without extra configuration.
	mustGit(t, git, root, "clone", "--bare", "--quiet", work, "repo.git")

	requests := 0
	handler := http.Handler(&cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requireAuth {
			user, pass, ok := r.BasicAuth()
			if !ok {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if user != testGitUser || pass != testGitToken {
				http.Error(w, "bad credential", http.StatusForbidden)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	return gitRepo{
		url:      srv.URL + "/repo.git",
		commit:   commit,
		prefix:   srv.URL + "/repo.git",
		requests: &requests,
	}
}

func writeFixture(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareAGitCheckout(t *testing.T) {
	git := gitOrSkip(t)
	repo := newGitRepo(t, git, false)
	s := newTestSeeder(t, nil, Config{AllowHosts: []string{"127.0.0.1"}})

	prepared, err := s.Prepare(context.Background(), Request{
		Type: TypeGit,
		URL:  repo.url,
		Ref:  "main",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer prepared.Close()

	res := prepared.Result()
	if res.Commit != repo.commit {
		t.Errorf("Commit = %q, want %q: the resolved commit is the only field that means the same thing tomorrow",
			res.Commit, repo.commit)
	}
	if res.Ref != "main" {
		t.Errorf("Ref = %q, want main", res.Ref)
	}

	var w recordingWriter
	if err := prepared.Write(context.Background(), &w); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := w.files["main.go"]; got.content != "package main\n" {
		t.Errorf("main.go = %q", got.content)
	}
	if got := w.files["src/lib.go"]; got.content != "package lib\n" {
		t.Errorf("src/lib.go = %q", got.content)
	}
	if got := w.files["run.sh"]; got.mode != "0755" {
		t.Errorf("run.sh mode = %q, want 0755: an executable that arrives unexecutable needs a chmod nobody knows to run", got.mode)
	}
	for path := range w.files {
		if strings.HasPrefix(path, gitDir+"/") {
			t.Errorf("the history was written into the sandbox: %s", path)
		}
	}
	// The manifest is what the caller checked its sandbox against, so it has to be
	// what was written.
	if man := prepared.Manifest(); man.Files != len(w.files) {
		t.Errorf("manifest promised %d files and %d were written", man.Files, len(w.files))
	}
}

// A full SHA cannot be a --branch, so it takes the other route. It is also the
// only ref worth recommending, which makes it worth its own test.
func TestPrepareAGitCheckoutAtACommit(t *testing.T) {
	git := gitOrSkip(t)
	repo := newGitRepo(t, git, false)
	s := newTestSeeder(t, nil, Config{AllowHosts: []string{"127.0.0.1"}})

	prepared, err := s.Prepare(context.Background(), Request{
		Type: TypeGit,
		URL:  repo.url,
		Ref:  repo.commit,
	})
	if err != nil {
		t.Fatalf("Prepare at a commit: %v", err)
	}
	defer prepared.Close()

	if got := prepared.Result().Commit; got != repo.commit {
		t.Errorf("Commit = %q, want %q", got, repo.commit)
	}
	if prepared.Manifest().Files != 3 {
		t.Errorf("Files = %d, want 3", prepared.Manifest().Files)
	}
}

// The credential test, and the reason the whole git path exists: the token
// reaches the remote, and it does so without ever being an argument to git.
func TestAGitCredentialReachesTheRemoteAndNotArgv(t *testing.T) {
	git := gitOrSkip(t)
	repo := newGitRepo(t, git, true)

	s := newTestSeeder(t, nil, Config{
		AllowHosts: []string{"127.0.0.1"},
		Credentials: map[string]Credential{"forge": {
			URLPrefix: repo.prefix,
			Secret:    testGitUser + ":" + testGitToken,
		}},
	})

	prepared, err := s.Prepare(context.Background(), Request{
		Type:          TypeGit,
		URL:           repo.url,
		CredentialRef: "forge",
	})
	if err != nil {
		t.Fatalf("Prepare of a private repository: %v", err)
	}
	defer prepared.Close()

	if got := prepared.Result().CredentialRef; got != "forge" {
		t.Errorf("CredentialRef = %q, want forge: the name is reported, and only the name", got)
	}

	// The secret is in the environment, which only this user can read, and in no
	// argument: /proc/<pid>/cmdline is world-readable, so an argument is a token
	// published to every process on the host.
	g := &gitRun{tmp: t.TempDir(), cred: gitCredential{
		set: true, name: "forge", username: testGitUser, secret: testGitToken,
	}}
	for _, arg := range g.settings() {
		if strings.Contains(arg, testGitToken) {
			t.Fatalf("the credential is in argv: %q", arg)
		}
	}
	var inEnv bool
	for _, kv := range g.environ() {
		if strings.Contains(kv, testGitToken) {
			inEnv = strings.HasPrefix(kv, envGitPassword+"=")
		}
	}
	if !inEnv {
		t.Error("the credential is not in git's environment either, so nothing can authenticate")
	}
	if strings.Contains(askpassScript, testGitToken) {
		t.Error("the askpass helper holds the secret; it must hold only the variable's name")
	}
}

func TestGitCredentialResolution(t *testing.T) {
	s := newTestSeeder(t, nil, Config{
		AllowHosts: []string{"example.com", ".example.com"},
		Credentials: map[string]Credential{
			"bare":  {URLPrefix: "https://example.com/acme/", Secret: "just-a-token"},
			"named": {URLPrefix: "https://example.com/acme/", Secret: "someone:their-token"},
			"empty": {URLPrefix: "https://example.com/acme/"},
			"exact": {URLPrefix: "https://example.com/acme/one.git", Secret: "one-token"},
			"whole": {URLPrefix: "https://example.com", Secret: "host-token"},
		},
	})

	tests := []struct {
		ref         string
		url         string
		wantUser    string
		wantSecret  string
		wantRefusal bool
		wantUnnamed bool
		description string
	}{
		{ref: "", url: "https://example.com/acme/x.git", wantUnnamed: true,
			description: "no credential asked for"},
		{ref: "bare", url: "https://example.com/acme/x.git",
			wantUser: defaultGitUsername, wantSecret: "just-a-token"},
		{ref: "named", url: "https://example.com/acme/x.git",
			wantUser: "someone", wantSecret: "their-token"},
		{ref: "exact", url: "https://example.com/acme/one.git",
			wantUser: defaultGitUsername, wantSecret: "one-token",
			description: "a prefix that is the repository itself"},
		{ref: "whole", url: "https://example.com/anything/at/all.git",
			wantUser: defaultGitUsername, wantSecret: "host-token",
			description: "a prefix with no path covers the host"},
		{ref: "empty", url: "https://example.com/acme/x.git", wantRefusal: true,
			description: "a name configured with nothing behind it"},
		{ref: "nope", url: "https://example.com/acme/x.git", wantRefusal: true,
			description: "a name the operator never configured"},

		// The binding. Each of these is a credential the caller named correctly and
		// may not spend here.
		{ref: "bare", url: "https://example.com/other/x.git", wantRefusal: true,
			description: "another organisation on the same host"},
		{ref: "bare", url: "https://evil.example.com/acme/x.git", wantRefusal: true,
			description: "another host under the same allowlist suffix"},
		{ref: "exact", url: "https://example.com/acme/one.git.evil", wantRefusal: true,
			description: "a path that merely starts with the prefix"},
		{ref: "bare", url: "https://example.com/acmecorp/x.git", wantRefusal: true,
			description: "a sibling whose name starts with the prefix's last component"},
	}

	for _, tc := range tests {
		got, err := s.credential(tc.ref, mustURL(t, tc.url))
		switch {
		case tc.wantRefusal:
			if !errors.Is(err, ErrNotPermitted) {
				t.Errorf("credential(%q, %q) = %v, want ErrNotPermitted (%s)",
					tc.ref, tc.url, err, tc.description)
			}
			continue
		case err != nil:
			t.Errorf("credential(%q, %q): %v", tc.ref, tc.url, err)
			continue
		case tc.wantUnnamed:
			if got.set {
				t.Errorf("credential(%q) resolved to something (%s)", tc.ref, tc.description)
			}
			continue
		}
		if got.username != tc.wantUser || got.secret != tc.wantSecret {
			t.Errorf("credential(%q, %q) = %q/%q, want %q/%q",
				tc.ref, tc.url, got.username, got.secret, tc.wantUser, tc.wantSecret)
		}
	}
}

// A refusal must not say which of the three reasons it was. Told apart, a caller
// enumerates the operator's credential names and then the repositories behind
// them: "no such credential" against "that repository does not exist" is a
// directory of the operator's private code.
func TestACredentialRefusalSaysNothingAboutWhy(t *testing.T) {
	s := newTestSeeder(t, nil, Config{
		AllowHosts: []string{"example.com"},
		Credentials: map[string]Credential{
			"real":  {URLPrefix: "https://example.com/acme/", Secret: "t"},
			"blank": {URLPrefix: "https://example.com/acme/"},
		},
	})

	url := mustURL(t, "https://example.com/other/x.git")
	_, unknown := s.credential("no-such-name", url)
	_, empty := s.credential("blank", url)
	_, outOfScope := s.credential("real", url)

	if unknown == nil || empty == nil || outOfScope == nil {
		t.Fatalf("one of them was permitted: unknown=%v empty=%v outOfScope=%v",
			unknown, empty, outOfScope)
	}
	// The name is the caller's own input, so it is quoted back; everything else
	// about the three has to read identically.
	shape := func(err error, ref string) string {
		return strings.ReplaceAll(err.Error(), ref, "<ref>")
	}
	got := shape(outOfScope, "real")
	for _, other := range []struct {
		err error
		ref string
	}{{unknown, "no-such-name"}, {empty, "blank"}} {
		if shape(other.err, other.ref) != got {
			t.Errorf("the refusals differ, which is the oracle:\n %v\n %v", other.err, outOfScope)
		}
	}
}

func TestParseCredentialPrefix(t *testing.T) {
	tests := []struct {
		prefix string
		wantOK bool
	}{
		{"https://github.com/acme/", true},
		{"https://github.com", true},
		{"https://github.com/acme/repo.git", true},
		{"http://github.com/acme/", false},
		{"github.com/acme/", false},
		{"https://user:pass@github.com/acme/", false},
		{"https://github.com/acme/?token=x", false},
		{"https://github.com/acme/#frag", false},
		{"https:github.com/acme", false},
		{"", false},
	}

	for _, tc := range tests {
		_, err := ParseCredentialPrefix(tc.prefix)
		if (err == nil) != tc.wantOK {
			t.Errorf("ParseCredentialPrefix(%q) = %v, want ok=%v", tc.prefix, err, tc.wantOK)
		}
	}
}

// Every URL refusal, on the git path too. The address policy and the allowlist
// live in one place for both types, and this is what says the git path goes
// through it rather than round it.
func TestGitURLsAreRefusedBeforeAnySocket(t *testing.T) {
	git := gitOrSkip(t)
	repo := newGitRepo(t, git, false)

	tests := []struct {
		name string
		url  string
	}{
		{"plain http", strings.Replace(repo.url, "https://", "http://", 1)},
		{"a path on this host", "file:///etc/passwd"},
		{"git's own command transport", "ext::sh -c whoami"},
		{"a host nobody allowlisted", "https://evil.example.net/repo.git"},
		{"a URL carrying its own credential", "https://user:token@127.0.0.1/repo.git"},
		{"the metadata service", "https://169.254.169.254/repo.git"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSeeder(t, nil, Config{AllowHosts: []string{"127.0.0.1", "169.254.169.254"}})
			before := *repo.requests

			_, err := s.Prepare(context.Background(), Request{Type: TypeGit, URL: tc.url})
			if !errors.Is(err, ErrNotPermitted) {
				t.Fatalf("Prepare(%s) = %v, want ErrNotPermitted", tc.url, err)
			}
			if *repo.requests != before {
				t.Error("the refusal happened after a request went out")
			}
			if strings.Contains(err.Error(), "token") {
				t.Errorf("the refusal quotes the URL's credential: %v", err)
			}
		})
	}
}

// The one case with the loopback exemption off, which is how the daemon runs. An
// allowlisted host that resolves to a blocked address gets no clone: git is never
// handed the name to resolve for itself.
func TestGitRefusesAnUnallowlistedHost(t *testing.T) {
	s, err := NewSeeder(Config{AllowHosts: []string{"127.0.0.1"}, GitPath: "/nonexistent/git"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.Prepare(context.Background(), Request{Type: TypeGit, URL: "https://127.0.0.1/repo.git"})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("Prepare of a loopback remote = %v, want ErrNotPermitted", err)
	}
}

// A host with no git says so as a refusal rather than failing somewhere inside
// exec, and tarballs keep working.
func TestGitIsRefusedWhenTheHostHasNone(t *testing.T) {
	s := newTestSeeder(t, nil, Config{AllowHosts: []string{"example.com"}})
	s.gitPath = ""

	_, err := s.Prepare(context.Background(), Request{Type: TypeGit, URL: "https://example.com/r.git"})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("Prepare with no git = %v, want ErrNotPermitted", err)
	}
}

// The pin has to actually bind git, and an unknown -c key is silently ignored --
// so the only convincing test is that a clone goes where the pin says and not
// where the name would have taken it. The remote is served on 127.0.0.1 and
// reached by name; pinned to a loopback address nothing listens on, the clone
// must fail without the server ever being asked.
//
// If this test ever passes while the pin is wrong, git resolves the name itself,
// and an allowlisted host that answers with the metadata address gets a socket.
func TestGitGoesWhereThePinSaysAndNotWhereDNSSays(t *testing.T) {
	git := gitOrSkip(t)
	repo := newGitRepo(t, git, false)
	byName := "https://localhost:" + mustURL(t, repo.url).Port() + "/repo.git"

	seeder := func(answer string) *Seeder {
		// A short deadline, because half of this test is a connection that must not
		// arrive: whether the kernel refuses it or drops it depends on the host, and
		// the timeout is what makes "nothing listens there" quick either way.
		s := newTestSeeder(t, nil, Config{AllowHosts: []string{"localhost"}, Timeout: 5 * time.Second})
		s.fetcher.lookup = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr(answer)}, nil
		}
		return s
	}

	// The address the server is really on: the clone works, so the pin is not
	// breaking anything by itself.
	prepared, err := seeder("127.0.0.1").Prepare(context.Background(), Request{Type: TypeGit, URL: byName})
	if err != nil {
		t.Fatalf("Prepare against the pinned address: %v", err)
	}
	prepared.Close()

	served := *repo.requests
	_, err = seeder("127.0.0.2").Prepare(context.Background(), Request{Type: TypeGit, URL: byName})
	if !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("Prepare pinned away from the server = %v, want ErrFetchFailed", err)
	}
	if *repo.requests != served {
		t.Error("git reached the server anyway, so it resolved the name itself and the pin does nothing")
	}
}

// The pins are the address policy applied to a program that does its own DNS. A
// blocked answer is dropped, and an answer with nothing left is refused.
func TestGitPinsOnlyPermittedAddresses(t *testing.T) {
	s := newTestSeeder(t, nil, Config{AllowHosts: []string{"mixed.example.com", "bad.example.com"}})
	s.fetcher.permitLoopback = false
	s.fetcher.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "mixed.example.com" {
			return []netip.Addr{
				netip.MustParseAddr("169.254.169.254"),
				netip.MustParseAddr("93.184.216.34"),
			}, nil
		}
		return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
	}

	pins, err := s.pinsFor(context.Background(), mustURL(t, "https://mixed.example.com/r.git"))
	if err != nil {
		t.Fatalf("pinsFor: %v", err)
	}
	if len(pins) != 1 || pins[0] != "mixed.example.com:443:93.184.216.34" {
		t.Errorf("pins = %v, want only the permitted address", pins)
	}

	if _, err := s.pinsFor(context.Background(), mustURL(t, "https://bad.example.com/r.git")); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("pinsFor a host with only private answers = %v, want ErrNotPermitted", err)
	}
}

func TestWalkCheckoutRefusesWhatCannotBeWritten(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "main.go"), "package main\n", 0o644)
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	// Refused rather than skipped: the guest file endpoints cannot express a link,
	// and a checkout silently missing one is a project that breaks later, somewhere
	// else.
	if _, err := walkCheckout(root, defaulted(t, Config{})); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("walkCheckout over a symlink = %v, want ErrInvalidArchive", err)
	}
}

func TestWalkCheckoutHonoursTheCaps(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		writeFixture(t, filepath.Join(root, name), strings.Repeat("x", 100), 0o644)
	}

	tests := []struct {
		name string
		cfg  Config
	}{
		{"the file count", Config{MaxFiles: 2}},
		{"the total size", Config{MaxExpandedBytes: 150}},
		{"one file's size", Config{MaxFileBytes: 50}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := walkCheckout(root, defaulted(t, tc.cfg)); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("walkCheckout past %s = %v, want ErrTooLarge", tc.name, err)
			}
		})
	}
}

// An empty checkout is refused rather than seeded, on the same grounds as an
// archive whose every member was stripped: a sandbox with nothing in it is the
// hardest failure to work out later.
func TestWalkCheckoutRefusesAnEmptyTree(t *testing.T) {
	if _, err := walkCheckout(t.TempDir(), defaulted(t, Config{})); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("walkCheckout over an empty tree = %v, want ErrInvalidArchive", err)
	}
}

func TestIsCommitSHA(t *testing.T) {
	tests := map[string]bool{
		strings.Repeat("a", 40):  true,
		strings.Repeat("F", 40):  true,
		strings.Repeat("ab", 32): true, // sha256
		"main":                   false,
		"":                       false,
		strings.Repeat("a", 39):  false,
		strings.Repeat("z", 40):  false,
		"v1.2.3":                 false,
	}
	for ref, want := range tests {
		if got := isCommitSHA(ref); got != want {
			t.Errorf("isCommitSHA(%q) = %v, want %v", ref, got, want)
		}
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func defaulted(t *testing.T, cfg Config) Config {
	t.Helper()
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The clone's footprint on this host, which nothing bounded before: -source-max-bytes
// lives in the HTTP fetcher's counting reader and -source-max-expanded-bytes on an
// archive's headers, and a clone goes through neither. What was left was
// -source-timeout, which is sixty seconds of whatever the link can deliver, written
// into a directory that is a tmpfs on a good many distributions and only measured
// after git had finished with it.
func TestACloneIsCancelledWhenItWritesTooMuch(t *testing.T) {
	tmp := t.TempDir()
	g := &gitRun{tmp: tmp, limit: 1 << 10}

	ctx, stop := g.watchDisk(context.Background())
	defer stop()

	if err := os.WriteFile(filepath.Join(tmp, "pack"), make([]byte, 8<<10), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the clone was never cancelled, so the caps do not apply to a git source at all")
	}
	if !g.overLimit.Load() {
		t.Error("the cancellation is not recorded, so it would be reported as a timeout")
	}
}

// And it is reported as the cap it is. A cancelled clone otherwise reads as a
// timeout, which sends an operator to raise -source-timeout for a repository that
// is simply too big.
func TestACloneOverTheCapIsNotReportedAsATimeout(t *testing.T) {
	git := gitOrSkip(t)
	g := &gitRun{git: git, tmp: t.TempDir(), limit: 1}
	g.overLimit.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := g.run(ctx, g.tmp, "clone", "https://example.com/x.git", "dst")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("run = %v, want ErrTooLarge", err)
	}
}

func TestDirSize(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "a"), strings.Repeat("x", 100), 0o644)
	writeFixture(t, filepath.Join(root, "sub", "b"), strings.Repeat("x", 50), 0o644)

	if got := dirSize(root); got != 150 {
		t.Errorf("dirSize = %d, want 150", got)
	}
	if got := dirSize(filepath.Join(root, "nope")); got != 0 {
		t.Errorf("dirSize of a missing directory = %d, want 0", got)
	}
}

// The pins are the address half of the policy on this path, and an unrecognised
// -c http.* key is accepted and ignored -- so on a git that does not know
// http.curloptResolve they do nothing, silently, and git does its own DNS. That is
// the rebinding window the tarball path was built to close: a public answer for the
// pre-flight check, the metadata address for git's lookup a moment later. There is
// no safe degraded mode, so a git that cannot be pinned serves no git source.
func TestGitIsRefusedWhenItCannotHonourThePins(t *testing.T) {
	tests := []struct {
		version string
		wantOK  bool
	}{
		{"git version 2.30.2", false},
		{"git version 2.31.0", true},
		{"git version 2.39.5 (Apple Git-154)", true},
		{"git version 3.0.0", true},
		{"git version 1.9.1", false},
		{"not a version at all", false},
	}

	for _, tc := range tests {
		fake := fakeGit(t, tc.version)
		err := gitPinsAddresses(fake)
		if (err == nil) != tc.wantOK {
			t.Errorf("gitPinsAddresses(%q) = %v, want ok=%v", tc.version, err, tc.wantOK)
		}
	}
}

// A Seeder built against such a git reports no git path at all, which is the same
// answer as a host without git: refused, with the reason kept for the operator.
func TestASeederRefusesAGitThatCannotBePinned(t *testing.T) {
	s, err := NewSeeder(Config{
		AllowHosts: []string{"example.com"},
		GitPath:    fakeGit(t, "git version 2.30.2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.GitPath() != "" {
		t.Errorf("GitPath = %q, want empty: a git that ignores the pins may not be used", s.GitPath())
	}
	if !strings.Contains(s.GitNote(), "curloptResolve") {
		t.Errorf("GitNote does not say why: %q", s.GitNote())
	}

	_, err = s.Prepare(context.Background(), Request{Type: TypeGit, URL: "https://example.com/x.git"})
	if !errors.Is(err, ErrNotPermitted) {
		t.Errorf("Prepare = %v, want ErrNotPermitted", err)
	}
}

// fakeGit is a script answering `--version` and nothing else.
func fakeGit(t *testing.T, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nprintf '%s\\n' " + strconv.Quote(version) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// A credential is bound to a URL prefix, and the binding is checked before a
// socket is opened. Without it the operator's token is spent on any repository a
// caller names, and handed to any allowlisted host that challenges for one -- git
// sends its credential to whatever answers.
func TestACredentialIsNotSpentOnAnotherRepository(t *testing.T) {
	git := gitOrSkip(t)
	repo := newGitRepo(t, git, true)

	s := newTestSeeder(t, nil, Config{
		AllowHosts: []string{"127.0.0.1"},
		Credentials: map[string]Credential{"forge": {
			// The same host, another organisation. The credential is real, the name
			// is right, and this repository is not what the operator handed over.
			URLPrefix: strings.TrimSuffix(repo.url, "repo.git") + "someone-else/",
			Secret:    testGitUser + ":" + testGitToken,
		}},
	})

	before := *repo.requests
	_, err := s.Prepare(context.Background(), Request{
		Type:          TypeGit,
		URL:           repo.url,
		CredentialRef: "forge",
	})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("Prepare = %v, want ErrNotPermitted", err)
	}
	if *repo.requests != before {
		t.Errorf("the remote was contacted %d times: the binding has to be settled before a socket",
			*repo.requests-before)
	}
}

// The secret is removed from git's complaint in both of the forms it takes. A
// secret written as "username:token" is split before it reaches scrub, so the half
// in front of the colon would otherwise be quoted into a 502 verbatim.
func TestScrubRemovesBothFormsOfTheSecret(t *testing.T) {
	g := &gitRun{cred: gitCredential{
		set: true, username: "ci-bot", secret: "tok:en", raw: "ci-bot:tok:en",
	}}

	for _, line := range []string{"failed for ci-bot:tok:en", "failed for tok:en"} {
		got := g.scrub(line)
		if strings.Contains(got, "tok:en") || strings.Contains(got, "ci-bot:tok") {
			t.Errorf("scrub(%q) = %q, and the secret is still in it", line, got)
		}
	}
}
