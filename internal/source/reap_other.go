//go:build !linux

package source

import "os/exec"

// reapWithProcess does nothing off Linux. Pdeathsig and process groups are what
// the Linux build uses to keep a clone from outliving the daemon; the daemon only
// runs on Linux, and the tests in this package run everywhere, so this is here to
// keep them building rather than to promise anything.
func reapWithProcess(*exec.Cmd) {}
