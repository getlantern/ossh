package ossh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

type DialerConfig struct {
	ssh.ClientConfig

	ObfuscationKeyword     string
	PaddingPRNGSeed        [32]byte
	MinPadding, MaxPadding int
}

type ListenerConfig struct {
	ssh.ServerConfig

	ObfuscationKeyword string
	SeedTTL            time.Duration
	Logger             func(clientIP string, err error, fields map[string]interface{})
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
		return nil, errors.New("network must be one of tcp, tcp4, or tcp6")
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
		// TODO: verify this is required
		return nil, errors.New("network must be one of tcp, tcp4, or tcp6")
	}
	return Server(tcpConn, l.ListenerConfig), nil
}

func WrapListener(l net.Listener, cfg ListenerConfig) Listener {
	return listener{l, cfg}
}

type Conn interface {
	io.ReadWriteCloser

	// RemoteAddr returns the remote address for this connection.
	RemoteAddr() net.Addr

	// LocalAddr returns the local address for this connection.
	LocalAddr() net.Addr
}

func Client(tcpConn Conn, cfg DialerConfig) Conn {
	// TODO: implement me!
	return nil
}

func Server(tcpConn Conn, cfg ListenerConfig) Conn {
	// TODO: implement me!
	return nil
}
