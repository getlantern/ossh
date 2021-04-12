package ossh

import (
	"context"
	"fmt"
	"io"
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
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		// TODO: verify this is required
		return nil, fmt.Errorf("network must be one of tcp, tcp4, or tcp6; got %v", network)
	}
	tcpConn, err := d.NetDialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("failed to dial TCP: %w", err)
	}
	return Client(tcpConn, d.DialerConfig), nil
}

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
	tcpConn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	switch tcpConn.LocalAddr().Network() {
	case "tcp", "tcp4", "tcp6":
	default:
		// TODO: verify this is required
		return nil, fmt.Errorf(
			"network must be one of tcp, tcp4, or tcp6; got %v", tcpConn.LocalAddr().Network())
	}
	return Server(tcpConn, l.ListenerConfig), nil
}

func WrapListener(l net.Listener, cfg ListenerConfig) Listener {
	return listener{l, cfg}
}

type Conn interface {
	io.ReadWriteCloser

	// Handshake executes an ossh handshake with the peer.
	Handshake() error
}

func Client(tcpConn net.Conn, cfg DialerConfig) Conn {
	return &conn{handshake: clientHandshake(tcpConn, cfg)}
}

func Server(tcpConn net.Conn, cfg ListenerConfig) Conn {
	return &conn{handshake: serverHandshake(tcpConn, cfg)}
}
