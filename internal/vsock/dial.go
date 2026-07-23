// Package vsock dials into a Firecracker guest's vsock device from the host.
//
// Firecracker does not expose the guest's AF_VSOCK ports directly. It multiplexes
// them onto a single Unix domain socket on the host, and a connection announces
// which guest port it wants with a text handshake:
//
//	host -> guest:  CONNECT 5000\n
//	guest -> host:  OK 1073741824\n
//
// The number the guest replies with is the ephemeral port it assigned on its
// side; it carries no meaning for the caller. After the handshake the socket is
// a plain bidirectional stream, which is what makes it possible to run ordinary
// HTTP over it.
package vsock

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// handshakeTimeout bounds the CONNECT exchange. A guest that has not booted far
// enough to listen will accept the Unix connection (Firecracker is listening,
// not the guest) and then never reply, so without a deadline the dial would
// block indefinitely.
const handshakeTimeout = 10 * time.Second

// Dial connects to the given port inside the guest whose vsock device is backed
// by the Unix socket at udsPath.
func Dial(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("dial vsock socket %s: %w", udsPath, err)
	}

	if err := handshake(ctx, conn, port); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func handshake(ctx context.Context, conn net.Conn, port uint32) error {
	deadline := time.Now().Add(handshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	// Clear the deadline before handing the connection back: the caller may hold
	// it open for the lifetime of a long exec stream, and an inherited deadline
	// would sever it mid-output.
	defer conn.SetDeadline(time.Time{})

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return fmt.Errorf("send CONNECT: %w", err)
	}

	// The reply must be read without over-reading: anything buffered past the
	// newline belongs to the caller's protocol. bufio would happily swallow it,
	// so read a byte at a time. It costs one syscall per byte, but only for the
	// ~20 bytes of a handshake that happens once per connection.
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("read CONNECT reply: %w", err)
	}

	if !strings.HasPrefix(line, "OK ") {
		// Firecracker answers a refused port with a bare "connection refused"
		// style line; surface whatever it said rather than a generic failure.
		return fmt.Errorf("vsock connect to port %d rejected: %q", port, line)
	}
	return nil
}

// readLine reads up to and including the first newline.
func readLine(conn net.Conn) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return b.String(), err
		}
		if n == 0 {
			continue
		}
		if buf[0] == '\n' {
			return strings.TrimRight(b.String(), "\r"), nil
		}
		b.WriteByte(buf[0])

		// A well-behaved peer sends ~20 bytes. Anything longer means we are not
		// talking to Firecracker, and reading on would be unbounded.
		if b.Len() > 128 {
			return b.String(), fmt.Errorf("handshake reply too long")
		}
	}
}

// Transport returns an http.Transport that routes every request to the given
// guest port over the guest's vsock socket, whatever host the URL names.
//
// This is what lets the host use net/http against the in-guest agent: the URL's
// host is a placeholder, and DialContext ignores it.
func Transport(udsPath string, port uint32) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return Dial(ctx, udsPath, port)
		},
		// The agent streams exec output as it is produced. Compression would
		// buffer to fill a block and destroy the per-frame latency that makes
		// streaming useful.
		DisableCompression: true,

		// Connections are cheap here (a Unix socket, no TLS, no DNS) and each
		// exec holds one for its whole life, so pooling buys little; but a small
		// pool avoids re-handshaking for bursts of short calls.
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     30 * time.Second,

		// No ResponseHeaderTimeout on purpose: an exec's headers arrive promptly
		// but its body may stream for hours, and this timeout only covers the
		// former. Per-request deadlines come from the caller's context.
	}
}
