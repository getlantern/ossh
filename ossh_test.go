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

func TestListenAndDial(t *testing.T) {
	t.Parallel()

	const (
		keyword   = "obfuscation-keyword"
		clientMsg = "hello from the client"
		serverMsg = "hello from the server"
	)

	_hostKey, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	hostKey, err := ssh.NewSignerFromKey(_hostKey)
	require.NoError(t, err)

	lCfg := ListenerConfig{
		HostKey:            hostKey,
		ObfuscationKeyword: keyword,
		Logger: func(clientIP string, err error, fields map[string]interface{}) {
			t.Logf(
				"listener logger invoked\n\tclientIP: %v\n\terr: %v\n\tfields: %+v\n",
				clientIP, err, fields,
			)
		},
	}
	dCfg := DialerConfig{
		ServerPublicKey:    hostKey.PublicKey(),
		ObfuscationKeyword: keyword,
	}

	_l, err := net.Listen("tcp", "")
	require.NoError(t, err)
	l := WrapListener(_l, lCfg)
	d := WrapDialer(&net.Dialer{}, dCfg)

	type result struct {
		msg string
		err error
	}

	serverResC := make(chan result)
	clientResC := make(chan result)
	go func() {
		msg, err := func() (string, error) {
			conn, err := l.Accept()
			if err != nil {
				return "", fmt.Errorf("accept error: %w", err)
			}

			_, err = conn.Write([]byte(serverMsg))
			if err != nil {
				return "", fmt.Errorf("write error: %w", err)
			}

			buf := make([]byte, 100)
			n, err := conn.Read(buf)
			if err != nil {
				return "", fmt.Errorf("read error: %w", err)
			}
			return string(buf[:n]), nil
		}()
		serverResC <- result{msg, err}
	}()
	go func() {
		msg, err := func() (string, error) {
			conn, err := d.Dial("tcp", l.Addr().String())
			if err != nil {
				return "", fmt.Errorf("dial error: %w", err)
			}

			_, err = conn.Write([]byte(clientMsg))
			if err != nil {
				return "", fmt.Errorf("write error: %w", err)
			}

			buf := make([]byte, 100)
			n, err := conn.Read(buf)
			if err != nil {
				return "", fmt.Errorf("read error: %w", err)
			}
			return string(buf[:n]), nil
		}()
		clientResC <- result{msg, err}
	}()

	for i := 0; i < 2; i++ {
		select {
		case res := <-serverResC:
			if assert.NoError(t, res.err) {
				assert.Equal(t, clientMsg, res.msg)
			}
		case res := <-clientResC:
			if assert.NoError(t, res.err) {
				assert.Equal(t, serverMsg, res.msg)
			}
		}
	}
}
