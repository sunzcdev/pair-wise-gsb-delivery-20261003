package sftp

import (
	"context"
	"io"
	"os"
	"path"
)

// DedupeMode selects how GlobStream decides that two matches are the same.
type DedupeMode int

const (
	// DedupeByPath dedupes on the logical path that matched the pattern.
	//
	// With FollowSymlinks enabled the same physical file can be reached
	// through more than one path (for example via a directory symlink), and
	// each such path is returned separately. This mode needs no extra
	// requests.
	DedupeByPath DedupeMode = iota

	// DedupeByFile dedupes on file identity rather than on the returned
	// path.
	//
	// SFTPv3 does not carry inode numbers, so identity is approximated by
	// the symlink-resolved (physical) path. A match reached through
	// different aliases is reported once, using the path found first.
	// Canonicalising symlinks may require extra READLINK requests; regular
	// files and directories need none.
	DedupeByFile
)

// GlobOption configures GlobStream.
type GlobOption func(*globConfig)

type globConfig struct {
	ctx     context.Context
	follow  bool
	dedupe  DedupeMode
	maxHops int
}

// FollowSymlinks makes GlobStream descend into directories that are symbolic
// links. Symlinks to non-directories are always followed for matching once
// their name matches. Without this option, directory symlinks are not
// traversed, so a loop of symlinks cannot be entered at all.
//
// Even with this option, symlink cycles are detected: re-entering a
// directory already on the current descent path, or exhausting the symlink
// hop budget, prunes that branch instead of looping forever.
func FollowSymlinks() GlobOption {
	return func(cfg *globConfig) { cfg.follow = true }
}

// GlobDedupe selects whether matches are deduped by returned path or by file
// identity. The default is DedupeByPath.
func GlobDedupe(mode DedupeMode) GlobOption {
	return func(cfg *globConfig) { cfg.dedupe = mode }
}

// GlobWithContext cancels the traversal with ctx. When the context is done,
// Next returns the context's error and no further OPENDIR or READDIR requests
// are sent; directory handles already opened are closed.
func GlobWithContext(ctx context.Context) GlobOption {
	return func(cfg *globConfig) { cfg.ctx = ctx }
}

// globMaxSymlinkHops bounds the number of READLINK hops performed while
// resolving a single symlink chain before giving up on that branch.
const globMaxSymlinkHops = 40

// GlobStream searches the remote file system for files matching pattern and
// returns them lazily, one match per call to Next.
//
// pattern uses the same syntax as Match (see path.Match), with the addition
// of the double-star segment "**" which matches zero or more complete
// directory segments, for example "logs/**/error-*.gz".
//
// Unlike Glob, GlobStream never builds the whole result set: it opens
// directories and issues READDIR requests on demand as the caller consumes
// matches, so neither the remote tree nor a full listing has to fit in
// memory. Call Close (or drain the stream) to release open directory
// handles.
//
// A malformed pattern is reported immediately with ErrBadPattern; missing
// directories encountered during the traversal are simply skipped.
func (c *Client) GlobStream(pattern string, opts ...GlobOption) (*GlobMatches, error) {
	cfg := globConfig{ctx: context.Background(), maxHops: globMaxSymlinkHops}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.ctx == nil {
		cfg.ctx = context.Background()
	}

	pat, err := compileGlobPattern(pattern)
	if err != nil {
		return nil, err
	}

	g := &GlobMatches{
		client:   c,
		cfg:      cfg,
		pattern:  pat,
		segments: pat.segments,
		emitted:  map[string]struct{}{},
		seen:     map[string]struct{}{},
		active:   map[string]struct{}{},
	}
	g.states = map[string]struct{}{}

	if pat.literal {
		fi, err := c.lstatContext(g.ctx(), pattern)
		if err != nil {
			if os.IsNotExist(err) {
				g.closed = true
				return g, nil
			}
			return nil, err
		}
		g.pending = append(g.pending, globMatch{
			path: path.Clean(pattern),
			info: fi,
		})
		g.closed = true
		return g, nil
	}

	fi, err := c.lstatContext(g.ctx(), pat.anchor)
	if err != nil || !fi.IsDir() {
		// Missing/inaccessible or non-directory fixed prefix: no matches.
		g.closed = true
		return g, nil
	}
	g.stack = append(g.stack, &globFrame{
		logical:  pat.anchor,
		physical: pat.anchor,
	})
	return g, nil
}

// GlobMatches is a lazy iterator over the results of GlobStream.
type GlobMatches struct {
	client   *Client
	cfg      globConfig
	pattern  globPattern
	segments []globSegment

	// stack of directories being traversed, innermost last.
	stack []*globFrame
	// matches already produced but not yet handed to the caller.
	pending []globMatch

	emitted map[string]struct{} // dedupe keys already reported
	seen    map[string]struct{} // physical dirs already entered (ByFile)
	active  map[string]struct{} // physical dirs on the current DFS path
	states  map[string]struct{} // memoised (physical dir, segIndex) states

	err    error
	closed bool
}

type globMatch struct {
	path string
	info os.FileInfo
}

// GlobMatch is a single result produced by GlobMatches.Next.
type GlobMatch struct {
	// Path is the remote path that matched the pattern, slash separated.
	Path string
	// Info describes the matched entry as reported by READDIR/LSTAT.
	Info os.FileInfo
}

func (g *GlobMatches) ctx() context.Context { return g.cfg.ctx }

// Close stops the traversal and closes every directory handle still open.
// It must be called when the caller gives up early; it is safe to call after
// Next has returned false.
func (g *GlobMatches) Close() error {
	if g.closed {
		return nil
	}
	g.closed = true
	var firstErr error
	for _, fr := range g.stack {
		if fr.handle != "" {
			if err := g.client.close(fr.handle); err != nil && firstErr == nil {
				firstErr = err
			}
			fr.handle = ""
		}
	}
	g.stack = nil
	g.pending = nil
	return firstErr
}

// Err returns the error, if any, that stopped the traversal.
// It must be inspected after Next returns false.
func (g *GlobMatches) Err() error { return g.err }

// Next reports whether another match is available.
//
// It works lazily: a call may open one directory or fetch one READDIR batch,
// but never walks beyond the directory required to produce the next match.
// When the context is cancelled, or the caller stops calling Next and
// invokes Close, no further directory traffic is generated.
func (g *GlobMatches) Next() bool {
	if g.err != nil {
		return false
	}
	for {
		if len(g.pending) > 0 {
			return true
		}
		if g.closed {
			return false
		}
		if err := g.ctx().Err(); err != nil {
			g.stop(err)
			return false
		}
		if len(g.stack) == 0 {
			g.finish()
			return false
		}
		if err := g.advanceTopFrame(); err != nil {
			g.stop(err)
			return false
		}
	}
}

// Match returns the most recent match produced by Next.
func (g *GlobMatches) Match() GlobMatch {
	if len(g.pending) == 0 {
		return GlobMatch{}
	}
	m := g.pending[0]
	g.pending = g.pending[1:]
	return GlobMatch{Path: m.path, Info: m.info}
}

func (g *GlobMatches) finish() {
	g.closed = true
	g.stack = nil
}

// stop aborts the traversal with err and releases open handles.
func (g *GlobMatches) stop(err error) {
	g.err = err
	_ = g.Close()
}

type globFrame struct {
	logical  string // slash-separated path as reached through the pattern
	physical string // slash-separated symlink-resolved directory path
	segIndex int    // segment entries are tested against
	handle   string // open READDIR handle, "" when not open
	eof      bool
	entries  []os.FileInfo // remaining entries of the current READDIR batch
	pins     bool          // frame registered its physical dir in active/seen
}

// advanceTopFrame processes one entry (or pulls one more READDIR batch) of
// the innermost directory.
func (g *GlobMatches) advanceTopFrame() error {
	fr := g.stack[len(g.stack)-1]

	if fr.handle == "" && !fr.eof {
		handle, err := g.client.opendir(g.ctx(), fr.logical)
		if err != nil {
			// An unreadable directory prunes only this branch.
			g.popFrame(fr)
			return nil
		}
		fr.handle = handle
	}

	for len(fr.entries) == 0 && !fr.eof {
		entries, err := g.readdirBatch(fr)
		if err == io.EOF {
			fr.eof = true
			break
		}
		if err != nil {
			return err
		}
		fr.entries = entries
	}

	if len(fr.entries) > 0 {
		entry := fr.entries[0]
		fr.entries = fr.entries[1:]
		g.handleEntry(fr, entry)
		return nil
	}

	// Directory exhausted: return to the parent frame.
	g.closeFrame(fr)
	g.popFrame(fr)
	return nil
}

func (g *GlobMatches) closeFrame(fr *globFrame) {
	if fr.handle != "" {
		_ = g.client.close(fr.handle)
		fr.handle = ""
	}
}

// popFrame removes fr, which must be the innermost frame, dropping its
// active-directory pin.
func (g *GlobMatches) popFrame(fr *globFrame) {
	g.closeFrame(fr)
	if fr.pins {
		delete(g.active, fr.physical)
	}
	n := len(g.stack)
	g.stack[n-1] = nil
	g.stack = g.stack[:n-1]
}
