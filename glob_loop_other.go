//go:build windows || plan9

package sftp

import "errors"

// errSymlinkLoop is returned after the symlink hop budget is exhausted.
var errSymlinkLoop = errors.New("too many symlink links")
