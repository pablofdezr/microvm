package source

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
)

// Writer receives an expanded archive.
//
// Both paths are relative and slash-separated, and neither is ever a host path:
// the caller joins them to a destination root of its own choosing. That is the
// whole reason this is an interface -- the destination is a sandbox, reached over
// the guest file endpoints, and this package has no business knowing that.
//
// The shape matches what the guest already offers, deliberately. A seed that
// needed a new guest primitive would need every rootfs image rebuilt and
// redistributed before it worked anywhere.
type Writer interface {
	// Mkdir creates a directory and its parents. An existing directory is
	// success.
	Mkdir(ctx context.Context, path string) error
	// WriteFile writes content at path, creating parent directories, with mode
	// given as octal digits.
	WriteFile(ctx context.Context, path string, content io.Reader, mode string) error
}

// Options configures an expansion.
type Options struct {
	// StripComponents drops that many leading path components from every member,
	// so the single top-level directory a release tarball wraps everything in does
	// not become a directory in the destination. A member with no components left
	// is skipped, which is what tar does; a value that skips everything is an
	// error, because silently seeding nothing is worse than refusing.
	StripComponents int
}

// Archive is a fetched archive, buffered on the host and not yet expanded.
type Archive struct {
	buf  *os.File
	size int64
	cfg  Config
}

// Size is the compressed size as it arrived.
func (a *Archive) Size() int64 { return a.size }

// Close releases the buffer.
func (a *Archive) Close() error {
	if a.buf == nil {
		return nil
	}
	err := a.buf.Close()
	a.buf = nil
	return err
}

// Entry is one member of a validated archive, described in terms of the
// destination rather than the archive.
type Entry struct {
	// Path is relative, slash-separated, and contains no "." or ".." component.
	Path string
	Dir  bool
	Size int64
	// Mode is "0755" for a file the archive marked executable and "0644"
	// otherwise, and empty for a directory. Nothing else about a member's mode
	// survives: the executable bit is the only one that changes what a checkout
	// does, and the host writes into the guest as root, so setuid, setgid and the
	// sticky bit would leave a root-owned setuid binary behind for whatever runs
	// next. Stripped rather than refused -- unlike an explicit upload, a member's
	// bits are incidental, and failing a whole archive over one is a denial of
	// service against legitimate tarballs.
	Mode string
}

// Manifest is what an archive would expand to.
type Manifest struct {
	Entries []Entry
	Files   int
	Dirs    int
	// Bytes is the total expanded size.
	Bytes int64
}

// Validate walks the whole archive and reports what it would expand to, writing
// nothing anywhere.
func (a *Archive) Validate(opts Options) (*Manifest, error) {
	if opts.StripComponents < 0 || opts.StripComponents > MaxStripComponents {
		return nil, invalidArchive("strip_components must be between 0 and %d", MaxStripComponents)
	}

	man := &Manifest{}
	members, err := a.walk(opts.StripComponents, func(m member, _ io.Reader) error {
		man.Entries = append(man.Entries, m.entry())
		if m.dir {
			man.Dirs++
			return nil
		}
		man.Files++
		man.Bytes += m.size
		return nil
	})
	if err != nil {
		return nil, err
	}

	switch {
	case members == 0:
		return nil, invalidArchive("archive contains no members")
	case len(man.Entries) == 0 && opts.StripComponents > 0:
		return nil, invalidArchive(
			"strip_components %d removed every member of an archive %d deep",
			opts.StripComponents, members)
	case len(man.Entries) == 0:
		return nil, invalidArchive("archive contains no files or directories")
	}
	if err := checkCollisions(man.Entries); err != nil {
		return nil, err
	}
	return man, nil
}

// checkCollisions refuses an archive whose members disagree about what a path
// is.
//
// Every other check judges a member on its own, which is one member short of
// enough: a file "a" and a file "a/b" are each fine and cannot both be written,
// and neither can a directory "a" alongside a file "a". The destination answers
// the second write with ENOTDIR or EISDIR, and a failure there is ours to explain
// rather than the archive's -- which is the wrong way round, because the archive
// is the thing that is wrong. Caught here, it is a refusal like any other member
// this cannot expand.
func checkCollisions(entries []Entry) error {
	isDir := make(map[string]bool, len(entries))
	for _, e := range entries {
		if was, seen := isDir[e.Path]; seen && was != e.Dir {
			return invalidArchive("member %q is both a file and a directory", elide(e.Path))
		}
		isDir[e.Path] = e.Dir
	}
	for _, e := range entries {
		for dir := path.Dir(e.Path); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if dirEntry, seen := isDir[dir]; seen && !dirEntry {
				return invalidArchive("member %q is inside %q, which the archive holds as a file",
					elide(e.Path), elide(dir))
			}
		}
	}
	return nil
}

// Expand validates every member and only then writes any of them.
//
// Two passes over one buffer, which is the same rule the batch file route
// follows: nothing reaches the destination until the whole archive is known
// good, so a hostile member near the end cannot leave a half-seeded sandbox
// behind. Both passes go through walk, so the validation cannot drift between
// the pass that decides and the pass that writes.
func (a *Archive) Expand(ctx context.Context, opts Options, w Writer) (*Manifest, error) {
	man, err := a.Validate(opts)
	if err != nil {
		return nil, err
	}

	standalone := standaloneDirs(man.Entries)
	_, err = a.walk(opts.StripComponents, func(m member, r io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.dir {
			// Every other directory is created by the write of the file inside it,
			// so creating them all would be one round trip per directory to make a
			// directory that is about to exist anyway.
			if !standalone[m.path] {
				return nil
			}
			if err := w.Mkdir(ctx, m.path); err != nil {
				return fmt.Errorf("create %s: %w", m.path, err)
			}
			return nil
		}
		if err := w.WriteFile(ctx, m.path, r, m.mode); err != nil {
			return fmt.Errorf("write %s: %w", m.path, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return man, nil
}

// standaloneDirs are the directory members no file member will create on the
// way in. An empty directory in the archive is an empty directory in the
// destination, and it is the only kind that has to be asked for.
func standaloneDirs(entries []Entry) map[string]bool {
	implied := map[string]bool{}
	for _, e := range entries {
		if e.Dir {
			continue
		}
		for dir := path.Dir(e.Path); dir != "." && dir != "/"; dir = path.Dir(dir) {
			implied[dir] = true
		}
	}

	standalone := map[string]bool{}
	for _, e := range entries {
		if e.Dir && !implied[e.Path] {
			standalone[e.Path] = true
		}
	}
	return standalone
}

// member is one validated archive member.
type member struct {
	path string
	dir  bool
	size int64
	mode string
}

func (m member) entry() Entry {
	return Entry{Path: m.path, Dir: m.dir, Size: m.size, Mode: m.mode}
}

// walk reads the buffer once, validating every member and calling fn for the
// ones that survive stripping. It returns the number of members the archive
// contained.
//
// This is the only place a member is inspected. Both passes of an expansion call
// it, so "validate everything, then write" is a property of the design rather
// than a rule an implementer has to remember.
func (a *Archive) walk(strip int, fn func(m member, r io.Reader) error) (int, error) {
	if a.buf == nil {
		return 0, errors.New("archive is closed")
	}
	if _, err := a.buf.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("rewind source buffer: %w", err)
	}

	r, capped, closeStream, err := a.decompress()
	if err != nil {
		return 0, err
	}
	defer closeStream()

	var (
		tr        = tar.NewReader(r)
		members   int
		files     int
		total     int64
		nameBytes int64
		nameCap   = int64(a.cfg.MaxFiles) * maxNameBytesPerMember
	)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if isCapExceeded(err) {
				return members, err
			}
			return members, invalidArchive("archive is not readable as a tar: %v", err)
		}
		// The names, before anything is done with one. A member name is not
		// counted against the expanded cap -- it is not content -- so without this
		// it is counted nowhere: 1 MiB per name is what a GNU longname record will
		// carry, the manifest retains every one of them, and the archive that asks
		// for it compresses to nothing. The budget is per member rather than
		// absolute so an operator raising -source-max-files raises it too.
		nameBytes += int64(len(hdr.Name))
		if nameBytes > nameCap {
			return members, tooLarge("member names total more than %d bytes", nameCap)
		}
		// Each member funds the framing its own headers cost. A fixed allowance
		// above the expanded cap would either refuse a legitimate archive of many
		// small members or, derived from the member cap, hand an operator who
		// raised that cap a decompression allowance measured in gigabytes.
		capped.allow(tarFramingPerMember + int64(len(hdr.Name)))

		m, kind, err := classify(hdr, strip)
		if err != nil {
			return members, err
		}

		// Counted before the metadata check, which is the only reason this is not
		// a member: a pax global header is a header the tar reader parses into a
		// map, so an archive of nothing else still costs per record and must still
		// meet the cap the operator set.
		members++
		if members > a.cfg.MaxFiles {
			return members, tooLarge("archive has more than %d members", a.cfg.MaxFiles)
		}
		if kind == kindMetadata {
			// A pax global header is not a member. `git archive` emits one, so
			// refusing it would refuse the most obvious way to produce a tarball.
			continue
		}
		if kind == kindSkip {
			continue
		}

		if !m.dir {
			if m.size > a.cfg.MaxFileBytes {
				return members, tooLarge("member %q is %d bytes, over the %d byte per-file limit",
					elide(m.path), m.size, a.cfg.MaxFileBytes)
			}
			total += m.size
			if total > a.cfg.MaxExpandedBytes {
				return members, tooLarge("archive expands past the %d byte limit",
					a.cfg.MaxExpandedBytes)
			}
			files++
		}

		if err := fn(m, tr); err != nil {
			return members, err
		}
	}
	return members, nil
}

// decompress wraps the buffer in a gzip reader when the buffer is gzipped. The
// returned cappedReader is what walk raises as it validates members; it is never
// nil, so no caller has to check.
//
// The magic bytes decide, not the URL's suffix: the suffix is the caller's claim
// about the bytes, and this package does not take the caller's word for anything
// else either.
func (a *Archive) decompress() (io.Reader, *cappedReader, func(), error) {
	var magic [2]byte
	if _, err := a.buf.ReadAt(magic[:], 0); err != nil {
		return nil, nil, nil, invalidArchive("archive is too short to be a tar")
	}
	if magic[0] != 0x1f || magic[1] != 0x8b {
		// An uncompressed buffer was already bounded by MaxBytes as it arrived, so
		// there is nothing left to cap. The reader is handed back anyway, with room
		// nothing will reach, so walk raises one thing rather than two.
		return a.buf, &cappedReader{r: a.buf, cap: a.size, what: "archive"}, func() {}, nil
	}

	gz, err := gzip.NewReader(a.buf)
	if err != nil {
		return nil, nil, nil, invalidArchive("archive is not readable as gzip: %v", err)
	}
	// The compressed cap said nothing about this stream, which is the whole point
	// of a decompression bomb. The allowance starts at the expanded cap plus one
	// member's worth of the tar format's own framing, and walk raises it per member
	// from there: see the allow call.
	capped := &cappedReader{
		r:    gz,
		cap:  a.cfg.MaxExpandedBytes + tarFramingSlack,
		what: "decompressed archive",
	}
	return capped, capped, func() { gz.Close() }, nil
}

// The tar format's own overhead, which is read out of the decompressed stream and
// is not a member's content.
const (
	// tarFramingPerMember covers one member's headers and the padding to the next
	// 512-byte boundary. Two blocks of slack over the one header a plain member
	// needs, because a long name arrives as a second header ahead of the real one.
	tarFramingPerMember = 4 * 512
	// tarFramingSlack is what the allowance starts at, before any member has
	// funded its own: the longest name a member may carry plus its headers, since
	// those bytes are read before there is a header to charge them to, and the
	// two-block trailer that ends every archive.
	tarFramingSlack = maxMemberNameBytes + 6*512
)

type memberKind int

const (
	kindInclude memberKind = iota
	// kindSkip is a member stripping removed. It still counts against the member
	// cap and it was still validated: a symlink is refused whether or not
	// strip_components would have thrown it away.
	kindSkip
	// kindMetadata is archive bookkeeping rather than a member.
	kindMetadata
)

// classify validates one header and turns it into a member.
func classify(hdr *tar.Header, strip int) (member, memberKind, error) {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return member{}, kindMetadata, nil
	}

	var dir bool
	switch hdr.Typeflag {
	case tar.TypeReg:
	case tar.TypeDir:
		dir = true
	case tar.TypeSymlink, tar.TypeLink:
		// Every link is refused, not only one whose target escapes the root. The
		// guest file endpoints cannot express a link at all, so the alternatives
		// were to drop it silently -- a subtly broken checkout someone debugs for
		// an hour -- or to add a guest endpoint, which means rebuilding and
		// redistributing every rootfs image before a seed works anywhere. Refusing
		// is the honest answer and it removes the entire escape class with it.
		return member{}, 0, invalidArchive(
			"member %q is a link, which a source archive may not contain", elide(hdr.Name))
	default:
		return member{}, 0, invalidArchive(
			"member %q is not a regular file or a directory", elide(hdr.Name))
	}

	name, err := memberPath(hdr.Name)
	if err != nil {
		return member{}, 0, err
	}
	if name == "" {
		// The archive's own root, as `tar -c .` writes it.
		return member{}, kindSkip, nil
	}

	parts := strings.Split(name, "/")
	if len(parts) <= strip {
		return member{}, kindSkip, nil
	}
	name = strings.Join(parts[strip:], "/")

	m := member{path: name, dir: dir}
	if !dir {
		m.size = hdr.Size
		// The executable bit and nothing else.
		m.mode = "0644"
		if hdr.FileInfo().Mode().Perm()&0o111 != 0 {
			m.mode = "0755"
		}
	}
	return m, kindInclude, nil
}

// memberPath validates a member name and normalises it. It returns an empty
// name for the archive root, which is not an error.
//
// The ".." check is on the name as written rather than on the cleaned form.
// Cleaning would quietly accept "a/../b" as "b", and an archive that describes a
// path that way is not one to start guessing about.
func memberPath(name string) (string, error) {
	if name == "" {
		return "", invalidArchive("archive contains a member with no name")
	}
	if len(name) > maxMemberNameBytes {
		return "", invalidArchive("a member name is %d bytes, over the %d byte limit",
			len(name), maxMemberNameBytes)
	}
	if strings.ContainsRune(name, 0) {
		return "", invalidArchive("member name contains a NUL byte")
	}
	if strings.HasPrefix(name, "/") {
		return "", invalidArchive("member %q is an absolute path", elide(name))
	}

	trimmed := strings.TrimSuffix(name, "/")
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == ".." {
			return "", invalidArchive("member %q contains \"..\"", elide(name))
		}
	}

	clean := path.Clean(trimmed)
	if clean == "." || clean == "" {
		return "", nil
	}
	// Unreachable after the checks above, and kept anyway: this is the one value
	// in the package that must never be able to point outside the destination.
	if strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", invalidArchive("member %q escapes the destination", elide(name))
	}
	return clean, nil
}

// Bounds on a member name. Neither is about traversal -- memberPath already
// settles that -- they are about what a name costs us.
const (
	// maxMemberNameBytes is Linux's PATH_MAX, which is what the destination will
	// take: a longer name is one the guest answers with ENAMETOOLONG, so nothing
	// legitimate is refused by capping it here instead. Without a cap, a GNU
	// longname record carries up to 1 MiB, the manifest retains every name twice
	// over, and a compressed archive of a few hundred kilobytes becomes hundreds of
	// megabytes of the daemon's heap.
	maxMemberNameBytes = 4096
	// maxNameBytesPerMember is the average name length an archive is allowed, so
	// the total is bounded by a number the operator already chose. Real archives
	// average well under a hundred bytes; this is generous by four times and still
	// bounds what a member cap of 20 000 can retain to a few megabytes.
	maxNameBytesPerMember = 256
)

// elide shortens a member name for an error message.
//
// Every refusal in here quotes the name at fault, which is right -- the operator
// reading it has to know which member -- and a name is attacker-supplied, so the
// error would otherwise be a way to have this daemon compose a kilobyte-scale
// reply and log line out of a byte-scale request. The same rule the API applies to
// a caller's own parameters.
func elide(name string) string {
	const max = 96
	if len(name) <= max {
		return name
	}
	return name[:max] + "..."
}

// cappedReader fails at its cap instead of ending at it.
//
// io.LimitReader returns EOF there, which a tar reader reports as a truncated
// archive: the wrong answer to "this is too big", and one that presents a
// decompression bomb as a corrupt file.
type cappedReader struct {
	r    io.Reader
	cap  int64
	read int64
	what string
}

func (c *cappedReader) Read(p []byte) (int, error) {
	// One byte past the cap, so a body of exactly the cap is not refused for
	// reaching it.
	remaining := c.cap + 1 - c.read
	if remaining <= 0 {
		return 0, c.exceeded()
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.cap {
		return 0, c.exceeded()
	}
	return n, err
}

// allow raises the cap by n. Only walk calls it, and only for bytes it has
// already accounted for some other way: a member's own framing, which is read out
// of the same stream as its content but is not content.
func (c *cappedReader) allow(n int64) { c.cap += n }

func (c *cappedReader) exceeded() error {
	return tooLarge("%s exceeds the %d byte limit", c.what, c.cap)
}

func isCapExceeded(err error) bool { return errors.Is(err, ErrTooLarge) }

// unwrapSentinel finds one of this package's own failures inside a transport
// error, and returns nil when the failure came from somewhere else.
//
// The *url.Error wrapper is dropped rather than returned, because it quotes the
// whole URL, and a presigned URL's query string is a credential. That belongs in
// no error, including one that only ever reaches a log.
func unwrapSentinel(err error) error {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if _, wrapper := e.(*url.Error); wrapper {
			continue
		}
		for _, sentinel := range []error{ErrNotPermitted, ErrTooLarge, ErrFetchFailed, ErrInvalidArchive} {
			if errors.Is(e, sentinel) {
				return e
			}
		}
	}
	return nil
}
