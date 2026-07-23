package sandbox

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
	"github.com/pablofdezr/microvm/internal/storage"
)

// guestHTTP returns a client that reaches a sandbox's host storage server the
// way code inside that sandbox would: over its own inbound socket, and nothing
// else. There is no address to get wrong and no credential to pass, which is
// the point.
func guestHTTP(inst *runtimetest.Instance) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return inst.Guest().Dial(ctx)
			},
		},
		Timeout: 5 * time.Second,
	}
}

func newStorageManager(t *testing.T) (*Manager, *storage.Memory) {
	t.Helper()
	backend := storage.NewMemory()
	logs := logstore.New(logstore.Config{})
	m := NewManager(runtimetest.New(), logs, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithStorage(backend))
	return m, backend
}

// newStorageManagerRT is newStorageManager but hands back the fake runtime too,
// so a test can inspect the spec the manager handed it -- the boot-time storage
// decisions among it.
func newStorageManagerRT(t *testing.T) (*Manager, *runtimetest.Runtime) {
	t.Helper()
	rt := runtimetest.New()
	logs := logstore.New(logstore.Config{})
	m := NewManager(rt, logs, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithStorage(storage.NewMemory()))
	return m, rt
}

// TestGuestIsToldToMountStorage checks the boot-time half of the storage path:
// a node with a bucket tells every guest, on its kernel command line, to mount
// storage at the default place. Without this the guest never mounts and the
// whole filesystem is dead no matter how well the server behind it works.
func TestGuestIsToldToMountStorage(t *testing.T) {
	m, rt := newStorageManagerRT(t)
	sb, err := m.Create(context.Background(), Spec{Spec: runtime.Spec{ID: "sb_a", Image: "python"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Stop(context.Background(), ReasonStopped)

	spec, ok := rt.LastSpec()
	if !ok {
		t.Fatal("runtime was never asked to create anything")
	}
	if spec.StorageMount != storage.DefaultMountPath {
		t.Errorf("StorageMount = %q, want %q", spec.StorageMount, storage.DefaultMountPath)
	}
	if spec.StorageReadOnly {
		t.Error("a writable mount was announced as read-only")
	}
}

// TestGuestMountPathIsConfigurable is the per-tenant configurable-mount feature:
// the caller's chosen path reaches the guest command line verbatim.
func TestGuestMountPathIsConfigurable(t *testing.T) {
	m, rt := newStorageManagerRT(t)
	sb, err := m.Create(context.Background(), Spec{
		Spec:    runtime.Spec{ID: "sb_a", Image: "python"},
		Storage: &storage.Mount{Prefix: "tenants/t_a", MountPath: "/data"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Stop(context.Background(), ReasonStopped)

	spec, _ := rt.LastSpec()
	if spec.StorageMount != "/data" {
		t.Errorf("StorageMount = %q, want /data", spec.StorageMount)
	}
}

// TestReadOnlyMountIsToldToGuest checks the read-only posture crosses to the
// guest, so it can mount -o ro and fail writes fast rather than round-tripping
// each one to the host only to be refused.
func TestReadOnlyMountIsToldToGuest(t *testing.T) {
	m, rt := newStorageManagerRT(t)
	sb, err := m.Create(context.Background(), Spec{
		Spec:    runtime.Spec{ID: "sb_a", Image: "python"},
		Storage: &storage.Mount{Prefix: "tenants/t_a", ReadOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Stop(context.Background(), ReasonStopped)

	spec, _ := rt.LastSpec()
	if !spec.StorageReadOnly {
		t.Error("a read-only mount was not announced to the guest")
	}
}

// TestNoBackendTellsGuestNothing is the other side: a node with no bucket must
// leave the storage flags empty, or the guest would try to mount a filesystem
// nothing is serving and hang on first access.
func TestNoBackendTellsGuestNothing(t *testing.T) {
	rt := runtimetest.New()
	logs := logstore.New(logstore.Config{})
	m := NewManager(rt, logs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sb, err := m.Create(context.Background(), Spec{Spec: runtime.Spec{ID: "sb_a", Image: "python"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Stop(context.Background(), ReasonStopped)

	spec, _ := rt.LastSpec()
	if spec.StorageMount != "" {
		t.Errorf("StorageMount = %q, want empty on a storageless node", spec.StorageMount)
	}
}

func createSandbox(t *testing.T, m *Manager, id string) (*Sandbox, *runtimetest.Instance) {
	t.Helper()
	sb, err := m.Create(context.Background(), Spec{Spec: runtime.Spec{ID: id, Image: "python"}})
	if err != nil {
		t.Fatal(err)
	}
	return sb, sb.inst.(*runtimetest.Instance)
}

func createSandboxWithMount(t *testing.T, m *Manager, id string, mount storage.Mount) (*Sandbox, *runtimetest.Instance) {
	t.Helper()
	sb, err := m.Create(context.Background(), Spec{
		Spec:    runtime.Spec{ID: id, Image: "python"},
		Storage: &mount,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb, sb.inst.(*runtimetest.Instance)
}

func putFile(t *testing.T, inst *runtimetest.Instance, path, content string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, "http://storage/objects"+path, strings.NewReader(content))
	res, err := guestHTTP(inst).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestGuestCanWriteAndReadItsOwnFiles is the end-to-end path: code inside a
// sandbox stores an object in the cloud without ever holding a credential.
func TestGuestCanWriteAndReadItsOwnFiles(t *testing.T) {
	m, backend := newStorageManager(t)
	sb, inst := createSandbox(t, m, "sb_a")
	defer sb.Stop(context.Background(), ReasonStopped)

	client := guestHTTP(inst)

	req, _ := http.NewRequest(http.MethodPut, "http://storage/objects/out/result.json",
		strings.NewReader(`{"answer":42}`))
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("the guest could not reach its storage server: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT from the guest returned %d, want 204", res.StatusCode)
	}

	// It landed under this sandbox's prefix on the host side, which is the part
	// the guest cannot see and cannot influence.
	rc, err := backend.Get(context.Background(), "sandboxes/sb_a/out/result.json", 0, -1)
	if err != nil {
		t.Fatalf("the object is not under the sandbox's prefix: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != `{"answer":42}` {
		t.Errorf("stored %q", got)
	}

	// And the guest reads back what it wrote, by the name it used.
	res, err = client.Get("http://storage/objects/out/result.json")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ = io.ReadAll(res.Body)
	if string(got) != `{"answer":42}` {
		t.Errorf("the guest read back %q", got)
	}
}

// TestOneSandboxCannotReachAnother is the security property the whole design
// exists for, tested the only way that means anything: with two real sandboxes,
// each speaking through its own socket.
//
// Sandbox A writes a secret. Sandbox B then asks for every name that secret
// could plausibly have. None of them may work -- not because B is refused, but
// because B's socket answers only for B, and B's prefix simply does not contain
// the file. There is no request B can construct that changes that.
func TestOneSandboxCannotReachAnother(t *testing.T) {
	m, backend := newStorageManager(t)

	sbA, instA := createSandbox(t, m, "sb_a")
	defer sbA.Stop(context.Background(), ReasonStopped)
	sbB, instB := createSandbox(t, m, "sb_b")
	defer sbB.Stop(context.Background(), ReasonStopped)

	// A stores something worth stealing.
	req, _ := http.NewRequest(http.MethodPut, "http://storage/objects/secret.txt",
		strings.NewReader("sandbox A's private data"))
	res, err := guestHTTP(instA).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if _, err := backend.Head(context.Background(), "sandboxes/sb_a/secret.txt"); err != nil {
		t.Fatalf("precondition: A's secret was not stored: %v", err)
	}

	// B tries every spelling it has.
	clientB := guestHTTP(instB)
	attempts := []string{
		"/objects/secret.txt",         // its own namespace, where the file is not
		"/objects/../sb_a/secret.txt", // climb out
		"/objects/../../sandboxes/sb_a/secret.txt",
		"/objects/sandboxes/sb_a/secret.txt", // guess the host layout
		"/objects//../sb_a/secret.txt",
		"/objects/./../sb_a/secret.txt",
	}

	for _, path := range attempts {
		req, err := http.NewRequest(http.MethodGet, "http://storage", nil)
		if err != nil {
			t.Fatal(err)
		}
		// Opaque, so the client sends the path exactly as written instead of
		// helpfully resolving the dots away before the server ever sees them.
		req.URL.Opaque = path

		res, err := clientB.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if res.StatusCode == http.StatusOK {
			t.Errorf("sandbox B read %s and got %q -- the isolation is gone", path, body)
		}
		if strings.Contains(string(body), "private data") {
			t.Errorf("sandbox B got A's secret via %s", path)
		}
	}
}

// TestStorageServerDiesWithItsSandbox checks the goroutine does not outlive the
// VM. A server still accepting on a stopped sandbox's socket is both a leak and
// a socket that should not exist.
func TestStorageServerDiesWithItsSandbox(t *testing.T) {
	m, _ := newStorageManager(t)
	sb, inst := createSandbox(t, m, "sb_a")

	client := guestHTTP(inst)
	res, err := client.Get("http://storage/usage")
	if err != nil {
		t.Fatalf("precondition: storage is not up: %v", err)
	}
	res.Body.Close()

	if err := sb.Stop(context.Background(), ReasonStopped); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Get("http://storage/usage"); err == nil {
		t.Error("the storage server still answers for a sandbox that is gone")
	}
}

// TestNoBackendMeansNoStorage checks the default. A deployment that never named
// a bucket must not have sandboxes quietly writing somewhere.
func TestNoBackendMeansNoStorage(t *testing.T) {
	m := NewManager(runtimetest.New(), logstore.New(logstore.Config{}),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	sb, err := m.Create(context.Background(), Spec{Spec: runtime.Spec{ID: "sb_a", Image: "python"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Stop(context.Background(), ReasonStopped)

	inst := sb.inst.(*runtimetest.Instance)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Nothing is accepting, so the dial blocks until the context gives up.
	if _, err := inst.Guest().Dial(ctx); err == nil {
		t.Error("something is serving storage on a manager that was given no backend")
	}
}

// --- per-tenant mounts (#33) ------------------------------------------------

// TestSameTenantSharesFilesAcrossSandboxes is the point of tenant scoping: a
// file written by one sandbox is there for the next, because both mount the
// same namespace. This is what the old per-sandbox prefix could not do.
func TestSameTenantSharesFilesAcrossSandboxes(t *testing.T) {
	m, _ := newStorageManager(t)
	mount := storage.Mount{Prefix: TenantPrefix("t_acme")}

	// First run writes.
	sb1, inst1 := createSandboxWithMount(t, m, "sb_first", mount)
	res := putFile(t, inst1, "/state/counter", "1")
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("write failed: %d", res.StatusCode)
	}
	sb1.Stop(context.Background(), ReasonStopped) // the VM is gone...

	// A second, later sandbox for the same tenant reads it back.
	_, inst2 := createSandboxWithMount(t, m, "sb_second", mount)
	res, err := guestHTTP(inst2).Get("http://storage/objects/state/counter")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(body) != "1" {
		t.Errorf("the second sandbox could not read the first's file: status %d, body %q", res.StatusCode, body)
	}
}

// TestDifferentTenantsAreIsolated is the isolation half. Tenant B, given every
// spelling of tenant A's file, reaches none of it -- because B's mount simply
// does not contain it.
func TestDifferentTenantsAreIsolated(t *testing.T) {
	m, _ := newStorageManager(t)

	_, instA := createSandboxWithMount(t, m, "sb_a", storage.Mount{Prefix: TenantPrefix("t_alice")})
	_, instB := createSandboxWithMount(t, m, "sb_b", storage.Mount{Prefix: TenantPrefix("t_bob")})

	// The sentinel must not appear in any tenant name or path, or a rejection's
	// error message -- which echoes the attempted path -- would look like a leak.
	const sentinel = "ZZZ_PAYLOAD_ZZZ"
	res := putFile(t, instA, "/secret", sentinel)
	res.Body.Close()

	clientB := guestHTTP(instB)
	for _, path := range []string{
		"/objects/secret",
		"/objects/../t_alice/secret",
		"/objects/../../tenants/t_alice/secret",
	} {
		req, _ := http.NewRequest(http.MethodGet, "http://storage", nil)
		req.URL.Opaque = path
		res, err := clientB.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode == http.StatusOK || strings.Contains(string(body), sentinel) {
			t.Errorf("tenant B reached A's file via %s: status %d, body %q", path, res.StatusCode, body)
		}
	}
}

// TestReadOnlyMountRefusesWrites checks the read-only posture a token (or a
// tightening request) can impose.
func TestReadOnlyMountRefusesWrites(t *testing.T) {
	m, _ := newStorageManager(t)
	_, inst := createSandboxWithMount(t, m, "sb_ro",
		storage.Mount{Prefix: TenantPrefix("t_ro"), ReadOnly: true})

	res := putFile(t, inst, "/x", "nope")
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("write to a read-only mount returned %d, want 403", res.StatusCode)
	}
}

// TestStorageInfoReportsTheNamespace checks that a caller can see where their
// files persist -- the tenant leaf, not the host-side category.
func TestStorageInfoReportsTheNamespace(t *testing.T) {
	m, _ := newStorageManager(t)
	sb, _ := createSandboxWithMount(t, m, "sb_x",
		storage.Mount{Prefix: TenantPrefix("t_visible"), ReadOnly: true})
	defer sb.Stop(context.Background(), ReasonStopped)

	info := sb.Info()
	if info.Storage == nil {
		t.Fatal("a sandbox with storage reported none")
	}
	if info.Storage.Namespace != "t_visible" {
		t.Errorf("namespace = %q, want t_visible (the leaf, not the prefix)", info.Storage.Namespace)
	}
	if !info.Storage.ReadOnly {
		t.Error("read-only mount reported as writable")
	}
}

// TestEvictionThroughTheGuestPath is the whole chain: a guest writes over vsock
// into a tenant with an evict policy, and the oldest object is deleted to make
// room -- exercising Store.Create -> TenantPolicy.Admit -> backend.Delete, none
// of it mocked.
func TestEvictionThroughTheGuestPath(t *testing.T) {
	m, backend := newStorageManager(t)
	mount := storage.Mount{
		Prefix: TenantPrefix("t_cache"),
		Tenant: storage.TenantPolicy{MaxBytes: 10, OnFull: storage.Evict},
	}
	_, inst := createSandboxWithMount(t, m, "sb_cache", mount)

	// Fill the tenant to its cap with two 5-byte objects.
	putFile(t, inst, "/old", "aaaaa").Body.Close()
	putFile(t, inst, "/new", "bbbbb").Body.Close()

	// A third 5-byte write must evict the oldest ("old") to fit.
	res := putFile(t, inst, "/newest", "ccccc")
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("evicting write returned %d, want 204", res.StatusCode)
	}

	keys := backend.Keys()
	has := func(suffix string) bool {
		for _, k := range keys {
			if strings.HasSuffix(k, suffix) {
				return true
			}
		}
		return false
	}
	if has("/old") {
		t.Error("the oldest object was not evicted")
	}
	if !has("/new") || !has("/newest") {
		t.Errorf("eviction deleted the wrong objects; keys = %v", keys)
	}
}

// TestPreserveThroughTheGuestPath is the other policy over the same chain: a full
// tenant with preserve rejects the write with EDQUOT and keeps everything.
func TestPreserveThroughTheGuestPath(t *testing.T) {
	m, backend := newStorageManager(t)
	mount := storage.Mount{
		Prefix: TenantPrefix("t_audit"),
		Tenant: storage.TenantPolicy{MaxBytes: 10, OnFull: storage.Preserve},
	}
	_, inst := createSandboxWithMount(t, m, "sb_audit", mount)

	putFile(t, inst, "/a", "aaaaa").Body.Close()
	putFile(t, inst, "/b", "bbbbb").Body.Close()

	res := putFile(t, inst, "/c", "ccccc")
	res.Body.Close()
	if res.StatusCode != http.StatusInsufficientStorage {
		t.Errorf("preserve returned %d, want 507", res.StatusCode)
	}
	if n := len(backend.Keys()); n != 2 {
		t.Errorf("preserve changed the object count to %d, want 2", n)
	}
}
