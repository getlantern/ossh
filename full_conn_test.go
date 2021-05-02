package ossh

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/getlantern/ossh/internal/nettest"

	"github.com/stretchr/testify/require"
)

func TestDeadline(t *testing.T) {
	d := newDeadline(make(chan struct{}))
	defer close(d.closed)

	d.set(time.Now())
	<-d.maybeExpired
	require.True(t, d.expired())

	d.set(time.Now())
	d.set(time.Now().Add(time.Hour))
	<-d.maybeExpired
	require.False(t, d.expired())
	select {
	case <-d.maybeExpired:
		t.Fatal("unexpected notification")
	default:
	}

	d.flushRoutines()
	d.set(time.Now().Add(time.Hour))
	d.set(time.Now())
	<-d.maybeExpired
	require.True(t, d.expired())

	d.flushRoutines()
	d.set(time.Now().Add(-1 * time.Hour))
	<-d.maybeExpired
	require.True(t, d.expired())
}

func TestFIFOExecutor(t *testing.T) {
	t.Parallel()

	var (
		wg          = new(sync.WaitGroup)
		fe          = newFIFOExecutor()
		ints        = []int{}
		numRoutines = 10
		closeOnce   = new(sync.Once)
	)
	defer closeOnce.Do(fe.close)

	// Routines sleep an increasing amount of time to ensure they get in line in the expected order.
	sleep := func(routineNum int) { time.Sleep(10 * time.Duration(routineNum) * time.Millisecond) }

	// Grab a spot in line and sleep longer than any other routine to ensure the line builds up.
	go fe.do(func() { sleep(numRoutines + 1) })

	for i := 0; i < numRoutines; i++ {
		wg.Add(1)
		go func(_i int) {
			sleep(_i)
			fe.do(func() { ints = append(ints, _i) })
			wg.Done()
		}(i)
	}
	wg.Wait()
	for i := 0; i < numRoutines; i++ {
		require.Equal(t, i, ints[i])
	}

	// A closed executor should execute functions without blocking.
	closeOnce.Do(fe.close)
	executed := false
	fe.do(func() { executed = true })
	require.True(t, executed)
}

func TestFullConn(t *testing.T) {
	// The time allowed for concurrent goroutines to get started and into the actual important bits.
	// Empirically, this seems to take about 300 ns on a modern MacBook Pro.
	const handshakeStartTime = 10 * time.Millisecond

	var (
		inThePast  = time.Now().Add(-1 * time.Hour)
		noDeadline = time.Time{}
	)

	// Tests I/O, deadline support, net.Conn adherence, and data races.
	nettest.TestConn(t, makeFullConnPipe)

	t.Run("CloseThenHandshake", func(t *testing.T) {
		c1, c2, stop, err := makeFullConnPipe()
		require.NoError(t, err)
		defer stop()

		require.NoError(t, c1.Close())
		require.NoError(t, c2.Close())
		require.ErrorIs(t, c1.(*fullConn).Handshake(), net.ErrClosed)
		require.ErrorIs(t, c2.(*fullConn).Handshake(), net.ErrClosed)
	})
	t.Run("CloseOneThenHandshake", func(t *testing.T) {
		c1, c2, stop, err := makeFullConnPipe()
		require.NoError(t, err)
		defer stop()

		require.NoError(t, c1.Close())
		require.ErrorIs(t, c1.(*fullConn).Handshake(), net.ErrClosed)
		require.Error(t, c2.(*fullConn).Handshake())
	})
	t.Run("CloseDuringHandshake", func(t *testing.T) {
		c1, c2, stop, err := makeFullConnPipe()
		require.NoError(t, err)
		defer stop()

		errC := make(chan error)
		go func() { errC <- c1.(*fullConn).Handshake() }()
		// TODO: if we create a 'fullConn.shakeStarted' channel, make use of it here
		time.Sleep(handshakeStartTime)
		require.NoError(t, c1.Close())
		require.ErrorIs(t, <-errC, net.ErrClosed)
		require.Error(t, c2.(*fullConn).Handshake())
	})
	t.Run("TimeoutThenHandshake", func(t *testing.T) {
		c1, c2, stop, err := makeFullConnPipe()
		require.NoError(t, err)
		defer stop()

		require.NoError(t, c1.SetDeadline(inThePast))
		require.ErrorIs(t, c1.(*fullConn).Handshake(), os.ErrDeadlineExceeded)

		// Should be able to recover.
		errC := make(chan error)
		require.NoError(t, c1.SetDeadline(noDeadline))
		go func() { errC <- c2.(*fullConn).Handshake() }()
		require.NoError(t, c1.(*fullConn).Handshake())
	})
	t.Run("TimeoutDuringHandshake", func(t *testing.T) {
		c1, c2, stop, err := makeFullConnPipe()
		require.NoError(t, err)
		defer stop()

		c1.SetDeadline(time.Now().Add(handshakeStartTime))

		errC := make(chan error)
		go func() { errC <- c1.(*fullConn).Handshake() }()
		require.ErrorIs(t, <-errC, os.ErrDeadlineExceeded)

		// Should be able to recover.
		require.NoError(t, c1.SetDeadline(noDeadline))
		go func() { errC <- c2.(*fullConn).Handshake() }()
		require.NoError(t, c1.(*fullConn).Handshake())
	})
}

// makeFullConnPipe implements nettest.MakePipe.
func makeFullConnPipe() (c1, c2 net.Conn, stop func(), err error) {
	_c1, _c2 := almostConnPipe()
	c1 = newFullConn(_c1)
	c2 = newFullConn(_c2)
	return c1, c2, func() { c1.Close(); c2.Close() }, nil
}

func almostConnPipe() (almostConn, almostConn) {
	rx1, tx1 := io.Pipe()
	rx2, tx2 := io.Pipe()
	tac1 := newTestAlmostConn(rx1, tx2, nil)
	tac2 := newTestAlmostConn(rx2, tx1, nil)
	return tac1, tac2
}

type fakeAddr string

func (addr fakeAddr) Network() string { return "fake network" }
func (addr fakeAddr) String() string  { return "fake address: " + string(addr) }

// testAlmostConn implements almostConn for testing fullConn's implementation of the net.Conn
// interface. In particular, this type enforces the minimum required by almostConn (for example,
// concurrent Reads and Writes will result in errors.).
type testAlmostConn struct {
	rx            io.Reader
	tx            io.WriteCloser
	closed        chan struct{}
	handshakeWait <-chan error
	handshakeDone chan struct{}

	readSema, writeSema chan struct{}

	// Optionally set this after initialization. Otherwise it is initialized to the type name.
	name string
}

// The Handshake will block until an error is sent on hsWait (or the channel is closed). The error
// sent is returned by Handshake. If hsWait is nil, Handshake does not block and returns nil.
func newTestAlmostConn(rx io.Reader, tx io.WriteCloser, hsWait <-chan error) *testAlmostConn {
	if hsWait == nil {
		closedChannel := make(chan error)
		close(closedChannel)
		hsWait = closedChannel
	}
	conn := &testAlmostConn{
		rx: rx, tx: tx,
		closed:        make(chan struct{}),
		handshakeWait: hsWait,
		handshakeDone: make(chan struct{}),

		readSema:  make(chan struct{}, 1),
		writeSema: make(chan struct{}, 1),
		name:      "testAlmostConn",
	}
	return conn
}

func (conn *testAlmostConn) Handshake() error {
	err := <-conn.handshakeWait
	close(conn.handshakeDone)
	return err
}

func (conn *testAlmostConn) Read(b []byte) (n int, err error) {
	select {
	case conn.readSema <- struct{}{}:
	default:
		return 0, errors.New("illegal concurrent read")
	}
	select {
	case <-conn.handshakeDone:
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
	default:
		return 0, errors.New("illegal write before handshake")
	}
	defer func() { <-conn.writeSema }()
	return conn.tx.Write(b)
}

func (conn *testAlmostConn) Close() error {
	select {
	case <-conn.closed:
		return errors.New("illegal extra close")
	default:
		conn.tx.Close()
		return nil
	}
}

func (conn *testAlmostConn) LocalAddr() net.Addr  { return fakeAddr(conn.name) }
func (conn *testAlmostConn) RemoteAddr() net.Addr { return fakeAddr(conn.name) }
