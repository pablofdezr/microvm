// Package vmgenid reseeds a restored VM's entropy.
//
// A microVM snapshot is a copy of RAM, so every VM restored from one snapshot
// starts byte-identical -- including the kernel's CSPRNG state. Left alone, two
// restores would produce the SAME "random" numbers: identical TLS keys, session
// tokens, nonces. That is a real cryptographic break, not a theoretical one, and
// it is the reason a warm pool built on snapshots (rather than on distinct booted
// VMs) must reseed before it runs anything.
//
// The fix mirrors the hardware VMGenID mechanism: on every restore the host mints
// a fresh, unique token and hands it to the guest, which stirs it into the kernel
// entropy pool before any workload starts. The token is bound to the snapshot's
// content digest, so a token minted for one snapshot cannot be replayed against a
// different one.
package vmgenid

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// TokenBytes is the reseed token length, matching the 128-bit ACPI VMGenID.
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
