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

// Assumes nothing is ever sent on c.
func isClosedChan(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// deadline is an abstraction for handling timeouts. This code is taken from the pipeDeadline type
// defined in https://golang.org/src/net/pipe.go.
type deadline struct {
	mu     sync.Mutex // Guards timer and cancel
	timer  *time.Timer
	cancel chan struct{} // Must be non-nil
}

func newDeadline() deadline {
	return deadline{cancel: make(chan struct{})}
}

// set sets the point in time when the deadline will time out.
// A timeout event is signaled by closing the channel returned by waiter.
// Once a timeout has occurred, the deadline can be refreshed by specifying a
// t value in the future.
//
// A zero value for t prevents timeout.
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // Wait for the timer callback to finish and close cancel
	}
	d.timer = nil

	// Time is zero, then there is no deadline.
	closed := isClosedChan(d.cancel)
	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}

	// Time in the future, setup a timer to cancel in the future.
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		d.timer = time.AfterFunc(dur, func() {
			close(d.cancel)
		})
		return
	}

	// Time in the past, so close immediately.
	if !closed {
		close(d.cancel)
	}
}

// wait returns a channel that is closed when the deadline is exceeded.
func (d *deadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

// This function was added by us; it was not ported from https://golang.org/src/net/pipe.go.
func (d *deadline) expired() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return isClosedChan(d.cancel)
}

// close the deadline. Note that this does not close the channel returned by wait.
// This function was added by us; it was not ported from https://golang.org/src/net/pipe.go.
func (d *deadline) close() {
	d.mu.Lock()
	if d.timer != nil {
		d.timer.Stop()
	}
	d.mu.Unlock()
}

// Enforces exclusive and first-in, first-out operations between concurrent goroutines.
type fifoScheduler struct {
	reqs   chan func()
	closed chan struct{}
}

func newFIFOScheduler() fifoScheduler {
	fs := fifoScheduler{
		make(chan func()),
		make(chan struct{}),
	}
	go fs.run()
	return fs
}

func (fs fifoScheduler) run() {
	// The first element of queue is the currently executing function.
	// The 'bell' is rung when we are ready to execute the next function.
	queue := []func(){}
	bell := make(chan struct{}, 1)

	exec := func(f func()) {
		f()
		bell <- struct{}{}
	}

	// n.b. Outstanding functions in the queue are dropped and never executed when fs is closed.
	for {
		select {
		case req := <-fs.reqs:
			if fs.isClosed() {
				return
			}
			queue = append(queue, req)
			if len(queue) == 1 {
				// No currently executing function.
				go exec(req)
			}

		case <-bell:
			if fs.isClosed() {
				return
			}
			if len(queue) <= 1 {
				// The only remaining function just finished. Deregister and wait for more.
				queue = []func(){}
				continue
			}
			go exec(queue[1])
			queue = queue[1:]

		case <-fs.closed:
			return
		}
	}
}

// Schedules the input function. Functions are invoked one-at-a-time in the order they are received.
// If fs is already closed or closed before f is called, then f will never be invoked.
func (fs fifoScheduler) schedule(f func()) {
	select {
	case fs.reqs <- f:
	case <-fs.closed:
	}
}

// Should only be called once. Pending functions (passed to schedule) will never be invoked.
func (fs fifoScheduler) close() {
	close(fs.closed)
}

func (fs fifoScheduler) isClosed() bool {
	return isClosedChan(fs.closed)
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

	readDeadline, writeDeadline deadline

	// Ensures exclusive, FIFO access to wrapped.Write.
	writeScheduler fifoScheduler

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
	return &fullConn{
		wrapped:              conn,
		readDeadline:         newDeadline(),
		writeDeadline:        newDeadline(),
		writeScheduler:       newFIFOScheduler(),
		closed:               make(chan struct{}),
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
		if drw.pos == drw.n {
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
			drw.readResults <- ioResult{n, err}
		}()
	}

	// We know now that a read is active. Wait for the result.
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
	case <-drw.readDeadline.wait():
		return 0, os.ErrDeadlineExceeded
	case <-drw.closed:
		return 0, net.ErrClosed
	}
}

// Write implements net.Conn.Write.
//
// Unlike Read, Write may return 0, os.ErrDeadlineExceeded, but still successfully occur later. In
// this way, writes are more like fire-and-forget calls. In practice, this is unlikely to be an
// issue as the underlying connection is unlikely to block for long on a write.
func (drw *fullConn) Write(b []byte) (n int, err error) {
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
	drw.writeScheduler.schedule(func() {
		n, err := 0, drw.Handshake()
		if err == nil {
			n, err = drw.wrapped.Write(bCopy)
		}
		writeResultC <- ioResult{n, err}
	})

	select {
	case res := <-writeResultC:
		return res.n, res.err
	case <-drw.writeDeadline.wait():
		return 0, os.ErrDeadlineExceeded
	case <-drw.closed:
		return 0, net.ErrClosed
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
		drw.writeScheduler.close()
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
	return isClosedChan(drw.closed)
}
