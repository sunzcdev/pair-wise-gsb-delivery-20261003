//go:build unix

package sftp

// These tests run only on Unix. They require the symbolic-link, hard-link,
// and *at operations used by the confined backend. Windows symbolic-link tests
// are in local_backend_test.go and skip when the process cannot create links.

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

const localRaceOperations = 100

type localRaceFixture struct {
	t       *testing.T
	root    string
	outside string
	h       *LocalFileHandler
	stop    atomic.Bool
	wg      sync.WaitGroup
}

func newLocalRaceFixture(t *testing.T) *localRaceFixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o777))
	require.NoError(t, os.MkdirAll(outside, 0o777))
	require.NoError(t, os.Symlink("../outside", filepath.Join(root, "sub-evil")))

	h, err := NewLocalFileHandler(root)
	require.NoError(t, err)
	t.Cleanup(func() { h.Close() })

	return &localRaceFixture{t: t, root: root, outside: outside, h: h}
}

func (f *localRaceFixture) start() {
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		safe := true
		for !f.stop.Load() {
			if safe {
				require.NoError(f.t, os.Rename(filepath.Join(f.root, "sub"), filepath.Join(f.root, "sub-safe")))
				require.NoError(f.t, os.Rename(filepath.Join(f.root, "sub-evil"), filepath.Join(f.root, "sub")))
			} else {
				require.NoError(f.t, os.Rename(filepath.Join(f.root, "sub"), filepath.Join(f.root, "sub-evil")))
				require.NoError(f.t, os.Rename(filepath.Join(f.root, "sub-safe"), filepath.Join(f.root, "sub")))
			}
			safe = !safe
			runtime.Gosched()
		}
		if !safe {
			_ = os.Rename(filepath.Join(f.root, "sub"), filepath.Join(f.root, "sub-evil"))
			_ = os.Rename(filepath.Join(f.root, "sub-safe"), filepath.Join(f.root, "sub"))
		}
	}()
}

func (f *localRaceFixture) finish() {
	f.stop.Store(true)
	f.wg.Wait()
}

func (f *localRaceFixture) safePath(name string) string {
	return filepath.Join(f.root, "sub", name)
}

func (f *localRaceFixture) outsidePath(name string) string {
	return filepath.Join(f.outside, name)
}

func (f *localRaceFixture) writeFile(path, content string) {
	require.NoError(f.t, os.WriteFile(path, []byte(content), 0o666))
}

func (f *localRaceFixture) assertFile(path, content string) {
	got, err := os.ReadFile(path)
	require.NoError(f.t, err)
	require.Equal(f.t, content, string(got))
}

func TestLocalFileHandlerRaceReadWrite(t *testing.T) {
	t.Log("Unix scope: symbolic links are exercised; directory handles are retained through the final *at operation")
	f := newLocalRaceFixture(t)

	for i := 0; i < localRaceOperations; i++ {
		id := strconv.Itoa(i)
		f.writeFile(f.safePath("file-"+id), "safe")
		f.writeFile(f.outsidePath("file-"+id), "outside")
		f.writeFile(f.outsidePath("write-"+id), "outside")
		f.writeFile(f.safePath("symlink-target-"+id), "safe")
		f.writeFile(f.outsidePath("symlink-target-"+id), "outside")
	}

	f.start()
	for i := 0; i < localRaceOperations; i++ {
		id := strconv.Itoa(i)
		req := &Request{Method: "Get", Filepath: "/sub/file-" + id, Flags: sshFxfRead}
		reader, err := f.h.Fileread(req)
		if err == nil {
			buf := make([]byte, len("outside"))
			n, _ := reader.ReadAt(buf, 0)
			require.NotEqual(t, "outside", string(buf[:n]))
			require.NoError(t, reader.(io.Closer).Close())
		}

		writeReq := &Request{Method: "Put", Filepath: "/sub/write-" + id, Flags: sshFxfWrite | sshFxfCreat}
		writer, err := f.h.Filewrite(writeReq)
		if err == nil {
			_, err = writer.WriteAt([]byte("safe"), 0)
			require.NoError(t, err)
			require.NoError(t, writer.(io.Closer).Close())
		}

		_ = f.h.Filecmd(&Request{
			Method:   "Symlink",
			Filepath: "symlink-target-" + id,
			Target:   "/sub/symlink-" + id,
		})
	}
	f.finish()

	for i := 0; i < localRaceOperations; i++ {
		id := strconv.Itoa(i)
		f.assertFile(f.outsidePath("file-"+id), "outside")
		f.assertFile(f.outsidePath("write-"+id), "outside")
		f.assertFile(f.outsidePath("symlink-target-"+id), "outside")
		_, err := os.Lstat(f.outsidePath("symlink-" + id))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestLocalFileHandlerRaceMkdirRemove(t *testing.T) {
	f := newLocalRaceFixture(t)

	for i := 0; i < localRaceOperations; i++ {
		id := strconv.Itoa(i)
		f.writeFile(f.safePath("remove-"+id), "safe")
		f.writeFile(f.outsidePath("remove-"+id), "outside")
		require.NoError(t, os.Mkdir(filepath.Join(f.root, "sub", "rmdir-"+id), 0o777))
		require.NoError(t, os.Mkdir(f.outsidePath("rmdir-"+id), 0o777))
		f.writeFile(f.outsidePath(filepath.Join("rmdir-"+id, "keep")), "outside")
		f.writeFile(f.outsidePath("mkdir-"+id), "outside")
	}

	f.start()
	for i := 0; i < localRaceOperations; i++ {
		id := strconv.Itoa(i)
		_ = f.h.Filecmd(&Request{Method: "Mkdir", Filepath: "/sub/mkdir-" + id})
		_ = f.h.Filecmd(&Request{Method: "Remove", Filepath: "/sub/remove-" + id})
		_ = f.h.Filecmd(&Request{Method: "Rmdir", Filepath: "/sub/rmdir-" + id})
	}
	f.finish()

	for i := 0; i < localRaceOperations; i++ {
		id := strconv.Itoa(i)
		f.assertFile(f.outsidePath("remove-"+id), "outside")
		f.assertFile(f.outsidePath("mkdir-"+id), "outside")
		_, err := os.Lstat(f.outsidePath("rmdir-" + id))
		require.NoError(t, err)
		f.assertFile(f.outsidePath(filepath.Join("rmdir-"+id, "keep")), "outside")
	}
}

func TestLocalFileHandlerRaceRenameAndLink(t *testing.T) {
	t.Log("Unix scope: both ends of Rename, PosixRename, and hard links are resolved from directory handles")
	f := newLocalRaceFixture(t)

	for i := 0; i < localRaceOperations; i++ {
		id := strconv.Itoa(i)
		f.writeFile(f.safePath("old-"+id), "safe")
		f.writeFile(f.outsidePath("old-"+id), "outside-old")
		f.writeFile(f.outsidePath("new-"+id), "outside-new")
		f.writeFile(f.outsidePath("posix-new-"+id), "outside-posix")
		f.writeFile(f.outsidePath("link-"+id), "outside-link")
	}

	f.start()
	for i := 0; i < localRaceOperations; i++ {
		id := strconv.Itoa(i)
		_ = f.h.Filecmd(&Request{Method: "Rename", Filepath: "/sub/old-" + id, Target: "/sub/new-" + id})
		_ = f.h.PosixRename(&Request{Method: "PosixRename", Filepath: "/sub/old-" + id, Target: "/sub/posix-new-" + id})
		_ = f.h.Filecmd(&Request{Method: "Link", Filepath: "/sub/old-" + id, Target: "/sub/hardlink-" + id})
	}
	f.finish()

	for i := 0; i < localRaceOperations; i++ {
		id := strconv.Itoa(i)
		f.assertFile(f.outsidePath("old-"+id), "outside-old")
		f.assertFile(f.outsidePath("new-"+id), "outside-new")
		f.assertFile(f.outsidePath("posix-new-"+id), "outside-posix")
		f.assertFile(f.outsidePath("link-"+id), "outside-link")
		_, err := os.Lstat(f.outsidePath("hardlink-" + id))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}
