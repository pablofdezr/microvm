package runtimetest

import (
	"context"
	"errors"
	"net"
	"sync"
)

// Listener is an in-memory stand-in for a sandbox's inbound vsock socket.
//
// It is a real net.Listener over real connections, so a real http.Server runs
// on it unmodified -- which is the only reason the host-side storage server is
// testable without KVM at all. Only the transport is pretend: net.Pipe instead
// of a Unix socket, which also sidesteps the sun_path length limit that makes
// socket files in temp directories such a reliable source of EINVAL.
//
// Dial is what the guest would do. There is no CONNECT handshake here for the
// same reason there is none in production: guest-initiated vsock connections
// carry no handshake at all, and a fake that invented one would let a bug
// through by testing a protocol nobody speaks.
type Listener struct {
	conns chan net.Conn

	closeOnce sync.Once
	closed    chan struct{}
}

// NewListener returns an open in-memory listener.
func NewListener() *Listener {
	return &Listener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

// ErrListenerClosed is returned by Accept and Dial after Close.
var ErrListenerClosed = errors.New("runtimetest: listener is closed")

func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		// Real listeners report a closed listener rather than blocking, and the
		// Accept loops above depend on it: that error is how a server learns its
		// sandbox is gone and stops.
		return nil, ErrListenerClosed
	}
}

func (l *Listener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *Listener) Addr() net.Addr { return pipeAddr{} }

// Dial opens a connection to whatever is serving, as the guest would.
//
// The handoff is synchronous: it blocks until something Accepts, or the context
// ends, or the listener closes. Nothing is buffered, so a test cannot "connect"
// to a server that is not actually accepting and then wonder why its request
// vanished.
func (l *Listener) Dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()

	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		client.Close()
		server.Close()
		return nil, ErrListenerClosed
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, ctx.Err()
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "runtimetest-vsock" }

var _ net.Listener = (*Listener)(nil)
