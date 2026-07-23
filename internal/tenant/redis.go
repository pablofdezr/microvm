package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pablofdezr/microvm/internal/storage"
)

// Redis is a Store shared by every node in a fleet.
//
// It is the same contract as Memory -- an admin sets a tenant's policy on one
// node and every node honours it -- with the two things a process-local map
// cannot give a fleet: the policy survives a node restart, and every node reads
// the same value. It mirrors the task queue's memory/Redis split (see
// internal/queue); the difference is that this needs no Lua. The whole store is
// one Redis hash, so every operation touches a single key and is atomic on its
// own -- there is no read-then-write race here for a script to close, because a
// policy is a value replaced wholesale, not a queue mutated in place.
type Redis struct {
	rdb *redis.Client
	// key is the one hash holding every tenant's policy, field = tenant. It is
	// hash-tagged so Redis Cluster keeps it in a single slot, matching the
	// queue's namespace convention.
	key string
}

// RedisConfig configures a Redis store.
type RedisConfig struct {
	// Addr is "host:port" or a "redis://" / "rediss://" URL.
	Addr     string
	Password string
	DB       int

	// Prefix namespaces the key, so one Redis can serve several fleets and two
	// fleets never read each other's limits.
	Prefix string
}

// NewRedis connects to Redis and returns a store. It pings before returning, so
// a wrong address fails at startup rather than on the first admin call -- the
// same eager check the queue makes, for the same reason.
func NewRedis(ctx context.Context, cfg RedisConfig) (*Redis, error) {
	if cfg.Prefix == "" {
		cfg.Prefix = "microvm"
	}

	var opts *redis.Options
	if strings.Contains(cfg.Addr, "://") {
		parsed, err := redis.ParseURL(cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("parse redis URL: %w", err)
		}
		opts = parsed
	} else {
		opts = &redis.Options{Addr: cfg.Addr, Password: cfg.Password, DB: cfg.DB}
	}

	rdb := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("cannot reach redis at %s: %w", cfg.Addr, err)
	}

	return &Redis{
		rdb: rdb,
		key: "{" + cfg.Prefix + "}:tenant:policies",
	}, nil
}

// policyWire is the stored form of a policy.
//
// A local struct with its own tags rather than serialising storage.TenantPolicy
// directly: the wire format is a compatibility surface -- a value written by one
// version is read by another during a rolling deploy -- and owning it here makes
// any change to it a change in one visible place.
type policyWire struct {
	MaxBytes int64  `json:"max_bytes"`
	OnFull   string `json:"on_full"`
}

func (r *Redis) Get(ctx context.Context, tenant string) (storage.TenantPolicy, bool, error) {
	raw, err := r.rdb.HGet(ctx, r.key, tenant).Result()
	if err == redis.Nil {
		// No policy set is not an error: it means unlimited, the pre-existing
		// default, exactly as the Memory store returns false here.
		return storage.TenantPolicy{}, false, nil
	}
	if err != nil {
		return storage.TenantPolicy{}, false, fmt.Errorf("redis hget tenant policy: %w", err)
	}
	policy, err := decodePolicy(raw)
	if err != nil {
		return storage.TenantPolicy{}, false, fmt.Errorf("tenant %q: %w", tenant, err)
	}
	return policy, true, nil
}

func (r *Redis) Set(ctx context.Context, tenant string, policy storage.TenantPolicy) error {
	b, err := json.Marshal(policyWire{MaxBytes: policy.MaxBytes, OnFull: string(policy.OnFull)})
	if err != nil {
		return fmt.Errorf("encode tenant policy: %w", err)
	}
	if err := r.rdb.HSet(ctx, r.key, tenant, b).Err(); err != nil {
		return fmt.Errorf("redis hset tenant policy: %w", err)
	}
	return nil
}

func (r *Redis) List(ctx context.Context) ([]Record, error) {
	all, err := r.rdb.HGetAll(ctx, r.key).Result()
	if err != nil {
		return nil, fmt.Errorf("redis hgetall tenant policies: %w", err)
	}
	out := make([]Record, 0, len(all))
	for tenant, raw := range all {
		policy, err := decodePolicy(raw)
		if err != nil {
			return nil, fmt.Errorf("tenant %q: %w", tenant, err)
		}
		out = append(out, Record{Tenant: tenant, Policy: policy})
	}
	return out, nil
}

// Close releases the Redis connection.
func (r *Redis) Close() error { return r.rdb.Close() }

func decodePolicy(raw string) (storage.TenantPolicy, error) {
	var w policyWire
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return storage.TenantPolicy{}, fmt.Errorf("decode policy: %w", err)
	}
	return storage.TenantPolicy{MaxBytes: w.MaxBytes, OnFull: storage.FullAction(w.OnFull)}, nil
}

var _ Store = (*Redis)(nil)
