// Package id mints the public identifiers.
//
// An ID is a prefix and a ULID: "sb_01JZ8QK3M4N5P6R7S8T9V0W1X2".
//
// # Why the prefix
//
// It makes an ID self-describing, which is worth more than it sounds. A caller
// who pastes an execution ID into a sandbox endpoint gets told exactly that,
// before the lookup, rather than an unhelpful 404 about an object that exists
// under a different name. Logs and error reports become readable without a
// schema to hand. It is Stripe's convention and it earns its keep every time
// somebody debugs from a screenshot.
//
// # Why a ULID rather than random bytes
//
// A ULID is 48 bits of millisecond timestamp followed by 80 random bits,
// encoded in Crockford base32 -- an alphabet chosen so that the encoding
// preserves order: sorting IDs as strings sorts them by creation time.
//
// That is what makes cursor pagination honest. With unordered IDs, a cursor
// means "find the object with this ID, then return what follows it" -- so the
// cursor stops working the moment its object is deleted, and paging a list that
// is being written to can skip or repeat rows. An ordered ID *is* the position,
// so `starting_after` needs nothing to exist and cannot drift.
//
// The 80 random bits are what keep it unguessable. Two IDs minted in the same
// millisecond collide with probability around 2^-80; the timestamp is not a
// secret, and nothing here depends on an ID being unpredictable, but leaking a
// sequence ("we have run 412 sandboxes") would be worse than not.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Prefixes. These are contract: an ID appears in a caller's database, so
// renaming one invalidates data we do not own.
const (
	SandboxPrefix   = "sb"
	ExecutionPrefix = "exe"
	TaskPrefix      = "tsk"
)

// crockford is base32 without I, L, O and U.
//
// The exclusions are the point: those four are what a human misreads as 1, 1, 0
// and V when copying an ID out of a terminal. An alphabet that cannot be
// misread is a support ticket that never gets filed.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// encodedLen is how many base32 characters 128 bits need: ceil(128/5).
const encodedLen = 26

// decodeMap inverts crockford. -1 marks a character that is not in the alphabet.
var decodeMap = func() [256]int8 {
	var m [256]int8
	for i := range m {
		m[i] = -1
	}
	for i, c := range crockford {
		m[c] = int8(i)
		// Accept lower case on the way in. Nothing mints it, but a caller who
		// lowercased an ID somewhere in their stack should get their object,
		// not a mystery 404.
		m[c|0x20] = int8(i)
	}
	return m
}()

// New mints an ID with the given prefix.
//
// It panics if the system's entropy source fails. That is deliberate: every
// alternative is worse. Returning an error would push the decision onto callers
// who cannot do anything useful with it, and falling back to a weaker source
// would hand out predictable IDs while looking like it worked.
func New(prefix string) string {
	var raw [16]byte

	// 48 bits of milliseconds, big-endian, so byte order matches time order.
	ms := uint64(time.Now().UnixMilli())
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	binary.BigEndian.PutUint32(raw[2:6], uint32(ms))

	if _, err := rand.Read(raw[6:]); err != nil {
		panic(fmt.Sprintf("id: the system entropy source failed: %v", err))
	}

	return prefix + "_" + encode(raw)
}

// encode renders 128 bits as 26 Crockford base32 characters.
//
// It reads the 16 bytes as one big-endian integer and peels off five bits at a
// time from the top. 26 characters carry 130 bits, so the first character only
// has 3 bits of the value and the top 2 bits are always zero -- which is why
// every ID starts with 0 through 7.
func encode(raw [16]byte) string {
	var out [encodedLen]byte
	for i := encodedLen - 1; i >= 0; i-- {
		out[i] = crockford[shiftRight5(&raw)]
	}
	return string(out[:])
}

// shiftRight5 divides the 128-bit big-endian value in raw by 32, returning the
// remainder. Called 26 times it walks the whole value out, lowest group first.
func shiftRight5(raw *[16]byte) byte {
	var carry byte
	for i := 0; i < 16; i++ {
		next := raw[i] & 0x1f // the 5 bits that fall off this byte
		raw[i] = raw[i]>>5 | carry<<3
		carry = next
	}
	return carry
}

var (
	// ErrMalformed means the string is not an ID at all.
	ErrMalformed = errors.New("malformed id")
	// ErrWrongType means it is a valid ID of the wrong kind -- an execution ID
	// where a sandbox ID belongs.
	ErrWrongType = errors.New("id is of the wrong type")
)

// Parse checks that s is a well-formed ID with the expected prefix.
//
// The two failure modes are separate errors because they deserve separate
// answers: gibberish is a client bug, whereas a real ID in the wrong slot is
// usually a caller who mixed up two variables, and telling them which is which
// saves an hour.
func Parse(s, wantPrefix string) error {
	prefix, body, ok := strings.Cut(s, "_")
	if !ok {
		return fmt.Errorf("%w: %q has no type prefix, expected one like %s_", ErrMalformed, s, wantPrefix)
	}
	if len(body) != encodedLen {
		return fmt.Errorf("%w: %q is %d characters after the prefix, expected %d",
			ErrMalformed, s, len(body), encodedLen)
	}
	for i := 0; i < len(body); i++ {
		if decodeMap[body[i]] < 0 {
			return fmt.Errorf("%w: %q contains %q, which is not in the alphabet",
				ErrMalformed, s, body[i])
		}
	}
	if prefix != wantPrefix {
		return fmt.Errorf("%w: %q is a %q id, expected a %q id", ErrWrongType, s, prefix, wantPrefix)
	}
	return nil
}

// Valid reports whether s is a well-formed ID with the given prefix.
func Valid(s, wantPrefix string) bool { return Parse(s, wantPrefix) == nil }

// Time returns when an ID was minted.
//
// This is a convenience for debugging and for sorting, not a security boundary:
// the timestamp is plainly visible in the ID and a client could forge one. Never
// decide anything on it that matters.
func Time(s string) (time.Time, error) {
	_, body, ok := strings.Cut(s, "_")
	if !ok || len(body) != encodedLen {
		return time.Time{}, fmt.Errorf("%w: %q", ErrMalformed, s)
	}

	// The first 10 characters carry 50 bits: the 2 padding bits, which are
	// always zero, followed by all 48 bits of the timestamp. So those 50 bits
	// already are the timestamp -- there is nothing to shift off. (Shifting
	// right by the 2 padding bits is the obvious-looking move and it quietly
	// divides the answer by four.)
	var ms uint64
	for i := 0; i < 10; i++ {
		v := decodeMap[body[i]]
		if v < 0 {
			return time.Time{}, fmt.Errorf("%w: %q", ErrMalformed, s)
		}
		ms = ms<<5 | uint64(v)
	}

	return time.UnixMilli(int64(ms)), nil
}
