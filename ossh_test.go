package ossh

import (
	"testing"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/prng"
	"github.com/stretchr/testify/require"
)

func TestPRNGSeedLength(t *testing.T) {
	// Sanity check.
	cfg := DialerConfig{}
	require.Equal(t, prng.SEED_LENGTH, len(cfg.PaddingPRNGSeed))
}
