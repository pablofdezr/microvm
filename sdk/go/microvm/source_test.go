package microvm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	microvm "github.com/pablofdezr/microvm-sdk-go/microvm"
)

// The helpers exist to spell the source out for the caller, so what they put on
// the wire is the thing worth asserting. A field named wrongly is a seed the
// server refuses, and a field sent when it should be absent is a default the
// caller never chose.
func TestSourceHelpersBuildTheWireBody(t *testing.T) {
	tests := []struct {
		name   string
		source *microvm.SandboxSourceParams
		want   map[string]any
	}{
		{
			name:   "a tarball, stripped",
			source: microvm.TarballSource("https://example.com/v1.2.3.tar.gz", 1),
			want: map[string]any{
				"type": "tarball", "url": "https://example.com/v1.2.3.tar.gz",
				"strip_components": float64(1),
			},
		},
		{
			// Zero is the server's default, so it is omitted rather than sent.
			name:   "a tarball, unstripped",
			source: microvm.TarballSource("https://example.com/v1.2.3.tar.gz", 0),
			want: map[string]any{
				"type": "tarball", "url": "https://example.com/v1.2.3.tar.gz",
			},
		},
		{
			name:   "a git ref",
			source: microvm.GitSource("https://example.com/acme/widgets", "main"),
			want: map[string]any{
				"type": "git", "url": "https://example.com/acme/widgets", "ref": "main",
			},
		},
		{
			// No ref means the remote's default branch, which is the server's choice
			// to make: an empty string would be a ref that resolves to nothing.
			name:   "no git ref",
			source: microvm.GitSource("https://example.com/acme/widgets", ""),
			want: map[string]any{
				"type": "git", "url": "https://example.com/acme/widgets",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &body)
				writeJSON(w, map[string]any{"id": "sb_1", "object": "sandbox"})
			}))
			t.Cleanup(srv.Close)

			client := microvm.New(srv.URL, microvm.WithToken("t"))
			_, err := client.Sandboxes.Create(context.Background(), microvm.SandboxCreateParams{
				Image:  "python",
				Source: tc.source,
			})
			if err != nil {
				t.Fatal(err)
			}

			got, ok := body["source"].(map[string]any)
			if !ok {
				t.Fatalf("body carried no source object: %v", body)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("source = %v, want %v", got, tc.want)
			}
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("source[%q] = %v, want %v", k, got[k], want)
				}
			}
		})
	}
}

// A private repository's credential is a name the operator configured, never the
// secret. It is set on the result rather than passed in, so assert it survives
// the round trip to the wire under the name the API expects.
func TestGitSourceCarriesACredentialByName(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeJSON(w, map[string]any{"id": "sb_1", "object": "sandbox"})
	}))
	t.Cleanup(srv.Close)

	src := microvm.GitSource("https://example.com/acme/private", "v1.0.0")
	src.CredentialRef = microvm.Ptr("github-ci")

	client := microvm.New(srv.URL, microvm.WithToken("t"))
	if _, err := client.Sandboxes.Create(context.Background(), microvm.SandboxCreateParams{
		Image:  "node",
		Source: src,
	}); err != nil {
		t.Fatal(err)
	}

	got := body["source"].(map[string]any)
	if got["credential_ref"] != "github-ci" {
		t.Errorf("credential_ref = %v, want github-ci", got["credential_ref"])
	}
}

// The two seeding failures need opposite reactions -- one is retried, the other
// needs an operator -- and they arrive as a 400 and a 502 that other things also
// use. So the guards match the code, and nothing else must match with them.
func TestSeedingGuardsMatchTheirCodeOnly(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		errType, code string
		notPermitted  bool
		fetchFailed   bool
	}{
		{
			name: "a refused source", status: http.StatusBadRequest,
			errType: "invalid_request_error", code: "source_not_permitted",
			notPermitted: true,
		},
		{
			name: "an origin that misbehaved", status: http.StatusBadGateway,
			errType: "api_error", code: "source_fetch_failed",
			fetchFailed: true,
		},
		{
			// Same status and type as the refusal, and not a seeding failure.
			name: "an ordinary bad parameter", status: http.StatusBadRequest,
			errType: "invalid_request_error", code: "invalid_parameter",
		},
		{
			// A 502 from something in front of the daemon must not read as a seed.
			name: "an upstream 502", status: http.StatusBadGateway,
			errType: "api_error", code: "internal_error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
					"type": tc.errType, "code": tc.code, "message": "no",
				}})
			}))
			t.Cleanup(srv.Close)

			// No retries: a 502 is retryable, and this asserts the classification of
			// the error, not the retry policy.
			client := microvm.New(srv.URL, microvm.WithMaxRetries(0))
			_, err := client.Sandboxes.Create(context.Background(), microvm.SandboxCreateParams{
				Image:  "python",
				Source: microvm.TarballSource("https://example.com/v1.tar.gz", 1),
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := microvm.IsSourceNotPermitted(err); got != tc.notPermitted {
				t.Errorf("IsSourceNotPermitted = %v, want %v", got, tc.notPermitted)
			}
			if got := microvm.IsSourceFetchFailed(err); got != tc.fetchFailed {
				t.Errorf("IsSourceFetchFailed = %v, want %v", got, tc.fetchFailed)
			}
			// A seeding failure is never capacity: reported as one, a caller would
			// retry a URL that is never going to work, or queue a task instead.
			if microvm.IsCapacity(err) {
				t.Error("IsCapacity = true, want false")
			}
		})
	}
}
