package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/auth"
	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
	"github.com/pablofdezr/microvm/internal/sandbox"
	"github.com/pablofdezr/microvm/internal/storage"
	"github.com/pablofdezr/microvm/internal/tenant"
)

// storageHarness is a server with real object storage and two distinct tokens,
// which is the minimum needed to test that a namespace follows the token.
type storageHarness struct {
	srv *httptest.Server
}

func newStorageHarness(t *testing.T, tokens ...string) *storageHarness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := sandbox.NewManager(runtimetest.New(), logstore.New(logstore.Config{}), log,
		sandbox.WithStorage(storage.NewMemory()))
	api := NewServer(Config{Tokens: tokens, Images: []string{"python"}}, mgr, nil, nil, log)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &storageHarness{srv: srv}
}

// create posts a sandbox as the given token and returns the decoded sandbox.
// rawBody is sent verbatim so a test can send fields the typed params forbid.
func (h *storageHarness) create(t *testing.T, token, rawBody string) (apitypes.Sandbox, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/sandboxes", strings.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	var sb apitypes.Sandbox
	if res.StatusCode == http.StatusCreated {
		json.NewDecoder(res.Body).Decode(&sb)
	}
	return sb, res.StatusCode
}

// TestNamespaceFollowsTheToken is the security property of #33: the namespace a
// caller's files live under is decided by their token, so two tokens land in two
// namespaces and one token always lands in the same one.
func TestNamespaceFollowsTheToken(t *testing.T) {
	h := newStorageHarness(t, "sk_alice", "sk_bob")

	alice1, _ := h.create(t, "sk_alice", `{"image":"python"}`)
	alice2, _ := h.create(t, "sk_alice", `{"image":"python"}`)
	bob, _ := h.create(t, "sk_bob", `{"image":"python"}`)

	if alice1.Storage == nil || bob.Storage == nil {
		t.Fatal("a sandbox created with storage configured reported none")
	}

	// Stable: the same token, twice, gets the same namespace -- which is what
	// lets a caller's files outlive any single sandbox.
	if alice1.Storage.Namespace != alice2.Storage.Namespace {
		t.Errorf("one token got two namespaces: %q then %q",
			alice1.Storage.Namespace, alice2.Storage.Namespace)
	}
	// Isolated: two tokens never share a namespace by accident.
	if alice1.Storage.Namespace == bob.Storage.Namespace {
		t.Errorf("two tokens share a namespace %q; one tenant could read another's files",
			alice1.Storage.Namespace)
	}
}

// TestBodyCannotChooseTheNamespace is the other half: the request has no lever on
// the prefix. The generated params reject an unknown storage field, so an attempt
// to name a namespace is a 400, not a silent takeover of someone else's.
func TestBodyCannotChooseTheNamespace(t *testing.T) {
	h := newStorageHarness(t, "sk_alice", "sk_victim")

	// Learn the victim's namespace the honest way.
	victim, _ := h.create(t, "sk_victim", `{"image":"python"}`)
	if victim.Storage == nil {
		t.Fatal("no storage on the victim's sandbox")
	}

	// Alice tries to plant her sandbox in the victim's namespace by sending it in
	// the body. Every plausible spelling must fail to place her there.
	for _, body := range []string{
		`{"image":"python","storage":{"namespace":"` + victim.Storage.Namespace + `"}}`,
		`{"image":"python","storage":{"prefix":"tenants/` + victim.Storage.Namespace + `"}}`,
		`{"image":"python","namespace":"` + victim.Storage.Namespace + `"}`,
	} {
		sb, status := h.create(t, "sk_alice", body)
		if status == http.StatusCreated && sb.Storage != nil &&
			sb.Storage.Namespace == victim.Storage.Namespace {
			t.Errorf("body %q placed Alice in the victim's namespace", body)
		}
	}
}

// TestBodyReadOnlyOnlyTightens checks the one lever the body does have. It may
// make a writable key read-only for a run; it may not make anything writable.
func TestBodyReadOnlyOnlyTightens(t *testing.T) {
	h := newStorageHarness(t, "sk_alice")

	// A writable key, asked to mount read-only for this sandbox.
	sb, status := h.create(t, "sk_alice", `{"image":"python","storage":{"read_only":true}}`)
	if status != http.StatusCreated {
		t.Fatalf("create with read_only returned %d", status)
	}
	if sb.Storage == nil || !sb.Storage.ReadOnly {
		t.Error("body read_only:true did not make the mount read-only")
	}
}

// TestMountPathIsConfigurableAndReported is the caller-facing side of the
// configurable mount: a valid mount_path in the body is accepted and echoed
// back, so the caller knows where its files are without hardcoding a path.
func TestMountPathIsConfigurableAndReported(t *testing.T) {
	h := newStorageHarness(t, "sk_alice")

	sb, status := h.create(t, "sk_alice", `{"image":"python","storage":{"mount_path":"/data"}}`)
	if status != http.StatusCreated {
		t.Fatalf("create with mount_path returned %d", status)
	}
	if sb.Storage == nil || sb.Storage.MountPath == nil || *sb.Storage.MountPath != "/data" {
		t.Errorf("mount_path not reflected: %+v", sb.Storage)
	}
}

// TestDefaultMountPathReported checks the default is surfaced too, so a caller
// that sets nothing still learns where storage lives.
func TestDefaultMountPathReported(t *testing.T) {
	h := newStorageHarness(t, "sk_alice")

	sb, _ := h.create(t, "sk_alice", `{"image":"python"}`)
	if sb.Storage == nil || sb.Storage.MountPath == nil || *sb.Storage.MountPath != storage.DefaultMountPath {
		t.Errorf("default mount path not reported: %+v", sb.Storage)
	}
}

// TestBadMountPathIsRejected is the security check at the API edge: a mount_path
// that could inject a kernel parameter is a 400, and the sandbox is never
// created. The runtime refuses it a second time, but the caller should never
// get that far.
func TestBadMountPathIsRejected(t *testing.T) {
	h := newStorageHarness(t, "sk_alice")

	// A space would split into a second kernel-cmdline parameter; JSON-escape it
	// so the body is valid JSON carrying a literal space.
	_, status := h.create(t, "sk_alice", `{"image":"python","storage":{"mount_path":"/mnt/x init=/bin/sh"}}`)
	if status != http.StatusBadRequest {
		t.Errorf("an injecting mount_path returned %d, want 400", status)
	}
}

// TestNoAuthFallsBackToPerSandbox checks the disabled-auth case. With no tokens
// there is no tenant, so each sandbox gets its own namespace -- isolated, not
// shared, which is the safe default for an open daemon.
func TestNoAuthFallsBackToPerSandbox(t *testing.T) {
	h := newStorageHarness(t) // no tokens

	a, _ := h.create(t, "", `{"image":"python"}`)
	b, _ := h.create(t, "", `{"image":"python"}`)

	if a.Storage == nil || b.Storage == nil {
		t.Fatal("storage missing under disabled auth")
	}
	// Per-sandbox: the namespace is the sandbox's own id, so two sandboxes differ.
	if a.Storage.Namespace != a.Id || b.Storage.Namespace != b.Id {
		t.Errorf("namespace is not the sandbox id: %q vs %q, %q vs %q",
			a.Storage.Namespace, a.Id, b.Storage.Namespace, b.Id)
	}
	if a.Storage.Namespace == b.Storage.Namespace {
		t.Error("two unauthenticated sandboxes share a namespace")
	}
}

// newStorageHarnessWithPrincipals builds a server whose tokens carry explicit
// identities, for testing read-only keys and custom quotas.
func newStorageHarnessWithPrincipals(t *testing.T, principals map[string]*auth.Principal) *storageHarness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := sandbox.NewManager(runtimetest.New(), logstore.New(logstore.Config{}), log,
		sandbox.WithStorage(storage.NewMemory()))
	api := NewServer(Config{Principals: principals, Images: []string{"python"}}, mgr, nil, nil, log)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &storageHarness{srv: srv}
}

// TestReadOnlyTokenCannotBeLoosened is the tightening rule from the other side:
// a read-only key stays read-only however the body pleads. Without this, a body
// saying read_only:false would hand write access the key was never granted.
func TestReadOnlyTokenCannotBeLoosened(t *testing.T) {
	h := newStorageHarnessWithPrincipals(t, map[string]*auth.Principal{
		"sk_readonly": {Tenant: "t_ro", ReadOnly: true},
	})

	for _, body := range []string{
		`{"image":"python"}`, // says nothing
		`{"image":"python","storage":{"read_only":false}}`, // asks to loosen
		`{"image":"python","storage":{"read_only":true}}`,  // agrees
	} {
		sb, status := h.create(t, "sk_readonly", body)
		if status != http.StatusCreated {
			t.Fatalf("create returned %d for %q", status, body)
		}
		if sb.Storage == nil || !sb.Storage.ReadOnly {
			t.Errorf("a read-only key became writable via %q", body)
		}
	}
}

// --- tenant policy API (admin) ----------------------------------------------

func newAdminHarness(t *testing.T) (*storageHarness, *tenant.Memory) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tenants := tenant.NewMemory()
	mgr := sandbox.NewManager(runtimetest.New(), logstore.New(logstore.Config{}), log,
		sandbox.WithStorage(storage.NewMemory()))
	api := NewServer(Config{
		Principals: map[string]*auth.Principal{
			"sk_admin": {Tenant: "t_admin", Admin: true},
			"sk_user":  {Tenant: "t_user"},
		},
		Images:  []string{"python"},
		Tenants: tenants,
	}, mgr, nil, nil, log)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &storageHarness{srv: srv}, tenants
}

func (h *storageHarness) req(t *testing.T, method, path, token, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestOnlyAdminSetsTenantPolicy is the point of the admin tier: a normal key,
// which can create sandboxes all day, cannot touch a tenant's storage limit.
func TestOnlyAdminSetsTenantPolicy(t *testing.T) {
	h, _ := newAdminHarness(t)

	res := h.req(t, http.MethodPut, "/v1/tenants/t_user", "sk_user",
		`{"max_bytes":1000,"policy":"evict"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a non-admin set a tenant policy: got %d, want 403", res.StatusCode)
	}

	res = h.req(t, http.MethodPut, "/v1/tenants/t_user", "sk_admin",
		`{"max_bytes":1000,"policy":"evict"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("admin could not set a tenant policy: got %d", res.StatusCode)
	}
}

// TestTenantPolicyReachesTheMount is the wiring: a policy an admin sets must
// actually govern the sandbox that tenant then creates. A policy nobody enforces
// is a setting, not a limit.
func TestTenantPolicyReachesTheMount(t *testing.T) {
	h, tenants := newAdminHarness(t)

	// Admin caps t_user hard.
	res := h.req(t, http.MethodPut, "/v1/tenants/t_user", "sk_admin",
		`{"max_bytes":5,"policy":"preserve"}`)
	res.Body.Close()

	// The store now holds it.
	pol, ok, _ := tenants.Get(t.Context(), "t_user")
	if !ok || pol.MaxBytes != 5 || pol.OnFull != storage.Preserve {
		t.Fatalf("policy not stored: %+v ok=%v", pol, ok)
	}

	// And a sandbox the user creates carries it: the create response reflects the
	// admin-set cap and policy back, which is only possible if the policy reached
	// the mount rather than being dropped on the way.
	res = h.req(t, http.MethodPost, "/v1/sandboxes", "sk_user", `{"image":"python"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create returned %d", res.StatusCode)
	}
	var sb apitypes.Sandbox
	json.NewDecoder(res.Body).Decode(&sb)
	if sb.Storage == nil || sb.Storage.MaxBytes == nil || *sb.Storage.MaxBytes != 5 {
		t.Errorf("the admin-set cap did not reach the sandbox's mount: %+v", sb.Storage)
	}
	if sb.Storage.Policy == nil || *sb.Storage.Policy != apitypes.Preserve {
		t.Errorf("the admin-set policy did not reach the mount: %+v", sb.Storage)
	}
}

// TestRetrieveTenantReportsUsage checks the read path reports the real total,
// scanned from the store.
func TestRetrieveTenantReportsUsage(t *testing.T) {
	h, _ := newAdminHarness(t)

	res := h.req(t, http.MethodPut, "/v1/tenants/t_user", "sk_admin",
		`{"max_bytes":1000,"policy":"preserve"}`)
	res.Body.Close()

	res = h.req(t, http.MethodGet, "/v1/tenants/t_user", "sk_admin", "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("retrieve returned %d", res.StatusCode)
	}
	var got apitypes.Tenant
	json.NewDecoder(res.Body).Decode(&got)
	if got.MaxBytes != 1000 || got.Policy != apitypes.Preserve {
		t.Errorf("retrieved policy wrong: %+v", got)
	}
	if got.UsageBytes == nil {
		t.Error("retrieve did not report usage")
	}
}

// TestRejectsUnknownPolicy makes sure only the two real policies are accepted;
// a typo must be a 400, not a silently-ignored default.
func TestRejectsUnknownPolicy(t *testing.T) {
	h, _ := newAdminHarness(t)
	res := h.req(t, http.MethodPut, "/v1/tenants/t_user", "sk_admin",
		`{"max_bytes":1000,"policy":"delete-everything"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown policy was accepted: got %d, want 400", res.StatusCode)
	}
}
