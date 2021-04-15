package ossh

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOnce(t *testing.T) {
	t.Parallel()

	// The time we wait for parallel routines to make progress.
	const sleepTime = 50 * time.Millisecond

	var (
		doErr     = errors.New("do")
		cancelErr = errors.New("cancelled")
	)

	t.Run("do then cancel", func(t *testing.T) {
		t.Parallel()

		o := newOnce()
		err := o.do(func() error { return doErr })
		require.ErrorIs(t, err, doErr)
		require.False(t, o.cancel(cancelErr))

		err = o.do(func() error { return errors.New("do again") })
		require.ErrorIs(t, err, doErr)
	})
	t.Run("cancel then do", func(t *testing.T) {
		t.Parallel()

		o := newOnce()
		require.True(t, o.cancel(cancelErr))
		err := o.do(func() error { return doErr })
		require.ErrorIs(t, err, cancelErr)

		err = o.do(func() error { return errors.New("do again") })
		require.ErrorIs(t, err, cancelErr)
	})
	t.Run("cancel concurrently", func(t *testing.T) {
		t.Parallel()

		o := newOnce()
		doing := make(chan struct{})
		doErrC := make(chan error)
		go func() {
			doErrC <- o.do(func() error {
				close(doing)
				time.Sleep(sleepTime)
				return doErr
			})
		}()

		<-doing
		require.False(t, o.cancel(cancelErr))
		require.ErrorIs(t, <-doErrC, doErr)

		err := o.do(func() error { return errors.New("do again") })
		require.ErrorIs(t, err, doErr)
	})
	t.Run("wait then do then cancel", func(t *testing.T) {
		t.Parallel()

		o := newOnce()
		waitErrC := make(chan error)
		go func() {
			waitErrC <- o.wait()
		}()

		time.Sleep(sleepTime)
		select {
		case <-waitErrC:
			t.Fatal("wait should not have completed yet")
		default:
		}

		o.do(func() error { return doErr })
		require.ErrorIs(t, <-waitErrC, doErr)

		o.cancel(cancelErr)
		require.ErrorIs(t, o.wait(), doErr)
	})
	t.Run("wait then cancel then do", func(t *testing.T) {
		t.Parallel()

		o := newOnce()
		waitErrC := make(chan error)
		go func() {
			waitErrC <- o.wait()
		}()

		time.Sleep(sleepTime)
		select {
		case <-waitErrC:
			t.Fatal("wait should not have completed yet")
		default:
		}

		o.cancel(cancelErr)
		require.ErrorIs(t, <-waitErrC, cancelErr)

		o.do(func() error { return doErr })
		require.ErrorIs(t, o.wait(), cancelErr)
	})
}
