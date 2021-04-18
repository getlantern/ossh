package ossh

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/nettest"
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

func TestDeadlineReadWriter(t *testing.T) {
	nettest.TestConn(t, makeDeadlineTestPipe)
}

// makeDeadlineTestPipe implements nettest.MakePipe.
func makeDeadlineTestPipe() (c1, c2 net.Conn, stop func(), err error) {
	_c1, _c2 := net.Pipe()
	c1 = addDeadlineSupport(noOpHandshaker{_c1})
	c2 = addDeadlineSupport(noOpHandshaker{_c2})
	return c1, c2, func() { c1.Close(); c2.Close() }, nil
}

type noOpHandshaker struct{ net.Conn }

func (noh noOpHandshaker) Handshake() error { return nil }
