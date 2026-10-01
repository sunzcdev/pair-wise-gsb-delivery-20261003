//go:build linux
// +build linux

package sftp

import (
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

func localSparseCapabilities() SparseCaps {
	return SparseCaps{Discover: true, Allocate: true}
}

// osSparseFile adapts an *os.File to AllocatedRanger and RegionAllocator on
// systems that provide SEEK_DATA/SEEK_HOLE and fallocate(2).
type osSparseFile struct {
	*os.File

	sparseOnce sync.Once
	sparseErr  error
}

// OSSparseFile wraps an *os.File so it can be used as a source or destination
// with CopySparseFile on platforms that support sparse files.
func OSSparseFile(f *os.File) SparseFile { return &osSparseFile{File: f} }

// sparseAdapter exposes sparse operations for an open legacy-server handle.
func sparseAdapter(f any) any {
	if osf, ok := f.(*os.File); ok {
		return &osSparseFile{File: osf}
	}

	return f
}

func (f *osSparseFile) QueryAllocatedRanges(offset uint64, count uint32) ([]FileRange, uint64, error) {
	if count == 0 {
		count = 1
	}

	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}

	size := uint64(info.Size())

	if offset >= size {
		return nil, size, nil
	}

	var ranges []FileRange

	for offset < size && uint32(len(ranges)) < count {
		dataStart, errno := unix.Seek(int(f.Fd()), int64(offset), unix.SEEK_DATA)
		if errno != nil {
			if errno == syscall.ENXIO {
				// No more data; the rest of the file is a (possibly trailing)
				// hole.
				return ranges, size, nil
			}

			return ranges, offset, errno
		}

		dataEnd, errno := unix.Seek(int(f.Fd()), dataStart, unix.SEEK_HOLE)
		if errno != nil {
			return ranges, uint64(dataStart), errno
		}

		ranges = append(ranges, FileRange{
			Offset: uint64(dataStart),
			Length: uint64(dataEnd - dataStart),
		})

		offset = uint64(dataEnd)
	}

	return ranges, offset, nil
}

func (f *osSparseFile) AllocateRegions(ranges []FileRange) error {
	if err := validateRanges(ranges); err != nil {
		return err
	}

	for _, r := range ranges {
		// FALLOC_FL_KEEP_SIZE so a region at/past EOF reserves blocks without
		// changing the logical size; the caller controls size via Truncate.
		err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE,
			int64(r.Offset), int64(r.Length))
		if err != nil {
			return err
		}
	}

	return nil
}
