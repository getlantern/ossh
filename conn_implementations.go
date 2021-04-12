package ossh

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/obfuscator"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/prng"
	"golang.org/x/crypto/ssh"
)

func clientHandshake(transport net.Conn, cfg DialerConfig) func() (ssh.Conn, ssh.Channel, error) {
	return func() (ssh.Conn, ssh.Channel, error) {
		if cfg.ServerPublicKey == nil {
			return nil, nil, errors.New("server public key must be configured")
		}
		if cfg.ObfuscationKeyword == "" {
			return nil, nil, errors.New("obfuscation keywork must be configured")
		}

		prngSeed, err := prng.NewSeed()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to generate PRNG seed: %w", err)
		}

		osshConn, err := obfuscator.NewClientObfuscatedSSHConn(
			transport,
			cfg.ObfuscationKeyword,
			prngSeed,
			// Set min/max padding to nil to use obfuscator package defaults.
			nil, nil,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("ossh handshake failed: %w", err)
		}

		sshCfg := ssh.ClientConfig{HostKeyCallback: ssh.FixedHostKey(cfg.ServerPublicKey)}
		sshConn, chans, reqs, err := ssh.NewClientConn(osshConn, "", &sshCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh handshake failed: %w", err)
		}
		go discardChannels(chans)
		go ssh.DiscardRequests(reqs)

		channel, reqs, err := sshConn.OpenChannel("channel0", []byte{})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open channel: %w", err)
		}
		go ssh.DiscardRequests(reqs)

		return sshConn, channel, nil
	}
}

func serverHandshake(transport net.Conn, cfg ListenerConfig) func() (ssh.Conn, ssh.Channel, error) {
	return func() (ssh.Conn, ssh.Channel, error) {
		if cfg.HostKey == nil {
			return nil, nil, errors.New("host key must be configured")
		}
		if cfg.ObfuscationKeyword == "" {
			return nil, nil, errors.New("obfuscation keywork must be configured")
		}

		osshConn, err := obfuscator.NewServerObfuscatedSSHConn(
			transport,
			cfg.ObfuscationKeyword,
			obfuscator.NewSeedHistory(nil), // use the obfuscator package defaults
			cfg.logger(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("ossh handshake failed: %w", err)
		}

		sshCfg := ssh.ServerConfig{NoClientAuth: true}
		sshCfg.AddHostKey(cfg.HostKey)

		sshConn, chans, reqs, err := ssh.NewServerConn(osshConn, &sshCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh handshake failed: %w", err)
		}
		go ssh.DiscardRequests(reqs)

		channel, reqs, err := (<-chans).Accept()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to accept channel: %w", err)
		}
		go discardChannels(chans)
		go ssh.DiscardRequests(reqs)

		return sshConn, channel, nil
	}
}

type conn struct {
	handshake func() (ssh.Conn, ssh.Channel, error)

	// Both nil until the handshake is complete (iff shakeErr is non-nil).
	conn    ssh.Conn
	channel ssh.Channel

	shakeOnce, closeOnce sync.Once
	shakeErr, closeErr   error

	// According to the documentation, the underlying ossh transport does not define behavior for
	// concurrent calls to one of Read or Write. We protect each to prevent subtle errors.
	// See https://pkg.go.dev/github.com/Psiphon-Labs/psiphon-tunnel-core@v2.0.14+incompatible/psiphon/common/obfuscator#ObfuscatedSSHConn
	readLock, writeLock sync.Mutex
}

func (conn *conn) Handshake() error {
	conn.shakeOnce.Do(func() {
		conn.conn, conn.channel, conn.shakeErr = conn.handshake()
	})
	return conn.shakeErr
}

func (conn *conn) Read(b []byte) (n int, err error) {
	if err := conn.Handshake(); err != nil {
		return 0, err
	}
	conn.readLock.Lock()
	defer conn.readLock.Unlock()
	return conn.channel.Read(b)
}

func (conn *conn) Write(b []byte) (n int, err error) {
	if err := conn.Handshake(); err != nil {
		return 0, err
	}
	conn.writeLock.Lock()
	defer conn.writeLock.Unlock()
	return conn.channel.Write(b)
}

func (conn *conn) Close() error {
	conn.closeOnce.Do(func() {
		conn.channel.Close()
		conn.closeErr = conn.conn.Close()
	})
	return conn.closeErr
}

func discardChannels(chans <-chan ssh.NewChannel) {
	for newChan := range chans {
		newChan.Reject(ssh.ResourceShortage, "not accepting any more channels")
	}
}
