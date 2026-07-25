package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pablofdezr/microvm/internal/source"
)

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A credential is named on the command line and read from a file, never given on
// it: a secret in an argument is a secret in ps, in shell history and in the unit
// file. This is the flag's whole shape, so it is worth a test.
func TestReadCredentials(t *testing.T) {
	bare := writeSecretFile(t, "ghp_abcdef\n")
	named := writeSecretFile(t, "someone:their-token\n")

	creds, err := readCredentials([]string{
		"forge@https://github.com/acme/=" + bare,
		"other@https://git.example.com/=" + named,
	}, discard())
	if err != nil {
		t.Fatalf("readCredentials: %v", err)
	}
	// The trailing newline goes, so a token written with `echo` works.
	if creds["forge"].Secret != "ghp_abcdef" {
		t.Errorf("forge = %q", creds["forge"].Secret)
	}
	if creds["other"].Secret != "someone:their-token" {
		t.Errorf("other = %q", creds["other"].Secret)
	}
	// And the prefix, which is the only thing standing between one caller's
	// credential_ref and every repository the operator's token can reach.
	if creds["forge"].URLPrefix != "https://github.com/acme/" {
		t.Errorf("forge prefix = %q", creds["forge"].URLPrefix)
	}
}

// Every way of getting it wrong is fatal at startup, where an operator is
// watching. The alternative is a daemon that starts and then fails every create
// that needed a private repository.
func TestReadCredentialsRefusesWhatCannotWork(t *testing.T) {
	good := writeSecretFile(t, "token\n")

	const at = "@https://github.com/acme/"
	tests := []struct {
		name    string
		entries []string
	}{
		{"no name", []string{at + "=" + good}},
		{"no path", []string{"forge" + at + "="}},
		{"not a pair at all", []string{"forge"}},
		{"a file that is not there", []string{"forge" + at + "=" + filepath.Join(t.TempDir(), "absent")}},
		{"a file with nothing in it", []string{"forge" + at + "=" + writeSecretFile(t, "\n")}},
		{"one name twice", []string{"forge" + at + "=" + good, "forge" + at + "=" + good}},

		// The prefix is what binds the credential to a repository. Every way of
		// leaving it out or writing it uselessly is fatal here, because the
		// alternative is a credential any caller may spend anywhere.
		{"no prefix at all", []string{"forge=" + good}},
		{"an empty prefix", []string{"forge@=" + good}},
		{"a prefix that is not https", []string{"forge@http://github.com/acme/=" + good}},
		{"a prefix that is not a URL", []string{"forge@github.com/acme/=" + good}},
		{"a prefix carrying its own credential", []string{"forge@https://u:p@github.com/acme/=" + good}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readCredentials(tc.entries, discard()); err == nil {
				t.Error("the daemon would have started with this")
			}
		})
	}
}

// A startup error names the file and never its contents: an error at boot is a
// line in the journal.
func TestACredentialErrorDoesNotQuoteTheSecret(t *testing.T) {
	path := writeSecretFile(t, "s3cret-value\n")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Skipf("cannot make the file unreadable here: %v", err)
	}

	_, err := readCredentials([]string{"forge@https://github.com/acme/=" + path}, discard())
	if err == nil {
		t.Skip("the file is readable anyway, probably because this test runs as root")
	}
	if strings.Contains(err.Error(), "s3cret-value") {
		t.Errorf("the startup error quotes the credential: %v", err)
	}
}

// The allowlist is repeatable, and a value carrying several hosts is split rather
// than refused: a hostname holds no comma, so there is nothing to be ambiguous
// about.
func TestParseSourceHosts(t *testing.T) {
	got := parseSourceHosts([]string{
		"codeload.github.com",
		" .githubusercontent.com , objects.githubusercontent.com ",
		"",
		",",
	})
	want := []string{"codeload.github.com", ".githubusercontent.com", "objects.githubusercontent.com"}
	if !slices.Equal(got, want) {
		t.Errorf("hosts = %v, want %v", got, want)
	}
}

// -source-fetch with no host named is still off, and the operator hears about it.
// Enabling the flag alone must not turn the daemon into a fetcher for every host
// on the internet.
func TestSeedingWithNoAllowlistFetchesNothing(t *testing.T) {
	seeder, err := newSeeder(config{sourceFetch: true}, discard())
	if err != nil {
		t.Fatalf("newSeeder: %v", err)
	}
	req := source.Request{Type: source.TypeTarball, URL: "https://codeload.github.com/acme/widgets.tar.gz"}
	if _, err := seeder.Prepare(t.Context(), req); err == nil {
		t.Error("a source was fetched with an empty allowlist")
	}
}

// A checkout left behind by a run that was killed mid-seed. Nothing else ever
// removes it: the create's own cleanup is a deferred Close, and SIGKILL -- which is
// what systemd sends when a seed in flight outlasts TimeoutStopSec -- does not run
// deferred functions.
func TestLeftoverSourceCheckoutsAreSweptAtStartup(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, "microvm-git-123456")
	if err := os.MkdirAll(filepath.Join(leftover, "repo", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "something-else")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}

	sweepSourceLeftovers(dir, discard())

	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("the leftover checkout is still there: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the sweep took a directory that is not ours: %v", err)
	}
}
