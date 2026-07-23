// Package verity turns a rootfs image's dm-verity sidecar into the kernel
// command-line parameters that make Firecracker boot it as a verified device.
//
// An image can ship two siblings next to its `.ext4`, both produced by
// images/packer/pack.sh with veritysetup:
//
//   - <image>.ext4.hash    the Merkle hash tree over the rootfs
//   - <image>.ext4.verity  a small JSON sidecar: the root hash and the geometry
//
// When both are present, the daemon boots the VM with dm-verity: the kernel
// assembles a device-mapper verity target from the read-only rootfs (/dev/vda)
// and its hash tree (/dev/vdb) *before* it mounts root, and refuses to boot --
// panicking before init ever runs -- if a single block of the image has been
// altered. The rootfs is shared and host-supplied, and the code inside a
// sandbox is assumed hostile, so tampering with the base image fails closed at
// boot rather than silently serving a modified userland.
//
// This is opt-in and backward compatible: an image with no `.verity` sidecar
// boots exactly as before, from /dev/vda directly.
//
// The guest kernel must be built with CONFIG_DM_VERITY and CONFIG_DM_INIT (the
// latter is what parses the `dm-mod.create=` parameter this package emits).
package verity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Guest-visible device names. The rootfs is the first Firecracker drive
// (/dev/vda) and the hash tree is the second (/dev/vdb); dm-init assembles the
// verity mapping as /dev/dm-0, which becomes the real, verified root.
const (
	DataDevice = "/dev/vda"
	HashDevice = "/dev/vdb"
	RootDevice = "/dev/dm-0"
	mapperName = "vroot"

	// HashName is the filename the hash device is staged under inside the jail.
	HashName = "rootfs.hash"

	metaSuffix = ".verity"
	hashSuffix = ".hash"
)

// Params is the geometry of a rootfs's verity hash tree, read from the sidecar.
// The values are all produced by veritysetup at build time; the daemon only
// formats them onto the command line, and validates them first because anything
// here lands on the kernel command line.
type Params struct {
	RootHash       string `json:"root_hash"`
	Salt           string `json:"salt"`
	DataBlocks     int64  `json:"data_blocks"`
	DataBlockSize  int64  `json:"data_block_size"`
	HashBlockSize  int64  `json:"hash_block_size"`
	HashStartBlock int64  `json:"hash_start_block"`
	Algorithm      string `json:"hash_algorithm"`

	// HashPath is the host path to the `.hash` device, filled in by Load. It is
	// not part of the on-disk sidecar.
	HashPath string `json:"-"`
}

// Load reads the verity sidecar sitting next to rootfsPath and returns the
// parameters, or (nil, nil) when there is no sidecar -- the signal to boot the
// image normally. It errors only on a real misconfiguration: a sidecar that
// does not parse, one whose hash device is missing, or geometry that could not
// be placed safely on a command line.
func Load(rootfsPath string) (*Params, error) {
	metaPath := rootfsPath + metaSuffix

	raw, err := os.ReadFile(metaPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // no verity for this image
	}
	if err != nil {
		return nil, fmt.Errorf("read verity sidecar %s: %w", metaPath, err)
	}

	var p Params
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse verity sidecar %s: %w", metaPath, err)
	}

	p.HashPath = rootfsPath + hashSuffix
	if _, err := os.Stat(p.HashPath); err != nil {
		return nil, fmt.Errorf("verity sidecar %s present but hash device missing: %w", metaPath, err)
	}

	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("verity sidecar %s: %w", metaPath, err)
	}
	return &p, nil
}

// validate rejects anything that is not safe to interpolate into a kernel
// command line. The root hash and salt are the only free-form fields, so they
// are held to strict hex -- which by construction contains no space, comma or
// quote and so cannot break out of the dm-mod.create value.
func (p *Params) validate() error {
	if !isHex(p.RootHash) {
		return fmt.Errorf("root_hash must be non-empty hex, got %q", p.RootHash)
	}
	if !isHex(p.Salt) {
		return fmt.Errorf("salt must be non-empty hex, got %q", p.Salt)
	}
	if p.Algorithm != "sha256" {
		return fmt.Errorf("unsupported hash algorithm %q (only sha256)", p.Algorithm)
	}
	if p.DataBlocks <= 0 || p.DataBlockSize <= 0 || p.HashBlockSize <= 0 {
		return fmt.Errorf("data_blocks, data_block_size and hash_block_size must be positive")
	}
	if p.HashStartBlock < 0 {
		return fmt.Errorf("hash_start_block must not be negative")
	}
	if p.DataBlockSize%512 != 0 {
		return fmt.Errorf("data_block_size %d must be a multiple of 512", p.DataBlockSize)
	}
	return nil
}

// BootParam returns the `dm-mod.create=` kernel parameter that makes dm-init
// assemble the verity device at boot. The value is double-quoted because the
// device-mapper table it carries is space-separated -- the kernel command-line
// parser keeps a quoted value as a single parameter.
//
// The table follows the dm-init format:
//
//	<name>,<uuid>,<minor>,<flags>,<start> <sectors> verity <ver> \
//	  <data_dev> <hash_dev> <data_bs> <hash_bs> <#data_blocks> \
//	  <hash_start> <algorithm> <root_hash> <salt>
func (p Params) BootParam() string {
	sectors := p.DataBlocks * p.DataBlockSize / 512
	table := fmt.Sprintf("0 %d verity 1 %s %s %d %d %d %d %s %s %s",
		sectors,
		DataDevice, HashDevice,
		p.DataBlockSize, p.HashBlockSize,
		p.DataBlocks, p.HashStartBlock,
		p.Algorithm, p.RootHash, p.Salt,
	)
	// name,uuid(empty),minor(empty),flags=ro, then the table line.
	value := fmt.Sprintf("%s,,,ro,%s", mapperName, table)
	return fmt.Sprintf(`dm-mod.create="%s"`, value)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
