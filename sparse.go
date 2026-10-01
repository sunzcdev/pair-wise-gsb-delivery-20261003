package sftp

import (
	"context"
	"errors"
	"io"
	"os"
)

var (
	// ErrSparseNotSupported is returned when the remote peer did not negotiate
	// the required sparse-file capability.
	ErrSparseNotSupported = errors.New("sftp: sparse-file extension not supported by peer")

	// errSparseUnsupported is used locally when a platform cannot perform a
	// particular sparse operation.
	errSparseUnsupported = ErrSSHFxOpUnsupported
)

// SparseCapabilities reports the sparse-file capabilities advertised by the
// server during the SSH_FXP_VERSION negotiation.
func (c *Client) SparseCapabilities() SparseCaps {
	data, ok := c.HasExtension(sparseFileExtension)
	if !ok {
		return SparseCaps{}
	}

	return parseSparseCaps(data)
}

// LocalSparseCapabilities reports which sparse-file operations the local
// operating system provides for regular files. It reflects the support
// advertised by the built-in filesystem Server.
func LocalSparseCapabilities() SparseCaps {
	return localSparseCapabilities()
}

// QueryAllocatedRanges asks the server for up to count allocated regions of the
// file referenced by handle, starting at or after offset.
//
// The returned ranges are ordered and non-overlapping. nextOffset is the file
// offset at which the caller should continue scanning: either the start of the
// first hole after the returned ranges, or the logical size of the file when
// there is no more allocated data.
//
// It returns ErrSparseNotSupported if the server did not negotiate the
// "discover" capability.
func (c *Client) QueryAllocatedRanges(ctx context.Context, handle string, offset uint64, count uint32) (ranges []FileRange, nextOffset uint64, err error) {
	if !c.SparseCapabilities().Discover {
		return nil, 0, ErrSparseNotSupported
	}

	id := c.nextID()
	typ, data, err := c.sendPacket(ctx, nil, &sshFxpQueryAllocatedPacket{
		ID:      id,
		Handle:  handle,
		Offset:  offset,
		Regions: count,
	})
	if err != nil {
		return nil, 0, err
	}

	switch typ {
	case sshFxpExtendedReply:
		var reply sshFxpAllocatedRangesPacket
		if err := reply.unmarshalBody(id, data); err != nil {
			return nil, 0, err
		}

		return reply.Ranges, reply.Offset, nil

	case sshFxpStatus:
		return nil, 0, normaliseError(unmarshalStatus(id, data))

	default:
		return nil, 0, unimplementedPacketErr(typ)
	}
}

// AllocateRegions asks the server to reserve the given ordered, non-overlapping
// regions of the file referenced by handle as real, allocated storage.
// Gaps between the regions are left as holes, so only real data needs to be
// transmitted.
//
// It returns ErrSparseNotSupported if the server did not negotiate the
// "allocate" capability.
func (c *Client) AllocateRegions(ctx context.Context, handle string, ranges []FileRange) error {
	if !c.SparseCapabilities().Allocate {
		return ErrSparseNotSupported
	}

	id := c.nextID()
	typ, data, err := c.sendPacket(ctx, nil, &sshFxpAllocateRegionsPacket{
		ID:     id,
		Handle: handle,
		Ranges: ranges,
	})
	if err != nil {
		return err
	}

	switch typ {
	case sshFxpExtendedReply, sshFxpStatus:
		return normaliseError(unmarshalStatus(id, data))

	default:
		return unimplementedPacketErr(typ)
	}
}

// QueryAllocatedRangesCtx is the File-handle, context-aware variant of
// Client.QueryAllocatedRanges.
func (f *File) QueryAllocatedRangesCtx(ctx context.Context, offset uint64, count uint32) ([]FileRange, uint64, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.handle == "" {
		return nil, 0, os.ErrClosed
	}

	return f.c.QueryAllocatedRanges(ctx, f.handle, offset, count)
}

// AllocateRegionsCtx is the File-handle, context-aware variant of
// Client.AllocateRegions.
func (f *File) AllocateRegionsCtx(ctx context.Context, ranges []FileRange) error {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.handle == "" {
		return os.ErrClosed
	}

	return f.c.AllocateRegions(ctx, f.handle, ranges)
}

// QueryAllocatedRanges implements AllocatedRanger using a background context,
// so that an *sftp.File can be passed directly to CopySparseFile.
func (f *File) QueryAllocatedRanges(offset uint64, count uint32) ([]FileRange, uint64, error) {
	return f.QueryAllocatedRangesCtx(context.Background(), offset, count)
}

// AllocateRegions implements RegionAllocator using a background context, so
// that an *sftp.File can be passed directly to CopySparseFile.
func (f *File) AllocateRegions(ranges []FileRange) error {
	return f.AllocateRegionsCtx(context.Background(), ranges)
}

// SparseDiscoverSupported reports whether the negotiated peer supports the
// discover direction of the sparse-file extension.
func (f *File) SparseDiscoverSupported() bool {
	return f.c.SparseCapabilities().Discover
}

// SparseAllocateSupported reports whether the negotiated peer supports the
// allocate direction of the sparse-file extension.
func (f *File) SparseAllocateSupported() bool {
	return f.c.SparseCapabilities().Allocate
}

// AllocatedRanger is implemented by a file backend that can report the
// regions of a file backed by real, allocated storage. Regions between the
// reported ranges are holes.
//
// QueryAllocatedRanges returns up to count allocated regions at or after
// offset. nextOffset is the offset of the first hole after the returned
// ranges, or the logical file size once all allocated data has been reported.
// A region of explicitly written zero bytes is allocated and must be
// reported; only unwritten holes are skipped.
type AllocatedRanger interface {
	QueryAllocatedRanges(offset uint64, count uint32) (ranges []FileRange, nextOffset uint64, err error)
}

// RegionAllocator is implemented by a file backend that can reserve ordered,
// non-overlapping regions as real storage. The gaps between regions are left
// as holes rather than being filled with allocated zero blocks.
type RegionAllocator interface {
	AllocateRegions(ranges []FileRange) error
}

// SparseFile is implemented by a concrete file handle that supports both
// discovering allocated regions and recreating holes, in addition to ordinary
// byte I/O and Truncate. OSSparseFile returns such a handle on platforms with
// real sparse-file support.
type SparseFile interface {
	io.ReaderAt
	io.WriterAt
	AllocatedRanger
	RegionAllocator
	Truncate(size int64) error
}

// SparseDiscoverCapab is an optional interface that reports whether a source
// can really discover allocated regions after protocol negotiation. Remote
// sftp.File handles implement it based on the server-advertised capability;
// local backends that implement AllocatedRanger are assumed capable.
type SparseDiscoverCapab interface {
	SparseDiscoverSupported() bool
}

// SparseAllocateCapab is the destination-side counterpart for hole creation.
type SparseAllocateCapab interface {
	SparseAllocateSupported() bool
}

// SparseMarker is implemented by platform file handles on which a file must be
// explicitly marked sparse before holes can be discovered or created (NTFS).
// On POSIX systems OSSparseFile does not implement it.
type SparseMarker interface {
	MarkSparse() error
}

// MarkSparse marks f as a sparse file if the platform requires it; otherwise
// it is a no-op. Pass the value returned by OSSparseFile.
func MarkSparse(f SparseFile) error {
	if marker, ok := f.(SparseMarker); ok {
		return marker.MarkSparse()
	}

	return nil
}

// sparseCapability reports whether the given endpoint effectively supports
// the requested direction. Endpoints without the optional probe interface are
// assumed capable if they implement the operation interface.
func sparseCapability(endpoint any, discover bool) bool {
	if discover {
		probe, ok := endpoint.(SparseDiscoverCapab)
		return !ok || probe.SparseDiscoverSupported()
	}

	probe, ok := endpoint.(SparseAllocateCapab)
	return !ok || probe.SparseAllocateSupported()
}

// SparseCopyStats reports the outcome of a sparse file transfer.
type SparseCopyStats struct {
	// LogicalSize is the logical length in bytes of the transferred file,
	// including any trailing hole.
	LogicalSize int64
	// BytesTransferred is the number of payload bytes actually sent for
	// allocated regions. It excludes holes that were skipped.
	BytesTransferred int64
	// Sparse is true when the sparse-file extension was negotiated and holes
	// were skipped; false when the file was sent as a plain byte stream.
	Sparse bool
}

const sparseCopyChunkSize = int64(32 * 1024)

// CopySparseFile copies the logical contents of src into dst, skipping holes
// when both ends implement the sparse-file extension.
//
// src must implement io.ReaderAt and may additionally implement
// AllocatedRanger. dst must implement io.WriterAt and may additionally
// implement RegionAllocator plus Truncate.
//
// Explicitly allocated zero data is always transferred: only real filesystem
// holes are skipped, so the destination cannot confuse a hole with data that
// happens to be all zeros. The logical file size, including a trailing hole,
// is preserved.
//
// If either side lacks the required capability, the file is copied as a plain
// byte stream and Sparse is reported as false.
func CopySparseFile(dst io.WriterAt, src io.ReaderAt, size int64) (SparseCopyStats, error) {
	stats := SparseCopyStats{LogicalSize: size}

	ranger, canDiscover := src.(AllocatedRanger)
	allocator, canAllocate := dst.(RegionAllocator)
	truncater, canTruncate := dst.(interface{ Truncate(int64) error })

	// Local type support is necessary but not sufficient: a remote sftp.File
	// always implements the interfaces, but the extension may not have been
	// negotiated with the peer. The optional SparseCapab interfaces below let
	// an endpoint report its effective (post-negotiation) capability.
	sparse := canDiscover && canAllocate && canTruncate &&
		sparseCapability(src, true) && sparseCapability(dst, false)

	if !sparse {
		buf := make([]byte, sparseCopyChunkSize)
		n, err := copyByteRange(dst, src, buf, 0, size)
		stats.BytesTransferred = n

		return stats, err
	}

	stats.Sparse = true

	// Establish the logical length first so that a trailing hole survives even
	// before any data region is written.
	if err := truncater.Truncate(size); err != nil {
		return stats, err
	}

	buf := make([]byte, sparseCopyChunkSize)

	var offset uint64

	for offset < uint64(size) {
		ranges, next, err := ranger.QueryAllocatedRanges(offset, 64)
		if err != nil {
			return stats, err
		}

		// next points at the following hole or logical EOF. When ranges are
		// returned, the scan resumes after the last region (which may equal
		// next, or precede it when the batch was capped). When nothing is
		// returned, next skips straight over the hole (up to EOF).
		scanTo := next

		if len(ranges) > 0 {
			if err := allocator.AllocateRegions(ranges); err != nil {
				return stats, err
			}

			for _, region := range ranges {
				n, err := copyRegion(dst, src, buf, region)
				stats.BytesTransferred += n

				if err != nil {
					return stats, err
				}

				if region.End() > scanTo {
					scanTo = region.End()
				}
			}
		}

		if scanTo <= offset {
			// A broken backend must not pin the scan into an infinite loop.
			return stats, errors.New("sftp: query-allocated returned non-advancing offset")
		}

		offset = scanTo
	}

	return stats, nil
}

// copyRegion streams the bytes of a single allocated region, in fixed-size
// chunks.
func copyRegion(dst io.WriterAt, src io.ReaderAt, buf []byte, region FileRange) (int64, error) {
	var transferred int64

	remaining := int64(region.Length)
	offset := int64(region.Offset)

	for remaining > 0 {
		n, err := copyByteRange(dst, src, buf, offset, min64(remaining, int64(len(buf))))
		transferred += n
		offset += n
		remaining -= n

		if err != nil {
			return transferred, err
		}
	}

	return transferred, nil
}

// copyByteRange copies up to length bytes from src at the given offset into
// dst, looping over short reads and short writes.
func copyByteRange(dst io.WriterAt, src io.ReaderAt, buf []byte, offset, length int64) (int64, error) {
	var transferred int64

	for transferred < length {
		want := int64(len(buf))
		if length-transferred < want {
			want = length - transferred
		}

		nr, err := io.ReadFull(io.NewSectionReader(src, offset+transferred, want), buf[:want])
		if nr > 0 {
			nw, werr := writeFullAt(dst, buf[:nr], offset+transferred)
			transferred += int64(nw)

			if werr != nil {
				return transferred, werr
			}
		}

		if err != nil {
			if err == io.EOF {
				return transferred, nil
			}

			return transferred, err
		}
	}

	return transferred, nil
}

// writeFullAt writes all of p at off, handling short writes.
func writeFullAt(w io.WriterAt, p []byte, off int64) (int, error) {
	var n int

	for n < len(p) {
		nw, err := w.WriteAt(p[n:], off+int64(n))
		if nw > 0 {
			n += nw
		}

		if err != nil {
			return n, err
		}

		if nw == 0 {
			return n, io.ErrShortWrite
		}
	}

	return n, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}

	return b
}
