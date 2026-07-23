// Package storagefs presents a sandbox's object storage as a filesystem.
//
// It is the guest half of the design whose host half is internal/storage: the
// guest sees ordinary files under a mount point, and every open/read/write/close
// becomes an HTTP call to the host over vsock (see internal/storageclient). The
// host holds the credentials and the guest holds none, which is the whole point
// -- the code in the VM is untrusted, so it must be able to use storage without
// ever being handed a key to it.
//
// # Why writes buffer
//
// Object storage has no partial write: you cannot change byte 10 of an object
// without rewriting the object. A filesystem, though, is all partial writes --
// a program opens a file, writes a header, seeks, writes a body, and closes. So
// a file open for writing accumulates in a buffer here in the guest, and the
// whole object is PUT once, on close. That is the honest shape of the underlying
// store; emulating rename or append on top of it would only hide a copy of the
// whole object behind a call that looks cheap.
//
// This file holds the buffer, which is platform-neutral and therefore testable
// anywhere. The FUSE wiring that turns kernel calls into these buffer operations
// lives in fs_linux.go, because /dev/fuse exists only on Linux.
package storagefs

import (
	"sync"
)

// buffer is the in-memory contents of one file open for writing.
//
// It is a plain grow-on-write byte slice with its own lock, because a single
// open file can see concurrent writes from a threaded guest program and the
// kernel does not serialise them for us. It is deliberately not a temp file:
// the guest's root is a RAM-backed overlay, so a temp file would be RAM anyway,
// with a syscall in the way. A file too large to hold in RAM is a real limit,
// and a documented one, not a bug.
type buffer struct {
	mu    sync.Mutex
	data  []byte
	dirty bool
}

// newBuffer returns an empty buffer, as for a freshly created or truncated file.
func newBuffer() *buffer { return &buffer{} }

// bufferOf returns a buffer preloaded with existing content, as for a file
// opened to be modified rather than replaced. The bytes are taken as-is; the
// caller owns them afterwards only through the buffer.
func bufferOf(data []byte) *buffer { return &buffer{data: data} }

// WriteAt copies p in at off, growing the buffer if the write lands past its
// end. A gap between the old end and off is zero-filled, matching a write after
// a seek past EOF on any real filesystem. It always writes all of p.
func (b *buffer) WriteAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	end := off + int64(len(p))
	if end > int64(len(b.data)) {
		grown := make([]byte, end)
		copy(grown, b.data)
		b.data = grown
	}
	copy(b.data[off:end], p)
	b.dirty = true
	return len(p), nil
}

// Truncate resizes the buffer to n bytes, padding with zeros when it grows.
// Truncating to a smaller size drops the tail. It marks the buffer dirty even
// when the size is unchanged: an explicit truncate to the current length is
// still a request to persist what is there.
func (b *buffer) Truncate(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch {
	case n < int64(len(b.data)):
		b.data = b.data[:n]
	case n > int64(len(b.data)):
		grown := make([]byte, n)
		copy(grown, b.data)
		b.data = grown
	}
	b.dirty = true
}

// Bytes returns a copy of the current contents, safe to hand to a reader that
// outlives the lock.
func (b *buffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.data))
	copy(out, b.data)
	return out
}

// Size is the current length in bytes.
func (b *buffer) Size() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(len(b.data))
}

// Dirty reports whether the buffer has been written since it was loaded. A
// clean buffer needs no PUT on close, which turns an open-for-write that never
// wrote into a no-op rather than an empty-object upload.
func (b *buffer) Dirty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dirty
}

// clearDirty marks the buffer as persisted, so a second Flush -- close(2) can
// fire more than once for one open file -- does not re-upload identical bytes.
func (b *buffer) clearDirty() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dirty = false
}

// markDirty forces the next Flush to upload, used when a file is created or
// truncated to a length it already has: there is nothing to write, but the
// object must still come into existence (or be emptied).
func (b *buffer) markDirty() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dirty = true
}
