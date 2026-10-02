//go:build !windows && !plan9

package sftp

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientGlobStreamSymlinks(t *testing.T) {
	base := t.TempDir()

	real := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(real, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(real, "error-a.gz"), []byte("x"), 0o644))

	// Alias link and a directory link cycle: loop -> .. (base).
	require.NoError(t, os.Symlink("real", filepath.Join(base, "alias")))
	require.NoError(t, os.MkdirAll(filepath.Join(real, "loopdir"), 0o755))
	require.NoError(t, os.Symlink("..", filepath.Join(real, "loopdir", "up")))

	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(base))
	t.Cleanup(func() { _ = os.Chdir(wd) })

	client, server := clientServerPair(t)
	defer client.Close()
	defer server.Close()

	collect := func(pattern string, opts ...GlobOption) []string {
		t.Helper()
		var paths []string
		seq, err := client.GlobStream(context.Background(), pattern, opts...)
		require.NoError(t, err)
		for m, merr := range seq {
			require.NoError(t, merr)
			paths = append(paths, filepath.ToSlash(m.Path))
		}
		sort.Strings(paths)
		return paths
	}

	// Default: links are leaves only, the alias directory is not entered.
	got := collect("**/error-a.gz")
	assert.Equal(t, []string{"real/error-a.gz"}, got)

	// Follow + by path: the file appears through both logical routes, and
	// the cycle (loopdir/up/...) does not run away.
	got = collect("**/error-a.gz", GlobFollowDirectories())
	assert.Equal(t, []string{"alias/error-a.gz", "real/error-a.gz"}, got)

	// Follow + by file: only one identity survives.
	got = collect("**/error-a.gz", GlobFollowDirectories(), GlobDeduplicate(GlobDedupByFile))
	assert.Len(t, got, 1)

	// A terminating "/**" walk through the cycle terminates and lists the
	// files a bounded number of times.
	got = collect("real/**", GlobFollowDirectories())
	count := 0
	for _, p := range got {
		if filepath.Base(p) == "error-a.gz" {
			count++
		}
	}
	assert.Equal(t, 1, count, "cycle must not repeat entries, got %v", got)
}
