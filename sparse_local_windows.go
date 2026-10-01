//go:build windows

package sftp

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/pkg/sftp/internal/sparse"
)

func localSparseCapabilities() sparse.Capability {
	return sparse.CapAll
}

type windowsSparseFile struct {
	f      *os.File
	handle windows.Handle
	sparse bool
}

func sparseFromOSFile(f *os.File) (SparseFile, error) {
	return &windowsSparseFile{f: f, handle: windows.Handle(f.Fd())}, nil
}

type fileZeroDataInformation struct {
	FileOffset      int64
	BeyondFinalZero int64
}

type fileAllocatedRange struct {
	FileOffset int64
	Length     int64
}

func openSparseFile(name string) (*os.File, SparseFile, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	return f, &windowsSparseFile{f: f, handle: windows.Handle(f.Fd())}, nil
}

func openSparseFileReadWritePlatform(name string) (*os.File, SparseFile, error) {
	f, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	return f, &windowsSparseFile{f: f, handle: windows.Handle(f.Fd())}, nil
}

func createSparseFile(name string) (*os.File, SparseFile, error) {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, nil, err
	}

	sf := &windowsSparseFile{f: f, handle: windows.Handle(f.Fd())}
	if err := sf.setSparse(); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, sf, nil
}

// setSparse marks the underlying file as sparse. On non-NTFS volumes the
// request fails; the caller treats that as "sparse unsupported".
func (f *windowsSparseFile) setSparse() error {
	if f.sparse {
		return nil
	}

	var bytesReturned uint32
	// A non-nil in-buffer containing a single TRUE byte asks the driver to
	// mark the file sparse (no FILE_SET_SPARSE_BUFFER struct is needed).
	var flag byte = 1
	err := windows.DeviceIoControl(
		f.handle,
		windows.FSCTL_SET_SPARSE,
		&flag, 1,
		nil, 0,
		&bytesReturned, nil,
	)
	if err != nil {
		return mapSparseErr("set sparse", f.f.Name(), err)
	}
	f.sparse = true
	return nil
}

func (f *windowsSparseFile) DataExtents(off, length int64) ([]sparse.Extent, error) {
	fi, err := f.f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if off < 0 || off > size {
		return nil, &os.PathError{Op: "query allocated ranges", Path: f.f.Name(), Err: windows.ERROR_INVALID_PARAMETER}
	}

	end := size
	if length > 0 && off+length < end {
		end = off + length
	}

	in := fileAllocatedRange{FileOffset: off, Length: end - off}

	// NTFS returns one FILE_ALLOCATED_RANGE_BUFFER per allocated run.
	n := int((end-off)/4096) + 1
	out := make([]fileAllocatedRange, n)
	var bytesReturned uint32
	err = windows.DeviceIoControl(
		f.handle,
		windows.FSCTL_QUERY_ALLOCATED_RANGES,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		(*byte)(unsafe.Pointer(&out[0])), uint32(len(out)*int(unsafe.Sizeof(out[0]))),
		&bytesReturned, nil,
	)
	// ERROR_MORE_DATA means the output buffer was too small; grow it and retry.
	for err == windows.ERROR_MORE_DATA {
		n *= 2
		out = make([]fileAllocatedRange, n)
		err = windows.DeviceIoControl(
			f.handle,
			windows.FSCTL_QUERY_ALLOCATED_RANGES,
			(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
			(*byte)(unsafe.Pointer(&out[0])), uint32(len(out)*int(unsafe.Sizeof(out[0]))),
			&bytesReturned, nil,
		)
	}
	if err != nil {
		return nil, mapSparseErr("query allocated ranges", f.f.Name(), err)
	}

	count := int(bytesReturned) / int(unsafe.Sizeof(out[0]))
	extents := make([]sparse.Extent, 0, count)
	for i := 0; i < count; i++ {
		start := out[i].FileOffset
		stop := start + out[i].Length
		if start < off {
			start = off
		}
		if stop > end {
			stop = end
		}
		if stop > start {
			extents = append(extents, sparse.Extent{
				Offset: uint64(start),
				Length: uint64(stop - start),
			})
		}
	}
	return extents, nil
}

func (f *windowsSparseFile) PunchHole(off, length int64) error {
	if length == 0 {
		return nil
	}
	if length < 0 {
		return &os.PathError{Op: "zero data", Path: f.f.Name(), Err: windows.ERROR_INVALID_PARAMETER}
	}
	if err := f.setSparse(); err != nil {
		return err
	}

	in := fileZeroDataInformation{
		FileOffset:      off,
		BeyondFinalZero: off + length,
	}
	var bytesReturned uint32
	err := windows.DeviceIoControl(
		f.handle,
		windows.FSCTL_SET_ZERO_DATA,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		nil, 0,
		&bytesReturned, nil,
	)
	if err != nil {
		return mapSparseErr("zero data", f.f.Name(), err)
	}
	return nil
}

func mapSparseErr(op, name string, err error) error {
	if err == windows.ERROR_INVALID_FUNCTION ||
		err == windows.ERROR_NOT_SUPPORTED ||
		err == windows.ERROR_INVALID_PARAMETER {
		return ErrSparseUnsupported
	}
	return &os.PathError{Op: op, Path: name, Err: err}
}
