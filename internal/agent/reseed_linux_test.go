//go:build linux

package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// The regression, stated as a test: writing the token somewhere is not reseeding.
//
// A plain file accepts the write and rejects the ioctl with ENOTTY, so it stands
// in for exactly the failure the old implementation could not distinguish from
// success -- bytes delivered, CSPRNG key untouched. If this ever returns nil, the
// rotation has been dropped and every restore of one template shares its keys
// again.
func TestReseedIsNotJustAWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-random-device")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	token := bytes.Repeat([]byte{0x5a}, protocol.ReseedTokenBytes)
	err := reseedCSPRNGVia(path, token)
	if err == nil {
		t.Fatal("reseeding a plain file reported success: the CSPRNG rotation is not being performed")
	}

	// The bytes did land -- which is the point: delivery is not the property being
	// asserted, and asserting it here is what makes the failure above meaningful.
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, token) {
		t.Fatalf("wrote %x, want %x -- the test is not exercising what it thinks", got, token)
	}
}

// A token of the wrong length never reaches a device: the guard is in the
// platform implementation too, not only in the HTTP handler, because the handler
// is not the only caller a future change could add.
func TestReseedRejectsAWrongSizedTokenBeforeOpening(t *testing.T) {
	if err := reseedCSPRNG(bytes.Repeat([]byte{1}, protocol.ReseedTokenBytes-1)); err == nil {
		t.Fatal("a short token was accepted")
	}
}
