//go:build linux || darwin
// +build linux darwin

package sftp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sparseRegion struct {
	offset int64
	data   []byte
}

func writeSparseTestFile(t *testing.T, path string, size int64, regions []sparseRegion) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	require.NoError(t, err)
	defer f.Close()

	// Truncate establishes the logical size with a trailing hole.
	require.NoError(t, f.Truncate(size))

	for _, r := range regions {
		_, err := f.WriteAt(r.data, r.offset)
		require.NoError(t, err)
	}
}

// TestRealFSCaps reports the capability matrix for the current platform.
func TestRealFSCaps(t *testing.T) {
	caps := localSparseCapabilities()
	t.Logf("real filesystem sparse support: discover=%v allocate=%v",
		caps.Discover, caps.Allocate)
}

// TestRealFSSparseCopy exercises sparse discovery/allocation against the real
// filesystem and reports the bytes actually transferred. On platforms without
// discovery (e.g. macOS) it falls back to a plain stream.
func TestRealFSSparseCopy(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.bin")
	dstPath := filepath.Join(dir, "dst.bin")

	const size = int64(1 << 20) // 1 MiB logical

	writeSparseTestFile(t, srcPath, size, []sparseRegion{
		{offset: 64 * 1024, data: make([]byte, 4*1024)},
		{offset: 512 * 1024, data: []byte("sparse-real-data!")},
	})

	srcFile, err := os.Open(srcPath)
	require.NoError(t, err)
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	require.NoError(t, err)
	defer dstFile.Close()

	src := OSSparseFile(srcFile)
	dst := OSSparseFile(dstFile)

	stats, err := CopySparseFile(dst, src, size)
	require.NoError(t, err)

	t.Logf("real FS: logical=%d bytes, actually transferred=%d bytes, sparse=%v",
		stats.LogicalSize, stats.BytesTransferred, stats.Sparse)

	assert.Equal(t, size, stats.LogicalSize)

	info, err := dstFile.Stat()
	require.NoError(t, err)
	assert.Equal(t, size, info.Size())

	if localSparseCapabilities().Discover && localSparseCapabilities().Allocate {
		assert.True(t, stats.Sparse)
		assert.Less(t, stats.BytesTransferred, size,
			"transfer must skip holes instead of streaming the whole file")
	} else {
		assert.False(t, stats.Sparse)
		assert.Equal(t, size, stats.BytesTransferred)
	}
}
