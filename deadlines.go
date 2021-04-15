package ossh

import (
	"io"
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

// deadlineReadWriter is used to add deadline support to an io.ReadWriteCloser. The intended use
// case is in a net.Conn and some assumptions are made to this effect. One such assumption is that
// the underlying ReadWriteCloser is a network transport and unlikely to block for long on Writes.
//
// A consequence of using the deadlineReadWriter is that Reads and Writes will be single-threaded.
//
// All exported methods are concurrency-safe.
type deadlineReadWriter struct {
	io.ReadWriteCloser

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

	// writeLock protects access to the Write method. This is actually not strictly necessary, but
	// our use case (with an obfuscator.ObfuscatedSSHConn as the underlying ReadWriter) requires
	// synchronization of Reads and Writes. Since we need a readLock, we add a writeLock as well for
	// convenience.
	writeLock sync.Mutex

	// Fields in this block are protected by closeOnce.
	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}
}

func addDeadlineSupport(rwc io.ReadWriteCloser) *deadlineReadWriter {
	closed := make(chan struct{})
	return &deadlineReadWriter{
		ReadWriteCloser: rwc,
		readDeadline:    newDeadline(closed),
		writeDeadline:   newDeadline(closed),
		closed:          closed,
	}
}

// Read implements net.Conn.Read.
func (drw *deadlineReadWriter) Read(b []byte) (n int, err error) {
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
			// This routine does not hold the lock, but we know that drw.readResults will be valid
			// until we send a value.
			n, err := drw.ReadWriteCloser.Read(drw.buf[:len(b)])
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
// issue as the underlying transport is unlikely to block for long on a write.
func (drw *deadlineReadWriter) Write(b []byte) (n int, err error) {
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
		n, err := drw.ReadWriteCloser.Write(bCopy)
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

// Close implements net.Conn.Close. It is safe to call Close multiple times.
func (drw *deadlineReadWriter) Close() error {
	drw.closeOnce.Do(func() {
		close(drw.closed)
		drw.closeErr = drw.ReadWriteCloser.Close()
	})
	return drw.closeErr
}

// SetReadDeadline implements net.Conn.SetReadDeadline.
func (drw *deadlineReadWriter) SetReadDeadline(t time.Time) error {
	drw.readDeadline.set(t)
	return nil
}

// SetWriteDeadline implements net.Conn.SetWriteDeadline.
func (drw *deadlineReadWriter) SetWriteDeadline(t time.Time) error {
	drw.writeDeadline.set(t)
	return nil
}

// SetDeadline implements net.Conn.SetDeadline.
func (drw *deadlineReadWriter) SetDeadline(t time.Time) error {
	drw.readDeadline.set(t)
	drw.writeDeadline.set(t)
	return nil
}

func (drw *deadlineReadWriter) isClosed() bool {
	select {
	case <-drw.closed:
		return true
	default:
		return false
	}
}
