package ossh

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/obfuscator"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/prng"
	"golang.org/x/crypto/ssh"
)

// almostConn is almost a net.Conn, but lacks full concurrency support and deadline handling. The
// intended use case for an almostConn is as part of a fullConn and method behavior is defined in
// this context.
type almostConn interface {
	// Read and Write behave as defined by the io package. A minor exception is that Write calls are
	// assumed to be short-lived. See fullConn.Write for more details.
	io.ReadWriter

	// Close must cause blocked Read and Write operations to unblock and return errors. This will
	// only be called once.
	Close() error

	// LocalAddr and RemoteAddr may be called at any time.
	LocalAddr() net.Addr
	RemoteAddr() net.Addr

	// Handshake initiates the connection. This method will be called exactly once. Read and Write
	// will not be called until this function returns and only if no error is returned. Close will
	// not be called concurrently, but may be called before or after.
	Handshake() error
}

func discardChannels(chans <-chan ssh.NewChannel) {
	for newChan := range chans {
		newChan.Reject(ssh.ResourceShortage, "not accepting any more channels")
	}
}

// TODO: think about multiplexing via channels rather than assuming it will be layered on top

// baseConn is an io.ReadWriteCloser over SSH, used to implement the almostConn interface.
type baseConn struct {
	conn ssh.Conn
	ch   ssh.Channel
}

func (conn *baseConn) Read(b []byte) (n int, err error)  { return conn.ch.Read(b) }
func (conn *baseConn) Write(b []byte) (n int, err error) { return conn.ch.Write(b) }

func (conn *baseConn) Close() error {
	var chErr error
	// If the peer has already closed the connection, we'll get io.EOF. We ignore this.
	if err := conn.ch.Close(); err != nil && !errors.Is(err, io.EOF) {
		chErr = fmt.Errorf("failed to close channel: %w", err)
	}
	// If the peer has already closed the connection, we'll get net.ErrClosed. We ignore this.
	if err := conn.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return chErr
}

// TODO: ensure resources are cleaned up in early exits of Handshake functions below

// The clientConn and serverConn types below use obfuscator.ObfuscatedSSHConn as the underlying
// transport. This connection does not define behavior for concurrent calls to one of Read or Write:
//
// https://pkg.go.dev/github.com/Psiphon-Labs/psiphon-tunnel-core@v2.0.14+incompatible/psiphon/common/obfuscator#ObfuscatedSSHConn
//
// This is okay because the clientConn and serverConn are designed to be wrapped in a fullConn,
// which enforces single-threaded Reads and Writes. If this changes or if the behavior of fullConn
// changes, we should ensure that we still enforce single-threaded Reads and Writes to prevent
// subtle errors.

// clientConn implements the almostConn interface for OSSH connections.
type clientConn struct {
	transport net.Conn
	cfg       DialerConfig

	// Uninitialized until Handshake is called (iff no error is returned).
	baseConn
}

func (conn *clientConn) LocalAddr() net.Addr  { return conn.transport.LocalAddr() }
func (conn *clientConn) RemoteAddr() net.Addr { return conn.transport.RemoteAddr() }

// Per the almostConn interface, we expect this to be called only once and we do not expect calls to
// Read or Write unless this function is called and returns no error.
func (conn *clientConn) Handshake() error {
	if conn.cfg.ServerPublicKey == nil {
		return errors.New("server public key must be configured")
	}
	if conn.cfg.ObfuscationKeyword == "" {
		return errors.New("obfuscation keywork must be configured")
	}

	prngSeed, err := prng.NewSeed()
	if err != nil {
		return fmt.Errorf("failed to generate PRNG seed: %w", err)
	}

	osshConn, err := obfuscator.NewClientObfuscatedSSHConn(
		conn.transport,
		conn.cfg.ObfuscationKeyword,
		prngSeed,
		// Set min/max padding to nil to use obfuscator package defaults.
		nil, nil,
	)
	if err != nil {
		return fmt.Errorf("ossh handshake failed: %w", err)
	}

	sshCfg := ssh.ClientConfig{HostKeyCallback: ssh.FixedHostKey(conn.cfg.ServerPublicKey)}
	sshConn, chans, reqs, err := ssh.NewClientConn(osshConn, "", &sshCfg)
	if err != nil {
		return fmt.Errorf("ssh handshake failed: %w", err)
	}
	go discardChannels(chans)
	go ssh.DiscardRequests(reqs)

	channel, reqs, err := sshConn.OpenChannel("channel0", []byte{})
	if err != nil {
		return fmt.Errorf("failed to open channel: %w", err)
	}
	go ssh.DiscardRequests(reqs)

	conn.conn, conn.ch = sshConn, channel
	return nil
}

// serverConn implements the almostConn interface for OSSH connections.
type serverConn struct {
	transport net.Conn
	cfg       ListenerConfig

	// Uninitialized until Handshake is called (iff no error is returned).
	baseConn
}

func (conn *serverConn) LocalAddr() net.Addr  { return conn.transport.LocalAddr() }
func (conn *serverConn) RemoteAddr() net.Addr { return conn.transport.RemoteAddr() }

// Per the almostConn interface, we expect this to be called only once and we do not expect calls to
// Read or Write unless this function is called and returns no error.
func (conn *serverConn) Handshake() error {
	if conn.cfg.HostKey == nil {
		return errors.New("host key must be configured")
	}
	if conn.cfg.ObfuscationKeyword == "" {
		return errors.New("obfuscation keywork must be configured")
	}

	osshConn, err := obfuscator.NewServerObfuscatedSSHConn(
		conn.transport,
		conn.cfg.ObfuscationKeyword,
		obfuscator.NewSeedHistory(nil), // use the obfuscator package defaults
		conn.cfg.logger(),
	)
	if err != nil {
		return fmt.Errorf("ossh handshake failed: %w", err)
	}

	sshCfg := ssh.ServerConfig{NoClientAuth: true}
	sshCfg.AddHostKey(conn.cfg.HostKey)

	sshConn, chans, reqs, err := ssh.NewServerConn(osshConn, &sshCfg)
	if err != nil {
		return fmt.Errorf("ssh handshake failed: %w", err)
	}
	go ssh.DiscardRequests(reqs)

	channel, reqs, err := (<-chans).Accept()
	if err != nil {
		return fmt.Errorf("failed to accept channel: %w", err)
	}
	go discardChannels(chans)
	go ssh.DiscardRequests(reqs)

	conn.conn, conn.ch = sshConn, channel
	return nil
}
