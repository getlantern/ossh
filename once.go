package ossh

import "sync"

// once is like sync.Once, but supports cancellation.
type once struct {
	sync.Mutex

	done chan struct{}
	err  error
}

func newOnce() *once {
	return &once{done: make(chan struct{})}
}

func (o *once) do(f func() error) error {
	o.Lock()
	defer o.Unlock()

	select {
	case <-o.done:
	default:
		o.err = f()
		close(o.done)
	}
	return o.err
}

// cancel ensures that future calls to do will fail.
//
// If do has not yet been called, cancel returns true and future calls to do will return doErr.
// If do has been called, cancel returns false.
// If do is in progress, cancel blocks until do returns, then returns false.
//
// The above ensures that, by the time cancel has returned, the caller can be confident that do has
// been cancelled or completed.
func (o *once) cancel(doErr error) (cancelled bool) {
	o.Lock()
	defer o.Unlock()

	select {
	case <-o.done:
		return false
	default:
		o.err = doErr
		close(o.done)
		return true
	}
}

// wait for o to complete (via do or cancel), then return the same error do would return.
func (o *once) wait() error {
	<-o.done
	// Writes to o.err are finished, so it is safe to read o.err without locking.
	// TODO: the race detector may not like this though
	return o.err
}
