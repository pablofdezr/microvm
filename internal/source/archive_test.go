package source

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
)

// tarEntry describes one member to write into a test archive. Archives are built
// in memory: a test that needs the network, or a fixture file, is a test that
// does not run in CI.
type tarEntry struct {
	name string
	typ  byte
	mode int64
	body string
	link string
	size int64
}

func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(e.body))
		if e.size != 0 {
			size = e.size
		}
		if typ != tar.TypeReg {
			size = 0
		}

		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: typ,
			Mode:     mode,
			Size:     size,
			Linkname: e.link,
		}
		if typ == tar.TypeXGlobalHeader {
			// What `git archive` emits, and the only shape tar.Writer will encode
			// for a global header.
			hdr = &tar.Header{
				Name:       e.name,
				Typeflag:   typ,
				PAXRecords: map[string]string{"comment": "0123456789abcdef"},
				Format:     tar.FormatPAX,
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if typ == tar.TypeReg && e.body != "" {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A header declaring more bytes than follow it is the shape of a decompression
	// bomb, and tar.Writer refuses to close over one. The bytes so far are
	// returned without a trailer, which is enough: the size cap trips on the
	// header, long before anything looks for the end of the archive.
	tw.Close()
	return buf.Bytes()
}

// archiveFrom wraps bytes as a fetched archive, skipping the fetch.
func archiveFrom(t *testing.T, cfg Config, body []byte) *Archive {
	t.Helper()

	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "arc-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(body); err != nil {
		t.Fatal(err)
	}
	arc := &Archive{buf: f, size: int64(len(body)), cfg: cfg}
	t.Cleanup(func() { arc.Close() })
	return arc
}

type writtenFile struct {
	content string
	mode    string
}

// recordingWriter stands in for a sandbox. It records what it was asked to
// create, which is how the "nothing is written until everything validates" tests
// see that nothing was written.
type recordingWriter struct {
	dirs  []string
	files map[string]writtenFile
	fail  error
}

func (w *recordingWriter) Mkdir(_ context.Context, path string) error {
	if w.fail != nil {
		return w.fail
	}
	w.dirs = append(w.dirs, path)
	return nil
}

func (w *recordingWriter) WriteFile(_ context.Context, path string, content io.Reader, mode string) error {
	if w.fail != nil {
		return w.fail
	}
	b, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	if w.files == nil {
		w.files = map[string]writtenFile{}
	}
	w.files[path] = writtenFile{content: string(b), mode: mode}
	return nil
}

func (w *recordingWriter) touched() int { return len(w.dirs) + len(w.files) }

func TestExpandWritesEveryMember(t *testing.T) {
	arc := archiveFrom(t, Config{}, gzipBytes(t, buildTar(t,
		tarEntry{name: "./", typ: tar.TypeDir},
		tarEntry{name: "README.md", body: "hi\n"},
		tarEntry{name: "src/", typ: tar.TypeDir},
		tarEntry{name: "src/main.go", body: "package main\n"},
		tarEntry{name: "bin/", typ: tar.TypeDir},
		tarEntry{name: "bin/run", body: "#!/bin/sh\n", mode: 0o755},
		tarEntry{name: "empty/", typ: tar.TypeDir},
	)))

	var w recordingWriter
	man, err := arc.Expand(context.Background(), Options{}, &w)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	if man.Files != 3 {
		t.Errorf("Files = %d, want 3", man.Files)
	}
	if man.Bytes != int64(len("hi\n")+len("package main\n")+len("#!/bin/sh\n")) {
		t.Errorf("Bytes = %d", man.Bytes)
	}
	want := map[string]writtenFile{
		"README.md":   {content: "hi\n", mode: "0644"},
		"src/main.go": {content: "package main\n", mode: "0644"},
		"bin/run":     {content: "#!/bin/sh\n", mode: "0755"},
	}
	for path, wf := range want {
		got, ok := w.files[path]
		if !ok {
			t.Errorf("%s was not written", path)
			continue
		}
		if got != wf {
			t.Errorf("%s = %+v, want %+v", path, got, wf)
		}
	}

	// Only the directory no file will create on the way in is asked for: the
	// others cost a round trip to make a directory that is about to exist.
	if len(w.dirs) != 1 || w.dirs[0] != "empty" {
		t.Errorf("created directories %v, want only [empty]", w.dirs)
	}
}

func TestStripComponents(t *testing.T) {
	body := gzipBytes(t, buildTar(t,
		tarEntry{name: "repo-abc123/", typ: tar.TypeDir},
		tarEntry{name: "repo-abc123/go.mod", body: "module x\n"},
		tarEntry{name: "repo-abc123/internal/", typ: tar.TypeDir},
		tarEntry{name: "repo-abc123/internal/a.go", body: "package a\n"},
	))

	tests := []struct {
		strip int
		want  []string
	}{
		{0, []string{"repo-abc123/go.mod", "repo-abc123/internal/a.go"}},
		{1, []string{"go.mod", "internal/a.go"}},
		{2, []string{"a.go"}},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("strip %d", tc.strip), func(t *testing.T) {
			var w recordingWriter
			if _, err := archiveFrom(t, Config{}, body).Expand(
				context.Background(), Options{StripComponents: tc.strip}, &w); err != nil {
				t.Fatalf("Expand: %v", err)
			}

			var got []string
			for p := range w.files {
				got = append(got, p)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("wrote %v, want %v", got, tc.want)
			}
		})
	}
}

// Stripping everything away is an error rather than an empty success: a seed
// that silently does nothing is a bug someone spends an afternoon on.
func TestStripComponentsTooDeep(t *testing.T) {
	arc := archiveFrom(t, Config{}, buildTar(t,
		tarEntry{name: "repo/", typ: tar.TypeDir},
		tarEntry{name: "repo/go.mod", body: "module x\n"},
	))

	_, err := arc.Validate(Options{StripComponents: 4})
	if !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("Validate returned %v, want ErrInvalidArchive", err)
	}
	if !strings.Contains(err.Error(), "strip_components") {
		t.Errorf("the error does not name the parameter at fault: %v", err)
	}
}

func TestStripComponentsOutOfRange(t *testing.T) {
	arc := archiveFrom(t, Config{}, buildTar(t, tarEntry{name: "a.txt", body: "a"}))
	for _, strip := range []int{-1, MaxStripComponents + 1} {
		if _, err := arc.Validate(Options{StripComponents: strip}); !errors.Is(err, ErrInvalidArchive) {
			t.Errorf("strip_components %d returned %v, want ErrInvalidArchive", strip, err)
		}
	}
}

// One row per member class this package refuses. Each is run through Expand
// rather than Validate, so each also asserts the refusal happens before anything
// is written.
func TestRefusedMembers(t *testing.T) {
	tests := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{
			"an absolute path",
			[]tarEntry{{name: "ok.txt", body: "ok"}, {name: "/etc/passwd", body: "root:x:0:0"}},
			"absolute",
		},
		{
			"a traversal out of the root",
			[]tarEntry{{name: "ok.txt", body: "ok"}, {name: "../../etc/passwd", body: "root:x:0:0"}},
			`".."`,
		},
		{
			"a traversal in the middle of a path",
			[]tarEntry{{name: "a/../../etc/passwd", body: "x"}},
			`".."`,
		},
		{
			"a bare parent reference",
			[]tarEntry{{name: "..", typ: tar.TypeDir}},
			`".."`,
		},
		{
			"a symlink out of the root",
			[]tarEntry{{name: "ok.txt", body: "ok"}, {name: "shadow", typ: tar.TypeSymlink, link: "/etc/shadow"}},
			"link",
		},
		{
			"a symlink inside the root, which is refused just the same",
			[]tarEntry{{name: "a.txt", body: "a"}, {name: "b.txt", typ: tar.TypeSymlink, link: "a.txt"}},
			"link",
		},
		{
			"a hardlink",
			[]tarEntry{{name: "a.txt", body: "a"}, {name: "b.txt", typ: tar.TypeLink, link: "a.txt"}},
			"link",
		},
		{
			"a fifo",
			[]tarEntry{{name: "pipe", typ: tar.TypeFifo}},
			"not a regular file",
		},
		{
			"a character device",
			[]tarEntry{{name: "mem", typ: tar.TypeChar}},
			"not a regular file",
		},
		{
			"a block device",
			[]tarEntry{{name: "disk", typ: tar.TypeBlock}},
			"not a regular file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			arc := archiveFrom(t, Config{}, gzipBytes(t, buildTar(t, tc.entries...)))

			var w recordingWriter
			_, err := arc.Expand(context.Background(), Options{}, &w)
			if !errors.Is(err, ErrInvalidArchive) {
				t.Fatalf("Expand returned %v, want ErrInvalidArchive", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not say why: %v", err)
			}
			if w.touched() != 0 {
				t.Errorf("wrote %d things before refusing: %v %v", w.touched(), w.dirs, w.files)
			}
		})
	}
}

// The invariant the two-pass walk exists for. The bad member is last, so a
// one-pass expander would already have written every good member before it saw
// the problem.
func TestNothingIsWrittenWhenTheLastMemberIsBad(t *testing.T) {
	arc := archiveFrom(t, Config{}, gzipBytes(t, buildTar(t,
		tarEntry{name: "a.txt", body: "a"},
		tarEntry{name: "b.txt", body: "b"},
		tarEntry{name: "c/", typ: tar.TypeDir},
		tarEntry{name: "c/d.txt", body: "d"},
		tarEntry{name: "escape", typ: tar.TypeSymlink, link: "/etc/shadow"},
	)))

	var w recordingWriter
	if _, err := arc.Expand(context.Background(), Options{}, &w); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("Expand returned %v, want ErrInvalidArchive", err)
	}
	if w.touched() != 0 {
		t.Fatalf("a half-seeded destination: wrote %v %v", w.dirs, w.files)
	}
}

// setuid is stripped, not refused. A member's mode is incidental -- unlike an
// explicit upload, where the mode was asked for and refusing it is honest -- so
// failing a whole tarball over one bit is a denial of service on legitimate
// archives. What must not survive is the bit itself: the host writes as root.
func TestModeKeepsOnlyTheExecutableBit(t *testing.T) {
	tests := []struct {
		mode int64
		want string
	}{
		{0o644, "0644"},
		{0o600, "0644"},
		{0o400, "0644"},
		{0o755, "0755"},
		{0o700, "0755"},
		{0o4755, "0755"}, // setuid
		{0o2755, "0755"}, // setgid
		{0o1755, "0755"}, // sticky
		{0o4644, "0644"},
		{0o777, "0755"},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%04o", tc.mode), func(t *testing.T) {
			arc := archiveFrom(t, Config{}, buildTar(t,
				tarEntry{name: "f", body: "x", mode: tc.mode}))

			var w recordingWriter
			if _, err := arc.Expand(context.Background(), Options{}, &w); err != nil {
				t.Fatalf("Expand: %v", err)
			}
			if got := w.files["f"].mode; got != tc.want {
				t.Errorf("mode %04o expanded as %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}

// The magic bytes decide, not the name the caller gave the URL.
func TestGzipIsDetectedByMagicBytes(t *testing.T) {
	plain := buildTar(t, tarEntry{name: "a.txt", body: "a"})

	for name, body := range map[string][]byte{
		"plain tar":   plain,
		"gzipped tar": gzipBytes(t, plain),
		"double gzip": gzipBytes(t, gzipBytes(t, plain)),
	} {
		t.Run(name, func(t *testing.T) {
			var w recordingWriter
			_, err := archiveFrom(t, Config{}, body).Expand(context.Background(), Options{}, &w)
			if name == "double gzip" {
				// One layer is unwrapped; what is inside is not a tar, and saying so
				// is better than unwrapping until something looks like one.
				if !errors.Is(err, ErrInvalidArchive) {
					t.Fatalf("a doubly gzipped archive returned %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			if w.files["a.txt"].content != "a" {
				t.Errorf("a.txt = %+v", w.files["a.txt"])
			}
		})
	}
}

func TestCaps(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		body func() []byte
		want string
	}{
		{
			"member count",
			Config{MaxFiles: 3},
			func() []byte {
				var entries []tarEntry
				for i := 0; i < 10; i++ {
					entries = append(entries, tarEntry{name: fmt.Sprintf("f%d", i), body: "x"})
				}
				return buildTar(t, entries...)
			},
			"more than 3 members",
		},
		{
			"total expanded size",
			Config{MaxExpandedBytes: 100},
			func() []byte {
				return buildTar(t,
					tarEntry{name: "a", body: strings.Repeat("a", 60)},
					tarEntry{name: "b", body: strings.Repeat("b", 60)},
				)
			},
			"expands past the 100 byte limit",
		},
		{
			"a single member",
			Config{MaxFileBytes: 10},
			func() []byte {
				return buildTar(t, tarEntry{name: "big", body: strings.Repeat("x", 64)})
			},
			"per-file limit",
		},
		{
			// The bomb: a header claiming 10 GiB, in an archive of a few hundred
			// bytes. It trips on the header, before a byte is written anywhere.
			"a decompression bomb, on the declared size",
			Config{MaxExpandedBytes: 1 << 20, MaxFileBytes: 20 << 30},
			func() []byte {
				return gzipBytes(t, buildTar(t,
					tarEntry{name: "bomb", size: 10 << 30}))
			},
			"expands past",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			arc := archiveFrom(t, tc.cfg, tc.body())

			var w recordingWriter
			_, err := arc.Expand(context.Background(), Options{}, &w)
			if !errors.Is(err, ErrTooLarge) {
				t.Fatalf("Expand returned %v, want ErrTooLarge", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not say which cap: %v", err)
			}
			if w.touched() != 0 {
				t.Errorf("wrote %d things before refusing", w.touched())
			}
		})
	}
}

// A gzip stream is capped on the way out as well as on the way in. Unreachable
// for a well-formed tar under the member cap, which is exactly why it is tested
// here rather than left to be discovered.
func TestCappedReader(t *testing.T) {
	tests := []struct {
		name  string
		size  int
		cap   int64
		wantN int
		fails bool
	}{
		{"under the cap", 10, 100, 10, false},
		{"exactly the cap", 100, 100, 100, false},
		{"one byte over", 101, 100, 100, true},
		{"far over", 1 << 20, 100, 100, true},
		{"empty", 0, 100, 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &cappedReader{
				r:    bytes.NewReader(bytes.Repeat([]byte("x"), tc.size)),
				cap:  tc.cap,
				what: "body",
			}
			n, err := io.Copy(io.Discard, r)

			if tc.fails {
				if !errors.Is(err, ErrTooLarge) {
					t.Fatalf("read %d bytes with err %v, want ErrTooLarge", n, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n != int64(tc.wantN) {
				t.Errorf("read %d bytes, want %d", n, tc.wantN)
			}
		})
	}
}

// `git archive` puts a pax global header in front of everything. Refusing it
// would refuse the most obvious way to make a tarball of a repository.
func TestPaxGlobalHeaderIsNotAMember(t *testing.T) {
	arc := archiveFrom(t, Config{}, buildTar(t,
		tarEntry{name: "pax_global_header", typ: tar.TypeXGlobalHeader},
		tarEntry{name: "a.txt", body: "a"},
	))

	man, err := arc.Validate(Options{})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if man.Files != 1 || len(man.Entries) != 1 {
		t.Errorf("manifest counted the global header: %+v", man)
	}
}

func TestEmptyAndUnreadableArchives(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want error
	}{
		{"an archive with only a trailer", buildTar(t), ErrInvalidArchive},
		{"not a tar at all", []byte(strings.Repeat("garbage.", 200)), ErrInvalidArchive},
		{"too short to have a header", []byte("x"), ErrInvalidArchive},
		{"gzip of nothing", gzipBytes(t, nil), ErrInvalidArchive},
		{"broken gzip", append([]byte{0x1f, 0x8b}, []byte("not gzip")...), ErrInvalidArchive},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			arc := archiveFrom(t, Config{}, tc.body)
			if _, err := arc.Validate(Options{}); !errors.Is(err, tc.want) {
				t.Fatalf("Validate returned %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMemberPath(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		refused bool
	}{
		{"a.txt", "a.txt", false},
		{"a/b/c.txt", "a/b/c.txt", false},
		{"a/b/", "a/b", false},
		{"./a.txt", "a.txt", false},
		{"a//b.txt", "a/b.txt", false},
		{"a/./b.txt", "a/b.txt", false},
		{".", "", false},
		{"./", "", false},

		{"", "", true},
		{"/etc/passwd", "", true},
		{"/", "", true},
		{"../a", "", true},
		{"a/../../b", "", true},
		{"..", "", true},
		{"a/..", "", true},
		{"a/\x00b", "", true},
	}

	for _, tc := range tests {
		got, err := memberPath(tc.in)
		if tc.refused {
			if err == nil {
				t.Errorf("memberPath(%q) = %q, want a refusal", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("memberPath(%q) refused: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("memberPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A destination failure is not an archive failure. The archive was fine; the
// sandbox is what went wrong, and the layers above owe those different answers.
func TestDestinationFailuresKeepTheirOwnClass(t *testing.T) {
	arc := archiveFrom(t, Config{}, buildTar(t, tarEntry{name: "a.txt", body: "a"}))

	boom := errors.New("guest is gone")
	w := &recordingWriter{fail: boom}

	_, err := arc.Expand(context.Background(), Options{}, w)
	if !errors.Is(err, boom) {
		t.Fatalf("Expand returned %v, want the destination's error", err)
	}
	for _, sentinel := range []error{ErrInvalidArchive, ErrTooLarge, ErrNotPermitted, ErrFetchFailed} {
		if errors.Is(err, sentinel) {
			t.Errorf("a destination failure was reported as %v", sentinel)
		}
	}
}

func TestExpandHonoursContextCancellation(t *testing.T) {
	arc := archiveFrom(t, Config{}, buildTar(t,
		tarEntry{name: "a.txt", body: "a"},
		tarEntry{name: "b.txt", body: "b"},
	))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var w recordingWriter
	if _, err := arc.Expand(ctx, Options{}, &w); !errors.Is(err, context.Canceled) {
		t.Fatalf("Expand returned %v, want context.Canceled", err)
	}
	if w.touched() != 0 {
		t.Errorf("wrote %d things after cancellation", w.touched())
	}
}

func TestStandaloneDirs(t *testing.T) {
	entries := []Entry{
		{Path: "a", Dir: true},
		{Path: "a/b", Dir: true},
		{Path: "a/b/c.txt"},
		{Path: "empty", Dir: true},
		{Path: "deep/nested/empty", Dir: true},
	}

	got := standaloneDirs(entries)
	want := map[string]bool{"empty": true, "deep/nested/empty": true}

	if len(got) != len(want) {
		t.Fatalf("standaloneDirs = %v, want %v", got, want)
	}
	for path := range want {
		if !got[path] {
			t.Errorf("%s is not standalone but should be", path)
		}
	}
}

// A member name is bounded twice: one name against what the destination could
// hold, and every name together against what the operator's member cap implies.
//
// Neither bound is about traversal. A name is content the archive chose and the
// manifest retains, so without them a compressed request measured in kilobytes
// becomes hundreds of megabytes of the daemon's heap -- a GNU longname record
// carries up to a megabyte, and nothing counted it against any cap.
func TestMemberNamesAreBounded(t *testing.T) {
	t.Run("one name past what the destination takes", func(t *testing.T) {
		arc := archiveFrom(t, Config{}, buildTar(t,
			tarEntry{name: strings.Repeat("a", maxMemberNameBytes+1), body: "x"}))

		_, err := arc.Validate(Options{})
		if !errors.Is(err, ErrInvalidArchive) {
			t.Fatalf("Validate = %v, want ErrInvalidArchive", err)
		}
	})

	t.Run("a name of exactly the limit", func(t *testing.T) {
		arc := archiveFrom(t, Config{}, buildTar(t,
			tarEntry{name: strings.Repeat("a", maxMemberNameBytes), body: "x"}))

		if _, err := arc.Validate(Options{}); err != nil {
			t.Fatalf("a name of exactly the limit was refused: %v", err)
		}
	})

	t.Run("every name together", func(t *testing.T) {
		// Each name is inside the per-name bound; the archive is not.
		const members = 40
		var entries []tarEntry
		for i := 0; i < members; i++ {
			entries = append(entries, tarEntry{
				name: fmt.Sprintf("%s%d", strings.Repeat("n", 2000), i),
				body: "x",
			})
		}
		arc := archiveFrom(t, Config{MaxFiles: members}, gzipBytes(t, buildTar(t, entries...)))

		_, err := arc.Validate(Options{})
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("Validate = %v, want ErrTooLarge", err)
		}
	})
}

// Every refusal quotes the member at fault, which is right, and a member name is
// attacker-supplied -- so the reply and the log line it becomes have to be bounded
// by something other than the archive's own generosity.
func TestARefusalDoesNotQuoteAWholeMemberName(t *testing.T) {
	name := strings.Repeat("a", 3000)
	arc := archiveFrom(t, Config{}, buildTar(t,
		tarEntry{name: name, typ: tar.TypeSymlink, link: "/"}))

	_, err := arc.Validate(Options{})
	if !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("Validate = %v, want ErrInvalidArchive", err)
	}
	if len(err.Error()) >= len(name) {
		t.Errorf("the refusal is %d bytes for a %d byte name", len(err.Error()), len(name))
	}
	if !strings.Contains(err.Error(), "aaaa") {
		t.Errorf("the refusal no longer says which member: %v", err)
	}
}

// Members are validated one at a time, which is one member short of enough: two
// that are each fine can still contradict each other about whether a path is a
// file or a directory, and the destination answers the second write with ENOTDIR
// or EISDIR. Caught here it is a refusal like any other member this cannot expand;
// missed, it is a 500 for an archive the caller controls entirely.
func TestCollidingMembersAreRefused(t *testing.T) {
	tests := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{
			"a file with a member inside it",
			[]tarEntry{{name: "a", body: "i am a file"}, {name: "a/b", body: "i need a dir"}},
			"which the archive holds as a file",
		},
		{
			"a file with a member deep inside it",
			[]tarEntry{{name: "a", body: "x"}, {name: "a/b/c/d", body: "x"}},
			"which the archive holds as a file",
		},
		{
			"a directory and a file of one name",
			[]tarEntry{{name: "a/", typ: tar.TypeDir}, {name: "a", body: "x"}},
			"both a file and a directory",
		},
		{
			"the same, in the other order",
			[]tarEntry{{name: "a", body: "x"}, {name: "a/", typ: tar.TypeDir}},
			"both a file and a directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			arc := archiveFrom(t, Config{}, buildTar(t, tc.entries...))

			var w recordingWriter
			_, err := arc.Expand(context.Background(), Options{}, &w)
			if !errors.Is(err, ErrInvalidArchive) {
				t.Fatalf("Expand = %v, want ErrInvalidArchive", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say what collided: %v", err)
			}
			if w.touched() != 0 {
				t.Errorf("wrote %d things before refusing", w.touched())
			}
		})
	}
}

// A directory holding a file of the same name as a directory elsewhere is not a
// collision, and neither is a file under a directory that was declared.
func TestNonCollidingMembersStillPass(t *testing.T) {
	arc := archiveFrom(t, Config{}, buildTar(t,
		tarEntry{name: "a/", typ: tar.TypeDir},
		tarEntry{name: "a/b", body: "x"},
		tarEntry{name: "b", body: "x"},
		tarEntry{name: "c/b", body: "x"},
	))

	if _, err := arc.Validate(Options{}); err != nil {
		t.Fatalf("a well-formed archive was refused as colliding: %v", err)
	}
}

// A pax global header is not a member, and it is still a header: the tar reader
// parses each one's records into a map, so an archive of nothing else costs per
// record. -source-max-files has to mean what its help text says.
func TestGlobalHeadersCountAgainstTheMemberCap(t *testing.T) {
	var entries []tarEntry
	for i := 0; i < 50; i++ {
		entries = append(entries, tarEntry{name: fmt.Sprintf("g%d", i), typ: tar.TypeXGlobalHeader})
	}
	entries = append(entries, tarEntry{name: "f", body: "x"})
	arc := archiveFrom(t, Config{MaxFiles: 4}, buildTar(t, entries...))

	_, err := arc.Validate(Options{})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Validate = %v, want ErrTooLarge", err)
	}
}

// The decompression allowance is what stops a bomb, and it must not be something
// an operator raises by accident. Derived from the member cap it was: raising
// -source-max-files for a monorepo silently bought gigabytes of decompressed
// stream. Each member funds its own framing instead, so the two are independent.
func TestTheDecompressionAllowanceDoesNotFollowTheMemberCap(t *testing.T) {
	body := gzipBytes(t, buildTar(t, tarEntry{name: "a.txt", body: "x"}))
	const expanded = 1 << 20

	for _, maxFiles := range []int{1, 20_000, 10_000_000} {
		arc := archiveFrom(t, Config{MaxExpandedBytes: expanded, MaxFiles: maxFiles}, body)
		if _, err := arc.buf.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		_, capped, closeStream, err := arc.decompress()
		if err != nil {
			t.Fatal(err)
		}
		closeStream()

		// Whatever the member cap is, the stream a caller may present before a
		// single member has been validated is the expanded cap plus one member's
		// framing. Members raise it from there, one member at a time, and each one
		// costs a header in the stream and a slot against the cap it was allowed by.
		if got := capped.cap; got > expanded+tarFramingSlack {
			t.Errorf("-source-max-files %d bought a %d byte decompression allowance over a %d byte expanded cap",
				maxFiles, got, expanded)
		}
	}
}

// And the other half of that: an archive of many small members is exactly what
// the member cap is for, so the framing those members legitimately need must not
// trip the decompression allowance instead.
func TestManySmallMembersAreNotADecompressionBomb(t *testing.T) {
	const members = 2000
	var entries []tarEntry
	for i := 0; i < members; i++ {
		entries = append(entries, tarEntry{name: fmt.Sprintf("dir/f%05d.txt", i), body: "x"})
	}
	// A tiny expanded cap, so nothing but the framing is in question: the tar is
	// over a megabyte and the content in it is two kilobytes.
	arc := archiveFrom(t, Config{MaxExpandedBytes: 8 << 10, MaxFiles: members},
		gzipBytes(t, buildTar(t, entries...)))

	man, err := arc.Validate(Options{})
	if err != nil {
		t.Fatalf("an archive of %d small files was refused: %v", members, err)
	}
	if man.Files != members {
		t.Errorf("Files = %d, want %d", man.Files, members)
	}
}
