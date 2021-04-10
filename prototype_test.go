package ossh

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/obfuscator"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/prng"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// Testing stuff out. TODO: delete this file

type sshChannel struct {
	ssh.Channel
	parent ssh.Conn
}

func (c sshChannel) Close() error {
	c.Channel.Close()
	return c.parent.Close()
}

func osshPipe(t *testing.T) (client, server io.ReadWriteCloser) {
	intPtr := func(n int) *int { return &n }

	seed, err := prng.NewSeed()
	require.NoError(t, err)

	seedHistory := obfuscator.NewSeedHistory(&obfuscator.SeedHistoryConfig{
		SeedTTL:            time.Hour,
		SeedMaxEntries:     50,
		ClientIPTTL:        time.Hour,
		ClientIPMaxEntries: 50,
	})

	logger := func(clientIP string, err error, logFields common.LogFields) {
		fmt.Printf(
			"logger invoked\n\tclientIP: %v\n\terr: %v\n\tlogFields: %+v\n",
			clientIP, err, logFields,
		)
	}

	discardChannels := func(chans <-chan ssh.NewChannel) {
		for newChan := range chans {
			fmt.Printf("discarding channel (type: %v)\n", newChan.ChannelType())
			newChan.Reject(ssh.ResourceShortage, "not accepting any more channels")
		}
	}

	_serverKey, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	serverKey, err := ssh.NewSignerFromKey(_serverKey)
	require.NoError(t, err)

	clientCfg := &ssh.ClientConfig{
		HostKeyCallback: ssh.FixedHostKey(serverKey.PublicKey()),
	}
	serverCfg := &ssh.ServerConfig{
		NoClientAuth: true,
		AuthLogCallback: func(conn ssh.ConnMetadata, method string, err error) {
			fmt.Printf(
				"auth-log callback invoked:\n\tuser: %v\n\tmethod: %v\n\terr: %v\n",
				conn.User(), method, err,
			)
		},
	}
	serverCfg.AddHostKey(serverKey)

	l, err := net.Listen("tcp", "")
	require.NoError(t, err)
	defer l.Close()

	wg := new(sync.WaitGroup)
	wg.Add(2)

	type result struct {
		conn io.ReadWriteCloser
		err  error
	}
	clientResultC := make(chan result)
	serverResultC := make(chan result)

	go func() {
		client, err := func() (*sshChannel, error) {
			clientTCP, err := net.Dial("tcp", l.Addr().String())
			if err != nil {
				return nil, fmt.Errorf("dial failed: %w", err)
			}

			clientOSSH, err := obfuscator.NewClientObfuscatedSSHConn(
				clientTCP,
				"foo",
				seed,
				intPtr(50), intPtr(200),
			)
			if err != nil {
				return nil, fmt.Errorf("ossh handshake failed: %w", err)
			}

			client, chans, reqs, err := ssh.NewClientConn(clientOSSH, "", clientCfg)
			if err != nil {
				return nil, fmt.Errorf("ssh handshake failed: %w", err)
			}
			go ssh.DiscardRequests(reqs)
			go discardChannels(chans)

			sshChan, reqs, err := client.OpenChannel("test-channel", []byte{})
			if err != nil {
				return nil, fmt.Errorf("failed to open channel: %w", err)
			}
			go ssh.DiscardRequests(reqs)

			return &sshChannel{sshChan, client}, nil
		}()
		clientResultC <- result{client, err}
	}()

	go func() {
		server, err := func() (*sshChannel, error) {
			serverTCP, err := l.Accept()
			if err != nil {
				return nil, fmt.Errorf("accept failed: %w", err)
			}

			serverOSSH, err := obfuscator.NewServerObfuscatedSSHConn(
				serverTCP,
				"foo",
				seedHistory,
				logger,
			)
			if err != nil {
				return nil, fmt.Errorf("ossh handshake failed: %w", err)
			}

			server, chans, reqs, err := ssh.NewServerConn(serverOSSH, serverCfg)
			if err != nil {
				return nil, fmt.Errorf("ssh handshake failed: %w", err)
			}
			go ssh.DiscardRequests(reqs)

			newChan := <-chans
			sshChan, reqs, err := newChan.Accept()
			if err != nil {
				return nil, fmt.Errorf("failed to accept channel: %w", err)
			}
			go ssh.DiscardRequests(reqs)
			go discardChannels(chans)

			return &sshChannel{sshChan, server}, nil
		}()
		serverResultC <- result{server, err}
	}()

	for i := 0; i < 2; i++ {
		select {
		case res := <-clientResultC:
			assert.NoError(t, res.err, "client error")
			client = res.conn
		case res := <-serverResultC:
			assert.NoError(t, res.err, "server error")
			server = res.conn
		}
	}
	return client, server
}

func TestClientServer(t *testing.T) {
	client, server := osshPipe(t)

	type result struct {
		msg string
		err error
	}

	clientResultC := make(chan result)
	serverResultC := make(chan result)

	go func() {
		msg, err := func() (string, error) {
			_, err := client.Write([]byte("hello, server"))
			if err != nil {
				return "", fmt.Errorf("write error: %w", err)
			}
			b := make([]byte, 1024)
			n, err := client.Read(b)
			if err != nil {
				return "", fmt.Errorf("read error: %w", err)
			}
			return string(b[:n]), nil
		}()
		clientResultC <- result{msg, err}
	}()
	go func() {
		msg, err := func() (string, error) {
			b := make([]byte, 1024)
			n, err := server.Read(b)
			if err != nil {
				return "", fmt.Errorf("read error: %w", err)
			}
			_, err = server.Write([]byte("hello, client"))
			if err != nil {
				return "", fmt.Errorf("write error: %w", err)
			}
			return string(b[:n]), nil
		}()
		serverResultC <- result{msg, err}
	}()

	for i := 0; i < 2; i++ {
		select {
		case res := <-clientResultC:
			require.NoError(t, res.err)
			fmt.Println("client receieved message:", res.msg)
		case res := <-serverResultC:
			require.NoError(t, res.err)
			fmt.Println("server receieved message:", res.msg)
		}
	}
}
