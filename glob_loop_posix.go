//go:build !windows && !plan9

package sftp

import "syscall"

// errSymlinkLoop is returned after the symlink hop budget is exhausted.
var errSymlinkLoop = syscall.ELOOP
