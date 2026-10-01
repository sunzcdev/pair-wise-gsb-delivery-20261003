package sftp

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/pkg/sftp/internal/sparse"
)

// SparseTransferStats reports what a sparse transfer actually moved.
type SparseTransferStats struct {
	// LogicalSize is the logical length, in bytes, of the transferred file.
	// It includes trailing holes that were never sent as data.
	LogicalSize int64
	// DataBytes is the number of payload bytes actually carried over the
	// connection. Hole ranges contribute zero bytes.
	DataBytes int64
	// Extents is the number of allocated data ranges that were transferred.
	Extents int
	// Sparse is true when the negotiated sparse path was used, and false when
	// the transfer fell back to an ordinary dense byte stream because one end
	// lacked the required capability.
	Sparse bool
}

// DataExtents reports the allocated, non-hole byte ranges of the remote file
// overlapping the half-open region [off, off+length). A length of zero means
// "to the end of the file".
//
// This requires the server to advertise sparse.CapExtents.
func (f *File) DataExtents(off, length int64) ([]sparse.Extent, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.handle == "" {
		return nil, os.ErrClosed
	}
	if f.c.SparseCapabilities()&sparse.CapExtents == 0 {
		return nil, ErrSparseUnsupported
	}

	id := f.c.nextID()
	typ, data, err := f.c.sendPacket(context.Background(), nil, &sshFxpExtentsPacket{
		ID:     id,
		Handle: f.handle,
		Offset: uint64(off),
		Length: uint64(length),
	})
	if err != nil {
		return nil, err
	}

	switch typ {
	case sshFxpExtendedReply:
		var reply sshFxpExtentsReplyPacket
		if err := reply.UnmarshalBinary(data); err != nil {
			return nil, err
		}
		if reply.ID != id {
			return nil, &unexpectedIDErr{id, reply.ID}
		}
		return reply.Extents, nil
	case sshFxpStatus:
		return nil, normaliseError(unmarshalStatus(id, data))
	default:
		return nil, unimplementedPacketErr(typ)
	}
}

// PunchHole asks the server to turn the half-open range
// [off, off+length) of the remote file into a hole without changing its
// logical length.
//
// This requires the server to advertise sparse.CapPunch.
func (f *File) PunchHole(off, length int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.handle == "" {
		return os.ErrClosed
	}
	if f.c.SparseCapabilities()&sparse.CapPunch == 0 {
		return ErrSparseUnsupported
	}

	id := f.c.nextID()
	typ, data, err := f.c.sendPacket(context.Background(), nil, &sshFxpPunchHolePacket{
		ID:     id,
		Handle: f.handle,
		Offset: uint64(off),
		Length: uint64(length),
	})
	if err != nil {
		return err
	}

	switch typ {
	case sshFxpStatus:
		return normaliseError(unmarshalStatus(id, data))
	default:
		return &unexpectedPacketErr{want: sshFxpStatus, got: typ}
	}
}

var (
	errNegativeOffset = errors.New("sftp: negative extent offset")
	errNegativeLength = errors.New("sftp: negative extent length")
)

func validateExtent(off, length uint64) error {
	if int64(off) < 0 {
		return errNegativeOffset
	}
	if int64(length) < 0 {
		return errNegativeLength
	}
	return nil
}

// holesBetween derives the hole ranges that separate a sorted, non-overlapping
// list of data extents within [0, size).
func holesBetween(extents []sparse.Extent, size uint64) []sparse.Extent {
	var holes []sparse.Extent
	var cursor uint64
	for _, e := range extents {
		if e.Offset > cursor {
			holes = append(holes, sparse.Extent{Offset: cursor, Length: e.Offset - cursor})
		}
		stop := e.Offset + e.Length
		if stop > cursor {
			cursor = stop
		}
	}
	if cursor < size {
		holes = append(holes, sparse.Extent{Offset: cursor, Length: size - cursor})
	}
	return holes
}

// copyRange copies exactly length bytes from r at srcOff to w at dstOff.
func copyRange(w io.WriterAt, r io.ReaderAt, dstOff, srcOff, length int64) error {
	buf := make([]byte, 32*1024)
	var copied int64
	for copied < length {
		n := length - copied
		if n > int64(len(buf)) {
			n = int64(len(buf))
		}

		m, err := r.ReadAt(buf[:n], srcOff+copied)
		if m > 0 {
			if _, werr := w.WriteAt(buf[:m], dstOff+copied); werr != nil {
				return werr
			}
			copied += int64(m)
		}
		if err != nil {
			if err == io.EOF && copied >= length {
				return nil
			}
			return err
		}
	}
	return nil
}

// SparseDownload downloads the open remote file to the local destination
// directory path dst, preserving holes and logical length. When sparse support
// is not available on either end it transparently falls back to a dense
// download and reports that in the returned stats.
func (c *Client) SparseDownload(remote, dst string) (SparseTransferStats, error) {
	var stats SparseTransferStats

	rf, err := c.Open(remote)
	if err != nil {
		return stats, err
	}
	defer rf.Close()

	info, err := rf.Stat()
	if err != nil {
		return stats, err
	}
	stats.LogicalSize = info.Size()

	if c.SparseCapabilities()&sparse.CapExtents == 0 || localSparseCapabilities()&sparse.CapPunch == 0 {
		n, err := c.denseDownload(rf, dst, stats.LogicalSize)
		stats.DataBytes = n
		return stats, err
	}

	extents, err := rf.DataExtents(0, 0)
	if errors.Is(err, ErrSparseUnsupported) {
		n, err := c.denseDownload(rf, dst, stats.LogicalSize)
		stats.DataBytes = n
		return stats, err
	}
	if err != nil {
		return stats, err
	}

	lf, sf, err := createSparseTarget(dst)
	if err != nil {
		return stats, err
	}
	defer lf.Close()

	// Establish the logical length first; this also preserves a trailing hole.
	if err := lf.Truncate(stats.LogicalSize); err != nil {
		return stats, err
	}

	for _, e := range extents {
		if err := validateExtent(e.Offset, e.Length); err != nil {
			return stats, err
		}
		if err := copyRange(lf, rf, int64(e.Offset), int64(e.Offset), int64(e.Length)); err != nil {
			return stats, err
		}
		stats.DataBytes += int64(e.Length)
		stats.Extents++
	}

	for _, h := range holesBetween(extents, uint64(stats.LogicalSize)) {
		if err := sf.PunchHole(int64(h.Offset), int64(h.Length)); err != nil && !errors.Is(err, ErrSparseUnsupported) {
			return stats, err
		}
	}

	stats.Sparse = true
	return stats, nil
}

func (c *Client) denseDownload(rf *File, dst string, size int64) (int64, error) {
	lf, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer lf.Close()

	if err := lf.Truncate(size); err != nil {
		return 0, err
	}
	return size, copyRange(lf, rf, 0, 0, size)
}

// SparseUpload uploads the local file src to the remote path remote,
// preserving holes and logical length. When sparse support is not available on
// either end it transparently falls back to a dense upload and reports that in
// the returned stats.
func (c *Client) SparseUpload(src, remote string) (SparseTransferStats, error) {
	var stats SparseTransferStats

	lf, sf, err := NewSparseFile(src)
	if err != nil {
		return stats, err
	}
	defer lf.Close()

	info, err := lf.Stat()
	if err != nil {
		return stats, err
	}
	stats.LogicalSize = info.Size()

	rf, err := c.OpenFile(remote, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return stats, err
	}
	defer rf.Close()

	canSparse := c.SparseCapabilities()&sparse.CapPunch != 0 &&
		localSparseCapabilities()&sparse.CapExtents != 0
	if !canSparse {
		n, err := c.denseUpload(lf, rf, stats.LogicalSize)
		stats.DataBytes = n
		return stats, err
	}

	extents, err := sf.DataExtents(0, 0)
	if errors.Is(err, ErrSparseUnsupported) {
		n, err := c.denseUpload(lf, rf, stats.LogicalSize)
		stats.DataBytes = n
		return stats, err
	}
	if err != nil {
		return stats, err
	}

	// Preserve the logical length, including a trailing hole.
	if err := rf.Truncate(stats.LogicalSize); err != nil {
		return stats, err
	}

	for _, e := range extents {
		if err := validateExtent(e.Offset, e.Length); err != nil {
			return stats, err
		}
		if err := copyRange(rf, lf, int64(e.Offset), int64(e.Offset), int64(e.Length)); err != nil {
			return stats, err
		}
		stats.DataBytes += int64(e.Length)
		stats.Extents++
	}

	for _, h := range holesBetween(extents, uint64(stats.LogicalSize)) {
		if err := rf.PunchHole(int64(h.Offset), int64(h.Length)); err != nil && !errors.Is(err, ErrSparseUnsupported) {
			return stats, err
		}
	}

	stats.Sparse = true
	return stats, nil
}

func (c *Client) denseUpload(lf *os.File, rf *File, size int64) (int64, error) {
	if err := rf.Truncate(size); err != nil {
		return 0, err
	}
	return size, copyRange(rf, lf, 0, 0, size)
}
