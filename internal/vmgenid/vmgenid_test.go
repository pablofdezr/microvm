package vmgenid

import (
	"strings"
	"testing"
)

func TestMintIsUniqueAndBound(t *testing.T) {
	digest := DigestOf([]byte("snapshot-A"))
	a, err := Mint(digest)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Mint(digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Value) != TokenBytes {
		t.Fatalf("token is %d bytes, want %d", len(a.Value), TokenBytes)
	}
	// The whole point: two mints must never collide, or two restores share state.
	if a.Hex() == b.Hex() {
		t.Fatal("two mints produced the same token")
	}
	if a.Digest != digest {
		t.Errorf("token not bound to its digest: %q != %q", a.Digest, digest)
	}
}

func TestMintRejectsEmptyDigest(t *testing.T) {
	if _, err := Mint(""); err == nil {
		t.Fatal("Mint should reject an empty digest")
	}
}

func TestWireRoundTrip(t *testing.T) {
	digest := DigestOf([]byte("snap"))
	tok, err := Mint(digest)
	if err != nil {
		t.Fatal(err)
	}
	gotDigest, gotVal, err := ParseWire(tok.Wire())
	if err != nil {
		t.Fatalf("ParseWire: %v", err)
	}
	if gotDigest != digest {
		t.Errorf("digest = %q, want %q", gotDigest, digest)
	}
	if string(gotVal) != string(tok.Value) {
		t.Error("round-tripped token value differs")
	}
}

func TestParseWireRejectsMalformed(t *testing.T) {
	cases := []string{
		"",                  // empty
		"nodigestseparator", // no colon
		":deadbeef",         // empty digest
		"abc:nothex",        // bad hex
		"abc:dead",          // wrong length (2 bytes, want 16)
		"abc:" + strings.Repeat("00", TokenBytes+1), // too long
	}
	for _, c := range cases {
		if _, _, err := ParseWire(c); err == nil {
			t.Errorf("ParseWire(%q) should have failed", c)
		}
	}
}

func TestDigestIsStable(t *testing.T) {
	if DigestOf([]byte("x")) != DigestOf([]byte("x")) {
		t.Fatal("digest of the same bytes must be stable")
	}
	if DigestOf([]byte("x")) == DigestOf([]byte("y")) {
		t.Fatal("digest of different bytes must differ")
	}
}
