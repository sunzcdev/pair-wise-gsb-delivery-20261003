//go:build !unix && !windows

package sftp

import (
	"os"
	"time"
)

var errLocalRootUnsupported = os.ErrInvalid

func (h *LocalFileHandler) link(_, _ string) error {
	return errLocalRootUnsupported
}

func (h *LocalFileHandler) remove(_ string) error {
	return errLocalRootUnsupported
}

func (h *LocalFileHandler) rmdir(_ string) error {
	return errLocalRootUnsupported
}

func (h *LocalFileHandler) rename(_, _ string, _ bool) error {
	return errLocalRootUnsupported
}

func (h *LocalFileHandler) chtimes(_ string, _ *os.File, _, _ time.Time) error {
	return errLocalRootUnsupported
}
