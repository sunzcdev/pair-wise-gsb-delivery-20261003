//go:build !windows

package webdav

import (
	"os"
	"syscall"
)

// lockFile acquires an advisory, exclusive, process-blocking lock on f. It
// serializes conditional mutations made through LocalFileSystem across
// processes.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockFile releases the lock acquired by lockFile.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
