package webdav

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// pathLock serializes conditional changes to a single local path across
// processes. Each target path maps to a dedicated lock file in the OS temp
// directory, so locking never leaves stray files inside the served tree. The
// lock files themselves are reusable and not removed (removing a locked file
// would race with other processes acquiring it).
type pathLock struct {
	f *os.File
}

// lockPath acquires the exclusive lock associated with absPath. The caller
// must call unlock when done.
func lockPath(absPath string) (*pathLock, error) {
	sum := sha256.Sum256([]byte(absPath))
	name := filepath.Join(os.TempDir(), "go-webdav-lock-"+hex.EncodeToString(sum[:])+".lock")

	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return &pathLock{f: f}, nil
}

func (l *pathLock) unlock() {
	if l == nil || l.f == nil {
		return
	}
	unlockFile(l.f)
	l.f.Close()
}
