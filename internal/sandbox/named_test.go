package sandbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/runtime"
)

// named creates a sandbox with a name for a tenant, over the admission fake.
func named(t *testing.T, m *Manager, id, tenant, name string) (*Sandbox, error) {
	t.Helper()
	return m.Create(context.Background(), Spec{
		Spec:   runtime.Spec{ID: id, Image: "python"},
		Tenant: tenant,
		Name:   name,
	})
}

// A name is a handle, so a second sandbox claiming a running one's name is
// refused rather than quietly booted -- the caller asked for "the build sandbox"
// and there can only be one.
func TestNamedCreateRefusesADuplicate(t *testing.T) {
	m, _ := newAdmissionManager(t)

	if _, err := named(t, m, "sb_1", "acme", "build"); err != nil {
		t.Fatalf("first named create: %v", err)
	}

	_, err := named(t, m, "sb_2", "acme", "build")
	var conflict *NameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("second create of the same name: err = %v, want a NameConflictError", err)
	}
	if conflict.Name != "build" {
		t.Errorf("conflict names %q, want \"build\"", conflict.Name)
	}
}

// get-or-create is the whole point of a name: the same name hands back the VM it
// already refers to rather than booting a second one, and says it did not create.
func TestGetOrCreateReturnsTheExistingSandbox(t *testing.T) {
	m, _ := newAdmissionManager(t)

	first, created, err := m.GetOrCreate(context.Background(), Spec{
		Spec: runtime.Spec{ID: "sb_1", Image: "python"}, Tenant: "acme", Name: "build",
	})
	if err != nil {
		t.Fatalf("first get-or-create: %v", err)
	}
	if !created {
		t.Fatal("first get-or-create did not report that it created the sandbox")
	}

	// A different ID in the spec, to prove the existing sandbox is returned rather
	// than a new one built from these fields.
	second, created, err := m.GetOrCreate(context.Background(), Spec{
		Spec: runtime.Spec{ID: "sb_2", Image: "node"}, Tenant: "acme", Name: "build",
	})
	if err != nil {
		t.Fatalf("second get-or-create: %v", err)
	}
	if created {
		t.Error("second get-or-create booted a second sandbox instead of resolving the name")
	}
	if second.ID() != first.ID() {
		t.Errorf("get-or-create returned %s, want the existing %s", second.ID(), first.ID())
	}
}

// A name belongs to a tenant, so two tenants may both have a "build" and neither
// collides with the other -- the same rule the concurrency cap follows.
func TestNamesAreScopedPerTenant(t *testing.T) {
	m, _ := newAdmissionManager(t)

	a, err := named(t, m, "sb_a", "tenant-a", "build")
	if err != nil {
		t.Fatalf("tenant a: %v", err)
	}
	b, err := named(t, m, "sb_b", "tenant-b", "build")
	if err != nil {
		t.Fatalf("tenant b refused a name tenant a holds: %v", err)
	}
	if a.ID() == b.ID() {
		t.Fatal("two tenants' same-named sandboxes resolved to one VM")
	}

	// And one tenant cannot resolve another's name.
	if _, ok := m.GetByName("tenant-a", "build"); !ok {
		t.Error("tenant a cannot find its own named sandbox")
	}
	if got, ok := m.GetByName("tenant-b", "build"); !ok || got.ID() != b.ID() {
		t.Error("tenant b resolved to the wrong sandbox")
	}
}

// The name frees the moment the sandbox stops, even though the record lingers for
// the retention window: a caller must be able to re-create "build" at once.
func TestANameFreesWhenItsSandboxStops(t *testing.T) {
	m, _ := newAdmissionManager(t)

	first, err := named(t, m, "sb_1", "acme", "build")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Stop(context.Background(), ReasonStopped); err != nil {
		t.Fatal(err)
	}

	// Gone from the name index the instant it stopped, though still listed by ID.
	if _, ok := m.GetByName("acme", "build"); ok {
		t.Error("a stopped sandbox is still resolvable by name")
	}

	second, err := named(t, m, "sb_2", "acme", "build")
	if err != nil {
		t.Fatalf("re-creating a name whose sandbox stopped: %v", err)
	}
	if second.ID() == first.ID() {
		t.Fatal("re-create returned the stopped sandbox rather than a fresh one")
	}
}

// A named create that never booted must free the name, or a node failing to boot
// would wedge a name for the daemon's life.
func TestAFailedBootFreesTheName(t *testing.T) {
	m, rt := newAdmissionManager(t)

	rt.CreateErr = errors.New("no free network slot")
	if _, err := named(t, m, "sb_1", "acme", "build"); err == nil {
		t.Fatal("a failing runtime produced a named sandbox")
	}

	rt.CreateErr = nil
	if _, err := named(t, m, "sb_2", "acme", "build"); err != nil {
		t.Fatalf("a failed boot held the name: %v", err)
	}
}

// Anonymous creates never collide: no name, nothing to reserve.
func TestUnnamedCreatesDoNotConflict(t *testing.T) {
	m, _ := newAdmissionManager(t)
	for _, id := range []string{"sb_1", "sb_2", "sb_3"} {
		if _, err := named(t, m, id, "acme", ""); err != nil {
			t.Fatalf("anonymous create %s: %v", id, err)
		}
	}
}

// The reservation holds under concurrency: many creates racing one name must
// yield exactly one sandbox, not one-per-goroutine that then fight to publish.
func TestOneNameSurvivesConcurrentCreates(t *testing.T) {
	m, rt := newAdmissionManager(t)
	rt.CreateDelay = 20 * time.Millisecond // widen the window every racer is inside

	var (
		wg       sync.WaitGroup
		accepted atomic.Int64
	)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A distinct ID each, so only the name can be what refuses them.
			if _, err := named(t, m, sbID(i), "acme", "build"); err == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := accepted.Load(); got != 1 {
		t.Errorf("%d of 20 creates racing one name were accepted, want 1", got)
	}
}

func sbID(i int) string {
	return "sb_race_" + string(rune('a'+i))
}

func TestValidateName(t *testing.T) {
	ok := []string{"build", "web-1", "a.b.c", "A_B", "0", "x-y_z.9"}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}

	bad := []string{
		"",                                   // empty
		"has space",                          // whitespace
		"has:colon",                          // colon
		"emoji😀",                             // outside the alphabet
		"slash/name",                         // path separator
		string(make([]byte, MaxNameBytes+1)), // too long (also all-NUL, doubly refused)
	}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", n)
		}
	}
}
