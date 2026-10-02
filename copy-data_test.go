package sftp

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copyDataMemFile is an in-memory io.ReaderAt and io.WriterAt used to test
// the copy-data fallback. readChunk and writeChunk, if positive, limit the
// number of bytes handled per call, to simulate short reads and writes.
type copyDataMemFile struct {
	content    []byte
	readChunk  int
	writeChunk int
}

func (f *copyDataMemFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= int64(len(f.content)) {
		return 0, io.EOF
	}
	if f.readChunk > 0 && len(p) > f.readChunk {
		p = p[:f.readChunk]
	}
	n := copy(p, f.content[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *copyDataMemFile) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if f.writeChunk > 0 && len(p) > f.writeChunk {
		p = p[:f.writeChunk]
	}
	if end := off + int64(len(p)); end > int64(len(f.content)) {
		f.content = append(f.content, make([]byte, end-int64(len(f.content)))...)
	}
	copy(f.content[off:], p)
	return len(p), nil
}

func testContent(size int) []byte {
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i*31 + 7)
	}
	return content
}

func TestCopyDataBuffered(t *testing.T) {
	srcContent := testContent(256)

	tests := []struct {
		name              string
		srcOffset, dstOff int64
		length            uint64
		chunkSize         int
	}{
		{"disjoint ranges", 0, 512, 100, 8},
		{"adjacent ranges", 0, 100, 100, 8},
		{"single chunk", 10, 300, 50, 1024},
		{"zero length", 0, 0, 0, 8},
		{"length beyond EOF", 200, 300, 100, 8},
		{"offset beyond EOF", 300, 400, 10, 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &copyDataMemFile{content: testContent(256)}
			dst := &copyDataMemFile{}

			err := copyDataBuffered(dst, src, tt.srcOffset, tt.dstOff, tt.length, tt.chunkSize)
			require.NoError(t, err)

			end := tt.srcOffset + int64(tt.length)
			if end > int64(len(srcContent)) {
				end = int64(len(srcContent))
			}
			var copied []byte
			if end > tt.srcOffset {
				copied = srcContent[tt.srcOffset:end]
			}
			var expected []byte
			if len(copied) > 0 {
				expected = make([]byte, int(tt.dstOff)+len(copied))
				copy(expected[tt.dstOff:], copied)
			}

			if len(expected) == 0 {
				assert.Empty(t, dst.content)
			} else {
				assert.Equal(t, expected, dst.content)
			}
		})
	}
}
func TestCopyDataBufferedOverlap(t *testing.T) {
	const size = 1000

	tests := []struct {
		name              string
		srcOffset, dstOff int64
		length            uint64
		chunkSize         int
	}{
		{"dst after src, overlapping", 0, 100, 800, 64},
		{"dst after src, small chunks", 10, 25, 900, 7},
		{"dst after src, adjacent", 0, 500, 500, 64},
		{"dst before src, overlapping", 100, 0, 800, 64},
		{"dst before src, small chunks", 25, 10, 900, 7},
		{"same offset", 50, 50, 500, 32},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := testContent(size)
			f := &copyDataMemFile{content: append([]byte(nil), content...)}

			err := copyDataBuffered(f, f, tt.srcOffset, tt.dstOff, tt.length, tt.chunkSize)
			require.NoError(t, err)

			// the copy must behave like memmove: the source bytes are the
			// ones present before the copy started
			expected := testContent(size)
			copy(expected[tt.dstOff:], content[tt.srcOffset:tt.srcOffset+int64(tt.length)])

			assert.Equal(t, expected, f.content)
		})
	}
}

func TestCopyDataShortReadWrite(t *testing.T) {
	srcContent := testContent(1000)
	src := &copyDataMemFile{content: srcContent, readChunk: 3}
	dst := &copyDataMemFile{writeChunk: 5}

	err := copyData(dst, src, 0, 0, uint64(len(srcContent)))
	require.NoError(t, err)
	assert.Equal(t, srcContent, dst.content)
}

type noProgressReader struct{}

func (noProgressReader) ReadAt(p []byte, off int64) (int, error) { return 0, nil }

func TestCopyDataReadNoProgress(t *testing.T) {
	err := copyData(&copyDataMemFile{}, noProgressReader{}, 0, 0, 10)
	assert.ErrorIs(t, err, io.ErrNoProgress)
}

type zeroWriter struct{}

func (zeroWriter) WriteAt(p []byte, off int64) (int, error) { return 0, nil }

func TestCopyDataShortWrite(t *testing.T) {
	src := &copyDataMemFile{content: testContent(10)}
	err := copyData(zeroWriter{}, src, 0, 0, 10)
	assert.ErrorIs(t, err, io.ErrShortWrite)
}

var errCopyDataTest = errors.New("copy-data test error")

type failWriter struct{}

func (failWriter) WriteAt(p []byte, off int64) (int, error) { return 0, errCopyDataTest }

func TestCopyDataWriteError(t *testing.T) {
	src := &copyDataMemFile{content: testContent(10)}
	err := copyData(failWriter{}, src, 0, 0, 10)
	assert.ErrorIs(t, err, errCopyDataTest)
}

// nativeCopyMemFile implements CopyFromWriterAt to test that a native
// server-side copy is preferred over the buffered fallback.
type nativeCopyMemFile struct {
	copyDataMemFile
	called int
}

func (f *nativeCopyMemFile) CopyFrom(src io.ReaderAt, srcOffset, dstOffset int64, length uint64) (int64, error) {
	f.called++
	buf := make([]byte, length)
	n, err := src.ReadAt(buf, srcOffset)
	if err != nil && err != io.EOF {
		return 0, err
	}
	_, werr := f.copyDataMemFile.WriteAt(buf[:n], dstOffset)
	return int64(n), werr
}

var _ CopyFromWriterAt = (*nativeCopyMemFile)(nil)

func TestCopyDataNativeCopier(t *testing.T) {
	srcContent := testContent(100)
	src := &copyDataMemFile{content: srcContent}
	dst := &nativeCopyMemFile{}

	err := copyData(dst, src, 10, 0, 50)
	require.NoError(t, err)
	assert.Equal(t, 1, dst.called)
	assert.Equal(t, srcContent[10:60], dst.content)
}

func TestCopyDataPacketRoundtrip(t *testing.T) {
	pkt := &sshFxpExtendedPacketCopyData{
		ID:              42,
		ExtendedRequest: "copy-data",
		CopyFromHandle:  "from-handle",
		CopyFromOffset:  100,
		CopyDataLength:  1000,
		CopyToHandle:    "to-handle",
		CopyToOffset:    200,
	}

	data, err := pkt.MarshalBinary()
	require.NoError(t, err)

	// the server unmarshals the payload after the 4-byte length prefix and
	// the type byte
	var decoded sshFxpExtendedPacket
	require.NoError(t, decoded.UnmarshalBinary(data[5:]))

	specific, ok := decoded.SpecificPacket.(*sshFxpExtendedPacketCopyData)
	require.True(t, ok, "unexpected specific packet type %T", decoded.SpecificPacket)
	assert.Equal(t, pkt, specific)
	assert.False(t, decoded.readonly())
}

// clientRequestServerPairPipe returns a client connected to a RequestServer
// with the given handlers over an in-memory pipe.
func clientRequestServerPairPipe(t *testing.T, handlers Handlers) (*Client, *RequestServer) {
	t.Helper()

	c1, c2 := net.Pipe()
	rs := NewRequestServer(c1, handlers)
	go rs.Serve()

	client, err := NewClientPipe(c2, c2)
	require.NoError(t, err)

	t.Cleanup(func() {
		client.Close()
		rs.Close()
	})

	return client, rs
}

func TestServerCopyData(t *testing.T) {
	client, server := clientServerPair(t)
	defer client.Close()
	defer server.Close()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src")
	dstPath := filepath.Join(dir, "dst")

	content := testContent(100000)
	require.NoError(t, os.WriteFile(srcPath, content, 0o600))

	src, err := client.Open(srcPath)
	require.NoError(t, err)
	defer src.Close()

	dst, err := client.Create(dstPath)
	require.NoError(t, err)
	defer dst.Close()

	require.NoError(t, client.CopyData(src, 10, dst, 0, 50000))

	got, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	assert.Equal(t, content[10:50010], got)
}

func TestServerCopyDataSameFileOverlap(t *testing.T) {
	client, server := clientServerPair(t)
	defer client.Close()
	defer server.Close()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "file")

	content := testContent(100000)
	require.NoError(t, os.WriteFile(filePath, content, 0o600))

	f, err := client.OpenFile(filePath, os.O_RDWR)
	require.NoError(t, err)
	defer f.Close()

	// copy forward, destination overlaps and starts after the source
	require.NoError(t, client.CopyData(f, 0, f, 10000, 80000))

	expected := testContent(100000)
	copy(expected[10000:], content[:80000])

	got, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, expected, got)

	// and backwards, destination overlaps and starts before the source
	require.NoError(t, client.CopyData(f, 10000, f, 5000, 80000))

	copy(expected[5000:], expected[10000:90000])

	got, err = os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
}

func TestServerCopyDataInvalidHandle(t *testing.T) {
	client, server := clientServerPair(t)
	defer client.Close()
	defer server.Close()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(filePath, testContent(100), 0o600))

	f, err := client.OpenFile(filePath, os.O_RDWR)
	require.NoError(t, err)
	defer f.Close()

	handle, err := f.currentHandle()
	require.NoError(t, err)

	// a closed file is rejected client-side
	closed, err := client.Open(filePath)
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	assert.ErrorIs(t, client.CopyData(closed, 0, f, 0, 10), os.ErrClosed)
	assert.ErrorIs(t, client.CopyData(f, 0, closed, 0, 10), os.ErrClosed)

	// unknown handles are rejected server-side, both for the source and
	// for the destination
	for _, handles := range [][2]string{
		{"no-such-handle", handle},
		{handle, "no-such-handle"},
		{"no-such-handle", "no-such-handle-either"},
	} {
		id := client.nextID()
		typ, data, err := client.sendPacket(context.Background(), nil, &sshFxpExtendedPacketCopyData{
			ID:              id,
			ExtendedRequest: "copy-data",
			CopyFromHandle:  handles[0],
			CopyFromOffset:  0,
			CopyDataLength:  10,
			CopyToHandle:    handles[1],
			CopyToOffset:    0,
		})
		require.NoError(t, err)
		require.Equal(t, fxp(sshFxpStatus), typ)

		statusErr, ok := unmarshalStatus(id, data).(*StatusError)
		require.True(t, ok)
		assert.Equal(t, ErrSSHFxFailure, statusErr.FxCode())
	}
}

func TestServerCopyDataExtensionDisabled(t *testing.T) {
	oldExtensions := sftpExtensions
	t.Cleanup(func() { sftpExtensions = oldExtensions })
	require.NoError(t, SetSFTPExtensions("hardlink@openssh.com", "posix-rename@openssh.com", "statvfs@openssh.com"))

	client, server := clientServerPair(t)
	defer client.Close()
	defer server.Close()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src")
	dstPath := filepath.Join(dir, "dst")
	require.NoError(t, os.WriteFile(srcPath, testContent(100), 0o600))

	src, err := client.Open(srcPath)
	require.NoError(t, err)
	defer src.Close()
	dst, err := client.Create(dstPath)
	require.NoError(t, err)
	defer dst.Close()

	// the client refuses to send the request if the server did not
	// advertise the extension
	err = client.CopyData(src, 0, dst, 0, 10)
	require.Error(t, err)
	statusErr, ok := err.(*StatusError)
	require.True(t, ok)
	assert.Equal(t, ErrSSHFxOpUnsupported, statusErr.FxCode())

	// and the server rejects the request if the extension is disabled
	srcHandle, err := src.currentHandle()
	require.NoError(t, err)
	dstHandle, err := dst.currentHandle()
	require.NoError(t, err)

	id := client.nextID()
	typ, data, err := client.sendPacket(context.Background(), nil, &sshFxpExtendedPacketCopyData{
		ID:              id,
		ExtendedRequest: "copy-data",
		CopyFromHandle:  srcHandle,
		CopyFromOffset:  0,
		CopyDataLength:  10,
		CopyToHandle:    dstHandle,
		CopyToOffset:    0,
	})
	require.NoError(t, err)
	require.Equal(t, fxp(sshFxpStatus), typ)

	statusErr, ok = unmarshalStatus(id, data).(*StatusError)
	require.True(t, ok)
	assert.Equal(t, ErrSSHFxOpUnsupported, statusErr.FxCode())
}

func TestRequestServerCopyData(t *testing.T) {
	client, _ := clientRequestServerPairPipe(t, InMemHandler())

	content := testContent(100000)

	w, err := client.Create("/src")
	require.NoError(t, err)
	_, err = w.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	src, err := client.Open("/src")
	require.NoError(t, err)
	defer src.Close()

	dst, err := client.Create("/dst")
	require.NoError(t, err)
	require.NoError(t, client.CopyData(src, 10, dst, 0, 50000))
	require.NoError(t, dst.Close())

	got, err := client.Open("/dst")
	require.NoError(t, err)
	defer got.Close()
	data, err := io.ReadAll(got)
	require.NoError(t, err)
	assert.Equal(t, content[10:50010], data)
}

func TestRequestServerCopyDataSameFileOverlap(t *testing.T) {
	client, _ := clientRequestServerPairPipe(t, InMemHandler())

	content := testContent(100000)

	f, err := client.OpenFile("/file", os.O_RDWR|os.O_CREATE)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Write(content)
	require.NoError(t, err)

	require.NoError(t, client.CopyData(f, 0, f, 10000, 80000))

	expected := testContent(100000)
	copy(expected[10000:], content[:80000])

	_, err = f.Seek(0, io.SeekStart)
	require.NoError(t, err)
	got, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
}

func TestRequestServerCopyDataInvalidHandle(t *testing.T) {
	client, _ := clientRequestServerPairPipe(t, InMemHandler())

	w, err := client.Create("/file")
	require.NoError(t, err)
	_, err = w.Write(testContent(100))
	require.NoError(t, err)

	handle, err := w.currentHandle()
	require.NoError(t, err)

	id := client.nextID()
	typ, data, err := client.sendPacket(context.Background(), nil, &sshFxpExtendedPacketCopyData{
		ID:              id,
		ExtendedRequest: "copy-data",
		CopyFromHandle:  "no-such-handle",
		CopyFromOffset:  0,
		CopyDataLength:  10,
		CopyToHandle:    handle,
		CopyToOffset:    0,
	})
	require.NoError(t, err)
	require.Equal(t, fxp(sshFxpStatus), typ)

	statusErr, ok := unmarshalStatus(id, data).(*StatusError)
	require.True(t, ok)
	assert.Equal(t, ErrSSHFxFailure, statusErr.FxCode())

	require.NoError(t, w.Close())
}
