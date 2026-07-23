//go:build !linux

package agent

import (
	"errors"
	"log/slog"
)

// InitGuest is a stub so the package builds during development on non-Linux
// hosts. The agent is never PID 1 there, so it is never called.
func InitGuest(log *slog.Logger) error {
	return errors.New("guest init is only supported on linux")
}

// RunInit is a stub for non-Linux builds. See the Linux implementation.
func RunInit(log *slog.Logger) error {
	return errors.New("init mode is only supported on linux")
}

// IsSupervisor is always true off Linux: without an init role to take on, the
// agent only ever serves.
func IsSupervisor() bool { return true }
