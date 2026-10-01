package sftp

import (
	"bytes"
	"io"
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pkg/sftp/internal/sparse"
)

// pipePair returns an SFTP client backed by a RequestServer over net.Pipe,
// which works on every platform (the unix-socket helpers skip on Windows).
func pipePair(t *testing.T, handlers Handlers, opts ...RequestServerOption) (*Client, func()) {
	t.Helper()

	cConn, sConn := net.Pipe()
	server := NewRequestServer(sConn, handlers, opts...)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	client, err := NewClientPipe(cConn, cConn)
	require.NoError(t, err)

	cleanup := func() {
		client.Close()
		server.Close()
		if err := <-serveErr; err != nil && err != io.EOF && err != io.ErrClosedPipe {
			t.Logf("server exited with error: %v", err)
		}
	}
	return client, cleanup
}

func TestSparseProtocolNegotiation(t *testing.T) {
	client, cleanup := pipePair(t, SparseInMemHandler())
	defer cleanup()

	data, ok := client.HasExtension(sparse.ExtensionName)
	require.True(t, ok, "sparse extension must be advertised")
	require.Equal(t, sparse.CapAll, sparse.ParseExtensionData(data))
	require.Equal(t, sparse.CapAll, client.SparseCapabilities())
}

func TestSparseProtocolNotAdvertisedByPlainBackend(t *testing.T) {
	client, cleanup := pipePair(t, InMemHandler())
	defer cleanup()

	// The dense example backend does not report extents or punch; the
	// extension is absent (or carries no known capability tokens).
	caps := client.SparseCapabilities()
	require.Zero(t, caps, "dense backend must not negotiate sparse capabilities")
}

func TestSparseProtocolForcedCaps(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps sparse.Capability
	}{
		{"extents only", sparse.CapExtents},
		{"punch only", sparse.CapPunch},
		{"none", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, cleanup := pipePair(t, InMemHandler(), WithRSSparseCapabilities(tc.caps))
			defer cleanup()
			require.Equal(t, tc.caps, client.SparseCapabilities())
		})
	}
}

// TestSparseProtocolRoundTrip verifies the extents/punch-hole operations over
// the wire against the range-recording in-memory backend.
func TestSparseProtocolRoundTrip(t *testing.T) {
	client, cleanup := pipePair(t, SparseInMemHandler())
	defer cleanup()

	f, err := client.Create("/sparse.bin")
	require.NoError(t, err)

	const size = int64(4096)
	// allocated data at the head, a hole in the middle, genuine allocated
	// zeroes afterwards, and a trailing hole.
	head := bytes.Repeat([]byte{'A'}, 1024)
	realZeroes := make([]byte, 1024) // allocated zeroes, not a hole

	_, err = f.WriteAt(head, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(realZeroes, 2048)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.PunchHole(1024, 1024))
	require.NoError(t, f.PunchHole(3072, 1024))

	extents, err := f.DataExtents(0, 0)
	require.NoError(t, err)
	require.Equal(t, []sparse.Extent{
		{Offset: 0, Length: 1024},
		// [1024,2048) punched -> absent
		{Offset: 2048, Length: 1024}, // genuine allocated zeroes retained
		// [3072,4096) punched -> absent
	}, extents)

	// A sub-range query clips to the requested window.
	sub, err := f.DataExtents(1500, 1000)
	require.NoError(t, err)
	require.Equal(t, []sparse.Extent{{Offset: 2048, Length: 452}}, sub)

	require.NoError(t, f.Close())

	// Logical length must be preserved despite trailing holes.
	rf, err := client.Open("/sparse.bin")
	require.NoError(t, err)
	defer rf.Close()

	info, err := rf.Stat()
	require.NoError(t, err)
	require.Equal(t, size, info.Size())

	got, err := io.ReadAll(rf)
	require.NoError(t, err)
	require.Len(t, got, int(size))
	require.Equal(t, head, got[:1024])
	require.Equal(t, make([]byte, 3072), got[1024:])
}

func TestSparsePunchUnsupportedWhenCapabilityMissing(t *testing.T) {
	// Punch-only disabled: client refuses the operation locally.
	client, cleanup := pipePair(t, InMemHandler(), WithRSSparseCapabilities(sparse.CapExtents))
	defer cleanup()

	f, err := client.Create("/x")
	require.NoError(t, err)
	defer f.Close()

	err = f.PunchHole(0, 10)
	require.ErrorIs(t, err, ErrSparseUnsupported)

	_, err = f.DataExtents(0, 0)
	// The dense memFile does not implement SparseDataReaderAt, so even with the
	// capability negotiated the server replies OP_UNSUPPORTED.
	require.Error(t, err)
}

func TestSparseDenseFallbackWhenBackendLacksCapabilities(t *testing.T) {
	if SparseSupport() != sparse.CapAll {
		t.Skip("local platform does not provide full sparse support")
	}

	// Plain backend advertises no sparse capabilities: transfers must still
	// complete, using the dense byte-stream fallback.
	client, cleanup := pipePair(t, InMemHandler())
	defer cleanup()

	require.Zero(t, client.SparseCapabilities())

	src := t.TempDir() + "/dense-src.bin"
	require.NoError(t, os.WriteFile(src, bytes.Repeat([]byte{'X'}, 5000), 0o644))

	up, err := client.SparseUpload(src, "/dense-up.bin")
	require.NoError(t, err)
	require.False(t, up.Sparse, "upload should fall back to dense")
	require.Equal(t, int64(5000), up.DataBytes)

	dst := t.TempDir() + "/dense-dst.bin"
	dn, err := client.SparseDownload("/dense-up.bin", dst)
	require.NoError(t, err)
	require.False(t, dn.Sparse, "download should fall back to dense")
	require.Equal(t, int64(5000), dn.DataBytes)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat([]byte{'X'}, 5000), got)
}

func TestSparseFullRoundTripOverMemBackend(t *testing.T) {
	// The local platform acts as one end, the range-recording in-memory server
	// as the other. Sparse transfer is only exercised on platforms that can
	// both discover extents and punch holes locally.
	if SparseSupport() != sparse.CapAll {
		t.Skip("local platform does not provide full sparse support")
	}

	client, cleanup := pipePair(t, SparseInMemHandler())
	defer cleanup()

	src := t.TempDir() + "/src.bin"
	lf, _, err := createSparseTarget(src)
	require.NoError(t, err)

	const logical = int64(1 << 20) // 1 MiB
	require.NoError(t, lf.Truncate(logical))

	// Three allocated regions, including a genuine allocated-zero run that
	// must be transferred; everything else (incl. the tail) is a hole.
	regions := []struct {
		off  int64
		data []byte
	}{
		{0, bytes.Repeat([]byte{'A'}, 4096)},
		{64 * 1024, make([]byte, 8192)}, // allocated zeroes, not a hole
		{logical - 4096, bytes.Repeat([]byte{'Z'}, 4096)},
	}
	for _, r := range regions {
		_, err := lf.WriteAt(r.data, r.off)
		require.NoError(t, err)
	}
	require.NoError(t, lf.Close())

	// Reopen and punch the gaps *after* all writes are done. On NTFS a later
	// allocation can round across existing holes, so punching last gives a
	// stable, reproducible sparse layout on both Linux and Windows.
	lf, sf, err := openSparseFileReadWrite(src)
	require.NoError(t, err)
	localExtents, err := sf.DataExtents(0, 0)
	require.NoError(t, err)
	for _, h := range holesBetween(localExtents, uint64(logical)) {
		require.NoError(t, sf.PunchHole(int64(h.Offset), int64(h.Length)))
	}
	localExtents, err = sf.DataExtents(0, 0)
	require.NoError(t, err)
	require.NoError(t, lf.Close())
	t.Logf("local logical=%d data=%d extents=%d", logical, func() int64 {
		var n int64
		for _, e := range localExtents {
			n += int64(e.Length)
		}
		return n
	}(), len(localExtents))

	up, err := client.SparseUpload(src, "/uploaded.bin")
	require.NoError(t, err)
	require.True(t, up.Sparse)
	require.Equal(t, logical, up.LogicalSize)

	// Only allocated data went over the wire. The exact granularity is
	// filesystem-specific (NTFS rounds up to compression units), so assert
	// against what the local FS actually reported instead of hard-coding it.
	var expectedData int64
	for _, e := range localExtents {
		expectedData += int64(e.Length)
	}
	require.Equal(t, expectedData, up.DataBytes, "uploaded payload bytes")
	require.Less(t, up.DataBytes, logical, "holes must not be transferred")
	require.Equal(t, len(localExtents), up.Extents)

	// The remote (in-memory) file must report exactly those extents.
	rf, err := client.Open("/uploaded.bin")
	require.NoError(t, err)
	remoteExtents, err := rf.DataExtents(0, 0)
	require.NoError(t, err)
	require.Equal(t, localExtents, remoteExtents)

	info, err := rf.Stat()
	require.NoError(t, err)
	require.Equal(t, logical, info.Size(), "trailing hole preserves logical length")
	require.NoError(t, rf.Close())

	// Download back to a second local file and verify the bytes line up.
	dst := t.TempDir() + "/dst.bin"
	dn, err := client.SparseDownload("/uploaded.bin", dst)
	require.NoError(t, err)
	require.True(t, dn.Sparse)
	require.Equal(t, expectedData, dn.DataBytes, "downloaded payload bytes")
	require.Less(t, dn.DataBytes, logical)

	orig, err := os.ReadFile(src)
	require.NoError(t, err)
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, orig, got)

	df, dsf, err := NewSparseFile(dst)
	require.NoError(t, err)
	defer df.Close()
	dstExtents, err := dsf.DataExtents(0, 0)
	require.NoError(t, err)
	require.Equal(t, localExtents, dstExtents, "downloaded file retains holes")
}
