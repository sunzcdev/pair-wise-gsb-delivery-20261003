package sftp

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copyDataMemFile is an in-memory io.ReaderAt/io.WriterAt used to test the
// generic copy-data fallback. maxRead and maxWrite, when positive, cap the
// number of bytes transferred by a single ReadAt/WriteAt call, to simulate
// short reads and short writes.
type copyDataMemFile struct {
	content  []byte
	maxRead  int
	maxWrite int
	readErr  error
	writeErr error
}

func (f *copyDataMemFile) ReadAt(b []byte, off int64) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if f.maxRead > 0 && len(b) > f.maxRead {
		b = b[:f.maxRead]
	}
	if off >= int64(len(f.content)) {
		return 0, io.EOF
	}
	n := copy(b, f.content[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

func (f *copyDataMemFile) WriteAt(b []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.maxWrite > 0 && len(b) > f.maxWrite {
		b = b[:f.maxWrite]
	}
	if grow := off + int64(len(b)) - int64(len(f.content)); grow > 0 {
		f.content = append(f.content, make([]byte, grow)...)
	}
	return copy(f.content[off:], b), nil
}

// copyDataMemFileInfo is a copyDataMemFile that also provides Stat, so that
// the copy length is clamped to the file size.
type copyDataMemFileInfo struct {
	copyDataMemFile
}

func (f *copyDataMemFileInfo) Stat() (os.FileInfo, error) {
	return &memFileInfo{name: "mem", size: int64(len(f.content))}, nil
}

type memFileInfo struct {
	name string
	size int64
}

func (fi *memFileInfo) Name() string       { return fi.name }
func (fi *memFileInfo) Size() int64        { return fi.size }
func (fi *memFileInfo) Mode() os.FileMode  { return 0o644 }
func (fi *memFileInfo) ModTime() time.Time { return time.Time{} }
func (fi *memFileInfo) IsDir() bool        { return false }
func (fi *memFileInfo) Sys() any           { return nil }

// withCopyDataChunkSize temporarily reduces the copy chunk size, so that
// tests exercise the multi-chunk code paths, and restores it afterwards.
func withCopyDataChunkSize(t *testing.T, size int) {
	t.Helper()
	old := copyDataChunkSize
	copyDataChunkSize = size
	t.Cleanup(func() { copyDataChunkSize = old })
}

func TestCopyDataBasic(t *testing.T) {
	withCopyDataChunkSize(t, 4) // force multiple chunks

	src := &copyDataMemFile{content: []byte("hello world, how are you?")}
	dst := &copyDataMemFile{content: []byte(strings.Repeat(".", 25))}

	err := copyData(src, 6, 5, dst, 3, false)
	require.NoError(t, err)
	assert.Equal(t, "..."+"world"+strings.Repeat(".", 17), string(dst.content))

	// a copy may also extend the destination file
	err = copyData(src, 0, 4, dst, 25, false)
	require.NoError(t, err)
	assert.Equal(t, "..."+"world"+strings.Repeat(".", 17)+"hell", string(dst.content))
}

func TestCopyDataZeroLength(t *testing.T) {
	src := &copyDataMemFile{content: []byte("hello"), readErr: errors.New("must not be called")}
	dst := &copyDataMemFile{writeErr: errors.New("must not be called")}

	require.NoError(t, copyData(src, 0, 0, dst, 0, true))
	require.NoError(t, copyData(src, 100, 0, dst, 100, false))
}

func TestCopyDataSameFileOverlapBackward(t *testing.T) {
	withCopyDataChunkSize(t, 3) // force multiple chunks

	// copying [0,8) over [2,10) must not clobber the not-yet-read source data
	f := &copyDataMemFile{content: []byte("0123456789")}

	err := copyData(f, 0, 8, f, 2, true)
	require.NoError(t, err)
	assert.Equal(t, "0101234567", string(f.content))
}

func TestCopyDataSameFileOverlapForward(t *testing.T) {
	withCopyDataChunkSize(t, 3) // force multiple chunks

	// copying [2,10) over [0,8) is safe in the forward direction
	f := &copyDataMemFile{content: []byte("0123456789")}

	err := copyData(f, 2, 8, f, 0, true)
	require.NoError(t, err)
	assert.Equal(t, "2345678989", string(f.content))
}

func TestCopyDataSameFileAdjacent(t *testing.T) {
	f := &copyDataMemFile{content: []byte("0123456789")}

	// adjacent ranges do not overlap: [0,5) -> [5,10)
	err := copyData(f, 0, 5, f, 5, true)
	require.NoError(t, err)
	assert.Equal(t, "0123401234", string(f.content))

	// a copy onto itself is a no-op
	err = copyData(f, 0, 10, f, 0, true)
	require.NoError(t, err)
	assert.Equal(t, "0123401234", string(f.content))
}

func TestCopyDataSameFileOverlapBeyondEOF(t *testing.T) {
	withCopyDataChunkSize(t, 3)

	// The requested range extends past the end of the file; when the source
	// size is known via Stat, the copy length is clamped and the overlapping
	// copy still behaves like a memmove of the existing data.
	f := &copyDataMemFileInfo{copyDataMemFile{content: []byte("abcd")}}

	err := copyData(f, 0, 6, f, 2, true)
	require.NoError(t, err)
	assert.Equal(t, "ababcd", string(f.content))
}

func TestCopyDataShortSource(t *testing.T) {
	// Without size information, a copy requesting more data than the source
	// holds copies what is available and reports success.
	src := &copyDataMemFile{content: []byte("hello")}
	dst := &copyDataMemFile{}

	err := copyData(src, 0, 100, dst, 0, false)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(dst.content))

	// starting past the end of the source copies nothing
	dst.content = nil
	err = copyData(src, 10, 100, dst, 0, false)
	require.NoError(t, err)
	assert.Empty(t, dst.content)
}

func TestCopyDataShortReads(t *testing.T) {
	withCopyDataChunkSize(t, 16)

	// the reader never returns more than 3 bytes per call, with a nil error
	src := &copyDataMemFile{content: []byte("the quick brown fox jumps over the lazy dog"), maxRead: 3}
	dst := &copyDataMemFile{}

	err := copyData(src, 4, 39, dst, 0, false)
	require.NoError(t, err)
	assert.Equal(t, "quick brown fox jumps over the lazy dog", string(dst.content))
}

func TestCopyDataShortWrites(t *testing.T) {
	withCopyDataChunkSize(t, 16)

	// the writer never accepts more than 3 bytes per call, with a nil error
	src := &copyDataMemFile{content: []byte("the quick brown fox jumps over the lazy dog")}
	dst := &copyDataMemFile{maxWrite: 3}

	err := copyData(src, 4, 39, dst, 0, false)
	require.NoError(t, err)
	assert.Equal(t, "quick brown fox jumps over the lazy dog", string(dst.content))
}

type zeroReaderAt struct{}

func (zeroReaderAt) ReadAt([]byte, int64) (int, error) { return 0, nil }

type zeroWriterAt struct{}

func (zeroWriterAt) WriteAt([]byte, int64) (int, error) { return 0, nil }

func TestCopyDataNoProgress(t *testing.T) {
	// a reader that makes no progress at all must not spin forever
	src := &zeroReaderAt{}
	dst := &copyDataMemFile{}

	err := copyData(src, 0, 10, dst, 0, false)
	assert.ErrorIs(t, err, io.ErrNoProgress)

	// a writer that accepts no data must not spin forever
	err = copyData(&copyDataMemFile{content: []byte("hello")}, 0, 5, &zeroWriterAt{}, 0, false)
	assert.ErrorIs(t, err, io.ErrShortWrite)
}

func TestCopyDataReadWriteErrors(t *testing.T) {
	readErr := errors.New("read error")
	writeErr := errors.New("write error")

	err := copyData(&copyDataMemFile{readErr: readErr}, 0, 5, &copyDataMemFile{}, 0, false)
	assert.ErrorIs(t, err, readErr)

	err = copyData(&copyDataMemFile{content: []byte("hello")}, 0, 5, &copyDataMemFile{writeErr: writeErr}, 0, false)
	assert.ErrorIs(t, err, writeErr)
}

func TestCopyDataOverflow(t *testing.T) {
	src := &copyDataMemFile{content: []byte("hello")}
	dst := &copyDataMemFile{}

	for _, args := range [][3]uint64{
		{1 << 63, 0, 0},   // read offset overflows int64
		{1<<63 - 1, 2, 0}, // read offset+length overflows int64
		{0, 1 << 63, 0},   // length overflows int64
		{0, 5, 1 << 63},   // write offset overflows int64
		{0, 5, 1<<63 - 1}, // write offset+length overflows int64
	} {
		err := copyData(src, args[0], args[1], dst, args[2], false)
		assert.ErrorIs(t, err, fxerr(sshFxInvalidParameter), "args: %v", args)
	}
}

// copyDataNativeCopier records a native CopyDataFrom call.
type copyDataNativeCopier struct {
	copyDataMemFile
	called  bool
	srcOff  int64
	length  int64
	dstOff  int64
	copyErr error
}

func (c *copyDataNativeCopier) CopyDataFrom(src io.ReaderAt, srcOffset, length, dstOffset int64) error {
	c.called = true
	c.srcOff, c.length, c.dstOff = srcOffset, length, dstOffset
	return c.copyErr
}

func TestCopyDataNativeCopier(t *testing.T) {
	src := &copyDataMemFile{content: []byte("hello"), readErr: errors.New("fallback must not be used")}
	dst := &copyDataNativeCopier{}

	// a destination providing a native copy takes over the whole operation
	err := copyData(src, 1, 3, dst, 2, false)
	require.NoError(t, err)
	assert.True(t, dst.called)
	assert.Equal(t, int64(1), dst.srcOff)
	assert.Equal(t, int64(3), dst.length)
	assert.Equal(t, int64(2), dst.dstOff)
	assert.Empty(t, dst.content)

	// native errors are propagated
	dst.copyErr = errors.New("native copy error")
	err = copyData(src, 1, 3, dst, 2, false)
	assert.ErrorIs(t, err, dst.copyErr)
}
