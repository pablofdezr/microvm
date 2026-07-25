//go:build !linux

package agent

import (
	"context"
	"log/slog"

	"github.com/pablofdezr/microvm/internal/protocol"
)

// The snapshot carry needs /dev/mem, per-thread CPU affinity and a guest device
// tree, none of which exist off Linux. The agent only ever runs inside a Linux
// microVM; this exists so the package still builds on a developer's machine.

type gicCarrier struct{}

func newGICCarrier(*slog.Logger) *gicCarrier { return &gicCarrier{} }

func (c *gicCarrier) arm(context.Context) (protocol.SnapshotArmResponse, error) {
	return protocol.SnapshotArmResponse{
		Armed:  false,
		Detail: "this agent was not built for Linux, so it holds no interrupt-controller state",
	}, nil
}

func (c *gicCarrier) disarm(context.Context) (protocol.SnapshotDisarmResponse, error) {
	return protocol.SnapshotDisarmResponse{Armed: false}, nil
}
