package tenant_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/storage"
	"github.com/pablofdezr/microvm/internal/tenant"
)

// testRedisAddr is the server every Redis test shares, or "" if there is none.
var testRedisAddr string

// TestMain starts a throwaway Redis for the suite, mirroring internal/queue: its
// own server on a random port with no persistence, so the tests can never
// silently attach to a developer's -- or production's -- Redis. Absent
// redis-server, the Redis tests skip and the rest of the package still runs.
func TestMain(m *testing.M) {
	addr, stop, err := startTestRedis()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tenant redis tests will skip: %v\n", err)
	} else {
		testRedisAddr = addr
	}
	code := m.Run()
	if stop != nil {
		stop()
	}
	os.Exit(code)
}

// storeFactory builds a fresh, empty Store. A shared Redis needs a unique prefix
// per store, or one test's leftovers become another's mystery.
type storeFactory func(t *testing.T) tenant.Store

// runConformance is the contract both Memory and Redis must satisfy. The split
// between deciding a policy and enforcing it is only real if every backend
// behaves identically here.
func runConformance(t *testing.T, newStore storeFactory) {
	t.Run("UnsetIsUnlimited", func(t *testing.T) {
		s := newStore(t)
		_, ok, err := s.Get(context.Background(), "t_never_set")
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("a tenant nobody configured reported a policy; it should read as unlimited")
		}
	})

	t.Run("SetThenGet", func(t *testing.T) {
		s := newStore(t)
		want := storage.TenantPolicy{MaxBytes: 1 << 20, OnFull: storage.Evict}
		if err := s.Set(context.Background(), "t_a", want); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.Get(context.Background(), "t_a")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("policy not found after Set")
		}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("SetReplaces", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if err := s.Set(ctx, "t_a", storage.TenantPolicy{MaxBytes: 100, OnFull: storage.Preserve}); err != nil {
			t.Fatal(err)
		}
		if err := s.Set(ctx, "t_a", storage.TenantPolicy{MaxBytes: 200, OnFull: storage.Evict}); err != nil {
			t.Fatal(err)
		}
		got, _, err := s.Get(ctx, "t_a")
		if err != nil {
			t.Fatal(err)
		}
		if got.MaxBytes != 200 || got.OnFull != storage.Evict {
			t.Fatalf("second Set did not replace the first: %+v", got)
		}
	})

	t.Run("ListReturnsAllAndOnlySet", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if err := s.Set(ctx, "t_a", storage.TenantPolicy{MaxBytes: 1, OnFull: storage.Preserve}); err != nil {
			t.Fatal(err)
		}
		if err := s.Set(ctx, "t_b", storage.TenantPolicy{MaxBytes: 2, OnFull: storage.Evict}); err != nil {
			t.Fatal(err)
		}
		recs, err := s.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		byTenant := map[string]storage.TenantPolicy{}
		for _, r := range recs {
			byTenant[r.Tenant] = r.Policy
		}
		if len(byTenant) != 2 {
			t.Fatalf("List returned %d tenants, want 2 (%+v)", len(byTenant), recs)
		}
		if byTenant["t_a"].MaxBytes != 1 || byTenant["t_b"].OnFull != storage.Evict {
			t.Fatalf("List lost a field: %+v", byTenant)
		}
	})

	t.Run("EmptyListIsEmptyNotError", func(t *testing.T) {
		s := newStore(t)
		recs, err := s.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 0 {
			t.Fatalf("a fresh store listed %d records", len(recs))
		}
	})
}

func TestMemoryStore(t *testing.T) {
	runConformance(t, func(t *testing.T) tenant.Store {
		return tenant.NewMemory()
	})
}

func TestRedisStore(t *testing.T) {
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}
	runConformance(t, func(t *testing.T) tenant.Store {
		return newRedisForTest(t)
	})
}

// TestRedisTwoNodesShareState is the property the whole Redis store exists for:
// a limit set through one node's store is visible through another's, because a
// fleet's admin call must not depend on which node it reached.
func TestRedisTwoNodesShareState(t *testing.T) {
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}
	prefix := uniquePrefix()
	nodeA, err := tenant.NewRedis(context.Background(), tenant.RedisConfig{Addr: testRedisAddr, Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	defer nodeA.Close()
	nodeB, err := tenant.NewRedis(context.Background(), tenant.RedisConfig{Addr: testRedisAddr, Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	defer nodeB.Close()

	want := storage.TenantPolicy{MaxBytes: 5 << 20, OnFull: storage.Evict}
	if err := nodeA.Set(context.Background(), "t_shared", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := nodeB.Get(context.Background(), "t_shared")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != want {
		t.Fatalf("node B saw %+v (ok=%v), want %+v set by node A", got, ok, want)
	}
}

// TestRedisSeparatePrefixesDoNotShare is the flip side: two fleets on one Redis
// must not read each other's limits.
func TestRedisSeparatePrefixesDoNotShare(t *testing.T) {
	if testRedisAddr == "" {
		t.Skip("redis-server is not on PATH")
	}
	fleet1, err := tenant.NewRedis(context.Background(), tenant.RedisConfig{Addr: testRedisAddr, Prefix: uniquePrefix()})
	if err != nil {
		t.Fatal(err)
	}
	defer fleet1.Close()
	fleet2, err := tenant.NewRedis(context.Background(), tenant.RedisConfig{Addr: testRedisAddr, Prefix: uniquePrefix()})
	if err != nil {
		t.Fatal(err)
	}
	defer fleet2.Close()

	if err := fleet1.Set(context.Background(), "t_a", storage.TenantPolicy{MaxBytes: 1, OnFull: storage.Preserve}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := fleet2.Get(context.Background(), "t_a"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("a policy leaked across prefixes; two fleets on one Redis are not isolated")
	}
}

func newRedisForTest(t *testing.T) tenant.Store {
	t.Helper()
	s, err := tenant.NewRedis(context.Background(), tenant.RedisConfig{
		Addr:   testRedisAddr,
		Prefix: uniquePrefix(),
	})
	if err != nil {
		t.Fatalf("connect test redis: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var prefixCounter atomic.Int64

func uniquePrefix() string {
	return fmt.Sprintf("test-tenant-%d", prefixCounter.Add(1))
}

// --- throwaway redis-server, as in internal/queue ------------------------

func startTestRedis() (addr string, stop func(), err error) {
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		return "", nil, fmt.Errorf("redis-server is not on PATH")
	}
	port, err := freePort()
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "microvm-tenant-redis-")
	if err != nil {
		return "", nil, err
	}
	cmd := exec.Command(bin,
		"--port", fmt.Sprint(port),
		"--bind", "127.0.0.1",
		"--dir", dir,
		"--save", "",
		"--appendonly", "no",
	)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("start redis-server: %w", err)
	}
	stop = func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		os.RemoveAll(dir)
	}
	addr = fmt.Sprintf("127.0.0.1:%d", port)
	if err := waitForRedis(addr, 10*time.Second); err != nil {
		stop()
		return "", nil, err
	}
	return addr, stop, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitForRedis(addr string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("redis at %s did not come up within %v", addr, within)
}
