package ossh

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// makePipe implements nettest.MakePipe.
func makePipe() (c1, c2 net.Conn, stop func(), err error) {
	const keyword = "obfuscation-keyword"

	_hostKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to generate host key (using RSA): %w", err)
	}
	hostKey, err := ssh.NewSignerFromKey(_hostKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to generate signer from RSA key: %w", err)
	}

	lCfg := ListenerConfig{
		HostKey:            hostKey,
		ObfuscationKeyword: keyword,
	}
	dCfg := DialerConfig{
		ServerPublicKey:    hostKey.PublicKey(),
		ObfuscationKeyword: keyword,
	}

	// It would be simpler to use net.Pipe to set up the peer connections (maybe with some internal
	// buffering as in tlsmasq/internal/testutil.BufferedPipe). However, golang.org/x/crypto/ssh
	// does not seem to like these piped connections. Instead, we just set up a local listener.

	_l, err := net.Listen("tcp", "")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to start TCP listener: %w", err)
	}
	l := WrapListener(_l, lCfg)
	d := WrapDialer(&net.Dialer{}, dCfg)
	defer l.Close()

	type result struct {
		conn Conn
		err  error
	}

	serverResC := make(chan result, 1)
	clientResC := make(chan result, 1)
	go func() {
		conn, err := func() (Conn, error) {
			conn, err := l.Accept()
			if err != nil {
				return nil, fmt.Errorf("accept error: %w", err)
			}
			return conn, nil
		}()
		serverResC <- result{conn, err}
	}()
	go func() {
		conn, err := func() (Conn, error) {
			conn, err := d.Dial("tcp", l.Addr().String())
			if err != nil {
				return nil, fmt.Errorf("dial error: %w", err)
			}
			return conn, nil
		}()
		clientResC <- result{conn, err}
	}()

	var server, client Conn
	for i := 0; i < 2; i++ {
		select {
		case res := <-serverResC:
			if res.err != nil {
				return nil, nil, nil, fmt.Errorf("failed to init server-side: %w", err)
			}
			server = res.conn
		case res := <-clientResC:
			if res.err != nil {
				return nil, nil, nil, fmt.Errorf("failed to init client-side: %w", err)
			}
			client = res.conn
		}
	}
	return client, server, func() { client.Close(); server.Close() }, nil
}

func TestAddr(t *testing.T) {
	c1, c2, stop, err := makePipe()
	require.NoError(t, err)
	defer stop()

	fmt.Println("c1.Local:", c1.LocalAddr())
	fmt.Println("c1.Remote:", c1.RemoteAddr())
	fmt.Println("c2.Local:", c2.LocalAddr())
	fmt.Println("c2.Remote:", c2.RemoteAddr())

	go c1.(Conn).Handshake()
	c2.(Conn).Handshake()

	fmt.Println("c1.Local:", c1.LocalAddr())
	fmt.Println("c1.Remote:", c1.RemoteAddr())
	fmt.Println("c2.Local:", c2.LocalAddr())
	fmt.Println("c2.Remote:", c2.RemoteAddr())
}

func TestListenAndDial(t *testing.T) {
	t.Parallel()

	const (
		clientMsg = "hello from the client"
		serverMsg = "hello from the server"
	)

	client, server, stop, err := makePipe()
	require.NoError(t, err)
	defer stop()

	type result struct {
		msg string
		err error
	}

	serverResC := make(chan result)
	clientResC := make(chan result)
	go func() {
		msg, err := func() (string, error) {
			_, err := client.Write([]byte(clientMsg))
			if err != nil {
				return "", fmt.Errorf("write error: %w", err)
			}

			buf := make([]byte, 100)
			n, err := client.Read(buf)
			if err != nil {
				return "", fmt.Errorf("read error: %w", err)
			}
			return string(buf[:n]), nil
		}()
		clientResC <- result{msg, err}
	}()
	go func() {
		msg, err := func() (string, error) {
			_, err := server.Write([]byte(serverMsg))
			if err != nil {
				return "", fmt.Errorf("write error: %w", err)
			}

			buf := make([]byte, 100)
			n, err := server.Read(buf)
			if err != nil {
				return "", fmt.Errorf("read error: %w", err)
			}
			return string(buf[:n]), nil
		}()
		serverResC <- result{msg, err}
	}()

	for i := 0; i < 2; i++ {
		select {
		case res := <-clientResC:
			if assert.NoError(t, res.err) {
				assert.Equal(t, serverMsg, res.msg)
			}
		case res := <-serverResC:
			if assert.NoError(t, res.err) {
				assert.Equal(t, clientMsg, res.msg)
			}
		}
	}
}
