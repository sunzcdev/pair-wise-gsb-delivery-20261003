//go:build unix && !darwin && !linux

package sftp

import "golang.org/x/sys/unix"

func renameat(oldfd int, oldname string, newfd int, newname string, _ bool) error {
	return unix.Renameat(oldfd, oldname, newfd, newname)
}
