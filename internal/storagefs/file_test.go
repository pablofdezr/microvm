package storagefs

import (
	"bytes"
	"errors"
	"syscall"
	"testing"

	"github.com/pablofdezr/microvm/internal/storageclient"
)

func TestBufferWriteAtGrows(t *testing.T) {
	b := newBuffer()
	if _, err := b.WriteAt([]byte("hello"), 0); err != nil {
		t.Fatal(err)
	}
	if got := b.Bytes(); !bytes.Equal(got, []byte("hello")) {
		t.Fatalf("got %q", got)
	}
	if b.Size() != 5 {
		t.Fatalf("size %d", b.Size())
	}
}

func TestBufferWriteAtOffsetZeroFillsGap(t *testing.T) {
	b := newBuffer()
	// Write at offset 3 into an empty buffer: the first three bytes were never
	// written and must read back as zeros, exactly as a seek-past-EOF then write
	// does on a real file.
	if _, err := b.WriteAt([]byte("xy"), 3); err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0, 0, 'x', 'y'}
	if got := b.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBufferOverwriteInPlaceDoesNotGrow(t *testing.T) {
	b := bufferOf([]byte("0123456789"))
	// Overwriting a middle slice must not extend the buffer -- a classic
	// off-by-one in the grow test would leave a stray trailing byte here.
	if _, err := b.WriteAt([]byte("AB"), 4); err != nil {
		t.Fatal(err)
	}
	if got := b.Bytes(); !bytes.Equal(got, []byte("0123AB6789")) {
		t.Fatalf("got %q", got)
	}
	if b.Size() != 10 {
		t.Fatalf("size %d, want 10", b.Size())
	}
}

func TestBufferTruncateShrinksAndGrows(t *testing.T) {
	b := bufferOf([]byte("0123456789"))
	b.Truncate(4)
	if got := b.Bytes(); !bytes.Equal(got, []byte("0123")) {
		t.Fatalf("shrink got %q", got)
	}
	b.Truncate(6)
	if got := b.Bytes(); !bytes.Equal(got, []byte{'0', '1', '2', '3', 0, 0}) {
		t.Fatalf("grow got %v", got)
	}
}

func TestBufferDirtyTracksWrites(t *testing.T) {
	b := bufferOf([]byte("existing"))
	if b.Dirty() {
		t.Fatal("a freshly loaded buffer should be clean")
	}
	if _, err := b.WriteAt([]byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if !b.Dirty() {
		t.Fatal("a written buffer should be dirty")
	}
}

func TestBytesIsACopy(t *testing.T) {
	b := bufferOf([]byte("abc"))
	got := b.Bytes()
	got[0] = 'Z'
	// Mutating the returned slice must not reach back into the buffer, or a
	// reader could corrupt the pending upload.
	if !bytes.Equal(b.Bytes(), []byte("abc")) {
		t.Fatal("Bytes() aliased the buffer")
	}
}

func TestToErrnoMapsHostCodes(t *testing.T) {
	cases := map[string]syscall.Errno{
		"ENOENT":     syscall.ENOENT,
		"EDQUOT":     syscall.EDQUOT,
		"EROFS":      syscall.EROFS,
		"EOPNOTSUPP": syscall.EOPNOTSUPP,
		"EINVAL":     syscall.EINVAL,
	}
	for code, want := range cases {
		err := &storageclient.Error{Errno: code, Msg: "x"}
		if got := toErrno(err); got != want {
			t.Errorf("%s -> %v, want %v", code, got, want)
		}
	}
}

func TestToErrnoNilIsSuccess(t *testing.T) {
	if got := toErrno(nil); got != 0 {
		t.Fatalf("nil -> %v, want 0", got)
	}
}

func TestToErrnoUnknownIsEIO(t *testing.T) {
	// An error with no errno tag, or one that never reached the host (a raw dial
	// failure), is EIO -- the answer a program already handles from a real disk.
	if got := toErrno(errors.New("connection refused")); got != syscall.EIO {
		t.Fatalf("untagged -> %v, want EIO", got)
	}
	if got := toErrno(&storageclient.Error{Msg: "no errno here"}); got != syscall.EIO {
		t.Fatalf("empty errno -> %v, want EIO", got)
	}
}
