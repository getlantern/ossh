package ossh

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlantern/errors"
	"github.com/getlantern/nettest"
	"github.com/stretchr/testify/require"

	"golang.org/x/crypto/ssh"
)

func TestConn(t *testing.T) {
	// Tests I/O, deadline support, net.Conn adherence, and data races.
	nettest.TestConn(t, makePipe)

	// Tests calls made before and during the handshake.
	testHandshake(t, makePipe)
}

// TestListenAndDial ensures the Listen and Dial functions set up viable connections. More thorough
// testing of the connection itself is left to TestConn.
func TestListenAndDial(t *testing.T) {
	lCfg, dCfg, err := configPair()
	require.NoError(t, err)

	l, err := Listen("tcp", "", *lCfg)
	require.NoError(t, err)
	defer l.Close()

	listenerErr := make(chan error, 1)
	go func() {
		listenerErr <- func() error {
			conn, err := l.Accept()
			if err != nil {
				return fmt.Errorf("accept error: %w", err)
			}
			defer conn.Close()
			// Echo everything back to the dialer.
			_, err = io.Copy(conn, conn)
			if err != nil {
				return fmt.Errorf("copy error: %w", err)
			}
			return nil
		}()
	}()

	conn, err := Dial("tcp", l.Addr().String(), *dCfg)
	require.NoError(t, err)

	msg := []byte("hello OSSH")
	_, err = conn.Write(msg)
	require.NoError(t, err)

	buf := make([]byte, len(msg)*2)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	require.Equal(t, string(msg), string(buf[:n]))

	require.NoError(t, conn.Close())
	require.NoError(t, <-listenerErr)
}

// makePipe implements nettest.MakePipe.
func makePipe() (c1, c2 net.Conn, stop func(), err error) {
	lCfg, dCfg, err := configPair()
	if err != nil {
		return nil, nil, nil, err
	}

	// It would be simpler to use net.Pipe to set up the peer transports. However, the ssh library
	// does not like these piped connections: https://github.com/golang/go/issues/32990.

	c1TCP, c2TCP, _, err := makePipeTCP()
	if err != nil {
		return nil, nil, nil, errors.New("failed to create TCP pipe: %v", err)
	}
	var c1Almost almostConn = &clientConn{c1TCP, *dCfg, baseConn{nil, nil}}
	var c2Almost almostConn = &serverConn{c2TCP, *lCfg, baseConn{nil, nil}}
	c1Almost, c2Almost = coordinateClose(c1Almost, c2Almost)
	c1, c2 = newFullConn(c1Almost), newFullConn(c2Almost)
	stop = func() { c1.Close(); c2.Close() }
	return
}

func configPair() (*ListenerConfig, *DialerConfig, error) {
	const keyword = "obfuscation-keyword"

	_hostKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, nil, errors.New("failed to generate host key (using RSA): %v", err)
	}
	hostKey, err := ssh.NewSignerFromKey(_hostKey)
	if err != nil {
		return nil, nil, errors.New("failed to generate signer from RSA key: %v", err)
	}

	lCfg := &ListenerConfig{
		HostKey:            hostKey,
		ObfuscationKeyword: keyword,
	}
	dCfg := &DialerConfig{
		ServerPublicKey:    hostKey.PublicKey(),
		ObfuscationKeyword: keyword,
	}
	return lCfg, dCfg, nil
}

func makePipeTCP() (c1, c2 net.Conn, stop func(), err error) {
	l, err := net.Listen("tcp", "")
	if err != nil {
		return nil, nil, nil, errors.New("failed to start TCP listener: %v", err)
	}
	defer l.Close()

	type result struct {
		conn net.Conn
		err  error
	}

	serverResC := make(chan result, 1)
	clientResC := make(chan result, 1)
	go func() {
		conn, err := func() (net.Conn, error) {
			conn, err := l.Accept()
			if err != nil {
				return nil, errors.New("accept error: %v", err)
			}
			return conn, nil
		}()
		serverResC <- result{conn, err}
	}()
	go func() {
		conn, err := func() (net.Conn, error) {
			conn, err := net.Dial("tcp", l.Addr().String())
			if err != nil {
				return nil, errors.New("dial error: %v", err)
			}
			return conn, nil
		}()
		clientResC <- result{conn, err}
	}()

	for i := 0; i < 2; i++ {
		select {
		case res := <-clientResC:
			if res.err != nil {
				return nil, nil, nil, errors.New("failed to init client-side: %v", err)
			}
			c1 = res.conn
		case res := <-serverResC:
			if res.err != nil {
				return nil, nil, nil, errors.New("failed to init server-side: %v", err)
			}
			c2 = res.conn
		}
	}
	return c1, c2, func() { c1.Close(); c2.Close() }, nil
}

// The ssh.Channel type underlying our Conn implementations has a quirk in which reads may fail
// early after the peer has closed the transport: https://github.com/golang/go/issues/45912.
// This is unlikely to present much of an issue in production use cases, but causes some of the
// tests provided by the nettest package to fail. We work around this by coordinating closes.
type coordinatedCloser struct {
	almostConn

	myReads, myWrites, peerReads       chan int
	closing, peerClosing, readyToClose chan struct{}
	pendingWrites                      int64
	closeOnce                          *sync.Once
	closeErr                           error
}

func coordinateClose(c1, c2 almostConn) (cc1, cc2 almostConn) {
	var (
		c1Reads, c1Writes    = make(chan int), make(chan int)
		c2Reads, c2Writes    = make(chan int), make(chan int)
		c1Closing, c2Closing = make(chan struct{}), make(chan struct{})
	)
	_c1 := coordinatedCloser{
		c1, c1Reads, c1Writes, c2Reads,
		c1Closing, c2Closing, make(chan struct{}),
		0, new(sync.Once), nil,
	}
	_c2 := coordinatedCloser{
		c2, c2Reads, c2Writes, c1Reads,
		c2Closing, c1Closing, make(chan struct{}),
		0, new(sync.Once), nil,
	}
	go _c1.watchPeerReads()
	go _c2.watchPeerReads()
	return _c1, _c2
}

func (cc coordinatedCloser) Read(b []byte) (n int, err error) {
	n, err = cc.almostConn.Read(b)
	if n > 0 {
		select {
		case cc.myReads <- n:
			return
		case <-cc.readyToClose:
			return n, net.ErrClosed
		}
	}
	return
}

func (cc coordinatedCloser) Write(b []byte) (n int, err error) {
	atomic.AddInt64(&cc.pendingWrites, 1)
	defer atomic.AddInt64(&cc.pendingWrites, -1)
	select {
	case <-cc.closing:
		return 0, net.ErrClosed
	default:
		n, err = cc.almostConn.Write(b)
		select {
		case cc.myWrites <- n:
			return
		case <-cc.readyToClose:
			return n, net.ErrClosed
		}
	}
}

func (cc coordinatedCloser) watchPeerReads() {
	var myWriteTotal, peerReadTotal int

	readyToClose := func() bool {
		select {
		case <-cc.peerClosing:
			return true
		default:
			return atomic.LoadInt64(&cc.pendingWrites) == 0 && peerReadTotal >= myWriteTotal
		}
	}

	// Turns into a busy loop when closing, but that shouldn't last long.
	for {
		select {
		case n := <-cc.myWrites:
			myWriteTotal += n
		case n := <-cc.peerReads:
			peerReadTotal += n
		case <-cc.closing:
			if readyToClose() {
				close(cc.readyToClose)
				return
			}
		}
	}
}

// When closing, we wait until (a) the peer has read everything we've written or (b) the peer is
// ready to close as well. However, sometimes we want to close and the peer is inactive. The peer
// has no more data to read, but is not going to close until some deferred call is invoked. In this
// case, we need the Close function to unblock and return anyway.
//
// This wait time may seem a little high, but we see long pauses in CI necessitating this.
const closeWaitTime = 10 * time.Second

func (cc coordinatedCloser) Close() error {
	cc.closeOnce.Do(func() {
		timer := time.NewTimer(closeWaitTime)
		close(cc.closing)
		select {
		case <-cc.readyToClose:
			timer.Stop()
		case <-timer.C:
		}
		cc.closeErr = cc.almostConn.Close()
	})
	return cc.closeErr
}
