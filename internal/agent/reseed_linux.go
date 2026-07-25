//go:build linux

package agent

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// randomDevice is the character device the reseed goes through. /dev/urandom and
// /dev/random share one write path and one ioctl handler in the kernel, so either
// works; /dev/urandom is the one guaranteed to exist in a minimal image.
const randomDevice = "/dev/urandom"

// reseedCSPRNG makes this guest's random numbers diverge from every other restore
// of the same snapshot.
//
// Two steps, and the second is the one that matters:
//
//  1. Write the token. This is mix_pool_bytes() into the kernel's input pool. It
//     puts unique bytes somewhere the CSPRNG will draw from, and on its own it
//     changes not one byte of what getrandom(2) returns.
//
//  2. ioctl(RNDRESEEDCRNG). This is crng_reseed(): extract_entropy() from the
//     pool we just wrote into, install the result as base_crng.key, and bump
//     base_crng.generation so every per-CPU fast key is re-derived from it on
//     next use. After this the guest's CSPRNG output is a function of our token,
//     which no other restore has.
//
// Step 2 needs CAP_SYS_ADMIN, which the agent has: it is PID 1 and root inside a
// VM whose isolation boundary is the VM. It also needs a CRNG that is already
// initialised, which a restored guest's is -- the template's was. If either does
// not hold, this returns the errno, the host fails the restore, and the warm pool
// falls back to cold boots. That is the correct trade: a slower pool beats a pool
// of VMs that share their keys.
func reseedCSPRNG(token []byte) error {
	if len(token) != protocol.ReseedTokenBytes {
		return fmt.Errorf("reseed token is %d bytes, want %d", len(token), protocol.ReseedTokenBytes)
	}
	return reseedCSPRNGVia(randomDevice, token)
}

// reseedCSPRNGVia is reseedCSPRNG against a named device, so a test can point it
// at something that is not the running kernel's entropy source.
func reseedCSPRNGVia(device string, token []byte) error {
	// O_WRONLY: this only ever writes and ioctls. No mode -- the device node
	// already exists, and a mode asked for is a mode the kernel would apply to
	// the guest's entropy source on our behalf.
	f, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s to reseed: %w", device, err)
	}
	defer f.Close()

	if _, err := f.Write(token); err != nil {
		return fmt.Errorf("mix the reseed token into %s: %w", device, err)
	}

	// The rotation. RNDRESEEDCRNG takes no argument; the zero is the ioctl's
	// unused third parameter.
	if err := unix.IoctlSetInt(int(f.Fd()), unix.RNDRESEEDCRNG, 0); err != nil {
		return fmt.Errorf("ioctl RNDRESEEDCRNG on %s: %w "+
			"(the token was mixed into the pool, but the CSPRNG key was NOT re-derived, "+
			"so this guest's random numbers are still every other restore's)", device, err)
	}
	return nil
}
