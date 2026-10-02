package sftp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildDeepTree builds logs/d0/d1/.../d(depth-1); each level contains an
// error-N.gz and an unrelated other.txt.
func buildDeepTree(depth int) *fakeEntry {
	logs := fakeDir("logs")
	level := logs
	for i := 0; i < depth; i++ {
		d := fakeDir(fmt.Sprintf("d%d", i),
			fakeFile(fmt.Sprintf("error-%d.gz", i), 1),
			fakeFile("other.txt", 2),
		)
		level.entries[d.name] = d
		level = d
	}
	return fakeDir("root", logs)
}

func deepExpected(depth int) []string {
	want := make([]string, 0, depth)
	dir := "logs"
	for i := 0; i < depth; i++ {
		dir = path.Join(dir, fmt.Sprintf("d%d", i))
		want = append(want, path.Join(dir, fmt.Sprintf("error-%d.gz", i)))
	}
	sort.Strings(want)
	return want
}

// TestGlobStreamDeepTree: a depth-D chain costs O(D) directory reads and
// constant ancestor state; matches stream one by one.
func TestGlobStreamDeepTree(t *testing.T) {
	const depth = 200
	root := buildDeepTree(depth)
	g := newStreamGlobTest(t, root, "logs/**/error-*.gz")

	paths, errs := g.run()
	require.Empty(t, errs)
	assert.Equal(t, deepExpected(depth), paths)

	// logs and every d0..d199 are opened once: 201 OPENEDIR/READDIR streams.
	assert.Equal(t, depth+1, g.fs.totalOpendirs(), "one readdir stream per dir")
	// Each directory has 2-3 entries (2 files, +1 child dir except deepest),
	// and batch size is 2, so multiple READDIR batches per directory are
	// required; none of that is ever held all at once.
	assert.Greater(t, g.fs.totalBatches(), depth+1)
	// No links involved, so no READLINK traffic.
	assert.Zero(t, g.fs.totalReadLinks())
}

// TestGlobStreamStreamingDeliveryAndStop proves matches can be taken one at
// a time and that stopping after the first aborts the walk. The tree is
// depth-first: a first chain and an unexplored second chain. Only the chain
// leading to the first match may be read.
func TestGlobStreamStreamingDeliveryAndStop(t *testing.T) {
	chain := fakeDir("a",
		fakeDir("a1",
			fakeFile("error-a.gz", 1),
		),
	)
	other := fakeDir("b",
		fakeDir("b1",
			fakeFile("error-b.gz", 1),
		),
	)
	logs := fakeDir("logs", chain, other)
	root := fakeDir("root", logs)
	g := newStreamGlobTest(t, root, "logs/**/error-*.gz")

	seq, err := globSequence(g.fs, g.ctx, g.pattern, g.opts...)
	require.NoError(t, err)

	consumed := 0
	var first string
	for m, merr := range seq {
		require.NoError(t, merr)
		first = m.Path
		consumed++
		break
	}
	assert.Equal(t, 1, consumed)
	assert.Contains(t, first, "error-a.gz")

	// The sibling chain b/b1 was never entered.
	assert.Contains(t, g.fs.opendirCalls, "logs")
	assert.Contains(t, g.fs.opendirCalls, "logs/a")
	assert.Contains(t, g.fs.opendirCalls, "logs/a/a1")
	assert.NotContains(t, g.fs.opendirCalls, "logs/b")
	assert.NotContains(t, g.fs.opendirCalls, "logs/b/b1")
}

// TestGlobStreamWideTree: a directory with N entries uses ceil(N/batch)
// READDIR requests but retains only one batch; cancellation between batches
// stops further READDIR immediately.
func TestGlobStreamWideTree(t *testing.T) {
	const n = 25
	entries := make([]*fakeEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, fakeFile(fmt.Sprintf("error-%02d.gz", i), 1))
	}
	root := fakeDir("root", fakeDir("logs", entries...))

	g := newStreamGlobTest(t, root, "logs/error-*.gz")
	paths, errs := g.run()
	require.Empty(t, errs)
	assert.Len(t, paths, n)

	// One OPENEDIR; batch size is 2 -> ceil(25/2) = 13 READDIR batches.
	assert.Equal(t, 1, g.fs.opendirCalls["logs"])
	assert.Equal(t, (n+1)/2, g.fs.readdirBatches["logs"])
	assert.Equal(t, n, g.fs.readdirEntries["logs"])
}

func TestGlobStreamWideTreeCancel(t *testing.T) {
	const n = 25
	entries := make([]*fakeEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, fakeFile(fmt.Sprintf("error-%02d.gz", i), 1))
	}
	root := fakeDir("root", fakeDir("logs", entries...))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newStreamGlobTest(t, root, "logs/error-*.gz")
	g.ctx = ctx

	seq, err := globSequence(g.fs, ctx, g.pattern, g.opts...)
	require.NoError(t, err)

	consumed := 0
	var gotErr error
	for m, merr := range seq {
		if merr != nil {
			gotErr = merr
			break
		}
		_ = m
		if consumed == 3 {
			cancel()
			// The iterator delivers ctx.Err on the next pull; the fake FS
			// refuses any batch past cancellation.
		}
		consumed++
	}
	assert.ErrorIs(t, gotErr, context.Canceled)

	// At most ceil(4/2)=2 batches were served before cancellation; the
	// remaining ~21 entries never produced a READDIR request.
	assert.LessOrEqual(t, g.fs.readdirBatches["logs"], 3)
}

// TestGlobStreamSymlinkCycle builds root/a/b with b/loop -> a and b/back ->
// root/a/b. Following links terminates and the single real file is reported.
func TestGlobStreamSymlinkCycle(t *testing.T) {
	a := fakeDir("a")
	b := fakeDir("b",
		fakeFile("error-1.gz", 1),
		fakeLink("loop", ".."),
	)
	a.entries["b"] = b
	b.entries["back"] = fakeLink("back", ".")
	root := fakeDir("root", a)

	t.Run("follow dedup by path", func(t *testing.T) {
		g := newStreamGlobTest(t, root, "a/**/error-*.gz", GlobFollowDirectories())
		paths, errs := g.run()
		require.Empty(t, errs)
		// The real file appears through b and again through both cycles
		// only if they were entered; the ancestor guard prevents entry, so
		// it is delivered exactly once (its one logical path).
		assert.Equal(t, []string{"a/b/error-1.gz"}, paths)
		assert.LessOrEqual(t, g.fs.totalReadLinks(), 2)
	})

	t.Run("follow dedup by file", func(t *testing.T) {
		g := newStreamGlobTest(t, root, "a/**/error-*.gz",
			GlobFollowDirectories(), GlobDeduplicate(GlobDedupByFile))
		paths, errs := g.run()
		require.Empty(t, errs)
		assert.Equal(t, []string{"a/b/error-1.gz"}, paths)
	})

	t.Run("no follow", func(t *testing.T) {
		g := newStreamGlobTest(t, root, "a/**/error-*.gz")
		paths, errs := g.run()
		require.Empty(t, errs)
		assert.Equal(t, []string{"a/b/error-1.gz"}, paths)
		// Links are never touched.
		assert.Zero(t, g.fs.totalReadLinks())
	})
}

// TestGlobStreamAliasDedup: root/real/f.gz and root/lnk -> real. By path the
// file is seen through two logical paths; by file only once.
func TestGlobStreamAliasDedup(t *testing.T) {
	real := fakeDir("real", fakeFile("error-x.gz", 1))
	root := fakeDir("root", real, fakeLink("lnk", "real"))

	byPath := newStreamGlobTest(t, root, "**/error-*.gz", GlobFollowDirectories())
	p, e := byPath.run()
	require.Empty(t, e)
	assert.Equal(t, []string{"lnk/error-x.gz", "real/error-x.gz"}, p)

	byFile := newStreamGlobTest(t, root, "**/error-*.gz",
		GlobFollowDirectories(), GlobDeduplicate(GlobDedupByFile))
	p, e = byFile.run()
	require.Empty(t, e)
	assert.Len(t, p, 1)
}

func TestGlobStreamBadPattern(t *testing.T) {
	g := newStreamGlobTest(t, new(fakeEntry), "[")
	_, err := globSequence(g.fs, g.ctx, g.pattern)
	assert.ErrorIs(t, err, ErrBadPattern)
}

func TestGlobStreamMissingRoot(t *testing.T) {
	g := newStreamGlobTest(t, fakeDir("root"), "nope/**/x")
	paths, errs := g.run()
	assert.Empty(t, paths)
	assert.Empty(t, errs)
}

func TestGlobStreamLiteralPattern(t *testing.T) {
	root := fakeDir("root", fakeDir("logs", fakeFile("error-1.gz", 1)))
	g := newStreamGlobTest(t, root, "logs/error-1.gz")
	paths, errs := g.run()
	require.Empty(t, errs)
	assert.Equal(t, []string{"logs/error-1.gz"}, paths)
	// Literal matching uses one LSTAT and no READDIR at all.
	assert.Zero(t, g.fs.totalOpendirs())
}

func TestGlobStreamDirectoryReadErrorIsDelivered(t *testing.T) {
	logs := fakeDir("logs", fakeFile("error-a.gz", 1))
	logs.entries["secret"] = fakeDir("secret", fakeFile("error-b.gz", 1))
	root := fakeDir("root", logs)

	g := newStreamGlobTest(t, root, "logs/**/error-*.gz")
	g.fs.denyReaddir["logs/secret"] = true
	paths, errs := g.run()
	assert.Equal(t, []string{"logs/error-a.gz"}, paths)
	require.Len(t, errs, 1)
	assert.ErrorIs(t, errs[0], os.ErrPermission)
}

func TestGlobStarZeroAndAnyDepth(t *testing.T) {
	// error at depth 0, 1 and 2.
	deep := fakeDir("x", fakeFile("error-deep.gz", 1))
	mid := fakeDir("m", deep, fakeFile("error-mid.gz", 1))
	root := fakeDir("root", fakeDir("logs", mid, fakeFile("error-top.gz", 1)))
	g := newStreamGlobTest(t, root, "logs/**/error-*.gz")
	paths, errs := g.run()
	require.Empty(t, errs)
	assert.Equal(t, []string{
		"logs/error-top.gz",
		"logs/m/error-mid.gz",
		"logs/m/x/error-deep.gz",
	}, paths)
}

func TestGlobDoubleStarOnlyEmitsEverything(t *testing.T) {
	sub := fakeDir("d", fakeFile("f", 1))
	root := fakeDir("root", fakeDir("logs", sub, fakeFile("a.gz", 1)))
	g := newStreamGlobTest(t, root, "logs/**")
	paths, errs := g.run()
	require.Empty(t, errs)
	assert.Equal(t, []string{"logs", "logs/a.gz", "logs/d", "logs/d/f"}, paths)
}

func TestCollapseDoubleStar(t *testing.T) {
	assert.Equal(t, []string{"**", "x"}, collapseDoubleStar([]string{"**", "**", "x"}))
	assert.Equal(t, []string{"a", "**", "b"}, collapseDoubleStar([]string{"a", "**", "**", "b"}))
}

func TestPrepareGlobPattern(t *testing.T) {
	cases := []struct {
		pattern string
		root    string
		segs    []string
	}{
		{"logs/**/error-*.gz", "logs", []string{"**", "error-*.gz"}},
		{"**", ".", []string{"**"}},
		{"/var/log/**/*.gz", "/var/log", []string{"**", "*.gz"}},
		{"a/b*", "a", []string{"b*"}},
		{"plain/file.gz", "plain/file.gz", nil},
		{"a/**/**/b", "a", []string{"**", "b"}},
	}
	for _, tc := range cases {
		root, segs, err := prepareGlobPattern(tc.pattern)
		require.NoError(t, err, tc.pattern)
		assert.Equal(t, tc.root, root, tc.pattern)
		assert.Equal(t, tc.segs, segs, tc.pattern)
	}

	_, _, err := prepareGlobPattern("a/[bad")
	assert.ErrorIs(t, err, ErrBadPattern)
}

func TestGlobStreamSymlinkChainLoop(t *testing.T) {
	// a -> b, b -> a: direct two-node loop (relative links).
	root := fakeDir("root",
		fakeLink("a", "b"),
		fakeLink("b", "a"),
		fakeFile("error-ok.gz", 1),
	)
	g := newStreamGlobTest(t, root, "**/error-*.gz", GlobFollowDirectories())
	paths, errs := g.run()
	assert.Equal(t, []string{"error-ok.gz"}, paths)
	for _, err := range errs {
		assert.False(t, errors.Is(err, ErrSymlinkLoop), "cycles are pruned, not delivered: %v", err)
	}
}

func TestGlobStreamDanglingLink(t *testing.T) {
	logs := fakeDir("logs", fakeLink("error-dangling.gz", "missing-target"))
	root := fakeDir("root", logs)

	// A dangling link still matches as a leaf (READDIR reports its name).
	g := newStreamGlobTest(t, root, "logs/error-*.gz", GlobFollowDirectories())
	paths, errs := g.run()
	require.Empty(t, errs)
	assert.Equal(t, []string{"logs/error-dangling.gz"}, paths)
}

func TestGlobStreamLinkToDirectoryNotFollowed(t *testing.T) {
	real := fakeDir("real", fakeFile("error-x.gz", 1))
	logs := fakeDir("logs", real, fakeLink("alias", "real"))
	root := fakeDir("root", logs)

	g := newStreamGlobTest(t, root, "logs/**/error-*.gz")
	paths, errs := g.run()
	require.Empty(t, errs)
	// alias is a leaf entry, never entered; only real is walked.
	assert.Equal(t, []string{"logs/real/error-x.gz"}, paths)
	assert.Zero(t, g.fs.totalReadLinks())
}

func TestGlobStarMiddleSegment(t *testing.T) {
	a := fakeDir("d1", fakeFile("error-a.gz", 1))
	b := fakeDir("d2", fakeFile("error-b.gz", 1))
	root := fakeDir("root", fakeDir("logs", a, b))
	g := newStreamGlobTest(t, root, "logs/d*/error-*.gz")
	paths, errs := g.run()
	require.Empty(t, errs)
	assert.Equal(t, []string{"logs/d1/error-a.gz", "logs/d2/error-b.gz"}, paths)
}

func TestGlobStreamQuestionAndClass(t *testing.T) {
	logs := fakeDir("logs",
		fakeFile("error-1.gz", 1),
		fakeFile("error-a.gz", 1),
		fakeFile("error-9.gz", 1),
	)
	root := fakeDir("root", logs)
	g := newStreamGlobTest(t, root, "logs/error-[0-9].gz")
	paths, errs := g.run()
	require.Empty(t, errs)
	assert.Equal(t, []string{"logs/error-1.gz", "logs/error-9.gz"}, paths)
}
