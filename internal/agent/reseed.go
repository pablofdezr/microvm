package agent

import (
	"fmt"
	"io"
	"net/http"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// This file is the guest half of the restore reseed: the route the host calls on
// a VM it has just restored, before anything else can reach it.
//
// # Why a route and not a file write
//
// The host used to reseed by uploading 16 bytes to /dev/urandom through the
// ordinary file route. That write is not a reseed. On Linux 5.18 and later --
// which is every guest kernel this runs -- writing to /dev/urandom reaches
// write_pool_user() -> mix_pool_bytes() and stops there: it credits no entropy,
// and it does not re-derive base_crng.key. That key is what getrandom(2),
// /dev/urandom reads, and every in-kernel consumer actually produce bytes from,
// and it is rotated on a jiffies deadline (base_crng.birth + 60s) or when the
// init-bit count crosses a threshold -- never by a write.
//
// A snapshot restores base_crng.{key,generation,birth} *and* the guest's jiffies
// identically into every restore, so the 60-second deadline is the same deadline
// in every restore too. Two VMs restored from one template therefore produced
// byte-identical getrandom(2) output -- identical session tokens, identical TLS
// private keys, identical nonces -- for the same first minute of guest execution,
// which is longer than most sandboxes live. The write succeeded, so the host
// reported it as a successful reseed. This is exactly the break internal/vmgenid
// exists to close, and it was open.
//
// So the reseed is a guest-side operation with a guest-side result: mix the
// host's token into the input pool, then force the CSPRNG to re-derive its key
// from that pool. The forcing is an ioctl the agent can make -- it is PID 1 and
// root inside the VM, so CAP_SYS_ADMIN is not a barrier -- and it either happens
// or it reports that it did not.

// handleSnapshotReseed stirs the host's restore token into the guest's CSPRNG.
//
// It answers 500 on any failure, and the host fails the restore on it. That is
// deliberate and it is the whole contract: a restored VM whose CSPRNG was not
// rotated is a VM that shares its keys with every other restore of the same
// snapshot, and a working sandbox is not worth that.
func (a *Agent) handleSnapshotReseed(w http.ResponseWriter, r *http.Request) {
	// One byte more than the token, so an oversized body is rejected rather than
	// silently truncated to something that looks the right size.
	token, err := io.ReadAll(io.LimitReader(r.Body, protocol.ReseedTokenBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the reseed token: %w", err))
		return
	}
	if len(token) != protocol.ReseedTokenBytes {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("reseed token is %d bytes, want exactly %d", len(token), protocol.ReseedTokenBytes))
		return
	}

	if err := a.reseed(token); err != nil {
		// Logged as well as returned: this is the one guest-side failure that
		// makes a sandbox unsafe rather than merely broken, and the host's own
		// error message is the only other place it appears.
		a.log.Error("could not rotate the guest CSPRNG after a restore", "err", err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
