package sftp

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// countingWriter counts SFTP request packets sent by the client. Every packet
// is written as a fixed 5-byte header (length + type) followed by a payload,
// so a 5-byte Write marks exactly one request of type p[4].
type countingWriter struct {
	conn   io.ReadWriteCloser
	mu     sync.Mutex
	counts map[fxp]int
}

func (w *countingWriter) Read(p []byte) (int, error) { return w.conn.Read(p) }
func (w *countingWriter) Close() error               { return w.conn.Close() }

func (w *countingWriter) Write(p []byte) (int, error) {
	// Each request starts with length(4) + type(1) in its first Write.
	if len(p) >= 5 {
		typ := fxp(p[4])
		if typ == sshFxpOpendir || typ == sshFxpReaddir || typ == sshFxpClose ||
			typ == sshFxpLstat || typ == sshFxpStat || typ == sshFxpReadlink {
			w.mu.Lock()
			w.counts[typ]++
			w.mu.Unlock()
		}
	}
	return w.conn.Write(p)
}

func (w *countingWriter) get(typ fxp) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.counts[typ]
}

func (w *countingWriter) reset() {
	w.mu.Lock()
	w.counts = map[fxp]int{}
	w.mu.Unlock()
}

// newCountingClient returns a Client backed by the in-process Go server and
// a counter for its outgoing request packets.
func newCountingClient(t *testing.T) (*Client, *countingWriter, func()) {
	t.Helper()
	c1, c2 := netPipe(t)

	server, err := NewServer(c1)
	require.NoError(t, err)
	go server.Serve()

	cw := &countingWriter{conn: c2, counts: map[fxp]int{}}
	client, err := NewClientPipe(cw, cw)
	require.NoError(t, err)

	cleanup := func() {
		client.Close()
		server.Close()
	}
	cw.reset() // ignore SSH_FXP_INIT
	return client, cw, cleanup
}

func globSlash(t *testing.T, elem ...string) string {
	t.Helper()
	all := append([]string{t.TempDir()}, elem...)
	return filepath.ToSlash(filepath.Join(all...))
}

func writeFile(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o755))
	require.NoError(t, os.WriteFile(name, []byte("x"), 0o644))
}

func collectGlob(t *testing.T, c *Client, pattern string, opts ...GlobOption) []string {
	t.Helper()
	g, err := c.GlobStream(pattern, opts...)
	require.NoError(t, err)
	defer g.Close()

	var got []string
	for g.Next() {
		m := g.Match()
		require.NotEmpty(t, m.Path)
		require.NotNil(t, m.Info)
		got = append(got, m.Path)
	}
	require.NoError(t, g.Err())
	sort.Strings(got)
	return got
}

func TestGlobStreamBasic(t *testing.T) {
	c, _, cleanup := newCountingClient(t)
	defer cleanup()

	root := globSlash(t)
	writeFile(t, filepath.Join(root, "a", "error-1.gz"))
	writeFile(t, filepath.Join(root, "a", "access.gz"))
	writeFile(t, filepath.Join(root, "b", "error-2.gz"))

	t.Run("single star", func(t *testing.T) {
		got := collectGlob(t, c, root+"/*/error-*.gz")
		want := []string{
			root + "/a/error-1.gz",
			root + "/b/error-2.gz",
		}
		require.Equal(t, want, got)
	})

	t.Run("double star", func(t *testing.T) {
		got := collectGlob(t, c, root+"/**/error-*.gz")
		want := []string{
			root + "/a/error-1.gz",
			root + "/b/error-2.gz",
		}
		require.Equal(t, want, got)
	})

	t.Run("literal hit", func(t *testing.T) {
		got := collectGlob(t, c, root+"/a/error-1.gz")
		require.Equal(t, []string{root + "/a/error-1.gz"}, got)
	})

	t.Run("literal miss", func(t *testing.T) {
		got := collectGlob(t, c, root+"/a/nope.gz")
		require.Empty(t, got)
	})

	t.Run("bad pattern", func(t *testing.T) {
		_, err := c.GlobStream(root + "/[")
		require.ErrorIs(t, err, ErrBadPattern)
	})
}

// TestGlobStreamDeep builds a 30-level directory chain with one match per
// level. Every directory is opened exactly once and read to EOF, so the
// request count is linear in depth rather than in the tree size, and memory
// stays proportional to depth times the server batch size.
func TestGlobStreamDeep(t *testing.T) {
	const depth = 30

	c, cw, cleanup := newCountingClient(t)
	defer cleanup()

	root := globSlash(t)
	writeFile(t, filepath.Join(root, "logs", "error-0.gz"))
	dir := filepath.Join(root, "logs")
	for i := 0; i < depth; i++ {
		dir = filepath.Join(dir, "d"+itoa(i))
		writeFile(t, filepath.Join(dir, "error-"+itoa(i+1)+".gz"))
	}

	g, err := c.GlobStream(root + "/logs/**/error-*.gz")
	require.NoError(t, err)

	var got []string
	for g.Next() {
		got = append(got, g.Match().Path)
	}
	require.NoError(t, g.Err())
	require.Len(t, got, depth+1)

	// logs plus depth subdirectories, each visited once.
	require.Equal(t, depth+1, cw.get(sshFxpOpendir))
	// Each directory fits in one READDIR batch, followed by one EOF status.
	require.Equal(t, 2*(depth+1), cw.get(sshFxpReaddir))
}

// TestGlobStreamWide puts more entries than a single server READDIR batch in
// one directory. Matches are taken one by one, and the next batch is only
// requested after the previous one has been consumed.
func TestGlobStreamWide(t *testing.T) {
	const files = 350

	c, cw, cleanup := newCountingClient(t)
	defer cleanup()

	root := globSlash(t)
	wide := filepath.Join(root, "wide")
	for i := 0; i < files; i++ {
		writeFile(t, filepath.Join(wide, "error-"+itoa(i)+".gz"))
	}

	g, err := c.GlobStream(root + "/wide/**/error-*.gz")
	require.NoError(t, err)
	defer g.Close()

	// Before the first Next no directory traffic has been generated.
	require.Equal(t, 0, cw.get(sshFxpReaddir))

	// The first Next opens the directory and pulls exactly one READDIR
	// batch; it does not prefetch the rest of this very wide directory.
	require.True(t, g.Next())
	_ = g.Match()
	require.Equal(t, 1, cw.get(sshFxpOpendir))
	require.Equal(t, 1, cw.get(sshFxpReaddir))

	// Consume the first batch; crossing its boundary costs exactly one more
	// READDIR and only then.
	n := 1
	for g.Next() {
		before := cw.get(sshFxpReaddir)
		_ = g.Match()
		n++
		after := cw.get(sshFxpReaddir)
		require.LessOrEqual(t, after-before, 1,
			"a single Next may issue at most one READDIR")
	}
	require.NoError(t, g.Err())
	require.Equal(t, files, n)

	// Several batches were needed, but far fewer READDIRs than files: the
	// whole listing is never materialised. The trailing EOF is fetched only
	// once the last match has been handed out.
	require.Equal(t, 1, cw.get(sshFxpOpendir))
	batches := cw.get(sshFxpReaddir) - 1
	require.GreaterOrEqual(t, batches, 2, "wide directory must span batches")
	require.Less(t, batches, files)
	t.Logf("readdir=%d opendir=%d close=%d lstat=%d stat=%d",
		cw.get(sshFxpReaddir), cw.get(sshFxpOpendir), cw.get(sshFxpClose),
		cw.get(sshFxpLstat), cw.get(sshFxpStat))
	t.Logf("wide dir: %d files in %d READDIR batches", files, batches)
}

// TestGlobStreamStopConsuming verifies that when the caller gives up and
// closes the stream, no further OPENDIR/READDIR requests are generated.
func TestGlobStreamStopConsuming(t *testing.T) {
	const files = 1000

	c, cw, cleanup := newCountingClient(t)
	defer cleanup()

	root := globSlash(t)
	wide := filepath.Join(root, "wide")
	for i := 0; i < files; i++ {
		writeFile(t, filepath.Join(wide, "error-"+itoa(i)+".gz"))
	}

	g, err := c.GlobStream(root + "/wide/**/error-*.gz")
	require.NoError(t, err)

	for i := 0; i < 150; i++ {
		require.True(t, g.Next())
		_ = g.Match()
	}
	require.Equal(t, 2, cw.get(sshFxpReaddir))
	require.NoError(t, g.Close())

	time.Sleep(50 * time.Millisecond)
	require.Equal(t, 2, cw.get(sshFxpReaddir),
		"no READDIR may be sent after the caller stops consuming")
	require.Equal(t, 1, cw.get(sshFxpOpendir))
}

// TestGlobStreamCancel verifies that cancelling the context stops the
// traversal with context.Canceled and freezes request traffic.
func TestGlobStreamCancel(t *testing.T) {
	const depth = 30

	c, cw, cleanup := newCountingClient(t)
	defer cleanup()

	root := globSlash(t)
	dir := filepath.Join(root, "logs")
	for i := 0; i < depth; i++ {
		writeFile(t, filepath.Join(dir, "error-"+itoa(i)+".gz"))
		dir = filepath.Join(dir, "d"+itoa(i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	g, err := c.GlobStream(root+"/logs/**/error-*.gz", GlobWithContext(ctx))
	require.NoError(t, err)
	defer g.Close()

	require.True(t, g.Next())
	_ = g.Match()

	cancel()
	for g.Next() {
		_ = g.Match()
	}
	require.ErrorIs(t, g.Err(), context.Canceled)

	readdir := cw.get(sshFxpReaddir)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, readdir, cw.get(sshFxpReaddir),
		"no READDIR may be sent after cancellation")
}

func itoa(i int) string { return strconv.Itoa(i) }
