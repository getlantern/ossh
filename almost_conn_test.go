package ossh

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFuncStack(t *testing.T) {
	t.Run("Executed", func(t *testing.T) {
		ints := []int{}
		func() {
			s := funcStack{}
			defer s.call()

			s.push(func() { ints = append(ints, 3) })
			s.push(func() { ints = append(ints, 2) })
			s.push(func() { ints = append(ints, 1) })
		}()
		require.Equal(t, []int{1, 2, 3}, ints)
	})
	t.Run("Cancelled", func(t *testing.T) {
		ints := []int{}
		func() {
			s := funcStack{}
			defer s.call()

			s.push(func() { ints = append(ints, 3) })
			s.push(func() { ints = append(ints, 2) })
			s.push(func() { ints = append(ints, 1) })
			s.clear()
		}()
		require.Equal(t, []int{}, ints)
	})
}
