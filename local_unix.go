//go:build unix

package sftp

import (
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type localParent struct {
	file *os.File
	name string
}

func (h *LocalFileHandler) parent(name string) (localParent, error) {
	dir, base := filepath.Split(filepath.Clean(name))
	var file *os.File
	var err error
	if dir == "" {
		file, err = h.root.Open(".")
	} else {
		var subroot *os.Root
		subroot, err = h.root.OpenRoot(filepath.Clean(dir))
		if err != nil {
			return localParent{}, err
		}
		defer subroot.Close()
		file, err = subroot.Open(".")
		if err != nil {
			return localParent{}, err
		}
	}
	return localParent{file: file, name: base}, nil
}

func (p localParent) close() error {
	if p.file == nil {
		return nil
	}
	return p.file.Close()
}

func (p localParent) fd() int {
	return int(p.file.Fd())
}

func (h *LocalFileHandler) link(oldname, newname string) error {
	oldParent, err := h.parent(oldname)
	if err != nil {
		return err
	}
	defer oldParent.close()

	newParent, err := h.parent(newname)
	if err != nil {
		return err
	}
	defer newParent.close()

	return unix.Linkat(oldParent.fd(), oldParent.name, newParent.fd(), newParent.name, 0)
}

func (h *LocalFileHandler) remove(name string) error {
	p, err := h.parent(name)
	if err != nil {
		return err
	}
	defer p.close()
	return unix.Unlinkat(p.fd(), p.name, 0)
}

func (h *LocalFileHandler) rmdir(name string) error {
	p, err := h.parent(name)
	if err != nil {
		return err
	}
	defer p.close()
	return unix.Unlinkat(p.fd(), p.name, unix.AT_REMOVEDIR)
}

func (h *LocalFileHandler) rename(oldname, newname string, noreplace bool) error {
	oldParent, err := h.parent(oldname)
	if err != nil {
		return err
	}
	defer oldParent.close()

	newParent, err := h.parent(newname)
	if err != nil {
		return err
	}
	defer newParent.close()

	return renameat(oldParent.fd(), oldParent.name, newParent.fd(), newParent.name, noreplace)
}

func (h *LocalFileHandler) chtimes(_ string, file *os.File, atime, mtime time.Time) error {
	return unix.Futimes(int(file.Fd()), []unix.Timeval{
		unix.NsecToTimeval(atime.UnixNano()),
		unix.NsecToTimeval(mtime.UnixNano()),
	})
}
