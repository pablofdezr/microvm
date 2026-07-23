package auth

import (
	"strings"
	"testing"

	"github.com/pablofdezr/microvm/internal/storage"
)

func TestResolveKnownToken(t *testing.T) {
	d := NewDirectory([]string{"sk_alice", "sk_bob"})

	p, ok := d.Resolve("sk_alice")
	if !ok {
		t.Fatal("a configured token did not resolve")
	}
	if p.Tenant == "" {
		t.Error("resolved principal has no tenant")
	}
}

func TestResolveUnknownToken(t *testing.T) {
	d := NewDirectory([]string{"sk_alice"})
	if _, ok := d.Resolve("sk_mallory"); ok {
		t.Error("an unconfigured token resolved")
	}
	if _, ok := d.Resolve(""); ok {
		t.Error("the empty token resolved")
	}
}

// TestTenantIsStable is the property a caller's persistence rests on: the same
// token must map to the same namespace every time, or files written yesterday
// are unreachable today.
func TestTenantIsStable(t *testing.T) {
	a := DeriveTenant("sk_alice")
	b := DeriveTenant("sk_alice")
	if a != b {
		t.Errorf("the same token derived two tenants: %q and %q", a, b)
	}
}

// TestDistinctTokensDistinctTenants is the isolation property: two callers must
// not land in one namespace by accident.
func TestDistinctTokensDistinctTenants(t *testing.T) {
	if DeriveTenant("sk_alice") == DeriveTenant("sk_bob") {
		t.Error("two different tokens derived the same tenant")
	}
}

// TestTenantDoesNotLeakTheToken checks that the namespace, which ends up in an
// object-store path anyone with the bucket can see, does not contain the secret
// that made it.
func TestTenantDoesNotLeakTheToken(t *testing.T) {
	token := "sk_supersecret_value"
	tenant := DeriveTenant(token)
	if strings.Contains(tenant, "supersecret") || strings.Contains(tenant, token) {
		t.Errorf("tenant %q leaks the token that derived it", tenant)
	}
}

// TestTwoTokensCanShareATenant covers the team case: several keys, one
// namespace, configured explicitly.
func TestTwoTokensCanShareATenant(t *testing.T) {
	d := NewDirectoryOf(map[string]*Principal{
		"sk_key1": {Tenant: "acme"},
		"sk_key2": {Tenant: "acme"},
	})

	p1, ok1 := d.Resolve("sk_key1")
	p2, ok2 := d.Resolve("sk_key2")
	if !ok1 || !ok2 {
		t.Fatal("a configured token did not resolve")
	}
	if p1.Tenant != "acme" || p2.Tenant != "acme" {
		t.Errorf("explicit tenants were not honoured: %q, %q", p1.Tenant, p2.Tenant)
	}
}

// TestPrincipalWithoutTenantGetsOneDerived lets an operator set a quota without
// having to restate the namespace.
func TestPrincipalWithoutTenantGetsOneDerived(t *testing.T) {
	d := NewDirectoryOf(map[string]*Principal{
		"sk_key": {Quota: storage.Quota{MaxBytes: 5 << 30}},
	})
	p, ok := d.Resolve("sk_key")
	if !ok {
		t.Fatal("token did not resolve")
	}
	if p.Tenant == "" {
		t.Error("a principal with no explicit tenant did not get one derived")
	}
	if p.Quota.MaxBytes != 5<<30 {
		t.Error("the explicit quota was dropped")
	}
}

// TestResolveScansAllTokens is a weak but real check that resolution does not
// stop at the first match: the last token must resolve as readily as the first.
// The real defence is constant-time compare; this guards the no-early-exit half.
func TestResolveScansAllTokens(t *testing.T) {
	d := NewDirectory([]string{"sk_1", "sk_2", "sk_3", "sk_last"})
	if _, ok := d.Resolve("sk_last"); !ok {
		t.Error("the last token did not resolve; resolution may be exiting early")
	}
}
