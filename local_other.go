//go:build windows

package sftp

import (
	"os"
	"time"
)

func (h *LocalFileHandler) link(oldname, newname string) error {
	return h.root.Link(oldname, newname)
}

func (h *LocalFileHandler) remove(name string) error {
	info, err := h.root.Lstat(name)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return &os.PathError{Op: "unlink", Path: name, Err: os.ErrInvalid}
	}
	return h.root.Remove(name)
}

func (h *LocalFileHandler) rmdir(name string) error {
	info, err := h.root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &os.PathError{Op: "rmdir", Path: name, Err: os.ErrInvalid}
	}
	return h.root.Remove(name)
}

func (h *LocalFileHandler) rename(oldname, newname string, noreplace bool) error {
	if noreplace {
		if _, err := h.root.Lstat(newname); err == nil {
			return os.ErrExist
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return h.root.Rename(oldname, newname)
}

func (h *LocalFileHandler) chtimes(name string, _ *os.File, atime, mtime time.Time) error {
	return h.root.Chtimes(name, atime, mtime)
}
