// Package protocol defines the wire format spoken between the host daemon and
// the in-guest agent. It is imported by both sides, so it must stay free of
// platform-specific dependencies.
package protocol

import "time"

// AgentPort is the AF_VSOCK port the guest agent listens on.
const AgentPort uint32 = 5000

// StoragePort is the port the *host* listens on, for the guest to call out to.
//
// It runs the opposite direction to AgentPort and over a different mechanism:
// the guest connects to HostCID on this port, and Firecracker hands that
// connection to a Unix socket the host opened beforehand. See vsock.Listen --
// the asymmetry is not a detail, and assuming symmetry does not fail loudly.
const StoragePort uint32 = 5001

// GuestCID is the context ID assigned to every guest. Firecracker gives each VM
// its own vsock device backed by a distinct host Unix socket, so the CID does
// not need to be unique across VMs.
const GuestCID uint32 = 3

// HostCID is the well-known address of the host, fixed by the AF_VSOCK spec
// (VMADDR_CID_HOST). A guest reaches the host by dialling this and nothing else:
// there is no address for it to get wrong, and no other VM it can name.
const HostCID uint32 = 2

// FrameType discriminates the NDJSON frames streamed back from an exec.
type FrameType string

const (
	// FrameStarted is emitted once, before any output, carrying the guest PID.
	FrameStarted FrameType = "started"
	// FrameStdout carries a chunk of the process's standard output.
	FrameStdout FrameType = "stdout"
	// FrameStderr carries a chunk of the process's standard error.
	FrameStderr FrameType = "stderr"
	// FrameExit is the final frame of a successful stream.
	FrameExit FrameType = "exit"
	// FrameError is the final frame when the agent itself failed.
	FrameError FrameType = "error"
)

// ExecRequest asks the agent to start a process inside the guest.
type ExecRequest struct {
	// ID correlates the exec with later signal and stdin calls. The host
	// generates it; the agent treats it as opaque.
	ID string `json:"id"`

	Cmd  string   `json:"cmd"`
	Args []string `json:"args,omitempty"`

	// Env entries are merged over the guest's base environment.
	Env map[string]string `json:"env,omitempty"`

	// Cwd defaults to the sandbox working directory when empty.
	Cwd string `json:"cwd,omitempty"`

	// Stdin is written to the process and the pipe is closed immediately after.
	// For interactive input use the streaming stdin endpoint instead.
	Stdin []byte `json:"stdin,omitempty"`

	// TTY allocates a pty and merges stderr into stdout.
	TTY bool `json:"tty,omitempty"`

	// Timeout kills the process group when exceeded. Zero means no limit; the
	// host is expected to always set one.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Frame is one NDJSON record in an exec's output stream.
type Frame struct {
	Type FrameType `json:"type"`

	// Data holds output bytes for stdout/stderr frames. encoding/json emits
	// []byte as base64, which keeps the stream valid NDJSON for binary output.
	Data []byte `json:"data,omitempty"`

	// PID is set on FrameStarted.
	PID int `json:"pid,omitempty"`

	// ExitCode is set on FrameExit. It is a pointer so that a zero exit status
	// is distinguishable from an absent field.
	ExitCode *int `json:"exit_code,omitempty"`

	// Signal is set on FrameExit when the process was terminated by a signal.
	Signal string `json:"signal,omitempty"`

	// TimedOut is set on FrameExit when the agent killed the process because
	// ExecRequest.Timeout elapsed.
	TimedOut bool `json:"timed_out,omitempty"`

	// Message carries the failure reason on FrameError.
	Message string `json:"message,omitempty"`
}

// SignalRequest asks the agent to signal a running exec's process group.
type SignalRequest struct {
	// Signal is a name such as "SIGTERM" or "SIGKILL".
	Signal string `json:"signal"`
}

// HealthResponse is returned by the agent's readiness endpoint. The host polls
// it after boot (or after a snapshot restore) before handing the sandbox out.
type HealthResponse struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	// UptimeMS is milliseconds since the agent started, which after a snapshot
	// restore reflects the snapshotted uptime rather than wall-clock time.
	UptimeMS int64 `json:"uptime_ms"`
}

// ErrorResponse is the agent's body for non-2xx replies.
type ErrorResponse struct {
	Error string `json:"error"`
}
