//go:build windows

package webdav

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")

	lockfileExclusiveLock = uint32(0x00000002)
)

// lockFile acquires a mandatory, exclusive, blocking lock on the first byte of
// f. Locking a single shared byte serializes conditional mutations made
// through LocalFileSystem across processes without interfering with ordinary
// reads of the rest of the file.
func lockFile(f *os.File) error {
	var ol syscall.Overlapped
	const maxUint32 = 1<<32 - 1
	r1, _, errno := procLockFileEx.Call(
		f.Fd(),
		uintptr(lockfileExclusiveLock),
		0,
		uintptr(maxUint32),
		uintptr(maxUint32),
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		return errno
	}
	return nil
}

// unlockFile releases the lock acquired by lockFile.
func unlockFile(f *os.File) error {
	const maxUint32 = 1<<32 - 1
	var ol syscall.Overlapped
	r1, _, errno := procUnlockFileEx.Call(
		f.Fd(),
		0,
		uintptr(maxUint32),
		uintptr(maxUint32),
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		return errno
	}
	return nil
}
