package ossh

import (
	"errors"
	"fmt"
	"net"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/obfuscator"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/prng"
	"golang.org/x/crypto/ssh"
)

// The sshReadWriteClosers returned by the handshake functions below use an
// obfuscator.ObfuscatedSSHConn as the underlying transport. This connection does not define
// behavior for concurrent calls to one of Read or Write:
//
// https://pkg.go.dev/github.com/Psiphon-Labs/psiphon-tunnel-core@v2.0.14+incompatible/psiphon/common/obfuscator#ObfuscatedSSHConn
//
// It so happens that we always wrap sshReadWriteClosers in a deadlineReadWriter, which enforces
// single-threaded Reads and Writes anyway. If this changes or if the behavior of deadlineReadWriter
// changes, we should ensure that we still enforce single-threaded Reads and Writes to prevent
// subtle errors.

func discardChannels(chans <-chan ssh.NewChannel) {
	for newChan := range chans {
		newChan.Reject(ssh.ResourceShortage, "not accepting any more channels")
	}
}

func clientHandshake(transport net.Conn, cfg DialerConfig) func() (*sshReadWriteCloser, error) {
	return func() (*sshReadWriteCloser, error) {
		if cfg.ServerPublicKey == nil {
			return nil, errors.New("server public key must be configured")
		}
		if cfg.ObfuscationKeyword == "" {
			return nil, errors.New("obfuscation keywork must be configured")
		}

		prngSeed, err := prng.NewSeed()
		if err != nil {
			return nil, fmt.Errorf("failed to generate PRNG seed: %w", err)
		}

		osshConn, err := obfuscator.NewClientObfuscatedSSHConn(
			transport,
			cfg.ObfuscationKeyword,
			prngSeed,
			// Set min/max padding to nil to use obfuscator package defaults.
			nil, nil,
		)
		if err != nil {
			return nil, fmt.Errorf("ossh handshake failed: %w", err)
		}

		sshCfg := ssh.ClientConfig{HostKeyCallback: ssh.FixedHostKey(cfg.ServerPublicKey)}
		sshConn, chans, reqs, err := ssh.NewClientConn(osshConn, "", &sshCfg)
		if err != nil {
			return nil, fmt.Errorf("ssh handshake failed: %w", err)
		}
		go discardChannels(chans)
		go ssh.DiscardRequests(reqs)

		channel, reqs, err := sshConn.OpenChannel("channel0", []byte{})
		if err != nil {
			return nil, fmt.Errorf("failed to open channel: %w", err)
		}
		go ssh.DiscardRequests(reqs)

		return &sshReadWriteCloser{sshConn, channel}, nil
	}
}

func serverHandshake(transport net.Conn, cfg ListenerConfig) func() (*sshReadWriteCloser, error) {
	return func() (*sshReadWriteCloser, error) {
		if cfg.HostKey == nil {
			return nil, errors.New("host key must be configured")
		}
		if cfg.ObfuscationKeyword == "" {
			return nil, errors.New("obfuscation keywork must be configured")
		}

		osshConn, err := obfuscator.NewServerObfuscatedSSHConn(
			transport,
			cfg.ObfuscationKeyword,
			obfuscator.NewSeedHistory(nil), // use the obfuscator package defaults
			cfg.logger(),
		)
		if err != nil {
			return nil, fmt.Errorf("ossh handshake failed: %w", err)
		}

		sshCfg := ssh.ServerConfig{NoClientAuth: true}
		sshCfg.AddHostKey(cfg.HostKey)

		sshConn, chans, reqs, err := ssh.NewServerConn(osshConn, &sshCfg)
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
	}
}

type sshReadWriteCloser struct {
	conn ssh.Conn
	ch   ssh.Channel
}

func (rwc *sshReadWriteCloser) LocalAddr() net.Addr               { return rwc.conn.LocalAddr() }
func (rwc *sshReadWriteCloser) RemoteAddr() net.Addr              { return rwc.conn.RemoteAddr() }
func (rwc *sshReadWriteCloser) Read(b []byte) (n int, err error)  { return rwc.ch.Read(b) }
func (rwc *sshReadWriteCloser) Write(b []byte) (n int, err error) { return rwc.ch.Write(b) }
func (rwc *sshReadWriteCloser) Close() error                      { rwc.ch.Close(); return rwc.conn.Close() }

type conn struct {
	// Fields in this block are nil until the handshake is complete (iff shakeErr is nil).
	*deadlineReadWriter
	localAddr, remoteAddr net.Addr

	handshake       func() (*sshReadWriteCloser, error)
	shakeOnce       *once
	cancelHandshake chan struct{}
}

func (conn *conn) Handshake() error {
	return conn.shakeOnce.do(func() error {
		var rwc *sshReadWriteCloser
		rwc, err := conn.handshake()
		if err != nil {
			return err
		}
		conn.deadlineReadWriter = addDeadlineSupport(rwc)
		conn.localAddr, conn.remoteAddr = rwc.LocalAddr(), rwc.RemoteAddr()
		return nil
	})
}

func (conn *conn) Read(b []byte) (n int, err error) {
	if err := conn.Handshake(); err != nil {
		return 0, err
	}
	return conn.deadlineReadWriter.Read(b)
}

func (conn *conn) Write(b []byte) (n int, err error) {
	if err := conn.Handshake(); err != nil {
		return 0, err
	}
	return conn.deadlineReadWriter.Write(b)
}

func (conn *conn) Close() error {
	if cancelled := conn.shakeOnce.cancel(net.ErrClosed); !cancelled {
		return conn.deadlineReadWriter.Close()
	}
	return nil
}

func (conn *conn) LocalAddr() net.Addr  { return conn.localAddr }
func (conn *conn) RemoteAddr() net.Addr { return conn.remoteAddr }
