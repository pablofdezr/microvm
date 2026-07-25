//go:build linux

package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The catalog the API serves trims ".ext4" off the files in the image
// directory, so a caller asks for "python" while the file is "python.ext4".
// Resolution has to put the suffix back, or every create on a real deployment
// fails with a rootfs that does not exist.
func TestRootfsPath(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"python.ext4", "bare"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runtime{cfg: Config{ImageDir: dir}}

	for _, tc := range []struct{ image, want string }{
		{"python", "python.ext4"},      // the name the API advertises
		{"python.ext4", "python.ext4"}, // the file, as the warm-pool flag names it
		{"bare", "bare"},               // a file with no suffix at all
	} {
		got, err := r.rootfsPath(tc.image)
		if err != nil {
			t.Errorf("rootfsPath(%q): %v", tc.image, err)
			continue
		}
		if got != filepath.Join(dir, tc.want) {
			t.Errorf("rootfsPath(%q) = %q, want %q", tc.image, got, filepath.Join(dir, tc.want))
		}
	}

	if _, err := r.rootfsPath("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing image: got %v, want ErrNotExist", err)
	}
}

// The image name arrives from an API caller. Left as a path it would stage an
// arbitrary host file into the jail as a rootfs, because filepath.Join cleans
// the traversal away rather than rejecting it.
func TestRootfsPathRejectsPaths(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "escaped.ext4")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	r := &Runtime{cfg: Config{ImageDir: dir}}
	for _, image := range []string{
		"",
		".",
		"..",
		"../escaped",
		"../escaped.ext4",
		"sub/python",
		"/etc/shadow",
		".hidden",
	} {
		if got, err := r.rootfsPath(image); err == nil {
			t.Errorf("rootfsPath(%q) = %q, want an error", image, got)
		}
	}
}
