package main

import (
	"strings"
	"testing"
)

// -source is the one flag whose value is not a scalar the flag package can parse
// for us, so what it accepts and what it refuses is worth pinning down.
func TestSourceParams(t *testing.T) {
	tests := []struct {
		name string
		opts options
		// want is the JSON the create body would carry, or "" for no source.
		wantType   string
		wantURL    string
		wantRef    string
		wantStrip  int
		wantCred   string
		wantErrHas string
	}{
		{
			name: "no source at all",
			opts: options{},
		},
		{
			name:     "a tarball",
			opts:     options{source: "tarball=https://example.com/v1.tar.gz", sourceStrip: 1},
			wantType: "tarball", wantURL: "https://example.com/v1.tar.gz", wantStrip: 1,
		},
		{
			// Zero is the server's own default, so it is left out rather than sent.
			name:     "a tarball with no strip",
			opts:     options{source: "tarball=https://example.com/v1.tar.gz"},
			wantType: "tarball", wantURL: "https://example.com/v1.tar.gz",
		},
		{
			name:     "a git clone",
			opts:     options{source: "git=https://example.com/acme/widgets", sourceRef: "main"},
			wantType: "git", wantURL: "https://example.com/acme/widgets", wantRef: "main",
		},
		{
			name:     "a private git clone",
			opts:     options{source: "git=https://example.com/acme/widgets", sourceCred: "github-ci"},
			wantType: "git", wantURL: "https://example.com/acme/widgets", wantCred: "github-ci",
		},
		{
			// The URL's own query keeps its "=" signs: only the first one separates.
			name:     "a presigned URL keeps its query",
			opts:     options{source: "tarball=https://example.com/p.tar?sig=abc=&x=1"},
			wantType: "tarball", wantURL: "https://example.com/p.tar?sig=abc=&x=1",
		},
		{
			// Sent, not dropped. The API refuses it naming the field, where dropping
			// it here would seed the archive and call a half-honoured request a
			// success.
			name:     "a ref on a tarball is passed through to be refused",
			opts:     options{source: "tarball=https://example.com/v1.tar.gz", sourceRef: "main"},
			wantType: "tarball", wantURL: "https://example.com/v1.tar.gz", wantRef: "main",
		},
		{
			name:     "a strip on a git clone is passed through to be refused",
			opts:     options{source: "git=https://example.com/acme/widgets", sourceStrip: 2},
			wantType: "git", wantURL: "https://example.com/acme/widgets", wantStrip: 2,
		},
		{
			name:       "a bare URL names no type",
			opts:       options{source: "https://example.com/v1.tar.gz"},
			wantErrHas: "-source tarball=<url>",
		},
		{
			name:       "an unknown type",
			opts:       options{source: "svn=https://example.com/trunk"},
			wantErrHas: "is not a source type",
		},
		{
			name:       "a type with no URL",
			opts:       options{source: "git="},
			wantErrHas: "-source tarball=<url>",
		},
		{
			// A modifier with nothing to modify. Refused, not ignored: the command
			// would otherwise run against an empty sandbox and look like it worked.
			name:       "a ref with no source",
			opts:       options{sourceRef: "main"},
			wantErrHas: "which was not given",
		},
		{
			name:       "a credential with no source",
			opts:       options{sourceCred: "github-ci"},
			wantErrHas: "which was not given",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sourceParams(tc.opts)
			if tc.wantErrHas != "" {
				if err == nil {
					t.Fatalf("sourceParams(%q) = %+v, want an error", tc.opts.source, got)
				}
				if !strings.Contains(err.Error(), tc.wantErrHas) {
					t.Fatalf("error %q does not mention %q", err, tc.wantErrHas)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantType == "" {
				if got != nil {
					t.Fatalf("got %+v, want no source", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got no source")
			}
			if string(got.Type) != tc.wantType {
				t.Errorf("type = %q, want %q", got.Type, tc.wantType)
			}
			if got.Url != tc.wantURL {
				t.Errorf("url = %q, want %q", got.Url, tc.wantURL)
			}
			if ref := derefStr(got.Ref); ref != tc.wantRef {
				t.Errorf("ref = %q, want %q", ref, tc.wantRef)
			}
			if strip := deref(got.StripComponents); strip != tc.wantStrip {
				t.Errorf("strip_components = %d, want %d", strip, tc.wantStrip)
			}
			if cred := derefStr(got.CredentialRef); cred != tc.wantCred {
				t.Errorf("credential_ref = %q, want %q", cred, tc.wantCred)
			}
		})
	}
}

// -source takes a value, so it has to pull the next argument along with it when
// the flag is written after the positional arguments -- which is how people type
// it: `microvm exec go go test ./... -source git=https://host/repo`.
func TestSplitArgsKeepsASourceValueWithItsFlag(t *testing.T) {
	positional, flags := splitArgs([]string{
		"go", "go", "test", "./...",
		"-source", "git=https://example.com/acme/widgets",
		"-source-ref", "v1.2.3",
		"-network",
	})

	wantPositional := []string{"go", "go", "test", "./..."}
	if len(positional) != len(wantPositional) {
		t.Fatalf("positional = %v, want %v", positional, wantPositional)
	}
	for i, want := range wantPositional {
		if positional[i] != want {
			t.Fatalf("positional = %v, want %v", positional, wantPositional)
		}
	}

	// -network is a boolean and must not swallow anything; the two value flags must
	// each have kept theirs.
	wantFlags := []string{
		"-source", "git=https://example.com/acme/widgets",
		"-source-ref", "v1.2.3",
		"-network",
	}
	if len(flags) != len(wantFlags) {
		t.Fatalf("flags = %v, want %v", flags, wantFlags)
	}
	for i, want := range wantFlags {
		if flags[i] != want {
			t.Fatalf("flags = %v, want %v", flags, wantFlags)
		}
	}
}
