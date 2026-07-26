package backupmgr

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSaturatingFilesystemBytes(t *testing.T) {
	for _, test := range []struct {
		name      string
		blocks    uint64
		blockSize uint64
		expected  int64
	}{
		{name: "no blocks", blocks: 0, blockSize: 4096, expected: 0},
		{name: "zero block size", blocks: 10, blockSize: 0, expected: 0},
		{name: "normal filesystem", blocks: 1024, blockSize: 4096, expected: 4 * 1024 * 1024},
		{
			name:      "largest representable value",
			blocks:    uint64(math.MaxInt64),
			blockSize: 1,
			expected:  math.MaxInt64,
		},
		{
			name:      "overflow saturates",
			blocks:    uint64(math.MaxInt64),
			blockSize: 2,
			expected:  math.MaxInt64,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(
				t,
				test.expected,
				saturatingFilesystemBytes(test.blocks, test.blockSize),
			)
		})
	}
}
