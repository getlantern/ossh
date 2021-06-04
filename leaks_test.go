package ossh

import (
	"flag"
	"testing"

	"github.com/getlantern/fdcount"
	"github.com/stretchr/testify/require"
)

var runs = flag.Int("leak-runs", 1, "number of times to run leak tests")

// TestFileDescriptorLeak is used to ensure that we do not leak file descriptors. This test runs
// other tests, but will sometimes hang if those tests fail. Thus this test should be run after
// other tests have run successfully.
func TestFileDescriptorLeak(t *testing.T) {
	flag.Parse()

	_, tcpFileDescriptors, err := fdcount.Matching("TCP")
	require.NoError(t, err)

	for i := 0; i < *runs; i++ {
		// TestConn is the most exhaustive and complete test of OSSH connections.
		TestConn(t)

		// TestListenAndDial covers the Listen and Dial methods, which are not covered by TestConn.
		TestListenAndDial(t)
	}

	require.NoError(t, tcpFileDescriptors.AssertDelta(0))
}
