// Package ossh provides facilities for creating network connections using obfuscated SSH.
// See https://github.com/brl/obfuscated-openssh/blob/master/README.obfuscation
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
type Conn interface {
	io.ReadWriteCloser

	// Handshake executes an ossh handshake with the peer. Most users of this package need not call
	// this function directly; the first Read or Write will trigger a handshake if needed.
	Handshake() error
}

// Client initializes a client-side connection.
func Client(transport net.Conn, cfg DialerConfig) Conn {
	return &conn{handshake: clientHandshake(transport, cfg)}
}

// Server initializes a server-side connection.
func Server(transport net.Conn, cfg ListenerConfig) Conn {
	return &conn{handshake: serverHandshake(transport, cfg)}
}
