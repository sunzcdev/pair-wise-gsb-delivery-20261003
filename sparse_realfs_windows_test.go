//go:build windows
// +build windows

package sftp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSparseTestFile creates a file with the given logical size and writes
// real payload only at the listed offsets, marking it sparse on platforms
// that require it.
func writeSparseTestFile(t *testing.T, path string, size int64, regions []struct {
	offset int64
	data   []byte
}) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	require.NoError(t, err)

	sf := OSSparseFile(f).(*osSparseFile)
	require.NoError(t, sf.markSparse())
	require.NoError(t, f.Truncate(size))

	for _, r := range regions {
		_, err := f.WriteAt(r.data, r.offset)
		require.NoError(t, err)
	}

	require.NoError(t, f.Close())
}

// TestRealFSSparseCopy exercises sparse discovery and allocation against the
// real NTFS filesystem and reports the actual bytes transferred versus the
// logical file size.
func TestRealFSSparseCopy(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.bin")
	dstPath := filepath.Join(dir, "dst.bin")

	const size = int64(1 << 20) // 1 MiB logical

	writeSparseTestFile(t, srcPath, size, []struct {
		offset int64
		data   []byte
	}{
		{offset: 64 * 1024, data: make([]byte, 4*1024)},         // 4 KiB allocated zeros
		{offset: 512 * 1024, data: []byte("sparse-real-data!")}, // 17 bytes
	})

	srcFile, err := os.Open(srcPath)
	require.NoError(t, err)
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	require.NoError(t, err)
	defer dstFile.Close()

	src := OSSparseFile(srcFile)
	dst := OSSparseFile(dstFile)

	ranges, next, err := src.QueryAllocatedRanges(0, 16)
	require.NoError(t, err)
	t.Logf("discovered %d allocated range(s), scan next=%d", len(ranges), next)

	stats, err := CopySparseFile(dst, src, size)
	require.NoError(t, err)

	t.Logf("real FS: logical=%d bytes, actually transferred=%d bytes, sparse=%v",
		stats.LogicalSize, stats.BytesTransferred, stats.Sparse)

	assert.True(t, stats.Sparse)
	assert.Equal(t, size, stats.LogicalSize)
	assert.Less(t, stats.BytesTransferred, size,
		"transfer must skip holes instead of streaming the whole file")

	// The destination must retain the logical size including the trailing hole.
	info, err := dstFile.Stat()
	require.NoError(t, err)
	assert.Equal(t, size, info.Size())
}

// TestRealFSCaps advertises the capability matrix for the current platform.
func TestRealFSCaps(t *testing.T) {
	caps := localSparseCapabilities()
	t.Logf("real filesystem sparse support: discover=%v allocate=%v (GOOS=%s)",
		caps.Discover, caps.Allocate, "windows")
	assert.True(t, caps.Discover || caps.Allocate)
}
