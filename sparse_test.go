package sftp

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pipeConn adapts a net.Conn to the io.ReadWriteCloser the servers expect.
type pipeCS struct {
	cli *Client
	svr *RequestServer
	h   *root
}

func (p *pipeCS) Close() {
	p.svr.Close()
	p.cli.Close()
}

func (p *pipeCS) handler() *root { return p.h }

// newPipePair builds an in-process client/RequestServer pair over net.Pipe so
// the protocol tests run on every platform (no unix domain socket needed).
func newPipePair(t *testing.T, handlers Handlers) *pipeCS {
	t.Helper()

	serverConn, clientConn := net.Pipe()

	server := NewRequestServer(serverConn, handlers)

	serveErr := make(chan error, 1)

	go func() { serveErr <- server.Serve() }()

	client, err := NewClientPipe(clientConn, clientConn)
	require.NoError(t, err)

	t.Cleanup(func() {
		server.Close()
		client.Close()
		serverConn.Close()
		clientConn.Close()

		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
		}
	})

	return &pipeCS{
		cli: client,
		svr: server,
		h:   handlers.FileGet.(*root),
	}
}

// sparseP describes one allocated region used to build a sparse test file.
type sparseP struct {
	offset int64
	data   []byte
}

// buildSparseMemFile creates a memFile of the given logical size, marks the
// listed regions as backed storage, and writes their payload. Any region not
// listed is a genuine hole (even where the bytes would read as zero).
func buildSparseMemFile(t *testing.T, fs *root, name string, logicalSize int64, regions []sparseP) *memFile {
	t.Helper()

	f := &memFile{modtime: time.Now()}
	require.NoError(t, f.Truncate(logicalSize))

	for _, r := range regions {
		_, err := f.WriteAt(r.data, r.offset)
		require.NoError(t, err)
	}

	fs.mu.Lock()
	fs.files[name] = f
	fs.mu.Unlock()

	return f
}

// allocatedRangesMem returns the recorded backing ranges of a memFile.
func allocatedRangesMem(f *memFile) []fileRangeMem {
	f.mu.RLock()
	defer f.mu.RUnlock()

	out := make([]fileRangeMem, len(f.allocated))
	copy(out, f.allocated)

	return out
}

// TestSparseExtensionNegotiation verifies that the in-memory backend
// advertises both sparse capabilities and that the client parses them.
func TestSparseExtensionNegotiation(t *testing.T) {
	p := newPipePair(t, InMemHandler())
	defer p.Close()

	data, ok := p.cli.HasExtension(sparseFileExtension)
	require.True(t, ok, "server should advertise the sparse-file extension")
	assert.Equal(t, "1", data)

	caps := p.cli.SparseCapabilities()
	assert.True(t, caps.Discover)
	assert.True(t, caps.Allocate)
}

// TestSparseQueryAllocated walks the allocated regions of a file with holes
// at the start, middle and end, exercising the query-allocated request and its
// continuation semantics.
func TestSparseQueryAllocated(t *testing.T) {
	p := newPipePair(t, InMemHandler())
	defer p.Close()

	const size = int64(10000)

	src := buildSparseMemFile(t, p.handler(), "/sparse", size, []sparseP{
		{offset: 1000, data: make([]byte, 2000)}, // allocated zeros
		{offset: 5000, data: []byte("middle-data")},
	})

	rf, err := p.cli.OpenFile("/sparse", os.O_RDONLY)
	require.NoError(t, err)

	// Logical length includes the trailing hole [6000,10000).
	info, err := rf.Stat()
	require.NoError(t, err)
	assert.Equal(t, size, info.Size())

	// First call starts in the leading hole [0,1000); the reply advances to
	// the first data region and returns it.
	ranges, next, err := rf.QueryAllocatedRanges(0, 1)
	require.NoError(t, err)
	require.Len(t, ranges, 1)
	assert.Equal(t, FileRange{Offset: 1000, Length: 2000}, ranges[0])
	assert.Equal(t, uint64(3000), next)

	// Skip the middle hole by scanning again from the reported next offset.
	ranges, next, err = rf.QueryAllocatedRanges(next, 1)
	require.NoError(t, err)
	require.Len(t, ranges, 1)
	assert.Equal(t, FileRange{Offset: 5000, Length: 11}, ranges[0])
	assert.Equal(t, uint64(5011), next)

	// Nothing allocated after the last data region; next reports logical EOF
	// so the trailing hole length is preserved.
	ranges, next, err = rf.QueryAllocatedRanges(next, 1)
	require.NoError(t, err)
	assert.Empty(t, ranges)
	assert.Equal(t, uint64(size), next)

	require.NoError(t, rf.Close())

	// The source distinguishes a real hole from allocated zeros: backing
	// ranges are exactly the two written regions.
	got := allocatedRangesMem(src)
	require.Len(t, got, 2)
	assert.Equal(t, fileRangeMem{1000, 2000}, got[0])
	assert.Equal(t, fileRangeMem{5000, 11}, got[1])
}

// TestSparseAllocateRegions verifies the allocate-regions request marks only
// the listed regions as backed storage and leaves the gaps as holes.
func TestSparseAllocateRegions(t *testing.T) {
	p := newPipePair(t, InMemHandler())
	defer p.Close()

	wf, err := p.cli.OpenFile("/dst", os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	require.NoError(t, err)

	err = wf.AllocateRegions([]FileRange{
		{Offset: 1000, Length: 2000},
		{Offset: 5000, Length: 11},
	})
	require.NoError(t, err)

	require.NoError(t, wf.Close())

	dst, err := p.handler().fetch("/dst")
	require.NoError(t, err)

	got := allocatedRangesMem(dst)
	require.Len(t, got, 2)
	assert.Equal(t, fileRangeMem{1000, 2000}, got[0])
	assert.Equal(t, fileRangeMem{5000, 11}, got[1])
}

// TestSparseCopyOverProtocol verifies end-to-end that CopySparseFile using
// remote SFTP files skips holes, keeps allocated zeros, preserves the
// trailing hole, and reports the actual number of bytes transferred.
func TestSparseCopyOverProtocol(t *testing.T) {
	p := newPipePair(t, InMemHandler())
	defer p.Close()

	const size = int64(10000)

	buildSparseMemFile(t, p.handler(), "/src", size, []sparseP{
		{offset: 1000, data: make([]byte, 2000)},    // 2000 allocated zero bytes
		{offset: 5000, data: []byte("middle-data")}, // 11 bytes
	})

	rf, err := p.cli.OpenFile("/src", os.O_RDONLY)
	require.NoError(t, err)

	wf, err := p.cli.OpenFile("/dst", os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	require.NoError(t, err)

	stats, err := CopySparseFile(wf, rf, size)
	require.NoError(t, err)

	assert.True(t, stats.Sparse)
	assert.Equal(t, size, stats.LogicalSize)
	assert.Equal(t, int64(2000+11), stats.BytesTransferred,
		"only allocated bytes should cross the wire")

	require.NoError(t, wf.Close())
	require.NoError(t, rf.Close())

	dst, err := p.handler().fetch("/dst")
	require.NoError(t, err)

	assert.Equal(t, size, dst.Size(), "logical size incl. trailing hole kept")

	got := allocatedRangesMem(dst)
	require.Len(t, got, 2)
	assert.Equal(t, fileRangeMem{1000, 2000}, got[0])
	assert.Equal(t, fileRangeMem{5000, 11}, got[1])

	// Content integrity across the whole logical file.
	src, err := p.handler().fetch("/src")
	require.NoError(t, err)
	assert.Equal(t, src.content, dst.content)
}

// TestSparseCopyPlainFallback verifies that a destination which cannot
// allocate holes causes a plain full-length byte stream transfer.
func TestSparseCopyPlainFallback(t *testing.T) {
	const size = int64(10000)

	src := &memFile{modtime: time.Now()}
	require.NoError(t, src.Truncate(size))
	_, err := src.WriteAt([]byte("hello"), 1000)
	require.NoError(t, err)

	// A plain bytes-backed writerAt that implements neither sparse interface.
	dst := &plainSink{size: size}

	stats, err := CopySparseFile(dst, src, size)
	require.NoError(t, err)

	assert.False(t, stats.Sparse)
	assert.Equal(t, size, stats.BytesTransferred)
	assert.Len(t, dst.buf, int(size))
}

// plainSink is a minimal io.WriterAt without any sparse capability.
type plainSink struct {
	buf  []byte
	size int64
}

func (s *plainSink) WriteAt(p []byte, off int64) (int, error) {
	if end := off + int64(len(p)); end > int64(len(s.buf)) {
		grown := make([]byte, end)
		copy(grown, s.buf)
		s.buf = grown
	}

	return copy(s.buf[off:], p), nil
}

// TestSparseCapabilityParsing covers the extension-data negotiation grammar.
func TestSparseCapabilityParsing(t *testing.T) {
	for _, tc := range []struct {
		data            string
		discover, alloc bool
	}{
		{"", true, true},
		{"1", true, true},
		{"discover", true, false},
		{"allocate", false, true},
		{"discover,allocate", true, true},
		{"allocate,discover", true, true},
		{"unknown", false, false},
	} {
		caps := parseSparseCaps(tc.data)
		assert.Equal(t, tc.discover, caps.Discover, "data=%q discover", tc.data)
		assert.Equal(t, tc.alloc, caps.Allocate, "data=%q allocate", tc.data)
	}

	assert.Equal(t, "1", marshalSparseCaps(SparseCaps{true, true}))
	assert.Equal(t, "discover", marshalSparseCaps(SparseCaps{Discover: true}))
	assert.Equal(t, "allocate", marshalSparseCaps(SparseCaps{Allocate: true}))
	assert.Equal(t, "", marshalSparseCaps(SparseCaps{}))
}

// capsRoot wraps the in-memory backend but declares a chosen (possibly empty)
// sparse capability set, letting us test negotiation downgrades.
type capsRoot struct {
	*root
	caps SparseCaps
}

func (r *capsRoot) SparseCapabilities() SparseCaps { return r.caps }

func newCapsPair(t *testing.T, caps SparseCaps) *pipeCS {
	inner := InMemHandler().FileGet.(*root)
	cr := &capsRoot{root: inner, caps: caps}

	return newPipePairCaps(t, cr)
}

func newPipePairCaps(t *testing.T, cr *capsRoot) *pipeCS {
	t.Helper()

	h := Handlers{cr, cr, cr, cr}
	serverConn, clientConn := net.Pipe()
	server := NewRequestServer(serverConn, h)

	serveErr := make(chan error, 1)

	go func() { serveErr <- server.Serve() }()

	client, err := NewClientPipe(clientConn, clientConn)
	require.NoError(t, err)

	t.Cleanup(func() {
		server.Close()
		client.Close()
		serverConn.Close()
		clientConn.Close()

		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
		}
	})

	return &pipeCS{cli: client, svr: server, h: cr.root}
}

// TestSparseNegotiationNone verifies a backend that declares no capability
// forces a plain transfer and that sparse requests fail cleanly.
func TestSparseNegotiationNone(t *testing.T) {
	p := newCapsPair(t, SparseCaps{})
	defer p.Close()

	_, ok := p.cli.HasExtension(sparseFileExtension)
	assert.False(t, ok)

	const size = int64(1000)

	src := buildSparseMemFile(t, p.handler(), "/src", size, []sparseP{
		{offset: 100, data: []byte("x")},
	})

	rf, err := p.cli.OpenFile("/src", os.O_RDONLY)
	require.NoError(t, err)

	wf, err := p.cli.Create("/dst")
	require.NoError(t, err)

	stats, err := CopySparseFile(wf, rf, size)
	require.NoError(t, err)
	assert.False(t, stats.Sparse)
	assert.Equal(t, size, stats.BytesTransferred)

	require.NoError(t, wf.Close())
	require.NoError(t, rf.Close())

	dst, err := p.handler().fetch("/dst")
	require.NoError(t, err)
	assert.Equal(t, src.content, dst.content)
}

// TestSparseNegotiationDiscoverOnly verifies that discovery without allocate
// still results in a plain transfer, because holes cannot be recreated.
func TestSparseNegotiationDiscoverOnly(t *testing.T) {
	p := newCapsPair(t, SparseCaps{Discover: true})
	defer p.Close()

	caps := p.cli.SparseCapabilities()
	assert.True(t, caps.Discover)
	assert.False(t, caps.Allocate)

	const size = int64(500)

	buildSparseMemFile(t, p.handler(), "/src", size, []sparseP{
		{offset: 10, data: make([]byte, 100)},
	})

	rf, err := p.cli.OpenFile("/src", os.O_RDONLY)
	require.NoError(t, err)

	// Discovery is negotiated and works on its own.
	ranges, next, err := rf.QueryAllocatedRanges(0, 10)
	require.NoError(t, err)
	require.Len(t, ranges, 1)
	assert.Equal(t, FileRange{Offset: 10, Length: 100}, ranges[0])
	assert.Equal(t, uint64(110), next)

	wf, err := p.cli.Create("/dst")
	require.NoError(t, err)

	stats, err := CopySparseFile(wf, rf, size)
	require.NoError(t, err)
	assert.False(t, stats.Sparse, "cannot preserve holes without allocate")
	assert.Equal(t, size, stats.BytesTransferred)

	require.NoError(t, wf.Close())
	require.NoError(t, rf.Close())

	// allocate-regions must be refused even though discovery works.
	err = p.cli.AllocateRegions(context.Background(), "nohandle", nil)
	assert.ErrorIs(t, err, ErrSparseNotSupported)
}

// TestSparseZeroAllocatedNotAHole verifies the key semantic distinction: a
// fully allocated zero region is transferred, not treated as a hole.
func TestSparseZeroAllocatedNotAHole(t *testing.T) {
	p := newPipePair(t, InMemHandler())
	defer p.Close()

	const size = int64(4096)

	// Entire file is allocated but contains only zeros: it must be reported
	// as one allocated range and transferred in full.
	buildSparseMemFile(t, p.handler(), "/zeros", size, []sparseP{
		{offset: 0, data: make([]byte, size)},
	})

	rf, err := p.cli.OpenFile("/zeros", os.O_RDONLY)
	require.NoError(t, err)

	ranges, next, err := rf.QueryAllocatedRanges(0, 10)
	require.NoError(t, err)
	require.Len(t, ranges, 1)
	assert.Equal(t, FileRange{Offset: 0, Length: uint64(size)}, ranges[0])
	assert.Equal(t, uint64(size), next)

	wf, err := p.cli.Create("/zcopy")
	require.NoError(t, err)

	stats, err := CopySparseFile(wf, rf, size)
	require.NoError(t, err)
	assert.True(t, stats.Sparse)
	assert.Equal(t, size, stats.BytesTransferred,
		"allocated zeros are real data and must be sent")

	require.NoError(t, wf.Close())
	require.NoError(t, rf.Close())
}

// TestSparseManyRegionsBatching verifies a file with more allocated regions
// than one query batch is copied completely across multiple query rounds.
func TestSparseManyRegionsBatching(t *testing.T) {
	p := newPipePair(t, InMemHandler())
	defer p.Close()

	const size = int64(10000)

	var patches []sparseP

	for off := int64(0); off < size; off += 500 {
		patches = append(patches, sparseP{offset: off, data: []byte("ab")})
	}

	src := buildSparseMemFile(t, p.handler(), "/many", size, patches)

	rf, err := p.cli.OpenFile("/many", os.O_RDONLY)
	require.NoError(t, err)

	wf, err := p.cli.Create("/manycopy")
	require.NoError(t, err)

	stats, err := CopySparseFile(wf, rf, size)
	require.NoError(t, err)

	// 20 regions x 2 bytes = 40 real bytes in a 10000-byte logical file.
	assert.True(t, stats.Sparse)
	assert.Equal(t, int64(40), stats.BytesTransferred)

	require.NoError(t, wf.Close())
	require.NoError(t, rf.Close())

	dst, err := p.handler().fetch("/manycopy")
	require.NoError(t, err)
	assert.Equal(t, size, dst.Size())
	assert.Equal(t, src.content, dst.content)
	assert.Len(t, allocatedRangesMem(dst), len(patches))
}
