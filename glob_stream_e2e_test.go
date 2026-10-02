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

func TestClientGlobStreamEndToEnd(t *testing.T) {
	base := t.TempDir()

	// Create logs/d0..d2/error-N.gz, an unrelated file each, and one file
	// directly below logs.
	logs := filepath.Join(base, "logs")
	require.NoError(t, os.MkdirAll(logs, 0o755))
	for i := 0; i < 3; i++ {
		dir := filepath.Join(logs, "d"+string(rune('0'+i)))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "error-"+string(rune('0'+i))+".gz"), []byte("x"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(logs, "error-top.gz"), []byte("x"), 0o644))

	// The in-process server serves the process working directory; run it with
	// the fixture as its working directory and use slash paths against it.
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(base))
	t.Cleanup(func() { _ = os.Chdir(wd) })

	client, server := clientServerPair(t)
	defer client.Close()
	defer server.Close()

	var got []GlobMatch
	seq, err := client.GlobStream(context.Background(), "logs/**/error-*.gz")
	require.NoError(t, err)
	for m, merr := range seq {
		require.NoError(t, merr)
		got = append(got, m)
	}

	paths := make([]string, len(got))
	for i, m := range got {
		paths[i] = filepath.ToSlash(m.Path)
	}
	sort.Strings(paths)
	assert.Equal(t, []string{
		"logs/d0/error-0.gz",
		"logs/d1/error-1.gz",
		"logs/d2/error-2.gz",
		"logs/error-top.gz",
	}, paths)

	// Every result carries READDIR metadata without an extra STAT.
	for _, m := range got {
		assert.False(t, m.Info.IsDir())
	}

	// A bad pattern is returned before iteration.
	_, err = client.GlobStream(context.Background(), "logs/[")
	assert.ErrorIs(t, err, ErrBadPattern)

	// Cancellation is surfaced and stops the iterator.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	seq2, err := client.GlobStream(ctx, "logs/**/*.gz")
	require.NoError(t, err)
	sawCancel := false
	for _, merr := range seq2 {
		if merr != nil {
			sawCancel = true
		}
	}
	assert.True(t, sawCancel, "expected context cancellation error")
}
