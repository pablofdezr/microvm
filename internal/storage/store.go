package storage

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Store is one sandbox's view of object storage.
//
// Every method takes a path the guest chose, so every method resolves it
// through the prefix first. There is no way to reach the backend from here
// without that happening -- the backend is unexported and no method hands it
// out.
type Store struct {
	backend Backend
	mount   Mount
	usage   *usage
}

// NewStore returns a sandbox's scoped view.
func NewStore(backend Backend, mount Mount) *Store {
	mount.Quota.applyDefaults()
	return &Store{
		backend: backend,
		mount:   mount,
		usage:   newUsage(),
	}
}

// Usage reports what the sandbox has consumed. It is a billing meter and a
// debugging aid, in that order -- and the answer to statfs, which is why it
// carries the limit as well as the consumption.
func (s *Store) Usage() Usage {
	u := s.usage.snapshot()
	u.Limit = effectiveLimit(s.mount.Quota.MaxBytes, s.mount.Tenant.MaxBytes)
	return u
}

// effectiveLimit is the byte ceiling this sandbox actually faces: the smaller of
// its own quota and its tenant's cap, with zero at either level meaning "no
// limit there". statfs reports it as the total, so `df` inside the guest shows
// the real bound a write will hit rather than the guessed headroom it used to.
//
// A caveat lives in the shared-tenant case: the total is the tenant's cap but
// the used figure is this sandbox's own bytes, so df can understate usage when
// siblings have written. The honest ceiling is still right, and the write that
// crosses it still gets EDQUOT -- which is where the real enforcement is, statfs
// being only advice.
func effectiveLimit(quotaMax, tenantMax int64) int64 {
	switch {
	case quotaMax == 0:
		return tenantMax
	case tenantMax == 0:
		return quotaMax
	case tenantMax < quotaMax:
		return tenantMax
	default:
		return quotaMax
	}
}

// Open returns a range of an object.
func (s *Store) Open(ctx context.Context, guestPath string, offset, length int64) (io.ReadCloser, error) {
	key, err := s.resolve(guestPath)
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, fmt.Errorf("%w: negative offset %d", ErrUnsupported, offset)
	}
	return s.backend.Get(ctx, key, offset, length)
}

// Create stores an object, replacing any existing one.
//
// It is write-once by construction: there is no way to modify part of an
// object, because S3 has no way to modify part of an object. See the package
// comment.
func (s *Store) Create(ctx context.Context, guestPath string, body io.Reader, size int64) error {
	key, err := s.resolve(guestPath)
	if err != nil {
		return err
	}
	if s.mount.ReadOnly {
		return fmt.Errorf("%w: cannot write %s", ErrReadOnly, guestPath)
	}

	// The tenant cap comes first, because eviction may need to delete objects
	// before this write can proceed, and a preserve policy must refuse before
	// anything is reserved or streamed. It is a no-op when the mount has no
	// tenant limit, which is the common case.
	if err := s.mount.Tenant.Admit(ctx, s.backend, s.mount.Prefix, key, size); err != nil {
		return err
	}

	// Reserved before the write, not counted after it. Counting afterwards
	// would let an unbounded body through and discover the overrun once the
	// bytes were already stored and already billed.
	//
	// The reservation hands back this write's own allowance rather than the
	// caller working it out. Recomputing it as "limit minus written minus
	// reserved" is the obvious move and it is wrong: `reserved` already
	// includes the bytes this very write just reserved, so the write trips over
	// its own reservation and a 60-byte write into an empty 100-byte quota
	// fails at 40 bytes.
	allowance, err := s.usage.reserve(size, s.mount.Quota)
	if err != nil {
		return err
	}

	// The body is metered as it streams, because size is a claim and a claim
	// from the guest is not a fact. A caller who says 10 bytes and sends 10GB
	// gets cut off at the limit rather than believed.
	counted := &countingReader{r: body, limit: allowance}

	if err := s.backend.Put(ctx, key, counted, size); err != nil {
		s.usage.release(size)
		if counted.overrun {
			return fmt.Errorf("%w: the write exceeded the sandbox's remaining quota", ErrQuotaExceeded)
		}
		return err
	}

	s.usage.commit(size, counted.n)
	return nil
}

// Stat returns an object's metadata.
func (s *Store) Stat(ctx context.Context, guestPath string) (ObjectInfo, error) {
	key, err := s.resolve(guestPath)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := s.backend.Head(ctx, key)
	if err != nil {
		return ObjectInfo{}, err
	}
	// The guest must never see the prefix. It is not a secret exactly, but it
	// is not the guest's business, and a path it can read is a path it will
	// eventually try to write.
	info.Key = s.unresolve(info.Key)
	return info, nil
}

// List returns one page of a directory.
func (s *Store) List(ctx context.Context, guestPath, cursor string, limit int) (Listing, error) {
	key, err := s.resolve(guestPath)
	if err != nil {
		return Listing{}, err
	}

	// A directory listing is a prefix scan, and the prefix has to end in a
	// slash or "logs" would also match "logs-old/x".
	prefix := key
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	out, err := s.backend.List(ctx, prefix, "/", cursor, limit)
	if err != nil {
		return Listing{}, err
	}

	for i := range out.Objects {
		out.Objects[i].Key = s.unresolve(out.Objects[i].Key)
	}
	for i := range out.CommonPrefixes {
		out.CommonPrefixes[i] = s.unresolve(out.CommonPrefixes[i])
	}
	return out, nil
}

// Remove deletes an object.
func (s *Store) Remove(ctx context.Context, guestPath string) error {
	key, err := s.resolve(guestPath)
	if err != nil {
		return err
	}
	if s.mount.ReadOnly {
		return fmt.Errorf("%w: cannot delete %s", ErrReadOnly, guestPath)
	}
	return s.backend.Delete(ctx, key)
}

// Rename is not supported, and the error says why.
//
// S3 has no rename. Emulating it means copying the whole object and deleting
// the original, which is not atomic, takes as long as the object is large, and
// costs money -- so a program that renames a 5GB file in a loop discovers all
// three at once. AWS's own client refuses it for the same reasons.
func (s *Store) Rename(ctx context.Context, from, to string) error {
	return fmt.Errorf("%w: object storage has no atomic rename, so %s -> %s cannot be done "+
		"safely; write to the final path instead", ErrUnsupported, from, to)
}

// resolve maps a guest path to a backend key, refusing anything outside.
func (s *Store) resolve(guestPath string) (string, error) {
	return resolve(s.mount.Prefix, guestPath)
}

// unresolve maps a backend key back to what the guest calls it.
func (s *Store) unresolve(key string) string {
	base := strings.TrimSuffix(s.mount.Prefix, "/")
	out := strings.TrimPrefix(key, base)
	if !strings.HasPrefix(out, "/") {
		out = "/" + out
	}
	return out
}

// Usage is what a sandbox has stored.
type Usage struct {
	BytesWritten int64
	Objects      int
	// WritesLastMinute is the rate the limiter is currently seeing.
	WritesLastMinute int
	// Limit is the byte ceiling this sandbox faces, or 0 for unlimited. It is
	// what statfs turns into a filesystem's total size. See effectiveLimit.
	Limit int64
}

// usage tracks a sandbox's consumption against its quota.
type usage struct {
	mu sync.Mutex
	// reserved is bytes promised by writes in flight but not yet committed.
	// Without it, ten concurrent 1GB writes each check against the same free
	// space and all ten pass.
	reserved int64
	written  int64
	objects  int
	// writes are the timestamps of recent writes, for the rate limit. A slice
	// rather than a token bucket because the window is small and the honesty is
	// worth more than the microsecond.
	writes []time.Time
}

func newUsage() *usage { return &usage{} }

// reserve claims room for a write and returns how many bytes it may actually
// consume.
//
// Returning the allowance is what keeps the caller from having to derive it,
// which is a calculation that looks trivial and is not: the obvious formula
// double-counts this write's own reservation.
func (u *usage) reserve(size int64, q Quota) (allowance int64, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.trimLocked()
	if len(u.writes) >= q.MaxWritesPerMinute {
		return 0, fmt.Errorf("%w: %d writes in the last minute, limit is %d",
			ErrQuotaExceeded, len(u.writes), q.MaxWritesPerMinute)
	}
	if u.objects >= q.MaxObjects {
		return 0, fmt.Errorf("%w: %d objects, limit is %d", ErrQuotaExceeded, u.objects, q.MaxObjects)
	}

	free := q.MaxBytes - u.written - u.reserved
	if free <= 0 {
		return 0, fmt.Errorf("%w: %d bytes written, %d in flight, limit is %d",
			ErrQuotaExceeded, u.written, u.reserved, q.MaxBytes)
	}

	if size > 0 {
		// A declared size can be reserved, which is what stops ten concurrent
		// writes from each seeing the same free space.
		if size > free {
			return 0, fmt.Errorf("%w: %d bytes written, %d in flight, %d more requested, limit is %d",
				ErrQuotaExceeded, u.written, u.reserved, size, q.MaxBytes)
		}
		u.reserved += size
		allowance = size
	} else {
		// Size unknown: nothing to reserve, so the streaming counter is the
		// only thing bounding it. It may have whatever is free.
		allowance = free
	}

	u.writes = append(u.writes, time.Now())
	return allowance, nil
}

func (u *usage) release(size int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if size > 0 {
		u.reserved -= size
	}
}

func (u *usage) commit(reserved, actual int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if reserved > 0 {
		u.reserved -= reserved
	}
	u.written += actual
	u.objects++
}

// trimLocked drops writes older than the rate window.
func (u *usage) trimLocked() {
	cutoff := time.Now().Add(-time.Minute)
	keep := u.writes[:0]
	for _, t := range u.writes {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	u.writes = keep
}

func (u *usage) snapshot() Usage {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.trimLocked()
	return Usage{
		BytesWritten:     u.written,
		Objects:          u.objects,
		WritesLastMinute: len(u.writes),
	}
}

// countingReader counts what passes through and stops just past a limit.
//
// It is what makes the quota true rather than advisory. The declared size is
// the guest's claim; this is the measurement. Without it, a sandbox declares
// one byte and streams forever.
//
// It reads up to one byte *past* the allowance, and that byte is the whole
// trick: it is what distinguishes a body that exactly fills its allowance from
// one that is too big. Refusing at `n >= limit` instead is the obvious version
// and it rejects the most common case there is -- a write whose size was
// declared correctly ends exactly at its allowance, so every honest write
// would fail. (This project has made that mistake before, in the log store's
// ring buffer: exact fill is not overflow.)
type countingReader struct {
	r       io.Reader
	n       int64
	limit   int64
	overrun bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	// Never let more than limit+1 bytes through, so an overrun is caught as it
	// streams rather than after the body has been buffered somewhere.
	if room := c.limit + 1 - c.n; int64(len(p)) > room {
		p = p[:max(room, 0)]
	}

	n, err := c.r.Read(p)
	c.n += int64(n)

	if c.n > c.limit {
		c.overrun = true
		return 0, fmt.Errorf("%w: write exceeded %d bytes", ErrQuotaExceeded, c.limit)
	}
	return n, err
}
