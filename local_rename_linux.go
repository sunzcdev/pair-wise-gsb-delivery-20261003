//go:build linux

package sftp

import "golang.org/x/sys/unix"

func renameat(oldfd int, oldname string, newfd int, newname string, noreplace bool) error {
	if !noreplace {
		return unix.Renameat(oldfd, oldname, newfd, newname)
	}
	return unix.Renameat2(oldfd, oldname, newfd, newname, unix.RENAME_NOREPLACE)
}
