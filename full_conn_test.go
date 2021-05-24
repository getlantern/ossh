package ossh

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlantern/nettest"

	"github.com/stretchr/testify/require"
)

// The time allowed for concurrent goroutines to get started and into the actual important bits.
// Empirically, this seems to take about 400 ns on a modern MacBook Pro and 65 µs in CircleCI.
const goroutineStartTime = 10 * time.Millisecond

var (
	inThePast  = time.Now().Add(-1 * time.Hour)
	noDeadline = time.Time{}
)

// debugging
var (
	avgDelay time.Duration
	delayC   = make(chan time.Duration)
)

func init() {
	go func() {
		lastN := 5
		delays := []time.Duration{}
		for delay := range delayC {
			delays = append(delays, delay)
			if len(delays)%lastN == 0 {
				var sum, max time.Duration
				for i := len(delays) - lastN; i < len(delays)-1; i++ {
					sum += delays[i]
				}
				for _, d := range delays {
					if d > max {
						max = d
					}
				}
				fmt.Printf("average delay of last %d runs: %v\n", lastN, sum/time.Duration(lastN))
				fmt.Printf("max delay seen: %v\n", max)
			}
		}
	}()
}

func TestGoroutineStartTime(t *testing.T) {
	started := make(chan time.Time)
	launched := time.Now()
	go func() {
		started <- time.Now()
	}()
	delayC <- (<-started).Sub(launched)
}

func TestFullConn(t *testing.T) {
	// Tests I/O, deadline support, net.Conn adherence, and data races.
	nettest.TestConn(t, makeFullConnPipe)

	// Tests calls made before and during the handshake.
	testHandshake(t, makeFullConnPipe)

	// Tests the case in which part of a read ends up buffered on the connection.
	// t.Run("BufferedRead", testBufferedRead)
}

type handshaker interface {
	Handshake() error
}

// Assumes the net.Conn instances returned by mp implement the handshaker interface.
func testHandshake(t *testing.T, mp nettest.MakePipe) {
	t.Run("CloseThenHandshake", func(t *testing.T) {
		t.Parallel()

		c1, c2, stop, err := mp()
		require.NoError(t, err)
		defer stop()

		require.NoError(t, c1.Close())
		require.NoError(t, c2.Close())
		require.ErrorIs(t, c1.(handshaker).Handshake(), net.ErrClosed)
		require.ErrorIs(t, c2.(handshaker).Handshake(), net.ErrClosed)
	})
	t.Run("CloseOneThenHandshake", func(t *testing.T) {
		t.Parallel()

		c1, c2, stop, err := mp()
		require.NoError(t, err)
		defer stop()

		require.NoError(t, c1.Close())
		require.ErrorIs(t, c1.(handshaker).Handshake(), net.ErrClosed)
		require.Error(t, c2.(handshaker).Handshake())
	})
	t.Run("CloseDuringHandshake", func(t *testing.T) {
		t.Parallel()

		c1, c2, stop, err := mp()
		require.NoError(t, err)
		defer stop()

		errC := make(chan error)
		go func() { errC <- c1.(handshaker).Handshake() }()
		time.Sleep(goroutineStartTime)
		require.NoError(t, c1.Close())
		require.ErrorIs(t, <-errC, net.ErrClosed)

		// Though c1's Handshake call returned, the spawned handshake routine is still waiting in
		// the background. There is no way to kill this routine as the underlying transport does not
		// support I/O cancellation. We initiate a handshake from c2 to avoid leaking the goroutine.
		c2.(handshaker).Handshake()
	})
	t.Run("TimeoutThenHandshake", func(t *testing.T) {
		t.Parallel()

		c1, c2, stop, err := mp()
		require.NoError(t, err)
		defer stop()

		require.NoError(t, c1.SetDeadline(inThePast))

		// Initiate the handshake with a Read. Handshake does not itself support deadlines.
		_, err = c1.Read(make([]byte, 10))
		require.ErrorIs(t, err, os.ErrDeadlineExceeded)

		// Should be able to recover.
		errC := make(chan error)
		require.NoError(t, c1.SetDeadline(noDeadline))
		go func() { errC <- c2.(handshaker).Handshake() }()
		require.NoError(t, c1.(handshaker).Handshake())
		require.NoError(t, <-errC)
	})
	t.Run("TimeoutDuringHandshake", func(t *testing.T) {
		t.Parallel()

		c1, c2, stop, err := mp()
		require.NoError(t, err)
		defer stop()

		c1.SetDeadline(time.Now().Add(goroutineStartTime))

		// Initiate the handshake with a Read. Handshake does not itself support deadlines.
		errC := make(chan error)
		go func() {
			_, err := c1.Read(make([]byte, 10))
			errC <- err
		}()
		require.ErrorIs(t, <-errC, os.ErrDeadlineExceeded)

		// Should be able to recover.
		require.NoError(t, c1.SetDeadline(noDeadline))
		go func() { errC <- c2.(handshaker).Handshake() }()
		require.NoError(t, c1.(handshaker).Handshake())
		require.NoError(t, <-errC)
	})
	t.Run("AddrPreHandshake", func(t *testing.T) {
		t.Parallel()

		c1, c2, stop, err := mp()
		require.NoError(t, err)
		defer stop()

		require.NotNil(t, c1.LocalAddr())
		require.NotNil(t, c1.RemoteAddr())
		require.NotNil(t, c2.LocalAddr())
		require.NotNil(t, c2.RemoteAddr())
	})
	t.Run("AddrDuringHandshake", func(t *testing.T) {
		t.Parallel()

		c1, c2, stop, err := mp()
		require.NoError(t, err)
		defer stop()

		errC := make(chan error)
		go func() { errC <- c1.(handshaker).Handshake() }()
		time.Sleep(goroutineStartTime)

		require.NotNil(t, c1.LocalAddr())
		require.NotNil(t, c1.RemoteAddr())
		require.NotNil(t, c2.LocalAddr())
		require.NotNil(t, c2.RemoteAddr())

		require.NoError(t, c2.(handshaker).Handshake())
		require.NoError(t, <-errC)
	})
	t.Run("AddrPostHandshake", func(t *testing.T) {
		t.Parallel()

		c1, c2, stop, err := mp()
		require.NoError(t, err)
		defer stop()

		errC := make(chan error)
		go func() { errC <- c1.(handshaker).Handshake() }()
		time.Sleep(goroutineStartTime)
		require.NoError(t, c2.(handshaker).Handshake())
		require.NoError(t, <-errC)

		require.NotNil(t, c1.LocalAddr())
		require.NotNil(t, c1.RemoteAddr())
		require.NotNil(t, c2.LocalAddr())
		require.NotNil(t, c2.RemoteAddr())
	})
}

// Makes some assumptions about the implementation fullConn.Read.
// func testBufferedRead(t *testing.T) {
// 	t.Parallel()

// 	// debugging
// 	var (
// 		launched, started time.Time
// 		startedC          = make(chan time.Time, 1)
// 	)

// 	const bufferSize = 1024

// 	c1, c2, stop, err := makeFullConnPipe()
// 	require.NoError(t, err)
// 	defer stop()

// 	// We force buffered data by:
// 	// 	(1) Starting a Read on c1.
// 	//	(2) Cancelling the Read.
// 	//	(3) Writing data to c2.

// 	readResult := make(chan ioResult)
// 	launched = time.Now()
// 	go func() {
// 		startedC <- time.Now()
// 		n, err := c1.Read(make([]byte, 1024))
// 		readResult <- ioResult{n, err}
// 	}()

// 	started = <-startedC
// 	delayC <- started.Sub(launched)

// 	time.Sleep(goroutineStartTime)
// 	c1.SetDeadline(inThePast)

// 	res := <-readResult
// 	require.ErrorIs(t, res.err, os.ErrDeadlineExceeded)
// 	require.Zero(t, res.n)

// 	data := make([]byte, bufferSize)
// 	_, err = rand.Read(data)
// 	require.NoError(t, err)

// 	_, err = c2.Write(data)
// 	require.NoError(t, err)

// 	// Now we try to read the buffered data.

// 	readBuf := make([]byte, bufferSize*2)
// 	c1.SetDeadline(noDeadline)

// 	// Try reading less than what is currently buffered.
// 	n, err := c1.Read(readBuf[:bufferSize/2])
// 	require.NoError(t, err)
// 	require.Equal(t, bufferSize/2, n)

// 	// Try reading more than what is currently buffered.
// 	n2, err := c1.Read(readBuf[bufferSize/2:])
// 	require.NoError(t, err)
// 	require.Equal(t, bufferSize-bufferSize/2, n2)
// 	n += n2

// 	require.Equal(t, data, readBuf[:n])

// 	// Ensure we can still receive new data.
// 	go io.Copy(c2, c2) // echo everything written to c1

// 	msg := []byte("hello")
// 	_, err = c1.Write(msg)
// 	require.NoError(t, err)

// 	buf := make([]byte, len(msg)*2)
// 	n, err = c1.Read(buf)
// 	require.NoError(t, err)
// 	require.Equal(t, string(msg), string(buf[:n]))
// }

// makeFullConnPipe implements nettest.MakePipe.
func makeFullConnPipe() (c1, c2 net.Conn, stop func(), err error) {
	_c1, _c2 := almostConnPipe()
	c1 = newFullConn(_c1)
	c2 = newFullConn(_c2)
	return c1, c2, func() { c1.Close(); c2.Close() }, nil
}

type fakeAddr string

func (addr fakeAddr) Network() string { return "fake network" }
func (addr fakeAddr) String() string  { return "fake address: " + string(addr) }

var testAlmostConnNumber int64

// testAlmostConn implements almostConn for testing fullConn's implementation of the net.Conn
// interface. In particular, this type enforces the strictest requirements of almostConn (for
// example, concurrent Reads and Writes will result in errors).
type testAlmostConn struct {
	rx                           io.Reader
	tx                           io.WriteCloser
	closed, peerClosed           chan struct{}
	handshaking, peerHandshaking chan struct{}
	handshakeDone                chan struct{}

	// Zero iff no error or not yet complete.
	handshakeErr int64

	readSema, writeSema chan struct{}

	// Optionally set this after initialization. Otherwise the connection is assigned an identifier.
	name string
}

func almostConnPipe() (almostConn, almostConn) {
	var (
		rx1, tx1           = io.Pipe()
		rx2, tx2           = io.Pipe()
		closed1, closed2   = make(chan struct{}), make(chan struct{})
		shaking1, shaking2 = make(chan struct{}), make(chan struct{})
		id1                = int(atomic.AddInt64(&testAlmostConnNumber, 1))
		id2                = int(atomic.AddInt64(&testAlmostConnNumber, 1))
	)
	c1 := &testAlmostConn{
		rx: rx1, tx: tx2,
		closed:          closed1,
		peerClosed:      closed2,
		handshaking:     shaking1,
		peerHandshaking: shaking2,
		handshakeDone:   make(chan struct{}),
		readSema:        make(chan struct{}, 1),
		writeSema:       make(chan struct{}, 1),
		name:            "testAlmostConn" + strconv.Itoa(id1),
	}
	c2 := &testAlmostConn{
		rx: rx2, tx: tx1,
		closed:          closed2,
		peerClosed:      closed1,
		handshaking:     shaking2,
		peerHandshaking: shaking1,
		handshakeDone:   make(chan struct{}),
		readSema:        make(chan struct{}, 1),
		writeSema:       make(chan struct{}, 1),
		name:            "testAlmostConn" + strconv.Itoa(id2),
	}
	return c1, c2
}

func (conn *testAlmostConn) Handshake() error {
	defer close(conn.handshakeDone)
	err := conn.handshake()
	if err != nil {
		atomic.StoreInt64(&conn.handshakeErr, 1)
	}
	return err
}

func (conn *testAlmostConn) handshake() error {
	select {
	case <-conn.handshaking:
		return errors.New("illegal second handshake")
	default:
		close(conn.handshaking)
	}

	// Block until one of the following:
	select {
	case <-conn.closed:
	case <-conn.peerClosed:
	case <-conn.peerHandshaking:
	}

	// Have we closed?
	select {
	case <-conn.closed:
		return net.ErrClosed
	default:
		// Has the peer closed?
		select {
		case <-conn.peerClosed:
			return io.EOF
		default:
			// Nobody has closed and the peer must be handshaking too.
			return nil
		}
	}
}

func (conn *testAlmostConn) handshakeReturnedError() bool {
	return atomic.LoadInt64(&conn.handshakeErr) == 1
}

func (conn *testAlmostConn) Read(b []byte) (n int, err error) {
	select {
	case conn.readSema <- struct{}{}:
	default:
		return 0, errors.New("illegal concurrent read")
	}
	select {
	case <-conn.handshakeDone:
		if conn.handshakeReturnedError() {
			return 0, errors.New("illegal read after handshake error")
		}
	default:
		return 0, errors.New("illegal read before handshake")
	}
	defer func() { <-conn.readSema }()
	return conn.rx.Read(b)
}

func (conn *testAlmostConn) Write(b []byte) (n int, err error) {
	select {
	case conn.writeSema <- struct{}{}:
	default:
		return 0, errors.New("illegal concurrent write")
	}
	select {
	case <-conn.handshakeDone:
		if conn.handshakeReturnedError() {
			return 0, errors.New("illegal write after handshake error")
		}
	default:
		return 0, errors.New("illegal write before handshake")
	}
	defer func() { <-conn.writeSema }()
	return conn.tx.Write(b)
}

func (conn *testAlmostConn) Close() error {
	// This channel will be closed iff the handshake overlaps this function call.
	var handshakeStarted <-chan struct{}

	select {
	case <-conn.handshakeDone:
		if conn.handshakeReturnedError() {
			return errors.New("illegal close after handshake error")
		}
		// Handshake is already done; create a channel which never closes.
		handshakeStarted = make(chan struct{})
	default:
		handshakeStarted = conn.handshaking
	}

	conn.tx.Close()
	select {
	case <-conn.closed:
		return errors.New("illegal extra close")
	case <-handshakeStarted:
		return errors.New("illegal close during handshake")
	default:
		close(conn.closed)
		return nil
	}
}

func (conn *testAlmostConn) LocalAddr() net.Addr  { return fakeAddr(conn.name) }
func (conn *testAlmostConn) RemoteAddr() net.Addr { return fakeAddr(conn.name) }
