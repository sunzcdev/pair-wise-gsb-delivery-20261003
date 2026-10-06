package sftp

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalFileHandler(t *testing.T) {
	root := t.TempDir()
	h, err := NewLocalFileHandler(root)
	require.NoError(t, err)
	defer h.Close()

	require.NoError(t, os.Mkdir(filepath.Join(root, "dir"), 0o777))
	writeRequest := &Request{Method: "Put", Filepath: "/dir/target.txt", Flags: sshFxfWrite | sshFxfCreat}
	writer, err := h.Filewrite(writeRequest)
	require.NoError(t, err)
	n, err := writer.WriteAt([]byte("local backend"), 0)
	require.NoError(t, err)
	require.Equal(t, len("local backend"), n)
	require.NoError(t, writer.(io.Closer).Close())
	require.NoError(t, os.Mkdir(filepath.Join(root, "links"), 0o777))

	readRequest := &Request{Method: "Get", Filepath: "/dir/target.txt", Flags: sshFxfRead}
	reader, err := h.Fileread(readRequest)
	require.NoError(t, err)
	buf := make([]byte, len("local backend"))
	_, err = reader.ReadAt(buf, 0)
	require.NoError(t, err)
	require.Equal(t, "local backend", string(buf))
	require.NoError(t, reader.(io.Closer).Close())

	require.NoError(t, h.Filecmd(&Request{Method: "Mkdir", Filepath: "/newdir"}))
	info, err := os.Stat(filepath.Join(root, "newdir"))
	require.NoError(t, err)
	require.True(t, info.IsDir())

	require.NoError(t, h.Filecmd(&Request{Method: "Rename", Filepath: "/dir/target.txt", Target: "/newdir/target.txt"}))
	require.NoError(t, h.Filecmd(&Request{Method: "Link", Filepath: "/newdir/target.txt", Target: "/target-link.txt"}))
	requireLocalSymlinks(t, root)

	for i, target := range []string{"/newdir/target.txt", "../newdir/target.txt"} {
		linkPath := "/links/target-" + strconv.Itoa(i)
		err := h.Filecmd(&Request{Method: "Symlink", Filepath: target, Target: linkPath})
		require.NoError(t, err)

		readTarget, err := h.Readlink(linkPath)
		require.NoError(t, err)
		require.Equal(t, "../newdir/target.txt", readTarget)

		statRequest := &Request{Method: "Stat", Filepath: linkPath}
		lister, err := h.Filelist(statRequest)
		require.NoError(t, err)
		infos := make([]os.FileInfo, 1)
		_, err = lister.ListAt(infos, 0)
		require.NoError(t, err)
		require.Equal(t, "target.txt", infos[0].Name())
	}
}

func TestLocalFileHandlerRejectsEscapingSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside"), 0o666))

	h, err := NewLocalFileHandler(root)
	require.NoError(t, err)
	defer h.Close()
	require.NoError(t, os.Mkdir(filepath.Join(root, "links"), 0o777))
	requireLocalSymlinks(t, root)

	for _, target := range []string{"/../outside/sentinel", "../../outside/sentinel", "../../../outside/sentinel"} {
		err := h.Filecmd(&Request{Method: "Symlink", Filepath: target, Target: "/links/escape"})
		require.ErrorIs(t, err, os.ErrPermission, "target %q", target)
	}
	_, err = os.Lstat(filepath.Join(root, "links", "escape"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestLocalFileHandlerRenameDoesNotReplace(t *testing.T) {
	root := t.TempDir()
	h, err := NewLocalFileHandler(root)
	require.NoError(t, err)
	defer h.Close()

	require.NoError(t, os.WriteFile(filepath.Join(root, "old"), []byte("old"), 0o666))
	require.NoError(t, os.WriteFile(filepath.Join(root, "new"), []byte("new"), 0o666))

	err = h.Filecmd(&Request{Method: "Rename", Filepath: "/old", Target: "/new"})
	require.Error(t, err)
	require.True(t, os.IsExist(err) || strings.Contains(strings.ToLower(err.Error()), "file exists"), err)

	require.NoError(t, h.PosixRename(&Request{Method: "PosixRename", Filepath: "/old", Target: "/new"}))
	got, err := os.ReadFile(filepath.Join(root, "new"))
	require.NoError(t, err)
	require.Equal(t, "old", string(got))
}

func TestLocalFileHandlerVirtualPaths(t *testing.T) {
	require.Equal(t, filepath.FromSlash("sub/file"), virtualToRootPath("/sub/file"))
	require.Equal(t, ".", virtualToRootPath("/"))

	valid := []struct {
		linkPath string
		target   string
		relative string
	}{
		{"/links/a", "/newdir/target", "../newdir/target"},
		{"/links/a", "../newdir/target", "../newdir/target"},
		{"/a", "/target", "target"},
	}
	for _, tc := range valid {
		relative, err := relativeSymlinkTargetForTest(tc.linkPath, tc.target)
		require.NoError(t, err, tc.target)
		require.Equal(t, tc.relative, relative, tc.target)
	}

	for _, target := range []string{"/../outside", "../../outside", "../../../outside"} {
		_, err := relativeSymlinkTargetForTest("/links/a", target)
		require.ErrorIs(t, err, os.ErrPermission, target)
	}
}

func relativeSymlinkTargetForTest(linkPath, target string) (string, error) {
	targetComponents, err := resolvedLinkTarget(linkPath, target)
	if err != nil {
		return "", err
	}
	parentComponents, err := virtualComponentsBelowRoot(path.Dir(cleanVirtualPath(linkPath)))
	if err != nil {
		return "", err
	}
	common := 0
	for common < len(parentComponents) && common < len(targetComponents) && parentComponents[common] == targetComponents[common] {
		common++
	}
	parts := make([]string, 0, len(parentComponents)-common+len(targetComponents)-common)
	for range parentComponents[common:] {
		parts = append(parts, "..")
	}
	parts = append(parts, targetComponents[common:]...)
	if len(parts) == 0 {
		return ".", nil
	}
	return strings.Join(parts, "/"), nil
}

func requireLocalSymlinks(t *testing.T, root string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return
	}
	link := filepath.Join(root, "symlink-capability")
	if err := os.Symlink("target", link); err != nil {
		t.Skipf("this platform/process does not permit symbolic-link creation: %v", err)
	}
	require.NoError(t, os.Remove(link))
}
