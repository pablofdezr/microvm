//go:build !linux

package agent

import (
	"errors"
	"log/slog"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// This file lets the agent package build off Linux, where the guest never runs
// but the tests do. Network configuration is a Linux facility reached through
// netlink, so there is nothing to do here. See net_linux.go for the real one.

var errNetworkUnsupported = errors.New("network reconfiguration is not supported on this platform")

func reconfigureNetwork(_ *slog.Logger, _ protocol.NetConfigRequest) error {
	return errNetworkUnsupported
}
