package sftp

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLegacyServerSparseNegotiation verifies the built-in filesystem Server
// advertises the sparse-file extension with the platform's capabilities.
func TestLegacyServerSparseNegotiation(t *testing.T) {
	serverConn, clientConn := net.Pipe()

	server, err := NewServer(serverConn)
	require.NoError(t, err)

	go server.Serve()

	client, err := NewClientPipe(clientConn, clientConn)
	require.NoError(t, err)

	defer server.Close()
	defer client.Close()

	data, ok := client.HasExtension(sparseFileExtension)
	expected := localSparseCapabilities()

	if expected.Any() {
		require.True(t, ok, "filesystem server should advertise sparse-file")
		assert.Equal(t, marshalSparseCaps(expected), data)
	} else {
		assert.False(t, ok)
	}
}

// TestLegacyServerSparseCopy runs an end-to-end sparse copy through the real
// filesystem-backed Server (over in-process pipes) and reports the bytes that
// actually crossed the protocol.
func TestLegacyServerSparseCopy(t *testing.T) {
	dir := t.TempDir()

	// Run the server rooted at the temp dir.
	saved, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))

	t.Cleanup(func() { _ = os.Chdir(saved) })

	serverConn, clientConn := net.Pipe()

	server, err := NewServer(serverConn)
	require.NoError(t, err)

	go server.Serve()

	client, err := NewClientPipe(clientConn, clientConn)
	require.NoError(t, err)

	defer server.Close()
	defer client.Close()

	const size = int64(1 << 20)

	// Build a sparse source directly on disk.
	srcPath := filepath.Join(dir, "src.bin")
	dstPath := filepath.Join(dir, "dst.bin")

	srcDisk, err := os.OpenFile(srcPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	require.NoError(t, err)

	srcSparse := OSSparseFile(srcDisk)
	require.NoError(t, MarkSparse(srcSparse))
	require.NoError(t, srcDisk.Truncate(size))

	_, err = srcDisk.WriteAt(make([]byte, 4*1024), 64*1024)
	require.NoError(t, err)

	_, err = srcDisk.WriteAt([]byte("remote-sparse-data"), 512*1024)
	require.NoError(t, err)
	require.NoError(t, srcDisk.Close())

	// Remote SFTP source and destination.
	rf, err := client.OpenFile("src.bin", os.O_RDONLY)
	require.NoError(t, err)

	wf, err := client.OpenFile("dst.bin", os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	require.NoError(t, err)

	caps := client.SparseCapabilities()
	t.Logf("legacy server caps: discover=%v allocate=%v", caps.Discover, caps.Allocate)

	stats, err := CopySparseFile(wf, rf, size)
	require.NoError(t, err)

	require.NoError(t, wf.Close())
	require.NoError(t, rf.Close())

	t.Logf("legacy SFTP: logical=%d transferred=%d sparse=%v",
		stats.LogicalSize, stats.BytesTransferred, stats.Sparse)

	assert.Equal(t, size, stats.LogicalSize)

	if caps.Discover && caps.Allocate {
		assert.True(t, stats.Sparse)
		assert.Less(t, stats.BytesTransferred, size)
	} else {
		assert.False(t, stats.Sparse)
		assert.Equal(t, size, stats.BytesTransferred)
	}

	info, err := os.Stat(dstPath)
	require.NoError(t, err)
	assert.Equal(t, size, info.Size(), "logical length incl. trailing hole kept")
}
