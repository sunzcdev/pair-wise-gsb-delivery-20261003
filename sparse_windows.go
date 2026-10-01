//go:build windows
// +build windows

package sftp

import (
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

func localSparseCapabilities() SparseCaps {
	return SparseCaps{Discover: true, Allocate: true}
}

type osSparseFile struct {
	*os.File

	sparseOnce sync.Once
	sparseErr  error
}

// OSSparseFile wraps an *os.File so it can be used as a source or destination
// with CopySparseFile on Windows NTFS.
func OSSparseFile(f *os.File) SparseFile { return &osSparseFile{File: f} }

// sparseAdapter exposes sparse operations for an open legacy-server handle.
func sparseAdapter(f any) any {
	if osf, ok := f.(*os.File); ok {
		return &osSparseFile{File: osf}
	}

	return f
}

func unsafePointer[T any](p *T) unsafe.Pointer { return unsafe.Pointer(p) }

// fileAllocatedRangeBuffer is the input structure for
// FSCTL_QUERY_ALLOCATED_RANGES.
type fileAllocatedRangeBuffer struct {
	FileOffset uint64
	Length     uint64
}

// fileAllocatedRange is one entry of the FSCTL_QUERY_ALLOCATED_RANGES output.
type fileAllocatedRange struct {
	FileOffset uint64
	Length     uint64
}

// markSparse marks the underlying file as sparse; required on NTFS before
// zeroed regions are deallocated.
func (f *osSparseFile) markSparse() error {
	f.sparseOnce.Do(func() {
		var returned uint32

		f.sparseErr = windows.DeviceIoControl(
			windows.Handle(f.Fd()),
			windows.FSCTL_SET_SPARSE,
			nil, 0,
			nil, 0,
			&returned, nil,
		)
	})

	return f.sparseErr
}

// MarkSparse marks the underlying file as sparse. It is exposed so callers
// creating a new file that will contain holes can opt in before writing.
func (f *osSparseFile) MarkSparse() error { return f.markSparse() }

func (f *osSparseFile) QueryAllocatedRanges(offset uint64, count uint32) ([]FileRange, uint64, error) {
	if count == 0 {
		count = 1
	}

	// FSCTL_QUERY_ALLOCATED_RANGES collapses everything to a single range for
	// non-sparse files; marking the file sparse is required for holes to be
	// reported. A read-only handle cannot change the sparse attribute; in that
	// case the query simply reports a single dense range.
	_ = f.markSparse()

	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}

	size := uint64(info.Size())
	if offset >= size {
		return nil, size, nil
	}

	in := fileAllocatedRangeBuffer{
		FileOffset: offset,
		Length:     size - offset,
	}

	out := make([]byte, 16*count)

	var returned uint32

	err = windows.DeviceIoControl(
		windows.Handle(f.Fd()),
		windows.FSCTL_QUERY_ALLOCATED_RANGES,
		(*byte)(unsafePointer(&in)),
		uint32(16),
		&out[0],
		uint32(len(out)),
		&returned,
		nil,
	)
	if err != nil {
		// ERROR_MORE_DATA just means the buffer was too small; partial
		// results returned are still valid.
		if werr, ok := err.(windows.Errno); !ok || werr != windows.ERROR_MORE_DATA {
			return nil, offset, err
		}
	}

	n := returned / 16
	ranges := make([]FileRange, 0, n)

	next := offset

	for i := uint32(0); i < n; i++ {
		e := *(*fileAllocatedRange)(unsafePointer(&out[i*16]))
		ranges = append(ranges, FileRange{
			Offset: e.FileOffset,
			Length: e.Length,
		})
		next = e.FileOffset + e.Length
	}

	if len(ranges) == 0 {
		return nil, size, nil
	}

	return ranges, next, nil
}

func (f *osSparseFile) AllocateRegions(ranges []FileRange) error {
	if err := validateRanges(ranges); err != nil {
		return err
	}

	if err := f.markSparse(); err != nil {
		return err
	}

	// Deallocate the gaps before/between the requested regions. The data
	// regions are left for the subsequent real byte writes to allocate.
	var holeStart uint64

	for _, r := range ranges {
		if r.Offset > holeStart {
			if err := f.punchHole(holeStart, r.Offset-holeStart); err != nil {
				return err
			}
		}

		holeStart = r.End()
	}

	return nil
}

// punchHole deallocates a region by issuing FSCTL_SET_ZERO_DATA on a sparse
// file. On NTFS this releases the clusters and leaves a genuine hole.
func (f *osSparseFile) punchHole(offset, length uint64) error {
	var returned uint32

	z := windows.FileZeroDataInformation{
		FileOffset:      int64(offset),
		BeyondFinalZero: int64(offset + length),
	}

	return windows.DeviceIoControl(
		windows.Handle(f.Fd()),
		windows.FSCTL_SET_ZERO_DATA,
		(*byte)(unsafePointer(&z)),
		uint32(unsafe.Sizeof(z)),
		nil, 0,
		&returned, nil,
	)
}
