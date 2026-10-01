//go:build linux

package sftp

import (
	"os"

	"golang.org/x/sys/unix"

	"github.com/pkg/sftp/internal/sparse"
)

func localSparseCapabilities() sparse.Capability {
	return sparse.CapAll
}

type linuxSparseFile struct {
	f *os.File
}

func sparseFromOSFile(f *os.File) (SparseFile, error) {
	return &linuxSparseFile{f: f}, nil
}

func openSparseFile(name string) (*os.File, SparseFile, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	return f, &linuxSparseFile{f: f}, nil
}

func openSparseFileReadWritePlatform(name string) (*os.File, SparseFile, error) {
	f, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	return f, &linuxSparseFile{f: f}, nil
}

func createSparseFile(name string) (*os.File, SparseFile, error) {
	// On Linux no pre-registration is needed: writing past EOF leaves holes,
	// and fallocate(PUNCH_HOLE) works on any regular file whose filesystem
	// supports it.
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return f, &linuxSparseFile{f: f}, nil
}

func (f *linuxSparseFile) DataExtents(off, length int64) ([]sparse.Extent, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.f.Fd()), &st); err != nil {
		return nil, &os.PathError{Op: "fstat", Path: f.f.Name(), Err: err}
	}

	size := st.Size
	if off < 0 || off > size {
		return nil, &os.PathError{Op: "seek_data", Path: f.f.Name(), Err: unix.EINVAL}
	}

	end := size
	if length > 0 && off+length < end {
		end = off + length
	}
	if off == end {
		return nil, nil
	}

	var extents []sparse.Extent
	pos := off
	for pos < end {
		data, errno := unix.Seek(int(f.f.Fd()), pos, unix.SEEK_DATA)
		if errno != nil {
			// ENXIO means there is no allocated data at or after pos:
			// the remainder of the region is a single hole.
			return extents, nil
		}
		if data >= end {
			return extents, nil
		}

		hole, errno := unix.Seek(int(f.f.Fd()), data, unix.SEEK_HOLE)
		if errno != nil {
			if errno == unix.ENXIO {
				hole = size
			} else {
				return nil, &os.PathError{Op: "seek_hole", Path: f.f.Name(), Err: errno}
			}
		}
		if hole > end {
			hole = end
		}

		extents = append(extents, sparse.Extent{
			Offset: uint64(data),
			Length: uint64(hole - data),
		})
		pos = hole
	}

	return extents, nil
}

func (f *linuxSparseFile) PunchHole(off, length int64) error {
	if length <= 0 {
		if length == 0 {
			return nil
		}
		return &os.PathError{Op: "fallocate", Path: f.f.Name(), Err: unix.EINVAL}
	}

	err := unix.Fallocate(int(f.f.Fd()),
		unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, length)
	if err != nil {
		if err == unix.EOPNOTSUPP || err == unix.ENOTTY || err == unix.EINVAL {
			return ErrSparseUnsupported
		}
		return &os.PathError{Op: "fallocate", Path: f.f.Name(), Err: err}
	}
	return nil
}
