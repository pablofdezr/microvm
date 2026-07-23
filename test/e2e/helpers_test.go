//go:build linux

package e2e

import (
	"bytes"
	"os/exec"
)

// runHost runs a command on the host under test and returns its combined output.
// Used to check host-side facts a guest cannot see: which uid owns the VMM,
// whether a TAP device still exists.
func runHost(name string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}
