package sftp

import (
	"errors"
	"os"

	"github.com/pkg/sftp/internal/sparse"
)

// ErrSparseUnsupported reports that the local filesystem or platform cannot
// perform the requested sparse operation. Sparse transfers fall back to
// ordinary dense transfer in that case.
var ErrSparseUnsupported = errors.New("sftp: sparse operation not supported")

// DataExtent describes one allocated, non-hole byte range of a file. It is a
// half-open range [Offset, Offset+Length).
type DataExtent = sparse.Extent

// SparseFile is implemented by an open local file that can report its
// allocated data extents and punch holes.
type SparseFile interface {
	// DataExtents returns the allocated, non-hole byte ranges that overlap
	// the half-open region [off, off+length). A length of zero means "to the
	// end of the file". Ranges fully contained in a hole are omitted, so a
	// region that merely happens to contain allocated zeroes is still
	// reported.
	DataExtents(off, length int64) ([]sparse.Extent, error)
	// PunchHole turns the half-open region [off, off+length) into a hole
	// without changing the logical length of the file. Reads from the range
	// subsequently return zeroes.
	PunchHole(off, length int64) error
}

// SparseSupport reports the sparse operations this operating system can
// perform on regular files. Individual filesystems may support fewer
// operations at runtime, in which case the failing method returns
// ErrSparseUnsupported.
func SparseSupport() sparse.Capability {
	return localSparseCapabilities()
}

// NewSparseFile opens name for reading and, if the platform supports it,
// returns a SparseFile view of it. The returned *os.File must be closed by
// the caller.
func NewSparseFile(name string) (*os.File, SparseFile, error) {
	if localSparseCapabilities() == 0 {
		return nil, nil, ErrSparseUnsupported
	}
	return openSparseFile(name)
}

// openSparseFileReadWrite opens name for reading and writing and returns a
// SparseFile view of it, used to punch holes in an existing file.
func openSparseFileReadWrite(name string) (*os.File, SparseFile, error) {
	if localSparseCapabilities() == 0 {
		return nil, nil, ErrSparseUnsupported
	}
	return openSparseFileReadWritePlatform(name)
}

// createSparseTarget creates or truncates name for writing and arranges for
// the file to be sparse where the platform requires explicit setup. The
// returned *os.File must be closed by the caller.
func createSparseTarget(name string) (*os.File, SparseFile, error) {
	if localSparseCapabilities() == 0 {
		return nil, nil, ErrSparseUnsupported
	}
	return createSparseFile(name)
}

// CreateSparseFile creates (or truncates) name for reading and writing and
// arranges for the file to be sparse where the platform requires explicit
// setup. The returned *os.File must be closed by the caller.
//
// On platforms with no sparse support it returns ErrSparseUnsupported.
func CreateSparseFile(name string) (*os.File, SparseFile, error) {
	return createSparseTarget(name)
}
