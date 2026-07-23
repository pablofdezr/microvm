//go:build linux

package agent

import (
	"fmt"
	"net"

	"github.com/mdlayher/vsock"
	"github.com/pablofdezr/microvm/internal/protocol"
)

// ListenVsock returns a listener on the guest's AF_VSOCK port. Firecracker
// bridges this to a Unix socket on the host, which is how the daemon reaches
// the agent without the guest needing any network configuration at all.
func ListenVsock() (net.Listener, error) {
	l, err := vsock.Listen(protocol.AgentPort, nil)
	if err != nil {
		return nil, fmt.Errorf("listen vsock port %d: %w", protocol.AgentPort, err)
	}
	return l, nil
}
