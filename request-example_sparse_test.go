package sftp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSparseMemFileExtents(t *testing.T) {
	mf := &memFile{name: "/x"}
	f := &sparseMemFile{memFile: mf}

	require.NoError(t, mf.Truncate(100))
	_, err := mf.WriteAt([]byte("data-data-data"), 10)
	require.NoError(t, err)

	// holes: [0,10) head gap and [24,100) tail
	require.NoError(t, f.PunchHole(0, 10))
	require.NoError(t, f.PunchHole(24, 76))

	got, err := f.DataExtents(0, 0)
	require.NoError(t, err)
	require.Equal(t, []DataExtent{{Offset: 10, Length: 14}}, got)
}

func TestSparseMemFileAllocatedZeroesAreData(t *testing.T) {
	mf := &memFile{name: "/x"}
	f := &sparseMemFile{memFile: mf}

	require.NoError(t, mf.Truncate(100))
	// content is all allocated zeroes; with no punched holes it is all data.
	got, err := f.DataExtents(0, 0)
	require.NoError(t, err)
	require.Equal(t, []DataExtent{{Offset: 0, Length: 100}}, got)
}
