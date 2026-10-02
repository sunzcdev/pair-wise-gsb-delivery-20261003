//go:build !windows && !plan9

package sftp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGlobStreamSymlinkLoop builds a -> b -> a directory symlink cycle under
// logs and verifies that following links terminates and does not loop, while
// still finding the error logs reachable through the cycle a bounded number
// of times.
func TestGlobStreamSymlinkLoop(t *testing.T) {
	c, cw, cleanup := newCountingClient(t)
	defer cleanup()

	root := globSlash(t)
	logs := filepath.Join(root, "logs")
	a := filepath.Join(logs, "a")
	b := filepath.Join(logs, "a", "b")
	require.NoError(t, os.MkdirAll(b, 0o755))
	writeFile(t, filepath.Join(a, "error-a.gz"))
	writeFile(t, filepath.Join(b, "error-b.gz"))

	// Two aliases point back at a itself, so naive following would never
	// terminate: logs/a/loop -> a  and  logs/a/b/back -> a.
	require.NoError(t, os.Symlink(a, filepath.Join(b, "back")))
	require.NoError(t, os.Symlink(a, filepath.Join(a, "loop")))

	done := make(chan struct{})
	var got []string
	go func() {
		defer close(done)
		g, err := c.GlobStream(root+"/logs/**/error-*.gz", FollowSymlinks())
		if err != nil {
			return
		}
		defer g.Close()
		for g.Next() {
			got = append(got, g.Match().Path)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("GlobStream did not terminate on a symlink cycle")
	}

	// Physical files are found; cycle aliases are bounded, never infinite.
	require.NotEmpty(t, got)
	require.LessOrEqual(t, len(got), 12, "cycle aliases must be bounded")
	// OPENDIR traffic must be bounded by the (small) reachable graph, not
	// proportional to runtime.
	require.Less(t, cw.get(sshFxpOpendir), 20)
}

// TestGlobStreamNoFollow skips directory symlinks entirely.
func TestGlobStreamNoFollow(t *testing.T) {
	c, _, cleanup := newCountingClient(t)
	defer cleanup()

	root := globSlash(t)
	real := filepath.Join(root, "logs", "real")
	link := filepath.Join(root, "logs", "link")
	require.NoError(t, os.MkdirAll(real, 0o755))
	writeFile(t, filepath.Join(real, "error-x.gz"))
	require.NoError(t, os.Symlink(real, link))

	got := collectGlob(t, c, root+"/logs/**/error-*.gz")
	require.Equal(t, []string{root + "/logs/real/error-x.gz"}, got)

	// Following links exposes the same file under an additional alias.
	gotFollow := collectGlob(t, c, root+"/logs/**/error-*.gz", FollowSymlinks())
	require.ElementsMatch(t, []string{
		root + "/logs/real/error-x.gz",
		root + "/logs/link/error-x.gz",
	}, gotFollow)
}

// TestGlobStreamDedupe verifies the explicit ByPath vs ByFile contract when
// one physical directory is reachable through two symlink aliases.
func TestGlobStreamDedupe(t *testing.T) {
	c, _, cleanup := newCountingClient(t)
	defer cleanup()

	root := globSlash(t)
	logs := filepath.Join(root, "logs")
	dir := filepath.Join(logs, "dir")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeFile(t, filepath.Join(dir, "error-1.gz"))
	require.NoError(t, os.Symlink(dir, filepath.Join(logs, "alias1")))
	require.NoError(t, os.Symlink(dir, filepath.Join(logs, "alias2")))

	// Default: dedupe by returned path -> all three aliases are reported.
	byPath := collectGlob(t, c, root+"/logs/**/error-*.gz", FollowSymlinks())
	require.ElementsMatch(t, []string{
		root + "/logs/dir/error-1.gz",
		root + "/logs/alias1/error-1.gz",
		root + "/logs/alias2/error-1.gz",
	}, byPath)

	// Dedupe by file identity -> the physical file is reported once.
	byFile := collectGlob(t, c, root+"/logs/**/error-*.gz",
		FollowSymlinks(), GlobDedupe(DedupeByFile))
	require.Len(t, byFile, 1)
	require.Contains(t, byFile[0], "/error-1.gz")
}
