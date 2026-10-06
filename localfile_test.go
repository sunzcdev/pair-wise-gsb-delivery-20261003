package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sentinelContents = "top secret outside contents"

// localFileFixture builds a root directory with a sibling "outside" directory
// holding a sentinel file, plus a LocalFile handler rooted at "root".
type localFileFixture struct {
	t        *testing.T
	base     string
	rootDir  string
	outDir   string
	sentinel string
	lf       *LocalFile
}

func newLocalFileFixture(t *testing.T) *localFileFixture {
	t.Helper()

	base := t.TempDir()
	rootDir := filepath.Join(base, "root")
	outDir := filepath.Join(base, "outside")
	require.NoError(t, os.Mkdir(rootDir, 0o755))
	require.NoError(t, os.Mkdir(outDir, 0o755))

	sentinel := filepath.Join(outDir, "sentinel.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte(sentinelContents), 0o644))

	h, err := LocalFileHandler(rootDir)
	require.NoError(t, err)

	fx := &localFileFixture{
		t:        t,
		base:     base,
		rootDir:  rootDir,
		outDir:   outDir,
		sentinel: sentinel,
		lf:       h.FileGet.(*LocalFile),
	}

	t.Cleanup(func() {
		require.NoError(t, fx.lf.Close())
		fx.assertSentinel()
	})

	return fx
}

func (fx *localFileFixture) rootPath(rel string) string {
	return filepath.Join(fx.rootDir, filepath.FromSlash(rel))
}

func (fx *localFileFixture) outPath(rel string) string {
	return filepath.Join(fx.outDir, filepath.FromSlash(rel))
}

// assertSentinel verifies that the outside sentinel file was neither read nor
// modified. Every fixture-based test runs this at teardown.
func (fx *localFileFixture) assertSentinel() {
	fx.t.Helper()

	got, err := os.ReadFile(fx.sentinel)
	require.NoError(fx.t, err, "sentinel file outside the root must remain present")
	require.Equal(fx.t, sentinelContents, string(got),
		"sentinel file outside the root must never be modified")
}

func newReq(method, p string) *Request {
	r := NewRequest(method, p)
	r.ctx, r.cancelCtx = context.WithCancel(context.Background())
	return r
}

func newTargetReq(method, p, target string) *Request {
	r := newReq(method, p)
	r.Target = target
	return r
}

func (fx *localFileFixture) writeFile(t *testing.T, p, contents string, pflags uint32) {
	t.Helper()

	r := newReq("Put", p)
	r.Flags = pflags

	wr, err := fx.lf.Filewrite(r)
	require.NoError(t, err)

	closer, ok := wr.(interface{ Close() error })
	require.True(t, ok)

	_, err = wr.WriteAt([]byte(contents), 0)
	require.NoError(t, err)
	require.NoError(t, closer.Close())
}

func (fx *localFileFixture) readFile(t *testing.T, p string) string {
	t.Helper()

	rd, err := fx.lf.Fileread(newReq("Get", p))
	require.NoError(t, err)

	closer, _ := rd.(interface{ Close() error })

	buf := make([]byte, 4096)
	n, err := rd.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		require.NoError(t, err)
	}
	if closer != nil {
		require.NoError(t, closer.Close())
	}

	return string(buf[:n])
}

// symlinkSupported reports whether the current process can create symlinks.
// On Windows this normally requires Administrator rights or Developer Mode.
func symlinkSupported(t *testing.T) bool {
	t.Helper()

	dir := t.TempDir()
	err := os.Symlink(filepath.Join(dir, "x"), filepath.Join(dir, "link"))
	return err == nil
}

func TestLocalFileReadWriteMkdirRemove(t *testing.T) {
	fx := newLocalFileFixture(t)

	require.NoError(t, os.Mkdir(fx.rootPath("dir"), 0o755))

	fx.writeFile(t, "/dir/hello.txt", "hello world", sshFxfWrite|sshFxfCreat|sshFxfTrunc)
	assert.Equal(t, "hello world", fx.readFile(t, "/dir/hello.txt"))

	// Mkdir.
	require.NoError(t, fx.lf.Filecmd(newReq("Mkdir", "/dir/sub")))
	assert.True(t, isDir(fx.rootPath("dir/sub")))

	// Rmdir and Remove.
	require.NoError(t, fx.lf.Filecmd(newReq("Rmdir", "/dir/sub")))
	assert.NoDirExists(t, fx.rootPath("dir/sub"))

	require.NoError(t, fx.lf.Filecmd(newReq("Remove", "/dir/hello.txt")))
	assert.NoFileExists(t, fx.rootPath("dir/hello.txt"))

	// Rename.
	fx.writeFile(t, "/a.txt", "a", sshFxfWrite|sshFxfCreat)
	require.NoError(t, fx.lf.Filecmd(newTargetReq("Rename", "/a.txt", "/b.txt")))
	assert.NoFileExists(t, fx.rootPath("a.txt"))
	assert.Equal(t, "a", fx.readFile(t, "/b.txt"))

	// Rename over an existing target must fail (SFTP-v2 semantics).
	fx.writeFile(t, "/c.txt", "c", sshFxfWrite|sshFxfCreat)
	err := fx.lf.Filecmd(newTargetReq("Rename", "/c.txt", "/b.txt"))
	require.Error(t, err)
	assert.ErrorIs(t, err, fs.ErrExist)

	// PosixRename may replace the destination.
	require.NoError(t, fx.lf.PosixRename(newTargetReq("PosixRename", "/c.txt", "/b.txt")))
	assert.Equal(t, "c", fx.readFile(t, "/b.txt"))

	// List and Stat.
	la, err := fx.lf.Filelist(newReq("List", "/"))
	require.NoError(t, err)
	infos := make([]os.FileInfo, 16)
	n, err := la.ListAt(infos, 0)
	require.ErrorIs(t, err, io.EOF)

	var names []string
	for _, info := range infos[:n] {
		names = append(names, info.Name())
	}
	assert.ElementsMatch(t, []string{"b.txt", "dir"}, names)

	la, err = fx.lf.Filelist(newReq("Stat", "/b.txt"))
	require.NoError(t, err)
	n, err = la.ListAt(infos, 0)
	require.Equal(t, 1, n)
	require.ErrorIs(t, err, io.EOF)
	assert.Equal(t, int64(1), infos[0].Size())

	// Lstat of a regular file.
	la, err = fx.lf.Lstat(newReq("Lstat", "/b.txt"))
	require.NoError(t, err)
	n, _ = la.ListAt(infos, 0)
	require.Equal(t, 1, n)

	// Setstat: truncate via the file descriptor.
	fx.writeFile(t, "/grow.txt", "12345", sshFxfWrite|sshFxfCreat)
	setreq := newReq("Setstat", "/grow.txt")
	setreq.Flags = sshFileXferAttrSize
	setreq.Attrs = marshalFileStat(nil, sshFileXferAttrSize, &FileStat{Size: 2})
	require.NoError(t, fx.lf.Filecmd(setreq))
	assert.Equal(t, "12", fx.readFile(t, "/grow.txt"))

	// Remove refuses a directory, and Rmdir refuses a file.
	require.Error(t, fx.lf.Filecmd(newReq("Remove", "/dir")))
	require.Error(t, fx.lf.Filecmd(newReq("Rmdir", "/grow.txt")))
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func TestLocalFilePathEscapesRejected(t *testing.T) {
	fx := newLocalFileFixture(t)

	for _, p := range []string{
		"/../outside/sentinel.txt",
		"/dir/../../outside/sentinel.txt",
		"/dir/../outside/sentinel.txt",
	} {
		_, err := fx.lf.Fileread(newReq("Get", p))
		require.Error(t, err, "read of %q must fail", p)

		r := newReq("Put", p)
		r.Flags = sshFxfWrite | sshFxfCreat
		_, err = fx.lf.Filewrite(r)
		require.Error(t, err, "write to %q must fail", p)

		require.Error(t, fx.lf.Filecmd(newReq("Mkdir", "/../outside/newdir")))
		require.Error(t, fx.lf.Filecmd(newReq("Remove", p)))
		require.Error(t, fx.lf.Filecmd(newReq("Rmdir", p)))
		require.Error(t, fx.lf.Filecmd(newTargetReq("Rename", p, "/x")))
		require.Error(t, fx.lf.Filecmd(newTargetReq("Rename", "/b.txt", p)))
		require.Error(t, fx.lf.Filecmd(newTargetReq("Link", p, "/x")))
	}

	// Absolute symlink targets are refused at creation.
	require.Error(t, fx.lf.Filecmd(newTargetReq("Symlink", "/outside", "/abslink")))

	if !symlinkSupported(t) {
		t.Skip("symlink creation unsupported on this platform/account; " +
			"lexical and endpoint tests ran, symlink resolution tests skipped")
	}

	// A symlink (planted by some other local access) to the outside
	// directory cannot be followed through the handler.
	require.NoError(t, os.Symlink(fx.outDir, fx.rootPath("evil")))
	_, err := fx.lf.Fileread(newReq("Get", "/evil/sentinel.txt"))
	require.Error(t, err)

	r := newReq("Put", "/evil/written.txt")
	r.Flags = sshFxfWrite | sshFxfCreat
	if wr, err := fx.lf.Filewrite(r); err == nil {
		if closer, ok := wr.(interface{ Close() error }); ok {
			closer.Close()
		}
		t.Fatal("write through an escaping symlink must fail")
	}
	assert.NoFileExists(t, fx.outPath("written.txt"))
}

func TestLocalFileLegitimateRelativeSymlinks(t *testing.T) {
	fx := newLocalFileFixture(t)

	if !symlinkSupported(t) {
		t.Skip("symlink creation unsupported on this platform/account; relative-link tests skipped")
	}

	require.NoError(t, os.Mkdir(fx.rootPath("a"), 0o755))
	require.NoError(t, os.Mkdir(fx.rootPath("a/sub"), 0o755))
	fx.writeFile(t, "/a/sub/data.txt", "inside", sshFxfWrite|sshFxfCreat)

	// Relative symlink to a directory inside the root.
	require.NoError(t, os.Symlink(filepath.FromSlash("a"), fx.rootPath("link-to-a")))
	assert.Equal(t, "inside", fx.readFile(t, "/link-to-a/sub/data.txt"))

	// Relative symlink with ".." that stays inside the root.
	require.NoError(t, os.Symlink(filepath.FromSlash("../../a/sub/data.txt"),
		fx.rootPath("a/sub/up-link")))
	assert.Equal(t, "inside", fx.readFile(t, "/a/sub/up-link"))

	// Symlink created through the backend, with a relative target.
	require.NoError(t, fx.lf.Filecmd(newTargetReq("Symlink", "a/sub/data.txt", "/created-link")))
	assert.Equal(t, "inside", fx.readFile(t, "/created-link"))

	got, err := fx.lf.Readlink("/created-link")
	require.NoError(t, err)
	assert.Equal(t, "a/sub/data.txt", got)

	la, err := fx.lf.Lstat(newReq("Lstat", "/created-link"))
	require.NoError(t, err)
	infos := make([]os.FileInfo, 1)
	_, err = la.ListAt(infos, 0)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSymlink, infos[0].Mode()&os.ModeSymlink)

	// A relative symlink that escapes the root resolves to an error.
	require.NoError(t, os.Symlink(filepath.FromSlash("../../outside"), fx.rootPath("escaper")))
	_, err = fx.lf.Fileread(newReq("Get", "/escaper/sentinel.txt"))
	require.Error(t, err)
}

func TestLocalFileHardlinkAndRenameEndpointsConfined(t *testing.T) {
	fx := newLocalFileFixture(t)

	fx.writeFile(t, "/in.txt", "linked", sshFxfWrite|sshFxfCreat)

	// In-root hard link works.
	require.NoError(t, fx.lf.Filecmd(newTargetReq("Link", "/in.txt", "/in-link.txt")))
	assert.Equal(t, "linked", fx.readFile(t, "/in-link.txt"))

	// Hard link with either endpoint escaping is rejected.
	require.Error(t, fx.lf.Filecmd(newTargetReq("Link",
		"/../outside/sentinel.txt", "/steal.txt")))
	assert.NoFileExists(t, fx.rootPath("steal.txt"))
	require.Error(t, fx.lf.Filecmd(newTargetReq("Link",
		"/in.txt", "/../outside/stolen-link.txt")))
	assert.NoFileExists(t, fx.outPath("stolen-link.txt"))

	// Rename with either endpoint escaping is rejected.
	require.Error(t, fx.lf.Filecmd(newTargetReq("Rename",
		"/in.txt", "/../outside/moved.txt")))
	assert.FileExists(t, fx.rootPath("in.txt"))
	assert.NoFileExists(t, fx.outPath("moved.txt"))
	require.Error(t, fx.lf.Filecmd(newTargetReq("Rename",
		"/../outside/sentinel.txt", "/inside-now.txt")))
	assert.FileExists(t, fx.sentinel)

	// Mkdir / RemoveAll-style paths resolving outside are rejected.
	require.Error(t, fx.lf.Filecmd(newReq("Mkdir", "/../outside/newdir")))
	assert.NoDirExists(t, fx.outPath("newdir"))
}

// setupRacyLink builds a directory inside the root with a "hole" layout and
// an identical directory outside the root holding the secret sentinel. The
// attacker goroutine repeatedly replaces root/link between a *relative*
// symlink to the in-root directory (a legitimate state that must work) and
// an absolute symlink to the outside directory (which must be rejected).
type racyLink struct {
	fx *localFileFixture

	linkDir string // <root>/link (the swapped entry)
	safeDir string // <base>/safe (in-root target with hole)
	outDir  string // <base>/outside

	stop chan struct{}
	wg   sync.WaitGroup
}

func newRacyLink(t *testing.T, fx *localFileFixture) *racyLink {
	t.Helper()

	safeDir := fx.rootPath("real")
	require.NoError(t, os.Mkdir(filepath.Join(safeDir, "hole"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(safeDir, "hole", "sentinel.txt"),
		[]byte("benign in-root file"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(fx.outDir, "hole"), 0o755))
	// Place the secret sentinel under hole/ so that link/hole/sentinel.txt
	// is the attacked path in both the in-root and outside states.
	require.NoError(t, os.Rename(fx.sentinel, filepath.Join(fx.outDir, "hole", "sentinel.txt")))
	fx.sentinel = filepath.Join(fx.outDir, "hole", "sentinel.txt")

	rl := &racyLink{
		fx:      fx,
		linkDir: fx.rootPath("link"),
		safeDir: safeDir,
		outDir:  fx.outDir,
		stop:    make(chan struct{}),
	}

	// Initial state: link -> real (relative, inside the root).
	require.NoError(t, os.Symlink("real", rl.linkDir))

	rl.wg.Add(1)
	go rl.attacker(t)

	return rl
}

func (rl *racyLink) attacker(t *testing.T) {
	defer rl.wg.Done()

	// Three persistent symlinks are shuffled between three names. At any
	// moment <link> is one of the two targets, while the other two sit on
	// the spare names; this keeps the flapping cheap and tight.
	safeLink := rl.linkDir + ".safe"
	outLink := rl.linkDir + ".out"

	// The in-root variant uses a relative target, exactly as a user
	// would create a legal in-root link. The outside variant is absolute.
	if err := os.Symlink("real", safeLink); err != nil {
		return
	}
	if err := os.Symlink(rl.outDir, outLink); err != nil {
		return
	}

	// swap cyclically moves the symlink at src onto dst, and the former
	// dst onto the third (spare) name, so both symlinks are never lost.
	swap := func(dst, src, spare string) {
		_ = os.Rename(dst, spare)
		_ = os.Rename(src, dst)
	}

	for {
		select {
		case <-rl.stop:
			return
		default:
		}

		// Current: link=safe, .safe=safe(spare), .out=out.
		// Put "out" on link via the spare cycle.
		swap(rl.linkDir, outLink, safeLink)
		// Now: link=out, .out=safe(old link), .safe=safe.
		swap(rl.linkDir, safeLink, outLink)
		// Now: link=safe, .safe=out, .out=safe. Restore the spare layout
		// by cycling once more so both targets keep their spare slots.
		swap(rl.linkDir, outLink, safeLink)
		swap(rl.linkDir, safeLink, outLink)

		runtime.Gosched()
	}
}

func (rl *racyLink) stopAttacker() {
	close(rl.stop)
	rl.wg.Wait()
}

// TestLocalFileSymlinkSwapRace exercises every handler operation against a
// path whose parent directory entry is continuously being switched between an
// in-root directory and a symlink to an outside directory holding a sentinel
// file. The kernel performs resolution and access in a single confined
// operation (openat2/RESOLVE_BENEATH and friends), so the sentinel must never
// be read or modified regardless of when the swap occurs.
//
// Scope: the symlink and symlink-swap variants require symlink creation.
// On Windows that requires Administrator rights or Developer Mode; when
// unavailable the test skips with that limitation noted. The lexical and
// endpoint confinement tests run unconditionally.
func TestLocalFileSymlinkSwapRace(t *testing.T) {
	if runtime.GOOS == "plan9" {
		t.Skip("plan9 has no symlinks; confinement tests limited to lexical/endpoint cases")
	}

	fx := newLocalFileFixture(t)

	if !symlinkSupported(t) {
		t.Skip("symlink creation unsupported on this platform/account (on Windows " +
			"requires Administrator or Developer Mode); symlink swap race skipped")
	}

	rl := newRacyLink(t, fx)
	defer rl.stopAttacker()

	const duration = 600 * time.Millisecond

	var wg sync.WaitGroup

	// Stable victim files in the root, created before the goroutines so
	// a setup failure fails the test instead of killing a single victim.
	fx.writeFile(t, "/renamed.txt", "x", sshFxfWrite|sshFxfCreat)
	fx.writeFile(t, "/linksrc.txt", "x", sshFxfWrite|sshFxfCreat)

	// Read victim.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			rd, err := fx.lf.Fileread(newReq("Get", "/link/hole/sentinel.txt"))
			if err != nil {
				continue // expected most of the time
			}
			buf := make([]byte, 4096)
			n, err := rd.ReadAt(buf, 0)
			if closer, ok := rd.(interface{ Close() error }); ok {
				closer.Close()
			}
			if err == nil || errors.Is(err, io.EOF) {
				got := string(buf[:n])
				assert.NotContains(t, got, "top secret",
					"read must never return the outside sentinel contents")
				// A successful read can only be the legitimate in-root
				// relative link, which holds the benign file.
				assert.Equal(t, "benign in-root file", got)
			}
		}
	}()

	// Write victim.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			r := newReq("Put", "/link/hole/written.txt")
			r.Flags = sshFxfWrite | sshFxfCreat | sshFxfTrunc
			wr, err := fx.lf.Filewrite(r)
			if err != nil {
				continue
			}
			_, _ = wr.WriteAt([]byte("pwned"), 0)
			if closer, ok := wr.(interface{ Close() error }); ok {
				closer.Close()
			}
		}
	}()

	// Setstat (truncate) victim.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			r := newReq("Setstat", "/link/hole/sentinel.txt")
			r.Flags = sshFileXferAttrSize
			r.Attrs = marshalFileStat(nil, sshFileXferAttrSize, &FileStat{Size: 0})
			_ = fx.lf.Filecmd(r)
		}
	}()

	// Mkdir / Rmdir victims.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			name := fmt.Sprintf("/link/hole/newdir%d", time.Now().UnixNano())
			if err := fx.lf.Filecmd(newReq("Mkdir", name)); err == nil {
				_ = fx.lf.Filecmd(newReq("Rmdir", name))
			}
		}
	}()

	// Remove victim.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			_ = fx.lf.Filecmd(newReq("Remove", "/link/hole/sentinel.txt"))
		}
	}()

	// Rename victim, both endpoints racing the swap.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(duration)
		i := 0
		for time.Now().Before(deadline) {
			i++
			target := fmt.Sprintf("/link/hole/r%d.txt", i)
			err := fx.lf.Filecmd(newTargetReq("Rename",
				"/renamed.txt", target))
			if err == nil {
				// A success is only possible if link resolved inside the
				// root. Move the file back and confirm it did not land
				// outside.
				assert.NoFileExists(t, filepath.Join(rl.outDir, "hole",
					fmt.Sprintf("r%d.txt", i)))
				if _, statErr := os.Stat(filepath.Join(rl.safeDir, "hole",
					fmt.Sprintf("r%d.txt", i))); statErr == nil {
					_ = os.Rename(filepath.Join(rl.safeDir, "hole",
						fmt.Sprintf("r%d.txt", i)), fx.rootPath("renamed.txt"))
				}
			}
			if _, err := os.Stat(fx.rootPath("renamed.txt")); err != nil {
				// Non-fatal inside a goroutine: the victim continues on
				// the next iteration even if a recreate races a swap.
				if f, oerr := os.OpenFile(fx.rootPath("renamed.txt"),
					os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); oerr == nil {
					_, _ = f.Write([]byte("x"))
					_ = f.Close()
				}
			}
		}
	}()

	// Hard-link victim.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(duration)
		i := 0
		for time.Now().Before(deadline) {
			i++
			_ = fx.lf.Filecmd(newTargetReq("Link",
				"/linksrc.txt", fmt.Sprintf("/link/hole/h%d.txt", i)))
		}
	}()

	// List / Stat / Lstat victims.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			if la, err := fx.lf.Filelist(newReq("List", "/link/hole")); err == nil {
				buf := make([]os.FileInfo, 64)
				_, _ = la.ListAt(buf, 0)
			}
			_, _ = fx.lf.Filelist(newReq("Stat", "/link/hole/sentinel.txt"))
			_, _ = fx.lf.Lstat(newReq("Lstat", "/link/hole/sentinel.txt"))
		}
	}()

	wg.Wait()

	// Explicit post-race invariants, in addition to fixture teardown.
	// 1. Outside sentinel is intact.
	got, err := os.ReadFile(fx.sentinel)
	require.NoError(t, err)
	require.Equal(t, sentinelContents, string(got))

	// 2. Nothing the victims created leaked into the outside directory.
	var outsideEntries []string
	require.NoError(t, filepath.WalkDir(fx.outDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		outsideEntries = append(outsideEntries, p)
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "pwned",
			"outside file %q must not have been written", p)
		return nil
	}))
	assert.ElementsMatch(t, []string{fx.sentinel}, outsideEntries,
		"the outside directory must contain only the untouched sentinel")
	assert.NoFileExists(t, filepath.Join(fx.outDir, "hole", "written.txt"))
}

// TestLocalFileDirectoryChurnRace runs on every platform (no symlinks
// required). One goroutine continuously renames directories around *inside*
// the root while other goroutines operate through the handler, and a final
// goroutine continuously attempts lexical escapes to the outside sentinel.
// In-root operations either succeed with correct data or fail cleanly, and
// every escape attempt fails regardless of the directory churn.
func TestLocalFileDirectoryChurnRace(t *testing.T) {
	fx := newLocalFileFixture(t)

	require.NoError(t, os.MkdirAll(fx.rootPath("work/hole"), 0o755))
	require.NoError(t, os.WriteFile(fx.rootPath("work/hole/data.txt"),
		[]byte("churn-data"), 0o644))
	require.NoError(t, os.MkdirAll(fx.rootPath("spare/hole"), 0o755))
	require.NoError(t, os.WriteFile(fx.rootPath("spare/hole/data.txt"),
		[]byte("churn-data"), 0o644))

	const duration = 600 * time.Millisecond
	var wg sync.WaitGroup

	// Attacker: swap the two in-root directories via a temp name.
	wg.Add(1)
	go func() {
		defer wg.Done()
		a := fx.rootPath("work")
		b := fx.rootPath("spare")
		tmp := fx.rootPath("work-tmp")
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			if err := os.Rename(a, tmp); err == nil {
				if err := os.Rename(b, a); err == nil {
					_ = os.Rename(tmp, b)
				} else {
					_ = os.Rename(tmp, a)
				}
			}
		}
	}()

	runVictim := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(duration)
			for time.Now().Before(deadline) {
				fn()
			}
		}()
	}

	// In-root reads never return outside data or garbage as success.
	runVictim(func() {
		rd, err := fx.lf.Fileread(newReq("Get", "/work/hole/data.txt"))
		if err != nil {
			return
		}
		buf := make([]byte, 64)
		n, _ := rd.ReadAt(buf, 0)
		if closer, ok := rd.(interface{ Close() error }); ok {
			closer.Close()
		}
		assert.Equal(t, "churn-data", string(buf[:n]))
	})

	// Escape attempts via ".." always fail even mid-churn.
	runVictim(func() {
		for _, p := range []string{
			"/work/../../outside/sentinel.txt",
			"/../outside/sentinel.txt",
			"/spare/../../../outside/sentinel.txt",
		} {
			_, err := fx.lf.Fileread(newReq("Get", p))
			assert.Error(t, err)

			r := newReq("Put", p)
			r.Flags = sshFxfWrite
			if wr, err := fx.lf.Filewrite(r); err == nil {
				if closer, ok := wr.(interface{ Close() error }); ok {
					closer.Close()
				}
				t.Errorf("write to %q escaped the root", p)
			}

			assert.Error(t, fx.lf.Filecmd(newReq("Mkdir", "/work/../../outside/newdir")))
			assert.Error(t, fx.lf.Filecmd(newTargetReq("Rename",
				"/work/hole/data.txt", p)))
			assert.Error(t, fx.lf.Filecmd(newTargetReq("Link",
				p, "/work/hole/link.txt")))
		}
	})

	wg.Wait()
}

// TestLocalFileHandlerEndToEnd drives the backend through a real
// RequestServer and SFTP client over an in-process connection, exercising the
// wiring of all four handlers including Setstat, rename and listing.
func TestLocalFileHandlerEndToEnd(t *testing.T) {
	fx := newLocalFileFixture(t)

	serverConn, clientConn := net.Pipe()

	server := NewRequestServer(serverConn, Handlers{
		FileGet:  fx.lf,
		FilePut:  fx.lf,
		FileCmd:  fx.lf,
		FileList: fx.lf,
	})

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	client, err := NewClientPipe(clientConn, clientConn)
	require.NoError(t, err)

	// Upload a file and download it back.
	w, err := client.Create("/upload.txt")
	require.NoError(t, err)
	_, err = w.Write([]byte("round trip"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	r, err := client.Open("/upload.txt")
	require.NoError(t, err)
	got := make([]byte, 64)
	n, err := r.Read(got)
	require.True(t, err == nil || errors.Is(err, io.EOF))
	require.NoError(t, r.Close())
	assert.Equal(t, "round trip", string(got[:n]))

	// Mkdir, directory listing and stat through the wire.
	require.NoError(t, client.Mkdir("/listed"))
	inner, err := client.Create("/listed/inner")
	require.NoError(t, err)
	require.NoError(t, inner.Close())

	entries, err := client.ReadDir("/")
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"upload.txt", "listed"}, names)

	// Rename and remove.
	require.NoError(t, client.Rename("/upload.txt", "/renamed.txt"))
	require.NoError(t, client.Remove("/listed/inner"))
	require.NoError(t, client.Remove("/renamed.txt"))
	require.NoError(t, client.RemoveDirectory("/listed"))

	// A traversal request is rejected by the server with an error.
	_, err = client.ReadDir("/../outside")
	require.Error(t, err)

	require.NoError(t, client.Close())
	require.NoError(t, server.Close())
	<-serveErr
}
