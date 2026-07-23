package vsock

import (
	"fmt"
	"net"
	"os"
)

// HostListenerPath is where Firecracker looks for a host-side listener.
//
// The suffix is Firecracker's convention, not ours, and it is an underscore
// rather than a directory: the socket for port 5001 backed by /x/v.sock is
// /x/v.sock_5001, a sibling file. Getting this wrong produces no error anywhere
// -- Firecracker simply refuses the guest's connection, and the guest sees
// ECONNRESET with nothing on the host to explain it.
func HostListenerPath(udsPath string, port uint32) string {
	return fmt.Sprintf("%s_%d", udsPath, port)
}

// Listen accepts connections *from* a guest, which is not the reverse of Dial.
//
// The two directions look symmetric and are not, which is the single most
// important thing to know about this file:
//
//   - Host to guest (Dial): connect to the one socket, then announce the port
//     with a "CONNECT 5000\n" handshake. One socket, many ports, multiplexed.
//   - Guest to host (here): the guest connects to CID 2 on a port, and
//     Firecracker looks for a Unix socket named after *that port* and connects
//     to it. One socket per port, no multiplexing, and no handshake at all.
//
// So nothing is read from an accepted connection before handing it back. Trying
// to consume a CONNECT line here -- the obvious symmetry -- would eat the first
// bytes of the guest's actual request and hang forever waiting for a newline
// that is never coming.
//
// The listener must exist before the VM can use it. Firecracker resolves the
// path when the guest connects, and a missing file is a refused connection, not
// a retry.
//
// # Why the socket is the identity
//
// This is what makes the whole storage design work. The socket lives inside one
// sandbox's jail, so a connection arriving on it came from that sandbox and no
// other. That is not a claim the guest makes and not a token it holds: it is a
// fact about the filesystem, fixed before the VM booted. There is nothing to
// forge, nothing to steal, and no way for a sandbox to reach the socket that
// would answer for a different one. Callers must therefore keep one listener
// per sandbox and never share it -- the isolation is entirely in the path.
func Listen(udsPath string, port uint32, uid, gid int) (net.Listener, error) {
	path := HostListenerPath(udsPath, port)

	// A leftover socket file makes bind fail with EADDRINUSE, and the file
	// outlives the process that made it -- a crashed daemon leaves one behind and
	// every later sandbox at that path would fail to start. Removing it is safe
	// here only because each jail directory is freshly created for one sandbox:
	// there is no live listener this could be stealing.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale vsock listener %s: %w", path, err)
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on vsock host socket %s: %w", path, err)
	}

	// Whatever Go's umask left is not good enough for a socket that is the only
	// thing standing between one sandbox's data and another's. Narrow it to the
	// VMM's own uid before anything can connect: the jailed Firecracker is the
	// only process with any business here, and it runs as uid after dropping
	// privileges. The daemon itself keeps access through the open fd, which no
	// longer consults the file's mode.
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("chmod vsock host socket %s: %w", path, err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		l.Close()
		return nil, fmt.Errorf("chown vsock host socket %s to %d:%d: %w", path, uid, gid, err)
	}

	return l, nil
}
