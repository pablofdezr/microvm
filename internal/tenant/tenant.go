// Package tenant holds the per-tenant storage policies an admin sets.
//
// It is control-plane state, separate from the data plane in internal/storage:
// storage enforces a policy, this decides what the policy is. The split matters
// because the two have different owners. A policy is set rarely, by an operator
// with an admin key; it is enforced constantly, by every write. Keeping the
// decision here means the enforcement path never has to know how a limit was
// chosen, only what it is.
package tenant

import (
	"context"
	"sync"

	"github.com/pablofdezr/microvm/internal/storage"
)

// Store keeps each tenant's storage policy.
//
// An interface because a single node can keep this in memory but a fleet cannot:
// an admin who sets a limit on one node must have it honoured on all of them, so
// a real deployment backs this with something shared, exactly as the task queue
// is memory on one node and Redis across many. The Memory implementation here is
// correct for one node and for tests, and wrong for a fleet -- by construction,
// not by accident.
type Store interface {
	// Get returns a tenant's policy. The second result is false when none was
	// ever set, which a caller reads as "unlimited" rather than as an error: a
	// tenant nobody has configured has no cap, which is the pre-existing default.
	Get(ctx context.Context, tenant string) (storage.TenantPolicy, bool, error)

	// Set records a tenant's policy, replacing any previous one.
	Set(ctx context.Context, tenant string, policy storage.TenantPolicy) error

	// List returns every configured tenant, for an admin surveying what is set.
	List(ctx context.Context) ([]Record, error)
}

// Record is one tenant's configured policy.
type Record struct {
	Tenant string
	Policy storage.TenantPolicy
}

// Memory is an in-process Store. Correct for one node; see the Store comment.
type Memory struct {
	mu       sync.RWMutex
	policies map[string]storage.TenantPolicy
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{policies: make(map[string]storage.TenantPolicy)}
}

func (m *Memory) Get(_ context.Context, tenant string) (storage.TenantPolicy, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.policies[tenant]
	return p, ok, nil
}

func (m *Memory) Set(_ context.Context, tenant string, policy storage.TenantPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policies[tenant] = policy
	return nil
}

func (m *Memory) List(_ context.Context) ([]Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Record, 0, len(m.policies))
	for t, p := range m.policies {
		out = append(out, Record{Tenant: t, Policy: p})
	}
	return out, nil
}

var _ Store = (*Memory)(nil)
