package ossh

import (
	"context"
	"io"
	"net"
)

type DialerConfig struct{}

type ListenerConfig struct{}

type Dialer interface {
	Dial(network, address string) (net.Conn, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

func WrapDialer(d Dialer, cfg DialerConfig) Dialer {
	// TODO: implement me!
	return nil
}

func WrapListener(l net.Listener, cfg ListenerConfig) net.Listener {
	// TODO: implement me!
	return nil
}

type Conn interface {
	io.ReadWriteCloser
}

func Client(tcpConn net.Conn, cfg DialerConfig) Conn {
	// TODO: implement me!
	return nil
}

func Server(tcpConn net.Conn, cfg ListenerConfig) Conn {
	// TODO: implement me!
	return nil
}
