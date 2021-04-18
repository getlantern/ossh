package ossh

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/obfuscator"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/prng"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/nettest"
)

func TestConn(t *testing.T) {
	nettest.TestConn(t, makePipe)
}

// testing; TODO: delete me
func TestSSHReadClose(t *testing.T) {
	obfuscationKeyword := "dirty boots"
	_hostKey, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	hostKey, err := ssh.NewSignerFromKey(_hostKey)
	require.NoError(t, err)

	l, err := net.Listen("tcp", "")
	require.NoError(t, err)
	defer l.Close()

	type result struct {
		rwc *sshReadWriteCloser
		err error
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		rwc, err := func() (*sshReadWriteCloser, error) {
			conn, err := l.Accept()
			if err != nil {
				return nil, fmt.Errorf("accept error: %w", err)
			}
			sshCfg := ssh.ServerConfig{NoClientAuth: true}
			sshCfg.AddHostKey(hostKey)

			conn, err = obfuscator.NewServerObfuscatedSSHConn(
				conn,
				obfuscationKeyword,
				obfuscator.NewSeedHistory(nil), // use the obfuscator package defaults
				nil,
			)
			if err != nil {
				return nil, fmt.Errorf("ossh handshake failed: %w", err)
			}

			sshConn, chans, reqs, err := ssh.NewServerConn(conn, &sshCfg)
			if err != nil {
				return nil, fmt.Errorf("ssh handshake failed: %w", err)
			}
			go ssh.DiscardRequests(reqs)

			channel, reqs, err := (<-chans).Accept()
			if err != nil {
				return nil, fmt.Errorf("failed to accept channel: %w", err)
			}
			go discardChannels(chans)
			go ssh.DiscardRequests(reqs)

			return &sshReadWriteCloser{sshConn, channel}, nil
		}()
		assert.NoError(t, err)
		<-done
		rwc.Close()
	}()

	conn, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)

	prngSeed, err := prng.NewSeed()
	require.NoError(t, err)

	conn, err = obfuscator.NewClientObfuscatedSSHConn(
		conn,
		obfuscationKeyword,
		prngSeed,
		// Set min/max padding to nil to use obfuscator package defaults.
		nil, nil,
	)
	require.NoError(t, err)

	sshCfg := ssh.ClientConfig{HostKeyCallback: ssh.FixedHostKey(hostKey.PublicKey())}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, "", &sshCfg)
	require.NoError(t, err)
	defer sshConn.Close()
	go discardChannels(chans)
	go ssh.DiscardRequests(reqs)

	channel, reqs, err := sshConn.OpenChannel("channel0", []byte{})
	require.NoError(t, err)
	defer channel.Close()
	go ssh.DiscardRequests(reqs)

	errC := make(chan error)
	go func() {
		_, err := channel.Read(make([]byte, 100))
		errC <- err
	}()

	time.Sleep(time.Second)
	select {
	case err := <-errC:
		t.Fatal("read should not have returned; err:", err)
	default:
	}
	require.NoError(t, channel.Close())
	require.Error(t, <-errC)
}
