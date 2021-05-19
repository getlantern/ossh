// Package ossh provides facilities for creating network connections using obfuscated SSH.
// See https://github.com/brl/obfuscated-openssh/blob/master/README.obfuscation
//
// Connections created by this package cannot guarantee true cancellation of Write calls. See the
// Conn type for more details.
package ossh

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
	"golang.org/x/crypto/ssh"
)

/*
 Code Structure

 The output of this package is an API used to set up network connections which communicate over
 obfuscated SSH (OSSH). This is achieved using Psiphon's OSSH implementation, wrapped with Go's
 SSH implementation.

 Across the Lantern codebase (specifically in the Flashlight client and http-proxy-lantern server),
 it is expected that pluggable transports provide net.Conn implementations. However, Go's SSH
 library does not offer any such implementations. Thus the main objective for this package is to
 bridge that gap by providing a net.Conn implementation on top of the offerings of the SSH package.

 To achieve this, we define an interface representing what the SSH package *does* offer. We call
 this an 'almostConn' as it is almost a net.Conn. Our almostConn implementations define everything
 specific to O/SSH - how connections are set up, basic I/O, closing of resources, etc.

 We wrap almostConn implementations in what we call a 'fullConn'. This type fully implements the
 net.Conn interface by adding concurrency-safety, deadline support, and more. The fullConn type has
 no concern for anything O/SSH; it is purely concerned with turning an almostConn into a net.Conn.

*/

// DialerConfig specifies configuration for dialing. All fields are required.
type DialerConfig struct {
	// ObfuscationKeyword is used during the obfuscation handshake and must be agreed upon by the
	// client and the server.
	ObfuscationKeyword string

	ServerPublicKey ssh.PublicKey
}

func (cfg DialerConfig) validate() error {
	if cfg.ObfuscationKeyword == "" {
		return errors.New("obfuscation keyword is required")
	}
	if cfg.ServerPublicKey == nil {
		return errors.New("server public key is required")
	}
	return nil
}

// ListenerConfig specifies configuration for listening.
type ListenerConfig struct {
	// ObfuscationKeyword is used during the obfuscation handshake and must be agreed upon by the
	// client and the server. Required.
	ObfuscationKeyword string

	// HostKey is used to authenticate this server. Required.
	HostKey ssh.Signer

	Logger func(clientIP string, err error, fields map[string]interface{})
}

func (cfg ListenerConfig) validate() error {
	if cfg.ObfuscationKeyword == "" {
		return errors.New("obfuscation keyword is required")
	}
	if cfg.HostKey == nil {
		return errors.New("host key is required")
	}
	return nil
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

// Dialer is the interface implemented by network dialers.
type Dialer interface {
	Dial(network, address string) (net.Conn, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type dialer struct {
	Dialer
	DialerConfig
}

func (d dialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	transport, err := d.Dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("failed to dial TCP: %w", err)
	}
	conn, err := Client(transport, d.DialerConfig)
	if err != nil {
		transport.Close()
		return nil, err
	}
	return conn, nil
}

// WrapDialer wraps a network dialer, returning an ossh dialer.
func WrapDialer(d Dialer, cfg DialerConfig) (Dialer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return dialer{d, cfg}, nil
}

// Dial connects using the provided config. The handshake will not be initiated.
func Dial(network, address string, cfg DialerConfig) (Conn, error) {
	return DialContext(context.Background(), network, address, cfg)
}

// DialContext connects using the provided config and context. The handshake will not be initiated.
func DialContext(ctx context.Context, network, address string, cfg DialerConfig) (Conn, error) {
	d, err := WrapDialer(&net.Dialer{}, cfg)
	if err != nil {
		return nil, err
	}
	conn, err := d.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return conn.(Conn), nil
}

type listener struct {
	net.Listener
	ListenerConfig
}

func (l listener) Accept() (net.Conn, error) {
	transport, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	conn, err := Server(transport, l.ListenerConfig)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// WrapListener wraps a network listener, returning an ossh listener.
func WrapListener(l net.Listener, cfg ListenerConfig) (net.Listener, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return listener{l, cfg}, nil
}

// Listen for new connections using the provided config.
func Listen(network, address string, cfg ListenerConfig) (net.Listener, error) {
	_l, err := net.Listen(network, address)
	if err != nil {
		return nil, err
	}
	l, err := WrapListener(_l, cfg)
	if err != nil {
		_l.Close()
		return nil, err
	}
	return l, nil
}

// Conn is a network connection between two peers communicating via OSSH. Connections returned by
// listeners and dialers in this package will implement this interface. However, most users of this
// package can ignore this type.
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

	// Handshake initiates the connection with the peer. Most users of this package need not call
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
func Client(transport net.Conn, cfg DialerConfig) (Conn, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return newFullConn(&clientConn{transport, cfg, baseConn{nil, nil}}), nil
}

// Server initializes a server-side connection.
func Server(transport net.Conn, cfg ListenerConfig) (Conn, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return newFullConn(&serverConn{transport, cfg, baseConn{nil, nil}}), nil
}
