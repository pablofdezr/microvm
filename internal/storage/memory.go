package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is object storage in a map.
//
// It exists so the layers above -- scoping, quotas, the FUSE protocol -- are
// tested against a real implementation of the port rather than a mock that
// agrees with whatever the test expects. It is also a legitimate backend for a
// development daemon that should not touch a real bucket.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]memObject
	// now is overridable so a test can control LastModified.
	now func() time.Time
}

type memObject struct {
	body     []byte
	modified time.Time
}

// NewMemory returns an in-memory backend.
func NewMemory() *Memory {
	return &Memory{
		objects: make(map[string]memObject),
		now:     time.Now,
	}
}

func (m *Memory) Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if offset > int64(len(obj.body)) {
		// Reading past the end is an empty read, not an error: it is what a
		// filesystem does, and the layer above is a filesystem.
		return io.NopCloser(bytes.NewReader(nil)), nil
	}

	body := obj.body[offset:]
	if length >= 0 && length < int64(len(body)) {
		body = body[:length]
	}
	// Copied: the caller must not be handed a window onto a map value that a
	// concurrent Put is about to replace.
	out := make([]byte, len(body))
	copy(out, body)
	return io.NopCloser(bytes.NewReader(out)), nil
}

func (m *Memory) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{body: raw, modified: m.now()}
	return nil
}

func (m *Memory) Head(ctx context.Context, key string) (ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	obj, ok := m.objects[key]
	if !ok {
		return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return ObjectInfo{
		Key:          key,
		Size:         int64(len(obj.body)),
		LastModified: obj.modified,
	}, nil
}

func (m *Memory) List(ctx context.Context, prefix, delimiter, cursor string, limit int) (Listing, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if limit <= 0 {
		limit = 1000
	}

	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	// S3 lists in lexicographic order, and code depends on that whether or not
	// it should. A map's order would make this backend disagree with the real
	// one in a way that only shows up in production.
	sort.Strings(keys)

	var out Listing
	seenPrefix := map[string]bool{}

	for _, k := range keys {
		if cursor != "" && k <= cursor {
			continue
		}

		if delimiter != "" {
			rest := strings.TrimPrefix(k, prefix)
			if i := strings.Index(rest, delimiter); i >= 0 {
				// Everything below this level collapses into one "directory".
				cp := prefix + rest[:i+len(delimiter)]
				if !seenPrefix[cp] {
					seenPrefix[cp] = true
					out.CommonPrefixes = append(out.CommonPrefixes, cp)
				}
				continue
			}
		}

		out.Objects = append(out.Objects, ObjectInfo{
			Key:          k,
			Size:         int64(len(m.objects[k].body)),
			LastModified: m.objects[k].modified,
		})

		if len(out.Objects) >= limit {
			out.Cursor = k
			break
		}
	}
	return out, nil
}

func (m *Memory) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Deleting what is not there is not an error: the caller wanted it gone,
	// and it is. S3 behaves the same way, and a backend that disagreed would
	// make cleanup code that works in tests fail in production.
	delete(m.objects, key)
	return nil
}

// Keys returns every stored key, for tests that need to see what really landed
// rather than what the layer above believes landed.
func (m *Memory) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]string, 0, len(m.objects))
	for k := range m.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Object returns a stored object's bytes.
func (m *Memory) Object(key string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	obj, ok := m.objects[key]
	return obj.body, ok
}

// setModTime backdates an object, so a test can make "oldest" mean something
// without sleeping between writes.
func (m *Memory) setModTime(key string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if obj, ok := m.objects[key]; ok {
		obj.modified = t
		m.objects[key] = obj
	}
}

var _ Backend = (*Memory)(nil)
