//go:build !linux

package agent

import (
	"errors"
	"net"
)

// ListenVsock is unavailable off Linux. The agent only ever runs inside a
// guest, but it is built on other platforms during development, so this keeps
// the package compiling rather than gating every caller behind a build tag.
func ListenVsock() (net.Listener, error) {
	return nil, errors.New("vsock is only available on linux; use -listen tcp for local development")
}
