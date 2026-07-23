package verity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sample is a valid sidecar geometry: a 128 MiB rootfs (32768 blocks of 4096),
// pinned salt of 64 hex zeros, and a plausible root hash.
func sample() Params {
	return Params{
		RootHash:       "9d4e7f2a1b3c5d6e7f8a9b0c1d2e3f405162738495a6b7c8d9e0f1a2b3c4d5e6",
		Salt:           strings.Repeat("0", 64),
		DataBlocks:     32768,
		DataBlockSize:  4096,
		HashBlockSize:  4096,
		HashStartBlock: 0,
		Algorithm:      "sha256",
	}
}

func TestBootParamExactString(t *testing.T) {
	p := sample()
	got := p.BootParam()

	// 32768 data blocks * 4096 bytes / 512 = 262144 sectors.
	want := `dm-mod.create="vroot,,,ro,0 262144 verity 1 /dev/vda /dev/vdb 4096 4096 32768 0 sha256 ` +
		"9d4e7f2a1b3c5d6e7f8a9b0c1d2e3f405162738495a6b7c8d9e0f1a2b3c4d5e6 " +
		strings.Repeat("0", 64) + `"`

	if got != want {
		t.Errorf("BootParam mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestBootParamIsSingleQuotedParameter(t *testing.T) {
	// The whole device-mapper table has spaces in it; the value must be wrapped
	// in exactly one pair of double quotes so the kernel keeps it as one param.
	got := sample().BootParam()
	if !strings.HasPrefix(got, `dm-mod.create="`) || !strings.HasSuffix(got, `"`) {
		t.Fatalf("value is not a single quoted parameter: %s", got)
	}
	if strings.Count(got, `"`) != 2 {
		t.Errorf("expected exactly two quotes, got %d: %s", strings.Count(got, `"`), got)
	}
}

func TestLoadNoSidecarIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "python-arm64.ext4")
	if err := os.WriteFile(rootfs, []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := Load(rootfs)
	if err != nil {
		t.Fatalf("Load with no sidecar should not error: %v", err)
	}
	if p != nil {
		t.Fatalf("Load with no sidecar should return nil, got %+v", p)
	}
}

func TestLoadHappyPath(t *testing.T) {
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "python-arm64.ext4")
	writeSidecar(t, rootfs, `{
		"root_hash": "abc123",
		"salt": "`+strings.Repeat("0", 64)+`",
		"data_blocks": 32768,
		"data_block_size": 4096,
		"hash_block_size": 4096,
		"hash_start_block": 0,
		"hash_algorithm": "sha256"
	}`)

	p, err := Load(rootfs)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p == nil {
		t.Fatal("expected params, got nil")
	}
	if p.RootHash != "abc123" || p.DataBlocks != 32768 {
		t.Errorf("unexpected params: %+v", p)
	}
	if p.HashPath != rootfs+".hash" {
		t.Errorf("HashPath = %q, want %q", p.HashPath, rootfs+".hash")
	}
}

func TestLoadSidecarWithoutHashDeviceFails(t *testing.T) {
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "python-arm64.ext4")
	if err := os.WriteFile(rootfs, []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Sidecar but no .hash device next to it.
	if err := os.WriteFile(rootfs+".verity", []byte(`{"root_hash":"ab","salt":"cd","data_blocks":1,"data_block_size":4096,"hash_block_size":4096,"hash_algorithm":"sha256"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(rootfs); err == nil {
		t.Fatal("expected an error when the hash device is missing")
	}
}

func TestLoadRejectsUnsafeValues(t *testing.T) {
	cases := map[string]string{
		"non-hex root hash":      `{"root_hash":"zz zz","salt":"00","data_blocks":1,"data_block_size":4096,"hash_block_size":4096,"hash_algorithm":"sha256"}`,
		"root hash with a quote": `{"root_hash":"ab\"cd","salt":"00","data_blocks":1,"data_block_size":4096,"hash_block_size":4096,"hash_algorithm":"sha256"}`,
		"non-hex salt":           `{"root_hash":"abcd","salt":"nope","data_blocks":1,"data_block_size":4096,"hash_block_size":4096,"hash_algorithm":"sha256"}`,
		"empty root hash":        `{"root_hash":"","salt":"00","data_blocks":1,"data_block_size":4096,"hash_block_size":4096,"hash_algorithm":"sha256"}`,
		"unsupported algorithm":  `{"root_hash":"abcd","salt":"00","data_blocks":1,"data_block_size":4096,"hash_block_size":4096,"hash_algorithm":"md5"}`,
		"zero data blocks":       `{"root_hash":"abcd","salt":"00","data_blocks":0,"data_block_size":4096,"hash_block_size":4096,"hash_algorithm":"sha256"}`,
		"odd data block size":    `{"root_hash":"abcd","salt":"00","data_blocks":1,"data_block_size":4095,"hash_block_size":4096,"hash_algorithm":"sha256"}`,
		"negative hash start":    `{"root_hash":"abcd","salt":"00","data_blocks":1,"data_block_size":4096,"hash_block_size":4096,"hash_start_block":-1,"hash_algorithm":"sha256"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			rootfs := filepath.Join(dir, "img.ext4")
			writeSidecar(t, rootfs, body)
			if _, err := Load(rootfs); err == nil {
				t.Errorf("expected %s to be rejected, but Load succeeded", name)
			}
		})
	}
}

// writeSidecar writes both the rootfs, its hash device and its verity sidecar,
// so Load gets past the existence checks and reaches validation.
func writeSidecar(t *testing.T, rootfs, meta string) {
	t.Helper()
	if err := os.WriteFile(rootfs, []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs+".hash", []byte("hashtree"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs+".verity", []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}
