package sftp

import (
	"errors"
	"io"
	"math"
	"os"
)

// copyDataChunkSize bounds the buffers used by the generic copy-data fallback,
// so that arbitrarily large ranges can be copied with a bounded amount of memory.
// It is a variable so that tests can exercise the multi-chunk code paths.
var copyDataChunkSize = 32 * 1024

// DataCopier is an optional interface that an io.WriterAt returned by a
// FileWriter or OpenFileWriter handler (or served by a Server backend) can
// implement to provide a native, server-side data copy for the copy-data
// extension, e.g. reflinks, copy_file_range or a remote-object copy.
//
// CopyDataFrom must copy length bytes read from src starting at srcOffset
// into the receiver starting at dstOffset. If src and the receiver refer to
// the same file and the ranges overlap, the copy must behave as if the
// source range had been fully read before any byte is written.
//
// If a destination does not implement DataCopier, the server falls back to
// a generic bounded-buffer copy.
type DataCopier interface {
	CopyDataFrom(src io.ReaderAt, srcOffset, length, dstOffset int64) error
}

// copyData copies length bytes read from src starting at srcOffset into dst
// starting at dstOffset.
//
// If fewer than length bytes can be read from src (EOF), the copy stops
// there and no error is reported. Short reads and short writes that still
// make progress are transparently retried.
//
// sameFile must report whether src and dst refer to the same underlying
// file; when in doubt it should be true. If the source and destination
// ranges of the same file overlap, the copy is performed as if the source
// range had been read in full before writing anything (memmove semantics),
// so that writes cannot clobber source data that has not been read yet.
func copyData(src io.ReaderAt, srcOffset, length uint64, dst io.WriterAt, dstOffset uint64, sameFile bool) error {
	// Guard against offsets and lengths that cannot be represented as int64,
	// or whose sums would overflow.
	if srcOffset > math.MaxInt64 || length > math.MaxInt64 || srcOffset+length > math.MaxInt64 ||
		dstOffset > math.MaxInt64 || dstOffset+length > math.MaxInt64 {
		return fxerr(sshFxInvalidParameter)
	}

	if length == 0 {
		return nil
	}

	srcOff, dstOff, dataLen := int64(srcOffset), int64(dstOffset), int64(length)

	// If the source size is known, clamp the copy length to the data that
	// actually exists, so that hitting EOF does not depend on the direction
	// in which the copy is performed.
	if st, ok := src.(interface{ Stat() (os.FileInfo, error) }); ok {
		if info, err := st.Stat(); err == nil && info.Mode().IsRegular() {
			switch size := info.Size(); {
			case size <= srcOff:
				return nil
			case size < srcOff+dataLen:
				dataLen = size - srcOff
			}
		}
	}

	// A backend providing a native copy takes over entirely, including
	// any same-file overlap handling.
	if copier, ok := dst.(DataCopier); ok {
		return copier.CopyDataFrom(src, srcOff, dataLen, dstOff)
	}

	// When the ranges of the same file overlap and the destination starts
	// after the source, copying backwards (from the end of the range)
	// guarantees that every write only touches data that has already been
	// read. Overlapping ranges with dstOff <= srcOff are safe to copy
	// forwards, as are ranges of different files.
	backward := sameFile && dstOff > srcOff && dstOff < srcOff+dataLen

	chunkSize := int64(copyDataChunkSize)
	if chunkSize <= 0 {
		chunkSize = 32 * 1024
	}
	buf := make([]byte, min(chunkSize, dataLen))

	for copied := int64(0); copied < dataLen; {
		chunk := min(int64(len(buf)), dataLen-copied)

		readOff := srcOff + copied
		writeOff := dstOff + copied
		if backward {
			readOff = srcOff + dataLen - copied - chunk
			writeOff = dstOff + dataLen - copied - chunk
		}

		n, err := readAtFull(src, buf[:chunk], readOff)
		if n > 0 {
			if werr := writeAtFull(dst, buf[:n], writeOff); werr != nil {
				return werr
			}
			copied += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The source is shorter than expected (it may have
				// been truncated concurrently): stop the copy here,
				// keeping what has been copied so far.
				return nil
			}
			return err
		}
	}

	return nil
}

// readAtFull reads exactly len(buf) bytes from src, tolerating short reads
// that still make progress. It returns the number of bytes read; a short
// total is only ever returned together with a non-nil error.
func readAtFull(src io.ReaderAt, buf []byte, off int64) (int, error) {
	var total int
	for total < len(buf) {
		n, err := src.ReadAt(buf[total:], off+int64(total))
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
	return total, nil
}

// writeAtFull writes all of buf to dst, tolerating short writes that still
// make progress.
func writeAtFull(dst io.WriterAt, buf []byte, off int64) error {
	for len(buf) > 0 {
		n, err := dst.WriteAt(buf, off)
		if n > 0 {
			off += int64(n)
			buf = buf[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// sameFileInfo reports whether src and dst refer to the same underlying file
// by comparing Stat results. If the comparison cannot be made it
// conservatively returns true, so that callers fall back to the copy
// direction that is always safe for overlapping ranges of the same file.
func sameFileInfo(src io.ReaderAt, dst io.WriterAt) bool {
	type statter interface {
		Stat() (os.FileInfo, error)
	}

	s, sok := src.(statter)
	d, dok := dst.(statter)
	if !sok || !dok {
		return false
	}

	sinfo, serr := s.Stat()
	dinfo, derr := d.Stat()
	if serr != nil || derr != nil {
		return true
	}

	return os.SameFile(sinfo, dinfo)
}
