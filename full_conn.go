package ossh

import (
	"net"
	"os"
	"sync"
	"time"
)

type ioResult struct {
	n   int
	err error
}

// TODO: experiment with net.pipeDeadline: https://golang.org/src/net/pipe.go

// deadline represents a point in time. Users of a deadline can listen to deadline.maybeExpired for
// notifications on when the deadline may have expired. Values on deadline.maybeExpired may be
// stale, so expiration status should always be confirmed with a call to deadline.expired()
type deadline struct {
	sync.Mutex

	t                    time.Time
	maybeExpired, closed chan struct{}
}

// newDeadline creates an unset deadline. Close the input channel to free up all deadline resources.
func newDeadline(closed chan struct{}) *deadline {
	dl := &deadline{
		maybeExpired: make(chan struct{}),
		closed:       closed,
	}
	return dl
}

// set the deadline. Points in the past are valid input and will immediately expire the deadline.
// A zero value for t unsets the deadline. Calls to set create a new goroutine which blocks until
// its value is read off d.maybeExpired. Call d.flushRoutines to free up these goroutines.
func (d *deadline) set(t time.Time) {
	d.Lock()
	d.t = t
	d.Unlock()
	if t.IsZero() {
		return
	}
	go func() {
		select {
		case <-time.After(time.Until(t)):
		case <-d.closed:
			return
		}
		select {
		case d.maybeExpired <- struct{}{}:
		case <-d.closed:
			return
		}
	}()
}

func (d *deadline) expired() bool {
	d.Lock()
	defer d.Unlock()
	if d.t.IsZero() {
		return false
	}
	return time.Now().After(d.t)
}

// Clean up outstanding goroutines. Call this to free up resources when the program is no longer
// actively waiting on d.maybeExpired. After a call to flushRoutines, d.expired should be consulted
// before again waiting on d.maybeExpired.
func (d *deadline) flushRoutines() {
	for {
		select {
		case <-d.maybeExpired:
		default:
			return
		}
	}
}

func (d *deadline) close() {
	d.Lock()
	select {
	case <-d.closed:
	default:
		close(d.closed)
		d.flushRoutines()
	}
	d.Unlock()
}

// Enforces first-in, first-out, single-threaded operations between concurrent goroutines.
type fifoExecutor struct {
	reqs   chan (<-chan struct{})
	closed chan struct{}
}

func newFIFOExecutor() fifoExecutor {
	reqs := make(chan (<-chan struct{}))
	closed := make(chan struct{})
	go func() {
		for {
			select {
			case req := <-reqs:
				<-req
			case <-closed:
				return
			}
		}
	}()
	return fifoExecutor{reqs, closed}
}

// Calls the input function. Functions are invoked one-at-a-time in the order they are received. A
// closed fifoExecutor executes functions immediately, with no ordering or concurrency guarantees.
func (fe fifoExecutor) do(f func()) {
	req := make(chan struct{})
	select {
	case fe.reqs <- req:
	case <-fe.closed:
	}
	f()
	close(req)
}

// Should only be called once.
func (fe fifoExecutor) close() {
	close(fe.closed)
}

// fullConn adds concurrency support and deadline handling to an almostConn. See the almostConn type
// for requirements and assumptions about the behavior of this wrapped connection.
//
// All exported methods are concurrency-safe. All Reads and Writes are single-threaded (but a Read
// can operate concurrently with a Write). All methods behave as defined by the net.Conn interface.
type fullConn struct {
	wrapped almostConn

	// Fields in this block are protected by readLock.
	//
	// buf holds bytes read off the underlying reader, but not yet consumed by callers of Read.
	// pos and n define the portion of buf with unconsumed bytes.
	// readErr, if non-nil, is an unconsumed error returned by the underlying reader at n.
	// readResults is non-nil iff a read is pending in a separate routine.
	buf         []byte
	pos, n      int
	readErr     error
	readResults chan ioResult
	readLock    sync.Mutex

	readDeadline, writeDeadline *deadline

	// writeLock protects access to the Write method.
	// TODO: is this still necessary?
	writeLock sync.Mutex

	// Ensures FIFO, single-threaded access to wrapped.Write.
	writeExecutor fifoExecutor

	// TODO: can we abstract the handshake and close fields?

	// Fields in this block are protected by shakeOnce.
	shakeOnce sync.Once
	shakeErr  error

	// Fields in this block are protected by closeOnce.
	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}

	// We cannot call Handshake and Close concurrently on the wrapped connection. This binary
	// semaphore is used to synchronize calls to both methods.
	handshakeOrCloseSema chan struct{}
}

func newFullConn(conn almostConn) *fullConn {
	closed := make(chan struct{})
	return &fullConn{
		wrapped:              conn,
		readDeadline:         newDeadline(closed),
		writeDeadline:        newDeadline(closed),
		writeExecutor:        newFIFOExecutor(),
		closed:               closed,
		handshakeOrCloseSema: make(chan struct{}, 1),
	}
}

// Read implements net.Conn.Read.
func (drw *fullConn) Read(b []byte) (n int, err error) {
	drw.readLock.Lock()
	defer drw.readLock.Unlock()

	if drw.isClosed() {
		return 0, net.ErrClosed
	}
	if drw.readDeadline.expired() {
		return 0, os.ErrDeadlineExceeded
	}
	if drw.n > 0 {
		// Unconsumed bytes in the buffer.
		n = copy(b, drw.buf[drw.pos:drw.n])
		drw.pos += n
		if drw.n == drw.pos {
			drw.pos, drw.n = 0, 0
			err = drw.readErr
			drw.readErr = nil
		}
		return
	}

	// If no read is active, start one in a new routine.
	if drw.readResults == nil {
		drw.readResults = make(chan ioResult, 1)
		if len(drw.buf) < len(b) {
			drw.buf = make([]byte, len(b))
		}
		go func() {
			n, err := 0, drw.Handshake()
			if err == nil {
				n, err = drw.wrapped.Read(drw.buf[:len(b)])
			}
			// This routine does not hold the lock, but we know that drw.readResults will be valid
			// until we send a value.
			select {
			case drw.readResults <- ioResult{n, err}:
			case <-drw.closed:
			}
		}()
	}

	// We know now that a read is active. Wait for the result.
	defer drw.readDeadline.flushRoutines()
	for {
		select {
		case res := <-drw.readResults:
			n = copy(b, drw.buf[:res.n])
			if n < res.n {
				drw.pos, drw.n, drw.readErr = n, res.n, res.err
			} else {
				err = res.err
			}
			drw.readResults = nil
			return
		case <-drw.readDeadline.maybeExpired:
			if drw.readDeadline.expired() {
				return 0, os.ErrDeadlineExceeded
			}
		case <-drw.closed:
			return 0, net.ErrClosed
		}
	}
}

// Write implements net.Conn.Write.
//
// Unlike Read, Write may return 0, os.ErrDeadlineExceeded, but still successfully occur later. In
// this way, writes are more like fire-and-forget calls. In practice, this is unlikely to be an
// issue as the underlying connection is unlikely to block for long on a write.
func (drw *fullConn) Write(b []byte) (n int, err error) {
	drw.writeLock.Lock()
	defer drw.writeLock.Unlock()

	if drw.isClosed() {
		return 0, net.ErrClosed
	}
	if drw.writeDeadline.expired() {
		return 0, os.ErrDeadlineExceeded
	}

	// Start the write in a new routine. We may return before the write routine does. Thus we cannot
	// assume that b will be valid for the lifetime of the write routine, so we copy b.
	bCopy := make([]byte, len(b))
	copy(bCopy, b)
	writeResultC := make(chan ioResult, 1)
	go func() {
		n, err := 0, drw.Handshake()
		if err == nil {
			drw.writeExecutor.do(func() { n, err = drw.wrapped.Write(bCopy) })
		}
		select {
		case writeResultC <- ioResult{n, err}:
		case <-drw.closed:
		}
	}()

	defer drw.writeDeadline.flushRoutines()
	for {
		select {
		case res := <-writeResultC:
			return res.n, res.err
		case <-drw.writeDeadline.maybeExpired:
			if drw.writeDeadline.expired() {
				return 0, os.ErrDeadlineExceeded
			}
		case <-drw.closed:
			return 0, net.ErrClosed
		}
	}
}

// Handshake initiates the connection if necessary. It is safe to call this function multiple times.
func (drw *fullConn) Handshake() error {
	drw.shakeOnce.Do(func() {
		errC := make(chan error, 1)
		go func() {
			select {
			case drw.handshakeOrCloseSema <- struct{}{}:
				errC <- drw.wrapped.Handshake()
				<-drw.handshakeOrCloseSema
			default:
				// The connection must be closing. Abandon handshake.
			}
		}()
		select {
		case drw.shakeErr = <-errC:
		case <-drw.closed:
			drw.shakeErr = net.ErrClosed
		}
	})
	return drw.shakeErr
}

// Close implements net.Conn.Close. It is safe to call Close multiple times.
func (drw *fullConn) Close() error {
	drw.closeOnce.Do(func() {
		close(drw.closed)
		drw.writeExecutor.close()
		drw.readDeadline.close()
		drw.writeDeadline.close()
		select {
		case drw.handshakeOrCloseSema <- struct{}{}:
			drw.closeErr = drw.wrapped.Close()
			// Retain the semaphore to prevent future handshakes.
		default:
			// Handshake ongoing. Launch a routine to wait and close. In this (likely rare) case, we
			// fib about the connection being completely closed.
			go func() {
				drw.handshakeOrCloseSema <- struct{}{}
				drw.wrapped.Close()
				// Retain the semaphore to prevent future handshakes.
			}()
		}
	})
	return drw.closeErr
}

// LocalAddr implements net.Conn.LocalAddr.
func (drw *fullConn) LocalAddr() net.Addr { return drw.wrapped.LocalAddr() }

// RemoteAddr implements net.Conn.RemoteAddr.
func (drw *fullConn) RemoteAddr() net.Addr { return drw.wrapped.RemoteAddr() }

// SetReadDeadline implements net.Conn.SetReadDeadline.
func (drw *fullConn) SetReadDeadline(t time.Time) error { drw.readDeadline.set(t); return nil }

// SetWriteDeadline implements net.Conn.SetWriteDeadline.
func (drw *fullConn) SetWriteDeadline(t time.Time) error { drw.writeDeadline.set(t); return nil }

// SetDeadline implements net.Conn.SetDeadline.
func (drw *fullConn) SetDeadline(t time.Time) error {
	drw.readDeadline.set(t)
	drw.writeDeadline.set(t)
	return nil
}

func (drw *fullConn) isClosed() bool {
	select {
	case <-drw.closed:
		return true
	default:
		return false
	}
}
