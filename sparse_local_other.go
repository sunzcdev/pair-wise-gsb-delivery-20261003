//go:build !linux && !windows

package sftp

import (
	"os"

	"github.com/pkg/sftp/internal/sparse"
)

func localSparseCapabilities() sparse.Capability { return 0 }

func sparseFromOSFile(f *os.File) (SparseFile, error) {
	return nil, ErrSparseUnsupported
}

func openSparseFile(name string) (*os.File, SparseFile, error) {
	return nil, nil, ErrSparseUnsupported
}

func openSparseFileReadWritePlatform(name string) (*os.File, SparseFile, error) {
	return nil, nil, ErrSparseUnsupported
}

func createSparseFile(name string) (*os.File, SparseFile, error) {
	return nil, nil, ErrSparseUnsupported
}
