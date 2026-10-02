package encoding

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodedLenNo32BitOverflow(t *testing.T) {
	// The safe arithmetic matters most where int is 32 bits, but the formula
	// is exercised here with an int64-representable length. On a 32 bit
	// platform an int argument that large cannot exist, so skip.
	if strconv.IntSize == 32 {
		t.Skip("cannot construct a >2GiB encoded length on a 32 bit platform")
	}

	var huge64 = int64((math.MaxInt32 + 4) / 3 * 4) // > 4/3 * MaxInt32, n % 4 == 0
	var huge = int(huge64)

	decodedLen, ok := DecodedLen(huge)
	require.True(t, ok)

	expected := huge64 * 3 / 4
	require.Equal(t, expected, decodedLen)
	require.Greater(t, decodedLen, int64(math.MaxInt32))
}

func TestDecodedLenNegative(t *testing.T) {
	_, ok := DecodedLen(-1)
	require.False(t, ok)
}
