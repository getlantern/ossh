package ossh

import (
	"bytes"
	"errors"
	"io"
	"net"
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
	)
	defer close(fe)

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
}

func TestFullConn(t *testing.T) {
	// TODO: go test -race -count=1000 complains "limit on 8128 simultaneously alive goroutines is exceeded"
	// Look into a possible memory leak (maybe deadline.set routines?). Once this is fixed, use
	// almostConnPipe over net.Pipe in makeFullConnPipe.

	nettest.TestConn(t, makeFullConnPipe)

	// TODO: add tests for handshake operations (Close-Then-Handshake, Close-During-Handshake, etc.)
}

// makeFullConnPipe implements nettest.MakePipe.
func makeFullConnPipe() (c1, c2 net.Conn, stop func(), err error) {
	// TODO: we shouldn't use net.Pipe here as the resulting connections are more robust than defined by almostConn
	_c1, _c2 := net.Pipe()
	c1 = newFullConn(noOpHandshaker{_c1})
	c2 = newFullConn(noOpHandshaker{_c2})
	return c1, c2, func() { c1.Close(); c2.Close() }, nil
}

type noOpHandshaker struct{ net.Conn }

func (noh noOpHandshaker) Handshake() error { return nil }

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
	tx            io.Writer
	closed        chan struct{}
	handshakeWait <-chan error
	handshakeDone chan struct{}

	readSema, writeSema chan struct{}

	// Optionally set this after initialization. Otherwise, it is initialized to the type name.
	name string
}

// The Handshake will block until an error is sent on hsWait (or the channel is closed). The error
// sent is returned by Handshake. If hsWait is nil, Handshake does not block and returns nil.
func newTestAlmostConn(rx io.Reader, tx io.Writer, hsWait <-chan error) *testAlmostConn {
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
	case <-conn.readSema:
	default:
		return 0, errors.New("illegal concurrent read")
	}
	select {
	case <-conn.handshakeDone:
	default:
		return 0, errors.New("illegal read before handshake")
	}
	defer func() { conn.readSema <- struct{}{} }()
	return conn.rx.Read(b)
}

func (conn *testAlmostConn) Write(b []byte) (n int, err error) {
	select {
	case <-conn.writeSema:
	default:
		return 0, errors.New("illegal concurrent write")
	}
	select {
	case <-conn.handshakeDone:
	default:
		return 0, errors.New("illegal write before handshake")
	}
	defer func() { conn.writeSema <- struct{}{} }()
	return conn.rx.Read(b)
}

func (conn *testAlmostConn) Close() error {
	select {
	case <-conn.closed:
		return errors.New("illegal extra close")
	}
}

func (conn *testAlmostConn) LocalAddr() net.Addr  { return fakeAddr(conn.name) }
func (conn *testAlmostConn) RemoteAddr() net.Addr { return fakeAddr(conn.name) }

// debugging - taken from nettest
//
// resyncConn resynchronizes the connection into a sane state.
// It assumes that everything written into c is echoed back to itself.
// It assumes that 0xff is not currently on the wire or in the read buffer.
func resyncConn(t *testing.T, c net.Conn) {
	t.Helper()

	neverTimeout := time.Time{}
	c.SetDeadline(neverTimeout)
	errCh := make(chan error)
	go func() {
		_, err := c.Write([]byte{0xff})
		errCh <- err
	}()
	buf := make([]byte, 1024)
	for {
		n, err := c.Read(buf)
		if n > 0 && bytes.IndexByte(buf[:n], 0xff) == n-1 {
			break
		}
		if err != nil {
			t.Errorf("unexpected Read error: %v", err)
			break
		}
	}
	if err := <-errCh; err != nil {
		t.Errorf("unexpected Write error: %v", err)
	}
}
