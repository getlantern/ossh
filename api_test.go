package ossh

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/getlantern/ossh/internal/nettest"
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

	var _server, _client Conn
	for i := 0; i < 2; i++ {
		select {
		case res := <-serverResC:
			if res.err != nil {
				return nil, nil, nil, fmt.Errorf("failed to init server-side: %w", err)
			}
			_server = res.conn
		case res := <-clientResC:
			if res.err != nil {
				return nil, nil, nil, fmt.Errorf("failed to init client-side: %w", err)
			}
			_client = res.conn
		}
	}
	client, server := coordinateClose(_client, _server)
	return client, server, func() { client.Close(); server.Close() }, nil
}

func TestConn(t *testing.T) {
	nettest.TestConn(t, makePipe)
}

// debugging
func TestBasicIO(t *testing.T) {
	c1, c2, stop, err := makePipe()
	require.NoError(t, err)
	defer stop()

	want := make([]byte, 1<<20)
	mathrand.New(mathrand.NewSource(0)).Read(want)

	dataCh := make(chan []byte)
	go func() {
		_, err := c1.Write(want)
		if err != nil {
			t.Errorf("unexpected c1.Write error: %v", err)
		}
		if err := c1.Close(); err != nil {
			t.Errorf("unexpected c1.Close error: %v", err)
		}
	}()

	go func() {
		buf := new(bytes.Buffer)
		if _, err := io.Copy(buf, c2); err != nil {
			t.Errorf("unexpected c2.Read error: %v", err)
		}
		if err := c2.Close(); err != nil {
			t.Errorf("unexpected c2.Close error: %v", err)
		}
		dataCh <- buf.Bytes()
	}()

	if got := <-dataCh; !bytes.Equal(got, want) {
		t.Error("transmitted data differs")
		fmt.Printf("len(want): %d; len(got): %d\n", len(want), len(got))
	}
}

// The ssh.Channel type underlying our Conn implementations has a quirk in which reads may fail
// early after the peer has closed the transport: https://github.com/golang/go/issues/45912.
// This is unlikely to present much of an issue in production use cases, but causes some of the
// tests provided by the nettest package to fail. We work around this by coordinating closes.
type coordinatedCloser struct {
	Conn

	myReads, myWrites, peerReads chan int
	closing, readyToClose        chan struct{}
	pendingWrites                int64
	closeOnce                    *sync.Once
	closeErr                     error
}

func coordinateClose(c1, c2 Conn) (Conn, Conn) {
	var (
		c1Reads, c1Writes = make(chan int), make(chan int)
		c2Reads, c2Writes = make(chan int), make(chan int)
	)
	_c1 := coordinatedCloser{
		c1, c1Reads, c1Writes, c2Reads,
		make(chan struct{}), make(chan struct{}),
		0, new(sync.Once), nil,
	}
	_c2 := coordinatedCloser{
		c2, c2Reads, c2Writes, c1Reads,
		make(chan struct{}), make(chan struct{}),
		0, new(sync.Once), nil,
	}
	go _c1.watchPeerReads()
	go _c2.watchPeerReads()
	return _c1, _c2
}

func (cc coordinatedCloser) Read(b []byte) (n int, err error) {
	n, err = cc.Conn.Read(b)
	if n > 0 {
		cc.myReads <- n
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
		n, err = cc.Conn.Write(b)
		cc.myWrites <- n
		return
	}
}

func (cc coordinatedCloser) watchPeerReads() {
	var myWriteTotal, peerReadTotal int
	// Turns into a busy loop when closing, but that shouldn't last long.
	for {
		select {
		case n := <-cc.myWrites:
			myWriteTotal += n
		case n := <-cc.peerReads:
			peerReadTotal += n
		case <-cc.closing:
			if atomic.LoadInt64(&cc.pendingWrites) == 0 && peerReadTotal >= myWriteTotal {
				close(cc.readyToClose)
				return
			}
		}
	}
}

func (cc coordinatedCloser) Close() error {
	cc.closeOnce.Do(func() {
		close(cc.closing)
		<-cc.readyToClose
		cc.closeErr = cc.Conn.Close()
	})
	return cc.closeErr
}
