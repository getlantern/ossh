package ossh

import (
	goerrors "errors"
	"io"
	"net"

	"github.com/getlantern/errors"

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

	// Close will only be called once. It is not necessary for Close to unblock Reads or Writes.
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
	if err := conn.ch.Close(); err != nil && !goerrors.Is(err, io.EOF) {
		chErr = errors.New("failed to close channel: %v", err)
	}
	// If the peer has already closed the connection, we'll get net.ErrClosed. We ignore this.
	if err := conn.conn.Close(); err != nil && !goerrors.Is(err, net.ErrClosed) {
		return err
	}
	return chErr
}

// Intended for use in accumulating deferred calls, to be possibly cancelled later.
type funcStack []func()

func (s *funcStack) push(f func()) {
	*s = append(*s, f)
}

func (s *funcStack) call() {
	for i := len(*s) - 1; i >= 0; i-- {
		(*s)[i]()
	}
}

func (s *funcStack) clear() {
	*s = []func(){}
}

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
	cleanupOnFailure := funcStack{}
	defer cleanupOnFailure.call()

	prngSeed, err := prng.NewSeed()
	if err != nil {
		return errors.New("failed to generate PRNG seed: %v", err)
	}

	osshConn, err := obfuscator.NewClientObfuscatedSSHConn(
		conn.transport,
		conn.cfg.ObfuscationKeyword,
		prngSeed,
		// Set min/max padding to nil to use obfuscator package defaults.
		nil, nil,
	)
	if err != nil {
		return errors.New("ossh handshake failed: %v", err)
	}
	cleanupOnFailure.push(func() { osshConn.Close() })

	sshCfg := ssh.ClientConfig{HostKeyCallback: ssh.FixedHostKey(conn.cfg.ServerPublicKey)}
	sshConn, chans, reqs, err := ssh.NewClientConn(osshConn, "", &sshCfg)
	if err != nil {
		return errors.New("ssh handshake failed: %v", err)
	}
	go discardChannels(chans)
	go ssh.DiscardRequests(reqs)
	cleanupOnFailure.push(func() { sshConn.Close() })

	channel, reqs, err := sshConn.OpenChannel("channel0", []byte{})
	if err != nil {
		return errors.New("failed to open channel: %v", err)
	}
	go ssh.DiscardRequests(reqs)

	cleanupOnFailure.clear()
	conn.conn, conn.ch = sshConn, channel
	return nil
}

func (conn *clientConn) Close() error {
	if conn.conn == nil {
		// Handshake has not yet occurred.
		return conn.transport.Close()
	}
	return conn.baseConn.Close()
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
	cleanupOnFailure := funcStack{}
	defer cleanupOnFailure.call()

	osshConn, err := obfuscator.NewServerObfuscatedSSHConn(
		conn.transport,
		conn.cfg.ObfuscationKeyword,
		obfuscator.NewSeedHistory(nil), // use the obfuscator package defaults
		conn.cfg.logger(),
	)
	if err != nil {
		return errors.New("ossh handshake failed: %v", err)
	}
	cleanupOnFailure.push(func() { osshConn.Close() })

	sshCfg := ssh.ServerConfig{NoClientAuth: true}
	sshCfg.AddHostKey(conn.cfg.HostKey)

	sshConn, chans, reqs, err := ssh.NewServerConn(osshConn, &sshCfg)
	if err != nil {
		return errors.New("ssh handshake failed: %v", err)
	}
	go ssh.DiscardRequests(reqs)
	cleanupOnFailure.push(func() { sshConn.Close() })

	channel, reqs, err := (<-chans).Accept()
	if err != nil {
		return errors.New("failed to accept channel: %v", err)
	}
	go discardChannels(chans)
	go ssh.DiscardRequests(reqs)

	cleanupOnFailure.clear()
	conn.conn, conn.ch = sshConn, channel
	return nil
}

func (conn *serverConn) Close() error {
	if conn.conn == nil {
		// Handshake has not yet occurred.
		return conn.transport.Close()
	}
	return conn.baseConn.Close()
}
