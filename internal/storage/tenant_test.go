package storage

import (
	"context"
	"strings"
	"testing"
	"time"
)

// putAt writes an object with a chosen modification time, so a test can control
// what "oldest" means without sleeping.
func putAt(t *testing.T, m *Memory, key, content string, modTime time.Time) {
	t.Helper()
	if err := m.Put(context.Background(), key, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	m.setModTime(key, modTime)
}

func TestTenantUnlimitedAdmitsEverything(t *testing.T) {
	m := NewMemory()
	p := TenantPolicy{MaxBytes: 0} // unlimited
	if err := p.Admit(context.Background(), m, "tenants/t", "tenants/t/x", 1<<40); err != nil {
		t.Errorf("an unlimited tenant refused a write: %v", err)
	}
}

func TestPreserveRejectsWhenFull(t *testing.T) {
	m := NewMemory()
	putAt(t, m, "tenants/t/a", strings.Repeat("x", 8), time.Unix(100, 0))
	p := TenantPolicy{MaxBytes: 10, OnFull: Preserve}

	// 8 used, 10 cap, a 3-byte write does not fit (8+3 > 10).
	err := p.Admit(context.Background(), m, "tenants/t", "tenants/t/b", 3)
	if err == nil {
		t.Fatal("preserve admitted a write that overflows the tenant")
	}
	// And it left the existing object alone.
	if _, err := m.Head(context.Background(), "tenants/t/a"); err != nil {
		t.Error("preserve deleted an object; it must never delete")
	}
}

func TestPreserveAdmitsExactFill(t *testing.T) {
	m := NewMemory()
	putAt(t, m, "tenants/t/a", strings.Repeat("x", 7), time.Unix(100, 0))
	p := TenantPolicy{MaxBytes: 10, OnFull: Preserve}

	// 7 used + 3 = 10 = cap. Exact fill is not overflow -- the same boundary the
	// per-sandbox counter got wrong twice before.
	if err := p.Admit(context.Background(), m, "tenants/t", "tenants/t/b", 3); err != nil {
		t.Errorf("preserve refused a write that exactly fills the tenant: %v", err)
	}
}

// TestEvictDeletesOldestFirst is the heart of the feature. Three objects, the
// tenant full; a new write must delete the OLDEST and only as many as needed.
func TestEvictDeletesOldestFirst(t *testing.T) {
	m := NewMemory()
	putAt(t, m, "tenants/t/old", strings.Repeat("x", 4), time.Unix(100, 0))
	putAt(t, m, "tenants/t/mid", strings.Repeat("x", 4), time.Unix(200, 0))
	putAt(t, m, "tenants/t/new", strings.Repeat("x", 2), time.Unix(300, 0))
	// 10 used, cap 10. A 4-byte write needs 4 bytes freed.
	p := TenantPolicy{MaxBytes: 10, OnFull: Evict}

	if err := p.Admit(context.Background(), m, "tenants/t", "tenants/t/incoming", 4); err != nil {
		t.Fatalf("evict refused a write it should have made room for: %v", err)
	}

	// The oldest (4 bytes) is gone; that alone freed enough, so nothing else is.
	if _, err := m.Head(context.Background(), "tenants/t/old"); err == nil {
		t.Error("the oldest object survived eviction")
	}
	if _, err := m.Head(context.Background(), "tenants/t/mid"); err != nil {
		t.Error("eviction deleted more than it needed to")
	}
	if _, err := m.Head(context.Background(), "tenants/t/new"); err != nil {
		t.Error("eviction deleted the newest object")
	}
}

// TestEvictStopsAsSoonAsItFits guards against over-deletion: freeing one object
// is enough, so a second must not be touched even though the tenant is still
// nearly full.
func TestEvictStopsAsSoonAsItFits(t *testing.T) {
	m := NewMemory()
	putAt(t, m, "tenants/t/a", strings.Repeat("x", 5), time.Unix(100, 0))
	putAt(t, m, "tenants/t/b", strings.Repeat("x", 5), time.Unix(200, 0))
	p := TenantPolicy{MaxBytes: 10, OnFull: Evict}

	// One 3-byte write: freeing 'a' (5 bytes) leaves 5+3=8 <= 10. 'b' stays.
	if err := p.Admit(context.Background(), m, "tenants/t", "tenants/t/c", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Head(context.Background(), "tenants/t/b"); err != nil {
		t.Error("eviction deleted a second object it did not need to")
	}
}

// TestOverwriteDoesNotDoubleCount checks the targetKey exclusion: replacing an
// object frees its old bytes, so a write that fits by replacement must not be
// treated as if it adds on top.
func TestOverwriteDoesNotDoubleCount(t *testing.T) {
	m := NewMemory()
	putAt(t, m, "tenants/t/file", strings.Repeat("x", 9), time.Unix(100, 0))
	p := TenantPolicy{MaxBytes: 10, OnFull: Preserve}

	// Overwriting the 9-byte file with a 10-byte one: 10 <= cap once its own old
	// 9 bytes are excluded. A naive total (9 + 10) would wrongly reject.
	if err := p.Admit(context.Background(), m, "tenants/t", "tenants/t/file", 10); err != nil {
		t.Errorf("overwrite was double-counted against the cap: %v", err)
	}
}

// TestObjectBiggerThanCapIsRejectedNotEmptied checks that a hopeless write does
// not first delete the whole tenant and then fail anyway.
func TestObjectBiggerThanCapIsRejectedNotEmptied(t *testing.T) {
	m := NewMemory()
	putAt(t, m, "tenants/t/keep", strings.Repeat("x", 4), time.Unix(100, 0))
	p := TenantPolicy{MaxBytes: 10, OnFull: Evict}

	if err := p.Admit(context.Background(), m, "tenants/t", "tenants/t/huge", 11); err == nil {
		t.Fatal("a write larger than the whole cap was admitted")
	}
	if _, err := m.Head(context.Background(), "tenants/t/keep"); err != nil {
		t.Error("a doomed write evicted existing data before failing")
	}
}

func TestUnknownSizeUnderCapIsRefused(t *testing.T) {
	m := NewMemory()
	p := TenantPolicy{MaxBytes: 10, OnFull: Preserve}
	if err := p.Admit(context.Background(), m, "tenants/t", "tenants/t/x", -1); err == nil {
		t.Error("a streaming write with no length was admitted under a byte cap")
	}
}

// TestPrefixBoundary makes sure one tenant's usage is not another's: a scan of
// "tenants/t" must not count "tenants/t2".
func TestPrefixBoundary(t *testing.T) {
	m := NewMemory()
	putAt(t, m, "tenants/t/a", strings.Repeat("x", 9), time.Unix(100, 0))
	putAt(t, m, "tenants/t2/a", strings.Repeat("x", 9), time.Unix(100, 0))
	p := TenantPolicy{MaxBytes: 10, OnFull: Preserve}

	// t holds 9. A 1-byte write fits (9+1=10). If t2's 9 bytes leaked in, it would
	// wrongly reject.
	if err := p.Admit(context.Background(), m, "tenants/t", "tenants/t/b", 1); err != nil {
		t.Errorf("a neighbouring tenant's usage leaked into the cap: %v", err)
	}
}
