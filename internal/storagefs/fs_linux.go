//go:build linux

package storagefs

import (
	"bytes"
	"context"
	"io"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/pablofdezr/microvm/internal/storageclient"
)

// This file is the FUSE half of storagefs, and it is Linux-only: /dev/fuse
// exists nowhere else. It maps kernel VFS calls onto the storage client's HTTP
// calls, one for one where the two line up and with the buffer in file.go where
// they do not (writes). Everything platform-neutral -- the write buffer, the
// errno mapping -- lives in the other files so it can be tested off Linux.
//
// The object store is a flat keyspace, so directories here are synthetic: a
// "directory" is any prefix that has something under it, and an empty directory
// simply does not exist. mkdir therefore creates nothing on the host and only a
// node in the kernel's cache; the first file written under it is what makes the
// prefix real. This is the same model the host's List already speaks in
// CommonPrefixes, carried up to the VFS unchanged.

const (
	dirMode  = fuse.S_IFDIR | 0o755
	fileMode = fuse.S_IFREG | 0o644

	statfsBlockSize = 4096
	// statfsHeadroom is the free space statfs reports when the host declares no
	// limit (an unlimited tenant on an unmetered bucket), in blocks. With a real
	// limit the total comes from the host instead; this is only the fallback that
	// keeps df sane and stops tools that preflight free space from refusing a
	// write the host would actually accept.
	statfsHeadroom = 1 << 20 // 4 GiB in 4 KiB blocks
)

// FS is one sandbox's storage, presented as a filesystem. It holds the client
// and the mount-wide read-only flag; per-file state hangs off the nodes.
type FS struct {
	client   *storageclient.Client
	readOnly bool
}

// New returns a filesystem backed by client. readOnly makes every mutating call
// fail with EROFS before it reaches the host, which is a courtesy -- the host
// enforces it too -- but a faster and clearer one.
func New(client *storageclient.Client, readOnly bool) *FS {
	return &FS{client: client, readOnly: readOnly}
}

// Root returns the node to hand fs.Mount.
func (f *FS) Root() fs.InodeEmbedder { return &dirNode{fs: f} }

// Mount mounts the filesystem at mountpoint and serves it in the background.
// The caller owns the returned server: Unmount to tear it down, Wait to block
// until it is torn down. A missing mountpoint or a kernel without FUSE is an
// error here, not a panic, so the agent can carry on without storage.
func Mount(mountpoint string, client *storageclient.Client, readOnly, debug bool) (*fuse.Server, error) {
	timeout := time.Second
	return fs.Mount(mountpoint, New(client, readOnly).Root(), &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName: "microvm-storage",
			Name:   "microvmstorage",
			Debug:  debug,
		},
		// A short cache: attributes and lookups are answered from the host, and a
		// second of staleness saves a round trip per getattr in a directory walk
		// without letting the guest see a wildly wrong view of its own writes.
		AttrTimeout:  &timeout,
		EntryTimeout: &timeout,
	})
}

// dirNode is a directory: the mount root when path is "", otherwise a prefix
// like "/logs". It owns no state beyond its path.
type dirNode struct {
	fs.Inode
	fs   *FS
	path string
}

var (
	_ fs.NodeGetattrer = (*dirNode)(nil)
	_ fs.NodeLookuper  = (*dirNode)(nil)
	_ fs.NodeReaddirer = (*dirNode)(nil)
	_ fs.NodeCreater   = (*dirNode)(nil)
	_ fs.NodeMkdirer   = (*dirNode)(nil)
	_ fs.NodeUnlinker  = (*dirNode)(nil)
	_ fs.NodeRmdirer   = (*dirNode)(nil)
	_ fs.NodeRenamer   = (*dirNode)(nil)
	_ fs.NodeStatfser  = (*dirNode)(nil)
)

func (d *dirNode) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = dirMode
	return 0
}

// Lookup answers whether a name is a file, a directory, or nothing.
//
// A HEAD decides file-or-not in one call; only if that misses do we spend a
// second call listing the name as a prefix, which is what tells a real directory
// apart from a genuinely absent one. The order matters: a key can be both an
// object and a prefix in S3, and treating it as the file it literally is beats
// inventing a directory over the top of it.
func (d *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	child := childPath(d.path, name)

	info, err := d.fs.client.Stat(ctx, child)
	if err == nil {
		fn := &fileNode{fs: d.fs, path: child, size: info.Size, mtime: info.LastModified}
		fillFileAttr(&out.Attr, info.Size, info.LastModified)
		return d.NewInode(ctx, fn, fs.StableAttr{Mode: fuse.S_IFREG}), 0
	}
	if errno := toErrno(err); errno != syscall.ENOENT {
		return nil, errno
	}

	// Not an object. It is a directory only if something lives under it; an
	// empty prefix has no existence of its own to find.
	page, lerr := d.fs.client.List(ctx, child, "", 1)
	if lerr != nil {
		return nil, toErrno(lerr)
	}
	if len(page.Objects)+len(page.CommonPrefixes) == 0 {
		return nil, syscall.ENOENT
	}
	cd := &dirNode{fs: d.fs, path: child}
	out.Attr.Mode = dirMode
	return d.NewInode(ctx, cd, fs.StableAttr{Mode: fuse.S_IFDIR}), 0
}

// Readdir pages the whole directory. It follows the cursor to the end rather
// than returning one page, because the kernel asks for the directory, not a
// page, and a partial answer reads as a short directory.
func (d *dirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	var entries []fuse.DirEntry
	seen := make(map[string]bool)
	cursor := ""
	for {
		page, err := d.fs.client.List(ctx, d.listPath(), cursor, 0)
		if err != nil {
			return nil, toErrno(err)
		}
		for _, o := range page.Objects {
			name := baseName(o.Key)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			entries = append(entries, fuse.DirEntry{Name: name, Mode: fuse.S_IFREG})
		}
		for _, p := range page.CommonPrefixes {
			name := baseName(p)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			entries = append(entries, fuse.DirEntry{Name: name, Mode: fuse.S_IFDIR})
		}
		if page.Cursor == "" {
			break
		}
		cursor = page.Cursor
	}
	return fs.NewListDirStream(entries), 0
}

// Create makes a new file. The object does not exist on the host yet: it comes
// into being when the buffer is flushed on close, even if nothing was written,
// so `touch` leaves a zero-byte object exactly as it does on a real disk.
func (d *dirNode) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if d.fs.readOnly {
		return nil, nil, 0, syscall.EROFS
	}
	child := childPath(d.path, name)
	fn := &fileNode{fs: d.fs, path: child}

	buf := newBuffer()
	buf.markDirty() // an empty new file is still a write: close must create it.
	h := &handle{buf: buf}
	fn.setHandle(h)

	fillFileAttr(&out.Attr, 0, time.Time{})
	inode := d.NewInode(ctx, fn, fs.StableAttr{Mode: fuse.S_IFREG})
	return inode, h, 0, 0
}

// Mkdir creates a directory that exists only in the kernel's cache. There is
// nothing to store: a prefix with no children is not a thing object storage can
// hold. The first file written under it makes it real; until then it is ours to
// remember.
func (d *dirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if d.fs.readOnly {
		return nil, syscall.EROFS
	}
	cd := &dirNode{fs: d.fs, path: childPath(d.path, name)}
	out.Attr.Mode = dirMode
	return d.NewInode(ctx, cd, fs.StableAttr{Mode: fuse.S_IFDIR}), 0
}

func (d *dirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	if d.fs.readOnly {
		return syscall.EROFS
	}
	// Deleting what is not there is not an error, on the host or here: unlink of
	// a synthetic (never-flushed) file must succeed, and it does.
	return toErrno(d.fs.client.Delete(ctx, childPath(d.path, name)))
}

// Rmdir removes a directory, but only an empty one. A synthetic directory has
// nothing to delete and succeeds; a directory with objects under it is
// ENOTEMPTY, exactly as a real one would be.
func (d *dirNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	if d.fs.readOnly {
		return syscall.EROFS
	}
	page, err := d.fs.client.List(ctx, childPath(d.path, name), "", 1)
	if err != nil {
		return toErrno(err)
	}
	if len(page.Objects)+len(page.CommonPrefixes) > 0 {
		return syscall.ENOTEMPTY
	}
	return 0
}

// Rename is emulated as a cross-device move, not performed.
//
// Object storage has no rename, and the host refuses to fake one with a
// server-side copy because that hides the cost of rewriting a whole object
// behind a call that looks like a pointer swap. Returning EXDEV pushes the
// decision to userspace: `mv` and coreutils respond by copying the bytes and
// unlinking the original, which moves the same data but does it visibly, through
// the guest, counted against the quota. The cost is real either way; this makes
// it one the caller can see.
func (d *dirNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return syscall.EXDEV
}

func (d *dirNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	return d.fs.statfs(ctx, out)
}

// listPath is the path passed to the host's List for this directory. The root
// is "/", not "", so the host lists the prefix rather than rejecting an empty
// path.
func (d *dirNode) listPath() string {
	if d.path == "" {
		return "/"
	}
	return d.path
}

// fileNode is one object. size and mtime are cached from the lookup that found
// it, so getattr need not round-trip while nothing is writing; an open write
// handle supersedes them with the live buffer length.
type fileNode struct {
	fs.Inode
	fs   *FS
	path string

	mu     sync.Mutex
	size   int64
	mtime  time.Time
	handle *handle
}

var (
	_ fs.NodeGetattrer = (*fileNode)(nil)
	_ fs.NodeSetattrer = (*fileNode)(nil)
	_ fs.NodeOpener    = (*fileNode)(nil)
	_ fs.NodeReader    = (*fileNode)(nil)
	_ fs.NodeWriter    = (*fileNode)(nil)
	_ fs.NodeFlusher   = (*fileNode)(nil)
	_ fs.NodeFsyncer   = (*fileNode)(nil)
	_ fs.NodeReleaser  = (*fileNode)(nil)
	_ fs.NodeStatfser  = (*fileNode)(nil)
)

func (f *fileNode) setHandle(h *handle) {
	f.mu.Lock()
	f.handle = h
	f.mu.Unlock()
}

func (f *fileNode) clearHandle(h *handle) {
	f.mu.Lock()
	if f.handle == h {
		f.handle = nil
	}
	f.mu.Unlock()
}

func (f *fileNode) openHandle() *handle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handle
}

func (f *fileNode) setSize(n int64) {
	f.mu.Lock()
	f.size = n
	f.mu.Unlock()
}

func (f *fileNode) cachedSize() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.size
}

func (f *fileNode) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	size := f.cachedSize()
	if h := f.openHandle(); h != nil && h.buf != nil {
		// While a write is open its buffer is the truth: the caller expects to
		// see the bytes it just wrote, not the last flushed length.
		size = h.buf.Size()
	}
	f.mu.Lock()
	mtime := f.mtime
	f.mu.Unlock()
	fillFileAttr(&out.Attr, size, mtime)
	return 0
}

// Setattr handles the size changes truncate makes, and accepts everything else
// it cannot honour. chmod, chown and utimes have nowhere to go on an object
// store, but failing them breaks ordinary programs that set a mode after
// creating a file, so they succeed and change nothing.
func (f *fileNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if size, ok := in.GetSize(); ok {
		if f.fs.readOnly {
			return syscall.EROFS
		}
		if errno := f.truncate(ctx, fh, int64(size)); errno != 0 {
			return errno
		}
	}
	size := f.cachedSize()
	if h := f.openHandle(); h != nil && h.buf != nil {
		size = h.buf.Size()
	}
	f.mu.Lock()
	mtime := f.mtime
	f.mu.Unlock()
	fillFileAttr(&out.Attr, size, mtime)
	return 0
}

// truncate resizes the file to n. With a write handle open it retargets the
// buffer; without one it is a read-modify-write of the whole object, which is
// the only way a store with no partial write can shorten a file.
func (f *fileNode) truncate(ctx context.Context, fh fs.FileHandle, n int64) syscall.Errno {
	if h, ok := fh.(*handle); ok && h.buf != nil {
		h.buf.Truncate(n)
		return 0
	}
	if h := f.openHandle(); h != nil && h.buf != nil {
		h.buf.Truncate(n)
		return 0
	}

	data, errno := f.load(ctx)
	if errno != 0 && errno != syscall.ENOENT {
		return errno
	}
	buf := bufferOf(data)
	buf.Truncate(n)
	return f.putBytes(ctx, buf.Bytes())
}

// Open returns a handle. A read-only open carries no buffer and streams every
// read from the host; a writing open carries a buffer that is either empty
// (O_TRUNC) or preloaded with the object's current bytes (read-modify-write).
func (f *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	writing := flags&syscall.O_WRONLY != 0 || flags&syscall.O_RDWR != 0
	if !writing {
		return &handle{}, 0, 0
	}
	if f.fs.readOnly {
		return nil, 0, syscall.EROFS
	}

	var buf *buffer
	if flags&syscall.O_TRUNC != 0 {
		buf = newBuffer()
		buf.markDirty() // truncate-to-empty is a write: persist the emptiness.
	} else {
		data, errno := f.load(ctx)
		if errno != 0 && errno != syscall.ENOENT {
			return nil, 0, errno
		}
		buf = bufferOf(data)
	}
	h := &handle{buf: buf}
	f.setHandle(h)
	return h, 0, 0
}

func (f *fileNode) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	// A writing handle serves its own reads from the buffer, so a program that
	// writes then reads back the same open fd sees its own bytes.
	if h, ok := fh.(*handle); ok && h.buf != nil {
		data := h.buf.Bytes()
		if off >= int64(len(data)) {
			return fuse.ReadResultData(nil), 0
		}
		end := off + int64(len(dest))
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		return fuse.ReadResultData(data[off:end]), 0
	}

	rc, err := f.fs.client.Get(ctx, f.path, off, int64(len(dest)))
	if err != nil {
		return nil, toErrno(err)
	}
	defer rc.Close()

	n, err := io.ReadFull(rc, dest)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (f *fileNode) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	h, ok := fh.(*handle)
	if !ok || h.buf == nil {
		return 0, syscall.EBADF
	}
	n, _ := h.buf.WriteAt(data, off)
	return uint32(n), 0
}

// Flush uploads the buffer if it changed. close(2) can trigger Flush more than
// once for one open file (a dup'd fd, for one), so the upload clears the dirty
// flag and the next Flush is a no-op rather than a second identical PUT.
func (f *fileNode) Flush(ctx context.Context, fh fs.FileHandle) syscall.Errno {
	h, ok := fh.(*handle)
	if !ok || h.buf == nil {
		return 0
	}
	return f.flush(ctx, h)
}

// Fsync makes durability explicit: the same upload as Flush, on demand.
func (f *fileNode) Fsync(ctx context.Context, fh fs.FileHandle, flags uint32) syscall.Errno {
	h, ok := fh.(*handle)
	if !ok || h.buf == nil {
		return 0
	}
	return f.flush(ctx, h)
}

// Release is the final close. It flushes as a safety net for a handle that was
// never explicitly flushed, then forgets its buffer.
func (f *fileNode) Release(ctx context.Context, fh fs.FileHandle) syscall.Errno {
	h, ok := fh.(*handle)
	if !ok {
		return 0
	}
	var errno syscall.Errno
	if h.buf != nil {
		errno = f.flush(ctx, h)
		f.clearHandle(h)
	}
	return errno
}

func (f *fileNode) flush(ctx context.Context, h *handle) syscall.Errno {
	if !h.buf.Dirty() {
		return 0
	}
	if errno := f.putBytes(ctx, h.buf.Bytes()); errno != 0 {
		return errno
	}
	h.buf.clearDirty()
	return 0
}

func (f *fileNode) putBytes(ctx context.Context, data []byte) syscall.Errno {
	if err := f.fs.client.Put(ctx, f.path, bytes.NewReader(data), int64(len(data))); err != nil {
		return toErrno(err)
	}
	f.setSize(int64(len(data)))
	return 0
}

// load fetches the object's current bytes for a read-modify-write open.
func (f *fileNode) load(ctx context.Context) ([]byte, syscall.Errno) {
	rc, err := f.fs.client.Get(ctx, f.path, 0, -1)
	if err != nil {
		return nil, toErrno(err)
	}
	defer rc.Close()
	data, rerr := io.ReadAll(rc)
	if rerr != nil {
		return nil, syscall.EIO
	}
	return data, 0
}

func (f *fileNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	return f.fs.statfs(ctx, out)
}

// handle is one open file. A nil buf means read-only: reads stream from the
// host and there is nothing to flush. A non-nil buf is the pending contents of
// a writing open.
type handle struct {
	buf *buffer
}

// statfs answers df from the host's usage meter. When the host reports a limit
// -- a tenant cap or the per-sandbox quota, whichever binds -- the total is that
// limit and df tells the truth about how much room is left. When it reports none
// (unlimited), there is no honest total to give, so it falls back to
// used-plus-headroom just to keep df and space-preflighting tools working. Either
// way the real enforcement is the EDQUOT a write gets when it crosses the line,
// which df cannot show but which is where the limit actually lives.
func (f *FS) statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	u, err := f.client.Usage(ctx)
	if err != nil {
		return toErrno(err)
	}
	used := (u.BytesWritten + statfsBlockSize - 1) / statfsBlockSize

	var total int64
	if u.Limit > 0 {
		total = (u.Limit + statfsBlockSize - 1) / statfsBlockSize
		// A sandbox already over its cap (a tenant lowered after the fact, or a
		// sibling that filled a shared tenant) must not report negative free
		// space, which underflows to an absurd df. Clamp used to the total.
		if used > total {
			used = total
		}
	} else {
		total = used + statfsHeadroom
	}

	out.Bsize = statfsBlockSize
	out.Frsize = statfsBlockSize
	out.NameLen = 255
	out.Blocks = uint64(total)
	out.Bfree = uint64(total - used)
	out.Bavail = out.Bfree
	out.Files = uint64(u.Objects)
	out.Ffree = statfsHeadroom
	return 0
}

// childPath joins a directory path and a name into a guest path with a leading
// slash. The root's path is "", so its children are "/name"; a subdirectory
// "/a" yields "/a/name".
func childPath(dir, name string) string {
	if dir == "" {
		return "/" + name
	}
	return dir + "/" + name
}

// baseName is the last path segment, with any trailing slash (as CommonPrefixes
// carry) removed first.
func baseName(p string) string {
	return path.Base(strings.TrimRight(p, "/"))
}

// fillFileAttr writes a regular file's attributes, including an mtime when one
// is known so `ls -l` and make's timestamp checks see something real.
func fillFileAttr(a *fuse.Attr, size int64, mtime time.Time) {
	a.Mode = fileMode
	a.Size = uint64(size)
	if !mtime.IsZero() {
		a.Mtime = uint64(mtime.Unix())
		a.Mtimensec = uint32(mtime.Nanosecond())
		a.Ctime = a.Mtime
		a.Ctimensec = a.Mtimensec
	}
}
