// Package ossh provides facilities for creating network connections using obfuscated SSH.
// See https://github.com/brl/obfuscated-openssh/blob/master/README.obfuscation
//
// Connections created by this package cannot guarantee true cancellation of Write calls. See the
// Conn type for more details.
package ossh

import (
	"context"
	"fmt"
	"net"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
	"golang.org/x/crypto/ssh"
)

// DialerConfig specifies configuration for dialing.
type DialerConfig struct {
	// ServerPublicKey must be set.
	ServerPublicKey ssh.PublicKey

	// ObfuscationKeyword is used during the obfuscation handshake and must be agreed upon by the
	// client and the server. Must be set.
	ObfuscationKeyword string
}

// ListenerConfig specifies configuration for listening.
type ListenerConfig struct {
	// HostKey is provided to SSH clients trying to connect. Must be set.
	HostKey ssh.Signer

	// ObfuscationKeyword is used during the obfuscation handshake and must be agreed upon by the
	// client and the server. Must be set.
	ObfuscationKeyword string

	Logger func(clientIP string, err error, fields map[string]interface{})
}

func (cfg ListenerConfig) logger() func(string, error, common.LogFields) {
	_logger := cfg.Logger
	return func(clientIP string, err error, fields common.LogFields) {
		if _logger == nil {
			return
		}
		_logger(clientIP, err, map[string]interface{}(fields))
	}
}

// Dialer is the interface implemented by ossh dialers. Note that the methods return an ossh Conn,
// not a net.Conn.
type Dialer interface {
	Dial(network, address string) (Conn, error)
	DialContext(ctx context.Context, network, address string) (Conn, error)
}

// NetDialer is the interface implemented by most network dialers.
type NetDialer interface {
	Dial(network, address string) (net.Conn, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type dialer struct {
	NetDialer
	DialerConfig
}

func (d dialer) Dial(network, address string) (Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d dialer) DialContext(ctx context.Context, network, address string) (Conn, error) {
	transport, err := d.NetDialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("failed to dial TCP: %w", err)
	}
	return Client(transport, d.DialerConfig), nil
}

// WrapDialer wraps a network dialer, returning an ossh dialer.
func WrapDialer(d NetDialer, cfg DialerConfig) Dialer {
	return dialer{d, cfg}
}

// Listener is the interface implemented by ossh listeners. Note that the Accept method returns an
// ossh Conn, not a net.Conn.
type Listener interface {
	Accept() (Conn, error)
	Addr() net.Addr
	Close() error
}

type listener struct {
	net.Listener
	ListenerConfig
}

func (l listener) Accept() (Conn, error) {
	transport, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return Server(transport, l.ListenerConfig), nil
}

// WrapListener wraps a network listener, returning an ossh listener.
func WrapListener(l net.Listener, cfg ListenerConfig) Listener {
	return listener{l, cfg}
}

// Conn is a network connection between two peers over ossh.
//
// There is one minor difference between an ossh Conn and a net.Conn. A net.Conn allows for
// cancellation of I/O operations via the Close or Set*Deadline methods. Because an ossh Conn relies
// on a transport with no support for I/O cancellation, it cannot guarantee full cancellation of
// Write calls. Thus a Write call may return with net.ErrClosed or os.ErrDeadlineExceeded, but the
// buffered bytes may eventually be sent on the transport. In practice, this is unlikely to make
// much difference as network writes do not generally block long. An internal buffer is used to
// handle read cancellation.
type Conn interface {
	net.Conn

	// Handshake executes an ossh handshake with the peer. Most users of this package need not call
	// this function directly; the first Read or Write will trigger a handshake if needed.
	//
	// The Set*Deadline functions do not apply to this function. If the handshake is initiated by
	// Read or Write, the corresponding deadline will apply.
	//
	// This function will unblock and return net.ErrClosed if the connection is closed before or
	// during the handshake. As explained in the type documentation, full I/O cancellation is not
	// possible, so the handshake may still occur in the background after such cancellation.
	Handshake() error
}

// Client initializes a client-side connection.
func Client(transport net.Conn, cfg DialerConfig) Conn {
	return newFullConn(&clientConn{transport, cfg, baseConn{nil, nil}})
}

// Server initializes a server-side connection.
func Server(transport net.Conn, cfg ListenerConfig) Conn {
	return newFullConn(&serverConn{transport, cfg, baseConn{nil, nil}})
}
