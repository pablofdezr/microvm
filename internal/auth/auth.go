// Package auth maps a bearer token to who is behind it.
//
// The distinction this package exists to draw is between authentication and
// identity. The old model had only the former: a token was valid or it was not,
// and every valid caller was interchangeable. That is enough to keep strangers
// out and not enough to give a caller anything of their own -- in particular a
// place to keep files that persists across their sandboxes and belongs to them
// alone.
//
// So a token now resolves to a Principal, and the Principal carries a tenant.
// The tenant is the load-bearing part: it is what a sandbox's storage is
// namespaced under, and it is derived here, on the host, from the secret the
// caller presented. It is never read from a request body. That is the whole
// security property of per-tenant storage in one sentence -- a caller cannot
// name a namespace, only prove which one is theirs.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"

	"github.com/pablofdezr/microvm/internal/storage"
)

// Principal is the identity behind a token.
type Principal struct {
	// Tenant is the stable namespace the token belongs to. Two tokens may map to
	// one tenant (a team rotating keys); one token is always one tenant. Because
	// a sandbox's files live under it, it must not change across a token's life
	// -- which is why it is derived from the token itself and from nothing that
	// varies per request.
	Tenant string

	// Quota bounds what this principal's sandboxes may store. The zero value
	// takes the storage package's defaults, so a principal that cares about none
	// of this gets sensible limits rather than none.
	Quota storage.Quota

	// ReadOnly forbids this principal's sandboxes from writing at all. A request
	// may tighten this to true but never loosen it to false: a read-only key is
	// read-only whatever the body says.
	ReadOnly bool

	// Admin lets this principal set tenant policies -- storage caps and eviction
	// rules -- for anyone. It is a separate power from creating sandboxes, and a
	// dangerous one: an admin key decides what every tenant may store and whether
	// their old data gets deleted. It is never derived, only granted explicitly,
	// so a flat token list produces no admins at all.
	Admin bool
}

// pair is one token and who it belongs to.
//
// A slice rather than a map, and that is deliberate: resolving a token walks
// every pair with a constant-time compare and no early exit, so the time to
// resolve does not depend on which token matched or on how close a wrong guess
// got. A map lookup would be faster and would leak exactly that.
type pair struct {
	token     string
	principal *Principal
}

// Directory resolves tokens to principals.
type Directory struct {
	pairs []pair
}

// NewDirectory builds a directory from a flat list of tokens.
//
// Each token gets its own tenant, derived from the token, so a caller keeps one
// namespace across every sandbox they ever create and two callers never share
// one. This is the default an operator gets for free by listing tokens; naming
// tenants explicitly, or pointing several tokens at one tenant, is a richer
// configuration that NewDirectoryOf exists for.
func NewDirectory(tokens []string) *Directory {
	d := &Directory{pairs: make([]pair, 0, len(tokens))}
	for _, t := range tokens {
		if t == "" {
			continue
		}
		d.pairs = append(d.pairs, pair{token: t, principal: &Principal{Tenant: DeriveTenant(t)}})
	}
	return d
}

// NewDirectoryOf builds a directory from explicit principals, keyed by token.
//
// For the operator who wants named tenants, shared tenants, per-key quotas, or
// read-only keys -- anything the flat list cannot express. A principal with an
// empty Tenant has one derived from its token, so the common fields can be set
// without restating the namespace.
func NewDirectoryOf(byToken map[string]*Principal) *Directory {
	d := &Directory{pairs: make([]pair, 0, len(byToken))}
	for t, p := range byToken {
		if t == "" || p == nil {
			continue
		}
		if p.Tenant == "" {
			p.Tenant = DeriveTenant(t)
		}
		d.pairs = append(d.pairs, pair{token: t, principal: p})
	}
	return d
}

// Resolve returns the principal a token belongs to, or false.
//
// It compares against every configured token in constant time and never breaks
// early. A version that returned on the first match would leak, through timing,
// both whether a guess was right and how many tokens are configured.
func (d *Directory) Resolve(token string) (*Principal, bool) {
	var match *Principal
	for _, p := range d.pairs {
		if subtle.ConstantTimeCompare([]byte(token), []byte(p.token)) == 1 {
			match = p.principal
		}
	}
	return match, match != nil
}

// tenantPrefix is base32-ish hex, long enough that two distinct tokens sharing
// a tenant -- which would be one caller reading another's files -- is not a
// thing that happens. 128 bits is overkill for a namespace and exactly right
// for one whose collision is a data leak.
const tenantHexLen = 32

// DeriveTenant turns a token into a stable, opaque tenant id.
//
// Stable, because a caller's files must still be theirs tomorrow: the same token
// must always yield the same tenant. Opaque, because the tenant ends up in an
// object-store prefix and there is no reason for it to reveal the token that
// made it -- it is a one-way hash, so a leaked prefix is not a leaked key.
func DeriveTenant(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "t_" + hex.EncodeToString(sum[:])[:tenantHexLen]
}
