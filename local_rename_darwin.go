//go:build darwin

package sftp

import "golang.org/x/sys/unix"

func renameat(oldfd int, oldname string, newfd int, newname string, noreplace bool) error {
	if noreplace {
		return unix.RenameatxNp(oldfd, oldname, newfd, newname, unix.RENAME_EXCL)
	}
	return unix.Renameat(oldfd, oldname, newfd, newname)
}
