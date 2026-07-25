//go:build !linux

package agent

import "errors"

// Rotating the kernel's CSPRNG is an ioctl on a Linux character device. The agent
// only ever runs inside a Linux microVM; this exists so the package builds on a
// developer's machine.
//
// It refuses rather than pretending, so the fail-closed path is what a non-Linux
// build gets: a host restoring against an agent that cannot rotate its CSPRNG
// must be told so, not reassured.
func reseedCSPRNG([]byte) error {
	return errors.New("this agent was not built for Linux, so it cannot rotate the guest CSPRNG")
}
