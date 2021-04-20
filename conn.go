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
// TODO: think about a better way to structure the code in light of the above

// TODO: think about multiplexing via channels rather than layering multiplexing on top

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
	*fullConn
	localAddr, remoteAddr net.Addr

	handshake func() (*sshReadWriteCloser, error)

	shakeOnce sync.Once
	shakeErr  error

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  chan error
}

func newConn(handshakeFunc func() (*sshReadWriteCloser, error)) *conn {
	return &conn{
		handshake: handshakeFunc,
		closed:    make(chan struct{}),
		closeErr:  make(chan error, 1),
	}
}

func (conn *conn) Handshake() error {
	// According to net.Conn.Close, conn.Close should immediately cause any blocked Read or Write
	// operations to unblock and return an error. The underlying deadlineReadWriter guarantees this
	// behavior, but we need to ensure this still holds during the handshake (and the functions
	// provided by crypto/ssh and obfuscator do not support cancellation). We additionally want
	// conn.Close to always return any error returned by the underlying deadlineReadWriter.
	//
	// To achieve both goals, the handshake is executed in a separate goroutine, while the calling
	// routine waits for either handshake completion or closing of the connection. The handshake
	// goroutine lives on after the handshake. This goroutine becomes responsible for (i) closing
	// the deadlineReadWriter when conn.Close is called and (ii) communicating the result.
	// TODO: think about a simpler way to achieve these goals

	conn.shakeOnce.Do(func() {
		shakeChan := make(chan error, 1)
		go func() {
			// var rwc *sshReadWriteCloser
			// rwc, err := conn.handshake()
			// if err != nil {
			// 	shakeChan <- err
			// 	conn.closeErr <- nil
			// 	return
			// }
			// conn.deadlineReadWriter = addDeadlineSupport(rwc)
			// conn.localAddr, conn.remoteAddr = rwc.LocalAddr(), rwc.RemoteAddr()
			// shakeChan <- nil

			// <-conn.closed
			// conn.closeErr <- conn.deadlineReadWriter.Close()
		}()

		select {
		case conn.shakeErr = <-shakeChan:
		case <-conn.closed:
			conn.shakeErr = net.ErrClosed
		}
	})
	return conn.shakeErr
}

func (conn *conn) Read(b []byte) (n int, err error) {
	if err := conn.Handshake(); err != nil {
		return 0, err
	}
	return conn.fullConn.Read(b)
}

func (conn *conn) Write(b []byte) (n int, err error) {
	if err := conn.Handshake(); err != nil {
		return 0, err
	}
	return conn.fullConn.Write(b)
}

func (conn *conn) Close() error {
	conn.closeOnce.Do(func() { close(conn.closed) })
	err := <-conn.closeErr
	// Put the error back on the channel for future calls to Close.
	conn.closeErr <- err
	return err
}

// TODO: These may return nil (because ssh.Conn.*Addr returns nil sometimes). Is that a problem?
func (conn *conn) LocalAddr() net.Addr  { return conn.localAddr }
func (conn *conn) RemoteAddr() net.Addr { return conn.remoteAddr }
