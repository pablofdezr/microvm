package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// writeTokenFile writes a token file and returns its path.
func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The sources add up, which is what makes moving off the flag a rotation: for one
// restart the old token and the new one both work.
func TestLoadTokensUnionsEverySource(t *testing.T) {
	t.Setenv(envTokens, "sk_env")
	t.Setenv(envAdminTokens, "sk_admin_env")

	ts, err := loadTokens(config{
		tokensFile:      writeTokenFile(t, "sk_file\n"),
		adminTokensFile: writeTokenFile(t, "sk_admin_file\n"),
		tokens:          "sk_flag",
		adminTokens:     "sk_admin_flag",
	}, discard())
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"sk_file", "sk_env", "sk_flag"}; !slices.Equal(ts.tokens, want) {
		t.Errorf("tokens = %q, want %q", ts.tokens, want)
	}
	if want := []string{"sk_admin_file", "sk_admin_env", "sk_admin_flag"}; !slices.Equal(ts.admins, want) {
		t.Errorf("admins = %q, want %q", ts.admins, want)
	}
}

// A token in two sources is one token: an operator mid-rotation must not have the
// same key counted twice.
func TestLoadTokensDeduplicatesAcrossSources(t *testing.T) {
	t.Setenv(envTokens, "sk_a,sk_b")

	ts, err := loadTokens(config{
		tokensFile: writeTokenFile(t, "sk_a\n"),
		tokens:     "sk_b, sk_c",
	}, discard())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"sk_a", "sk_b", "sk_c"}; !slices.Equal(ts.tokens, want) {
		t.Errorf("tokens = %q, want %q", ts.tokens, want)
	}
}

// A file an operator maintains by hand gets comments and blank lines.
func TestLoadTokensReadsACommentedFile(t *testing.T) {
	ts, err := loadTokens(config{
		tokensFile: writeTokenFile(t, "# ci, rotated 2026-01\nsk_ci\n\n# staging\nsk_staging\n"),
	}, discard())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"sk_ci", "sk_staging"}; !slices.Equal(ts.tokens, want) {
		t.Errorf("tokens = %q, want %q -- a comment leaked a token, or a token was lost", ts.tokens, want)
	}
}

// Naming a file and being given none is not a shrug: a daemon that started
// anyway would serve VM creation to whoever found the port.
func TestABadTokenFileStopsTheDaemon(t *testing.T) {
	tests := []struct {
		name string
		cfg  config
	}{
		{
			name: "missing",
			cfg:  config{tokensFile: filepath.Join(t.TempDir(), "absent")},
		},
		{
			name: "empty",
			cfg:  config{tokensFile: writeTokenFile(t, "")},
		},
		{
			name: "comments only",
			cfg:  config{tokensFile: writeTokenFile(t, "# a note and nothing else\n")},
		},
		{
			name: "missing admin file",
			cfg:  config{adminTokensFile: filepath.Join(t.TempDir(), "absent")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadTokens(tc.cfg, discard()); err == nil {
				t.Error("the daemon started with a token file it could not read")
			}
		})
	}
}

// No sources at all is auth disabled, which is a real configuration and not an
// error: it is what a loopback dev daemon runs as.
func TestNoTokensIsNotAnError(t *testing.T) {
	ts, err := loadTokens(config{}, discard())
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.tokens) != 0 || len(ts.admins) != 0 {
		t.Errorf("tokens appeared from nowhere: %q / %q", ts.tokens, ts.admins)
	}
}

// --- what the API is configured with ---------------------------------------

// The flat list is the simple path and must stay: no admins, no limits, nothing
// to spell out.
func TestApiConfigStaysAFlatList(t *testing.T) {
	c := apiConfig(config{}, tokenSet{tokens: []string{"sk_a"}}, nil, nil)

	if c.Principals != nil {
		t.Errorf("principals were built for a plain token list: %v", c.Principals)
	}
	if want := []string{"sk_a"}; !slices.Equal(c.Tokens, want) {
		t.Errorf("tokens = %q, want %q", c.Tokens, want)
	}
}

// A limit belongs to an identity, so setting one forces every token to be spelled
// out as a principal -- the flat list has nowhere to carry it.
func TestLimitsReachEveryPrincipal(t *testing.T) {
	c := apiConfig(
		config{tenantMaxSandboxes: 5, tenantMaxRPS: 2.5},
		tokenSet{tokens: []string{"sk_a"}, admins: []string{"sk_admin"}},
		nil, nil,
	)

	if c.Tokens != nil {
		t.Errorf("a flat list survived alongside limits: %q", c.Tokens)
	}
	for token, p := range c.Principals {
		if p.MaxConcurrent != 5 || p.MaxRequestsPerSecond != 2.5 {
			t.Errorf("%s: limits = %d/%v, want 5/2.5", token, p.MaxConcurrent, p.MaxRequestsPerSecond)
		}
		if p.Tenant == "" {
			t.Errorf("%s: no tenant, so there is nothing to charge the limit to", token)
		}
	}
	if p := c.Principals["sk_admin"]; p == nil || !p.Admin {
		t.Error("the admin token lost its admin power to the limits")
	}
	if p := c.Principals["sk_a"]; p == nil || p.Admin {
		t.Error("a plain token became an admin")
	}
}

// A token listed as both is an admin: the more specific grant wins.
func TestATokenListedTwiceIsAnAdmin(t *testing.T) {
	c := apiConfig(config{}, tokenSet{
		tokens: []string{"sk_both"},
		admins: []string{"sk_both"},
	}, nil, nil)

	if p := c.Principals["sk_both"]; p == nil || !p.Admin {
		t.Error("a token listed as both plain and admin came out plain")
	}
}
