package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// FullAction is what a write does when it would push a tenant over its cap.
type FullAction string

const (
	// Preserve keeps what is stored and rejects the write. The right choice when
	// old data matters more than new -- an audit log, a dataset someone paid to
	// compute -- and the wrong choice for a cache, where it turns a full disk
	// into a hard stop.
	Preserve FullAction = "preserve"

	// Evict deletes the oldest objects until the write fits. The right choice for
	// a cache or scratch space, where the newest data is the point and the oldest
	// is the most disposable thing there is. It costs a delete per evicted object
	// and it is irreversible, which is exactly why it is not the default.
	Evict FullAction = "evict"
)

// TenantPolicy caps a tenant's total storage and says what to do when it fills.
//
// It is enforced against the object store itself, not an in-memory counter,
// because a tenant's files are written by many sandboxes across many nodes and
// outlive all of them. The bucket is the only thing that knows the real total,
// so the check reads it. That is honest and it is not free -- see Admit.
type TenantPolicy struct {
	// MaxBytes caps the tenant's total. Zero means unlimited: no tenant-level
	// enforcement happens at all, which is the behaviour before this existed.
	MaxBytes int64

	// OnFull is what happens when a write would exceed MaxBytes. The zero value
	// is Preserve, deliberately: eviction deletes data, and a policy that deletes
	// data must be asked for, never defaulted into.
	OnFull FullAction
}

// Admit makes room under prefix for a write of size bytes, or refuses it.
//
// This is where preserve and evict actually differ. It reads the tenant's
// current usage from the store -- every object under the prefix, summed -- and:
//
//   - if the write already fits, does nothing;
//   - under Preserve, refuses with a quota error;
//   - under Evict, deletes objects oldest-first until the write fits.
//
// targetKey is the object about to be written. It is excluded from the total and
// never evicted: a write that replaces an existing object frees that object's
// old bytes, so counting them against the new write -- or worse, deleting the
// thing being overwritten -- would be wrong twice.
//
// The cost is a full prefix listing per write, which is O(objects). That is the
// price of a correct total in a fleet where no single node has seen every write;
// a cached or shared counter is the obvious optimisation and a separate change.
// Eviction needs the listing regardless, to know what "oldest" means.
func (p TenantPolicy) Admit(ctx context.Context, backend Backend, prefix, targetKey string, size int64) error {
	if p.MaxBytes == 0 {
		return nil // unlimited: nothing to enforce
	}
	if size < 0 {
		// Without a known size there is nothing to check the cap against, and a
		// tenant cap is not a suggestion. The FUSE layer always knows the size by
		// flush time, so this only bites a raw streaming write, which can declare
		// its length.
		return fmt.Errorf("%w: a tenant byte limit needs a known content length", ErrUnsupported)
	}
	if size > p.MaxBytes {
		// Bigger than the whole allowance. No amount of eviction makes room for an
		// object that does not fit in an empty tenant, so say so rather than delete
		// everything and then fail.
		return fmt.Errorf("%w: %d bytes exceeds the tenant's %d-byte cap", ErrQuotaExceeded, size, p.MaxBytes)
	}

	objects, used, err := scanTenant(ctx, backend, prefix, targetKey)
	if err != nil {
		return fmt.Errorf("read tenant usage: %w", err)
	}

	if used+size <= p.MaxBytes {
		return nil
	}

	if p.OnFull != Evict {
		return fmt.Errorf("%w: tenant holds %d of %d bytes and its policy is preserve",
			ErrQuotaExceeded, used, p.MaxBytes)
	}

	// Evict oldest-first. Sorting by modification time is what makes "oldest"
	// mean anything; without it, eviction deletes whatever the store happened to
	// list first, which is not a policy, it is a coin toss.
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].LastModified.Before(objects[j].LastModified)
	})

	for _, o := range objects {
		if used+size <= p.MaxBytes {
			break
		}
		if err := backend.Delete(ctx, o.Key); err != nil {
			return fmt.Errorf("evict %s to make room: %w", o.Key, err)
		}
		used -= o.Size
	}
	return nil
}

// UsageBytes returns how many bytes a prefix currently holds.
//
// It reads the object store, so it is the true total across every sandbox and
// node that has ever written there -- and it costs a full listing, which is why
// it is a deliberate call an admin makes, not something on the write path.
func UsageBytes(ctx context.Context, backend Backend, prefix string) (int64, error) {
	_, used, err := scanTenant(ctx, backend, prefix, "")
	return used, err
}

// scanTenant totals a prefix and returns its objects, excluding one key.
//
// The prefix is forced to end in a slash so "tenants/t_ab" cannot sweep up
// "tenants/t_abcd" -- a real hazard if tenant ids were ever variable-length,
// and cheap insurance now that they are not.
func scanTenant(ctx context.Context, backend Backend, prefix, exclude string) (objects []ObjectInfo, used int64, err error) {
	scan := strings.TrimSuffix(prefix, "/") + "/"

	cursor := ""
	for {
		// No delimiter: a recursive sweep of everything under the tenant, not a
		// one-level directory listing. The total is of leaves, wherever they sit.
		page, err := backend.List(ctx, scan, "", cursor, 1000)
		if err != nil {
			return nil, 0, err
		}
		for _, o := range page.Objects {
			if o.Key == exclude {
				continue
			}
			objects = append(objects, o)
			used += o.Size
		}
		if page.Cursor == "" {
			return objects, used, nil
		}
		cursor = page.Cursor
	}
}
