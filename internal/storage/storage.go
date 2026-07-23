// Package storage lets code inside a sandbox read and write object storage,
// without ever holding a credential.
//
// # Why the host does the talking
//
// The obvious design is s3fs inside the guest, and it is the wrong one here.
// It needs AWS credentials in the VM, and this system's premise is that the
// code in the VM is hostile. Short-lived tokens do not fix it: they only bound
// how long the stolen credential works, and "your bucket, for fifteen minutes"
// is still your bucket.
//
// So nothing crosses into the guest. The guest gets a filesystem; the host
// holds the credentials and makes every call. The guest has no network path to
// S3 either -- the egress filter still blocks everything private, and the
// bucket is reached from the host's own network namespace.
//
// # Why the socket is the identity
//
// The guest reaches the host over vsock, and the host listens on a socket
// inside that sandbox's own jail. So a request's identity is *which socket it
// arrived on*, which is not a claim the guest makes -- it is a fact about the
// filesystem, established before the VM booted. There is no token to steal and
// nothing to forge. A sandbox cannot ask for another sandbox's prefix because
// it cannot reach the socket that would answer.
//
// # Why S3 is not a filesystem, and what that costs
//
// S3 has no atomic rename, no append, and no partial write. Every S3 filesystem
// either emulates those badly or refuses them honestly, and AWS's own
// mountpoint-s3 refuses them: rename becomes a copy of the whole object plus a
// delete, which is neither atomic nor cheap, and an append in a loop
// re-uploads the object every time. The Store below refuses them too. A loud
// EOPNOTSUPP the first time is kinder than a bill.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// Backend is object storage: S3, or anything shaped like it.
//
// It is deliberately small and deliberately not a filesystem. Everything that
// makes object storage awkward -- no rename, no append, no partial write -- is
// absent here rather than emulated, so that the refusal happens once, at the
// edge, instead of being reinvented per backend.
type Backend interface {
	// Get returns an object's bytes. A length of -1 means "to the end".
	//
	// The range matters: this is what a filesystem read becomes, and a read of
	// 4KB from a 5GB object must not fetch 5GB.
	Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)

	// Put stores an object, replacing any existing one. size may be -1 when it
	// is not known ahead of time.
	Put(ctx context.Context, key string, body io.Reader, size int64) error

	// Head returns an object's metadata without its bytes.
	Head(ctx context.Context, key string) (ObjectInfo, error)

	// List returns objects under a prefix. delimiter "/" collapses everything
	// below one level into CommonPrefixes, which is how a flat keyspace is made
	// to look like directories.
	List(ctx context.Context, prefix, delimiter, cursor string, limit int) (Listing, error)

	// Delete removes an object. Deleting what is not there is not an error:
	// the caller wanted it gone, and it is.
	Delete(ctx context.Context, key string) error
}

// ObjectInfo is what is known about an object without reading it.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
}

// Listing is one page of a prefix.
type Listing struct {
	Objects []ObjectInfo
	// CommonPrefixes are the "directories" one level down, when a delimiter was
	// given.
	CommonPrefixes []string
	// Cursor continues the listing. Empty means there is no more.
	Cursor string
}

// Errors the layers above distinguish.
var (
	// ErrNotFound means no such object.
	ErrNotFound = errors.New("no such object")

	// ErrUnsupported means object storage cannot do this, and we will not
	// pretend otherwise. Rename, append and partial writes are the whole list.
	ErrUnsupported = errors.New("object storage cannot do this")

	// ErrQuotaExceeded means the sandbox has written as much as it may.
	ErrQuotaExceeded = errors.New("storage quota exceeded")

	// ErrReadOnly means the mount forbids writing.
	ErrReadOnly = errors.New("this mount is read-only")

	// ErrEscapesPrefix means a path tried to leave the sandbox's prefix. It is
	// separate from ErrNotFound on purpose: the two are indistinguishable to
	// the caller, but only one of them is somebody probing.
	ErrEscapesPrefix = errors.New("path escapes the sandbox's prefix")
)

// DefaultMountPath is where storage appears inside the guest when the caller
// names no other place.
const DefaultMountPath = "/mnt/storage"

// Mount is what one sandbox may reach.
type Mount struct {
	// Prefix confines the sandbox. Every key it names is resolved beneath this
	// and verified to still be beneath it afterwards.
	Prefix string

	// MountPath is where the storage filesystem appears inside the guest. Empty
	// means DefaultMountPath. Unlike Prefix it is not a security boundary -- it
	// only picks a directory in the guest's own namespace, and the guest could
	// bind-mount it elsewhere anyway -- but it does land on the kernel command
	// line, so ValidMountPath gates what may be set.
	MountPath string

	// ReadOnly forbids writes.
	ReadOnly bool

	// Quota bounds this sandbox: its write rate, and the bytes it alone may
	// write. It is per-sandbox and in-memory.
	Quota Quota

	// Tenant caps the whole prefix's total across every sandbox that shares it,
	// and says whether a full tenant rejects writes or evicts old ones. The zero
	// value is unlimited. Unlike Quota this is enforced against the store, not a
	// counter, because the prefix outlives any one sandbox. See TenantPolicy.
	Tenant TenantPolicy
}

// MountPoint returns where the storage appears in the guest, applying the
// default when the mount named nothing.
func (m Mount) MountPoint() string {
	if m.MountPath == "" {
		return DefaultMountPath
	}
	return m.MountPath
}

// ValidMountPath reports whether p is an acceptable guest mount point.
//
// The bar is not aesthetic. p is relayed onto the guest's kernel command line,
// which is space-separated, so a value containing a space would inject a second
// boot parameter rather than name a deeper directory. It must be an absolute,
// lexically clean path built from the characters a mount point actually uses,
// and it must not climb (a "/mnt/../etc" that Clean would fold is refused
// outright rather than silently rewritten). An empty path is valid and means
// "use the default".
func ValidMountPath(p string) bool {
	if p == "" {
		return true
	}
	if p[0] != '/' || len(p) > 256 {
		return false
	}
	if p != path.Clean(p) {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

// Quota bounds what a sandbox may store.
//
// It exists for the same reason cpu.max and memory.max do, and it is more
// important than either: a runaway loop burning CPU costs a core for a few
// minutes, whereas a runaway loop writing to S3 costs money, silently, until
// somebody notices. The failure mode of storage is not a crash. It is an
// invoice.
type Quota struct {
	// MaxBytes caps total bytes written by this sandbox. Zero means unlimited,
	// which is only ever right on a bucket you do not pay for.
	MaxBytes int64

	// MaxObjects caps how many objects it may create. Bytes alone do not cover
	// this: a million empty objects cost almost no bytes and plenty of money.
	MaxObjects int

	// MaxWritesPerMinute caps the write rate. Bytes and objects bound the total;
	// this bounds the bill's slope, which is what an abuse report is about.
	MaxWritesPerMinute int
}

// DefaultQuota is applied to a mount that asks for nothing.
//
// Bounded rather than unlimited, deliberately. An unset limit on a resource
// that costs money is not a neutral default, it is a decision to trust the code
// -- and the code is the one thing here nobody trusts.
var DefaultQuota = Quota{
	MaxBytes:           1 << 30, // 1 GiB
	MaxObjects:         10_000,
	MaxWritesPerMinute: 600,
}

func (q *Quota) applyDefaults() {
	if q.MaxBytes == 0 {
		q.MaxBytes = DefaultQuota.MaxBytes
	}
	if q.MaxObjects == 0 {
		q.MaxObjects = DefaultQuota.MaxObjects
	}
	if q.MaxWritesPerMinute == 0 {
		q.MaxWritesPerMinute = DefaultQuota.MaxWritesPerMinute
	}
}

// climbsAboveRoot reports whether a path's ".." segments ever pop above the
// mount root.
//
// This is the detector, and it exists because the cleaning below is *too* good.
// Rooting the path ("/" + p, then Clean) makes an escape impossible -- Clean
// resolves "/../x" to "/x", because there is nothing above / -- which is safe
// and completely silent. The path is quietly clamped and served, and the
// attempt leaves no trace anywhere.
//
// That silence is worth breaking. With FUSE, ".." never reaches a filesystem at
// all: the kernel resolves it component by component and handles ".." itself,
// so a legitimate mount never sends one. A ".." that climbs above the root
// arriving at this API therefore did not come from a program calling open() --
// it came from something hand-writing HTTP at the vsock socket, which is not an
// accident and not a typo.
//
// The counting is what makes it precise. "a/../b" is ordinary and stays put;
// only a ".." with nothing left to pop is an attempt to leave.
func climbsAboveRoot(p string) bool {
	depth := 0
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		switch seg {
		case "", ".":
			// No movement.
		case "..":
			depth--
			if depth < 0 {
				return true
			}
		default:
			depth++
		}
	}
	return false
}

// resolve turns a guest-supplied path into a backend key inside the prefix.
//
// This is the security boundary, and it is the only one: a hostile guest picks
// the path, so everything downstream trusts whatever comes out of here.
//
// There are three layers here and they are not redundant:
//
//  1. climbsAboveRoot refuses an attempt to leave, and is the only one that
//     tells anyone it happened.
//  2. Rooting the path before Clean makes leaving impossible, because Clean
//     cannot resolve above "/". This is what actually holds.
//  3. The containment check on the *result* verifies the key sits under the
//     prefix regardless of how the input was spelled.
//
// Layer 3 cannot currently fire: after layer 2, `clean` can never begin with
// "..", so path.Join can never walk out. That is not a reason to delete it. It
// is the assertion that layer 2 is doing what this comment claims, and if
// someone ever "simplifies" the rooting away, it is the difference between a
// caught bug and a silent one. Mutation testing confirms the pairing: remove
// either layer alone and everything still holds; remove both and
// "../sb_b/secret" reaches sandbox B.
func resolve(prefix, guestPath string) (string, error) {
	if guestPath == "" {
		return "", fmt.Errorf("%w: empty path", ErrNotFound)
	}

	if climbsAboveRoot(guestPath) {
		return "", fmt.Errorf("%w: %q climbs above the mount root", ErrEscapesPrefix, guestPath)
	}

	// The guest names things as absolute paths within its mount. Strip the
	// leading slash so the join below is relative, or path.Join would discard
	// the prefix entirely.
	clean := path.Clean("/" + strings.TrimPrefix(guestPath, "/"))
	clean = strings.TrimPrefix(clean, "/")

	if clean == "." || clean == "" {
		// The mount root itself, which is a directory rather than an object.
		return strings.TrimSuffix(prefix, "/"), nil
	}

	key := path.Join(prefix, clean)

	// The result must still be inside. This catches every escape regardless of
	// how it was spelled, which is the point of checking the output rather than
	// pattern-matching the input: there is always another way to spell "..".
	base := strings.TrimSuffix(prefix, "/")
	if base != "" && key != base && !strings.HasPrefix(key, base+"/") {
		return "", fmt.Errorf("%w: %q resolves to %q, which is outside %q",
			ErrEscapesPrefix, guestPath, key, base)
	}
	return key, nil
}
