//go:build darwin
// +build darwin

package sftp

import (
	"os"

	"golang.org/x/sys/unix"
)

// macOS does not expose SEEK_DATA/SEEK_HOLE, so discovery is unavailable.
// fcntl(F_PREALLOCATE) can still reserve real blocks.
func localSparseCapabilities() SparseCaps {
	return SparseCaps{Allocate: true}
}

type osSparseFile struct {
	*os.File
}

// OSSparseFile wraps an *os.File so it can reserve allocated regions with
// CopySparseFile on macOS.
func OSSparseFile(f *os.File) SparseFile { return &osSparseFile{File: f} }

// sparseAdapter exposes sparse operations for an open legacy-server handle.
func sparseAdapter(f any) any {
	if osf, ok := f.(*os.File); ok {
		return &osSparseFile{File: osf}
	}

	return f
}

func (f *osSparseFile) QueryAllocatedRanges(offset uint64, count uint32) ([]FileRange, uint64, error) {
	return nil, 0, errSparseUnsupported
}

func (f *osSparseFile) AllocateRegions(ranges []FileRange) error {
	if err := validateRanges(ranges); err != nil {
		return err
	}

	// Allocate each region individually. F_PRECONTIG is intentionally omitted
	// so the filesystem may use non-contiguous extents.
	for _, r := range ranges {
		st := unix.Fstore_t{
			Flags:  unix.F_ALLOCATEALL | unix.F_ALLOCATECONTIG,
			Offset: int64(r.Offset),
			Length: int64(r.Length),
		}

		// First attempt a contiguous reservation without changing size.
		if err := unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &st); err != nil {
			// Fall back to a possibly non-contiguous reservation.
			st.Flags = unix.F_ALLOCATEALL
			if err := unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &st); err != nil {
				return err
			}
		}
	}

	return nil
}
