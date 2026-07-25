// Package vmgenid mints the token that reseeds a restored VM's entropy.
//
// A microVM snapshot is a copy of RAM, so every VM restored from one snapshot
// starts byte-identical -- including the kernel's CSPRNG state. Left alone, two
// restores produce the SAME "random" numbers: identical TLS keys, session tokens,
// nonces. That is a real cryptographic break, not a theoretical one, and it is the
// reason a warm pool built on snapshots (rather than on distinct booted VMs) must
// reseed before it runs anything.
//
// This package is only the mint: 128 bits from crypto/rand per restore, mirroring
// the size of the hardware VMGenID the mechanism is named after. What makes it a
// reseed is what the guest does with it, and the distinction is worth stating here
// because getting it wrong looks exactly like getting it right:
//
// Writing the token into the guest's /dev/urandom is NOT a reseed. On Linux 5.18
// and later that write reaches mix_pool_bytes() and stops -- it credits no entropy
// and it does not re-derive base_crng.key, which is the key getrandom(2) actually
// answers from. A snapshot restores that key, its generation counter, its 60-second
// jiffies deadline *and* the jiffies, identically into every restore, so restores of
// one template shared their random numbers for the same first minute of guest
// execution while the write reported success. The guest agent's reseed route
// therefore mixes the token in and then forces the re-derivation
// (ioctl RNDRESEEDCRNG), and fails if the forcing fails; see
// internal/agent/reseed.go. A restore whose reseed fails is destroyed.
//
// # On the digest
//
// Token carries the snapshot identity it was minted against, and Wire/ParseWire
// can carry that identity to a guest that checks it. Nothing does today: the
// restore path sends Token.Value alone, because the token travels host-to-guest
// over that guest's private vsock socket and there is no untrusted party in the
// path to replay anything. The binding is there for a channel that has one -- a
// token handed over ACPI or a kernel cmdline, say -- and until then it is capacity,
// not a control. Uniqueness does not depend on it: every Mint draws fresh bytes.
package vmgenid

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// TokenBytes is the reseed token length, matching the 128-bit ACPI VMGenID. The
// guest agent requires exactly this many bytes (protocol.ReseedTokenBytes).
const TokenBytes = 16

// Token is a one-time reseed token for a single restore.
type Token struct {
	// Value is the random bytes the guest stirs into its entropy pool.
	Value []byte
	// Digest binds the token to the snapshot it was minted for, so the guest can
	// refuse one minted for a different snapshot.
	Digest string
}

// Mint returns a fresh token bound to a snapshot digest. Every call is unique.
func Mint(snapshotDigest string) (Token, error) {
	if snapshotDigest == "" {
		return Token{}, fmt.Errorf("vmgenid: empty snapshot digest")
	}
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return Token{}, fmt.Errorf("vmgenid: read entropy: %w", err)
	}
	return Token{Value: b, Digest: snapshotDigest}, nil
}

// Hex encodes the token's bytes for transport on a text channel (vsock line, a
// cmdline value, a small control message).
func (t Token) Hex() string { return hex.EncodeToString(t.Value) }

// Wire is the on-the-wire form the host sends to the guest: the digest it is
// bound to and the hex value, joined by a colon. The guest checks the digest
// against the snapshot it restored from before applying the value.
func (t Token) Wire() string { return t.Digest + ":" + t.Hex() }

// ParseWire parses the value ParseWire's counterpart produced, returning the
// bound digest and the raw token bytes. It rejects anything malformed so a
// corrupt control message can never be mistaken for a valid reseed.
func ParseWire(s string) (digest string, value []byte, err error) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			digest = s[:i]
			value, err = hex.DecodeString(s[i+1:])
			if err != nil {
				return "", nil, fmt.Errorf("vmgenid: bad token hex: %w", err)
			}
			if digest == "" {
				return "", nil, fmt.Errorf("vmgenid: empty digest")
			}
			if len(value) != TokenBytes {
				return "", nil, fmt.Errorf("vmgenid: token is %d bytes, want %d", len(value), TokenBytes)
			}
			return digest, value, nil
		}
	}
	return "", nil, fmt.Errorf("vmgenid: malformed token, no digest separator")
}

// DigestOf is the content digest of a snapshot's bytes, used to bind a token to
// it. Callers hash the memory-and-state files; the exact input does not matter
// so long as host (minting) and guest (verifying) agree on it.
func DigestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
