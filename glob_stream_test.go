package sftp

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"testing"
	"time"
)

// fakeEntry is an in-memory file, directory or symbolic link.
type fakeEntry struct {
	name    string
	mode    fs.FileMode
	size    int64
	target  string // symlink target, slash separated
	entries map[string]*fakeEntry
}

func fakeFile(name string, size int64) *fakeEntry {
	return &fakeEntry{name: name, mode: 0o644, size: size}
}

func fakeDir(name string, entries ...*fakeEntry) *fakeEntry {
	d := &fakeEntry{name: name, mode: os.ModeDir | 0o755, entries: map[string]*fakeEntry{}}
	for _, e := range entries {
		e.name = path.Base(e.name)
		d.entries[e.name] = e
	}
	return d
}

func fakeLink(name, target string) *fakeEntry {
	return &fakeEntry{name: name, mode: os.ModeSymlink | 0o777, target: target}
}

type fakeFileInfo struct{ e *fakeEntry }

func (f fakeFileInfo) Name() string       { return path.Base(f.e.name) }
func (f fakeFileInfo) Size() int64        { return f.e.size }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.e.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.e.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

func (e *fakeEntry) info() os.FileInfo { return fakeFileInfo{e} }

// fakeGlobFS implements globFS over fakeEntry trees and counts the remote
// operations a real client translates into SFTP requests.
type fakeGlobFS struct {
	root      *fakeEntry
	batchSize int

	lstatCalls     map[string]int
	readLinkCalls  map[string]int
	opendirCalls   map[string]int
	readdirBatches map[string]int
	readdirEntries map[string]int
	denyReaddir    map[string]bool
}

func newFakeGlobFS(root *fakeEntry) *fakeGlobFS {
	return &fakeGlobFS{
		root:           root,
		batchSize:      2,
		lstatCalls:     map[string]int{},
		readLinkCalls:  map[string]int{},
		opendirCalls:   map[string]int{},
		readdirBatches: map[string]int{},
		readdirEntries: map[string]int{},
		denyReaddir:    map[string]bool{},
	}
}

func (f *fakeGlobFS) lookup(p string) (*fakeEntry, error) {
	p = path.Clean(p)
	cur := f.root
	rest := p
	if len(rest) > 0 && rest[0] == '/' {
		rest = rest[1:]
	}
	for rest != "" {
		var seg string
		if i := indexByte(rest, '/'); i >= 0 {
			seg, rest = rest[:i], rest[i+1:]
		} else {
			seg, rest = rest, ""
		}
		if seg == "" || seg == "." {
			continue
		}
		if cur == nil || !cur.mode.IsDir() {
			return nil, os.ErrNotExist
		}
		next, ok := cur.entries[seg]
		if !ok {
			return nil, os.ErrNotExist
		}
		cur = next
	}
	return cur, nil
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func (f *fakeGlobFS) lstat(_ context.Context, p string) (os.FileInfo, error) {
	f.lstatCalls[p]++
	e, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	return e.info(), nil
}

func (f *fakeGlobFS) readLink(_ context.Context, p string) (string, error) {
	f.readLinkCalls[p]++
	e, err := f.lookup(p)
	if err != nil {
		return "", err
	}
	if e.mode&os.ModeSymlink == 0 {
		return "", fmt.Errorf("%s is not a symlink", p)
	}
	return e.target, nil
}

func (f *fakeGlobFS) realPath(_ context.Context, p string) (string, error) {
	// Mirror ssh_fxp_realpath: clean and make absolute against the root.
	c := path.Clean(p)
	if !path.IsAbs(c) {
		c = "/" + c
	}
	return c, nil
}

// readdir serves entries in fixed batches and checks ctx before each batch,
// matching the client behavior: no further READDIR after cancellation.
func (f *fakeGlobFS) readdir(ctx context.Context, p string, fn func(string, os.FileInfo) bool) error {
	e, err := f.lookup(p)
	if err != nil {
		return err
	}
	if !e.mode.IsDir() {
		return fmt.Errorf("%s is not a directory", p)
	}
	if f.denyReaddir[p] {
		return os.ErrPermission
	}
	f.opendirCalls[p]++

	names := make([]string, 0, len(e.entries))
	for name := range e.entries {
		names = append(names, name)
	}
	sort.Strings(names)

	batch := f.batchSize
	if batch <= 0 {
		batch = len(names)
		if batch == 0 {
			batch = 1
		}
	}
	for start := 0; start < len(names); start += batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.readdirBatches[p]++
		end := start + batch
		if end > len(names) {
			end = len(names)
		}
		for _, name := range names[start:end] {
			f.readdirEntries[p]++
			if !fn(name, e.entries[name].info()) {
				return nil
			}
		}
	}
	return nil
}

func (f *fakeGlobFS) totalOpendirs() int { return sumCounts(f.opendirCalls) }
func (f *fakeGlobFS) totalBatches() int  { return sumCounts(f.readdirBatches) }
func (f *fakeGlobFS) totalReadLinks() int {
	return sumCounts(f.readLinkCalls)
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, c := range m {
		n += c
	}
	return n
}

// streamGlobTest runs one GlobStream over the given fake filesystem.
type streamGlobTest struct {
	fs      *fakeGlobFS
	pattern string
	ctx     context.Context
	opts    []GlobOption
}

func newStreamGlobTest(t *testing.T, root *fakeEntry, pattern string, opts ...GlobOption) *streamGlobTest {
	t.Helper()
	return &streamGlobTest{
		fs:      newFakeGlobFS(root),
		pattern: pattern,
		ctx:     context.Background(),
		opts:    opts,
	}
}

// run executes the iterator to completion.
func (g *streamGlobTest) run() ([]string, []error) {
	seq, err := globSequence(g.fs, g.ctx, g.pattern, g.opts...)
	if err != nil {
		return nil, []error{err}
	}
	var paths []string
	var errs []error
	for m, merr := range seq {
		if merr != nil {
			errs = append(errs, merr)
			continue
		}
		paths = append(paths, m.Path)
	}
	sort.Strings(paths)
	return paths, errs
}

// globSequence mirrors Client.GlobStream but targets an injectable globFS.
func globSequence(fsys globFS, ctx context.Context, pattern string, opts ...GlobOption) (func(func(GlobMatch, error) bool), error) {
	root, segs, err := prepareGlobPattern(pattern)
	if err != nil {
		return nil, err
	}
	cfg := globConfig{dedup: GlobDedupByPath}
	for _, opt := range opts {
		opt(&cfg)
	}
	return newGlobWalker(fsys, ctx, root, segs, cfg)
}
