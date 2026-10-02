package sftp

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path"
	"strings"
)

// ErrSymlinkLoop is reported for a symbolic link chain that loops or exceeds
// the maximum of 40 link resolutions. The subtree at the link is skipped;
// the rest of a GlobStream traversal continues.
var ErrSymlinkLoop = errors.New("sftp: too many levels of symbolic links")

// maxSymlinkHops bounds how many symbolic links are resolved in one path
// component chain, mirroring the local operating system limit. Together with
// the ancestor-chain guard it guarantees that following links terminates.
const maxSymlinkHops = 40

// GlobDedup selects how GlobStream suppresses duplicate results.
type GlobDedup int

const (
	// GlobDedupByPath (the default) emits a given returned path at most
	// once. Without GlobFollowDirectories the remote namespace is an
	// ordinary tree, so every file is reached by a single path anyway. When
	// directory symbolic links are followed, the same file can be reached
	// through more than one path (through a link and through the canonical
	// tree); each distinct path is then delivered.
	GlobDedupByPath GlobDedup = iota

	// GlobDedupByFile suppresses results whose resolved target has already
	// been delivered, even if reached through another symbolic link or path.
	//
	// Identity is established by canonicalizing symbolic link targets with
	// READLINK during traversal: this distinguishes symlink aliases but
	// cannot detect hard links, because the SFTP protocol carries no file
	// serial number (inode). Canonicalization costs one additional READLINK
	// per directory link descended; see GlobStream for request accounting.
	GlobDedupByFile
)

// GlobOption configures GlobStream.
type GlobOption func(*globConfig)

type globConfig struct {
	follow bool
	dedup  GlobDedup
}

// GlobFollowDirectories makes GlobStream descend into directories reached
// through symbolic links. It is disabled by default: links are treated as
// matching leaves only and never followed, so a link cannot cause a loop and
// no file is seen through more than one path.
//
// When enabled, link cycles are broken: a real directory already present on
// the current descent chain is never entered again, and a link chain longer
// than 40 components is delivered as ErrSymlinkLoop and pruned. Broken
// links and link resolution errors are delivered to the caller and never
// stop the rest of the traversal.
//
// One SSH_FXP_REALPATH is issued once at the start, so absolute link targets
// and relative pattern roots share one canonical namespace for cycle
// detection and identity de-duplication.
func GlobFollowDirectories() GlobOption {
	return func(cfg *globConfig) { cfg.follow = true }
}

// GlobDeduplicate sets the result de-duplication policy. The default is
// GlobDedupByPath.
func GlobDeduplicate(by GlobDedup) GlobOption {
	return func(cfg *globConfig) { cfg.dedup = by }
}

// GlobMatch is one GlobStream result.
type GlobMatch struct {
	// Path is the matching path in the notation of the pattern.
	Path string
	// Info holds the directory entry metadata returned by READDIR (or LSTAT
	// for a pattern without wildcards); no extra STAT request is issued.
	Info os.FileInfo
}

// GlobStream enumerates files matching a shell glob pattern lazily, yielding
// one result at a time as "range over function" pairs. Unlike Glob, it never
// materializes the whole tree: directories are read only when their entries
// are needed to decide what to emit next, and reading stops as soon as the
// range loop breaks or ctx is cancelled.
//
// Each segment uses the Match syntax; "**" as a complete segment matches
// zero or more directory segments, so "logs/**/error-*.gz" matches error
// logs at any depth below logs, including directly below logs. Consecutive
// "**" segments are collapsed, so "a/**/**/b" equals "a/**/b".
//
// A malformed pattern is reported by the returned error before iteration
// starts. Errors while reading individual directories (access denied, an
// unreadable link target, ...) are delivered to the loop and the affected
// subtree is skipped; the remaining tree is still traversed.
//
// Stopping early: a break in the range loop stops the walk immediately and
// closes the directory in progress. Cancelling ctx stops the walk between,
// and at the latest right after, the current READDIR batch, delivering
// ctx.Err() as the final error; no further OPENEDIR/READDIR requests are
// sent (the open handle is CLOSED so the server can release it).
//
// Requests and memory:
//
//   - Every directory entered costs one OPENEDIR, one or more READDIR (each
//     response is one server batch) and one CLOSE. Directories that cannot
//     contribute a match are never opened; with "logs/**/error-*.gz"
//     sibling trees still get enumerated because a match may hide at any
//     depth. A single "**" reads every directory in the rooted subtree once:
//     the suffix-at-this-depth test and the deeper descent are fused into
//     one listing. Patterns containing more than one "**" can revisit
//     directories (once per "**" frame).
//   - A tree of depth D costs O(D) such requests along a path; each level
//     only retains the state of its ancestors, so descending a tree with
//     10 000 levels stays O(D) memory with a small constant, never the whole
//     tree.
//   - A directory of N entries costs ceil(N/batch) READDIR requests, and
//     only the batch currently being consumed is retained, so memory is
//     O(batch), independent of directory width.
//   - With GlobFollowDirectories, every directory link descended costs one
//     additional READLINK. A link cycle costs at most one READLINK per link
//     on the chain before it is detected and pruned.
//   - De-duplication retains the keys of emitted results: O(matches)
//     memory. Nothing proportional to the number of directories visited is
//     retained; cycle protection only counts directories on the active
//     descent chain.
func (c *Client) GlobStream(ctx context.Context, pattern string, opts ...GlobOption) (iter.Seq2[GlobMatch, error], error) {
	root, segs, err := prepareGlobPattern(pattern)
	if err != nil {
		return nil, err
	}

	cfg := globConfig{dedup: GlobDedupByPath}
	for _, opt := range opts {
		opt(&cfg)
	}

	return newGlobWalker(&clientGlobFS{c}, ctx, root, segs, cfg)
}

// newGlobWalker builds the lazy iterator body for one prepared pattern.
func newGlobWalker(fsys globFS, ctx context.Context, root string, segs []string, cfg globConfig) (func(yield func(GlobMatch, error) bool), error) {
	w := &globWalker{
		fs:        fsys,
		cfg:       cfg,
		ctx:       ctx,
		seen:      make(map[string]bool),
		ancestors: make(map[string]int),
		resolved:  make(map[string]globResolved),
	}

	return func(yield func(GlobMatch, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(GlobMatch{}, err)
			return
		}

		realRoot := root
		if cfg.follow {
			// Canonicalize once so link targets expressed as absolute
			// server paths share a namespace with the traversal root;
			// otherwise a link back at the root would look unrelated.
			if canon, err := w.fs.realPath(ctx, root); err == nil {
				realRoot = canon
			}
		}

		fi, err := w.fs.lstat(ctx, root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return
			}
			yield(GlobMatch{}, err)
			return
		}

		real := root
		if cfg.follow {
			if fi.Mode()&os.ModeSymlink != 0 {
				target, tfi, terr := w.resolve(root, fi)
				switch {
				case terr == nil:
					real, fi = target, tfi
				case errors.Is(terr, os.ErrNotExist) && len(segs) == 0:
					// Broken root link; only a literal pattern matches it.
					w.emit(root, root, fi, fi, yield)
					return
				case errors.Is(terr, ErrSymlinkLoop):
					return
				default:
					yield(GlobMatch{}, terr)
					return
				}
			} else {
				real = realRoot
			}
		}

		if len(segs) == 0 {
			w.emit(root, real, fi, fi, yield)
			return
		}
		if !fi.IsDir() {
			return
		}

		w.starAt(root, real, fi, segs, yield)
	}, nil
}

// globFS is the subset of Client operations GlobStream needs; an in-memory
// implementation backs the unit tests.
type globFS interface {
	lstat(ctx context.Context, p string) (os.FileInfo, error)
	readLink(ctx context.Context, p string) (string, error)
	readdir(ctx context.Context, p string, fn func(name string, fi os.FileInfo) bool) error
	realPath(ctx context.Context, p string) (string, error)
}

// clientGlobFS adapts *Client to globFS.
type clientGlobFS struct{ c *Client }

func (fs *clientGlobFS) lstat(ctx context.Context, p string) (os.FileInfo, error) {
	return fs.c.lstat(ctx, p)
}

func (fs *clientGlobFS) readLink(ctx context.Context, p string) (string, error) {
	return fs.c.readLink(ctx, p)
}

func (fs *clientGlobFS) readdir(ctx context.Context, p string, fn func(string, os.FileInfo) bool) error {
	return fs.c.readdirEntries(ctx, p, fn)
}

func (fs *clientGlobFS) realPath(ctx context.Context, p string) (string, error) {
	return fs.c.realPath(ctx, p)
}

// globResolved caches one symbolic link resolution.
type globResolved struct {
	target string
	fi     os.FileInfo
	err    error
}

type globWalker struct {
	fs  globFS
	cfg globConfig
	ctx context.Context

	// seen suppresses duplicate emitted results.
	seen map[string]bool
	// ancestors counts real directories on the current descent chain; it is
	// the cycle guard and never grows beyond the chain depth.
	ancestors map[string]int
	// resolved memoizes READLINK resolutions per walker.
	resolved map[string]globResolved
}

type globYield func(GlobMatch, error) bool

// starAt enters an unbounded ("**") frame: real is pushed onto the ancestor
// chain for its duration and visited only through this guard, so a link that
// points back at an ancestor can never re-enter it.
func (w *globWalker) starAt(cur, real string, fi os.FileInfo, segs []string, yield globYield) bool {
	if w.ancestors[real] > 0 {
		return true
	}
	w.ancestors[real]++
	defer func() { w.ancestors[real]-- }()
	if len(segs) > 0 && segs[0] == "**" {
		return w.walkDoubleStar(cur, real, fi, segs, yield)
	}
	return w.visit(cur, real, fi, segs, yield)
}

func (w *globWalker) visit(cur, real string, fi os.FileInfo, segs []string, yield globYield) bool {
	seg, rest := segs[0], segs[1:]
	switch {
	case seg == "**":
		return w.starAt(cur, real, fi, segs, yield)
	case !hasMeta(seg):
		return w.walkLiteral(cur, real, seg, rest, yield)
	default:
		return w.walkSingleStar(cur, real, seg, rest, yield)
	}
}

// walkLiteral handles one meta-free segment.
func (w *globWalker) walkLiteral(cur, real, seg string, rest []string, yield globYield) bool {
	logicalChild := joinChild(cur, seg)
	realChild := joinChild(real, seg)
	// When following links the entry must be located in the canonical tree
	// so a link addressed through another link is found; otherwise the
	// logical name (which does not follow links) is used.
	statPath := logicalChild
	if w.cfg.follow {
		statPath = realChild
	}
	fi, err := w.fs.lstat(w.ctx, statPath)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	if w.cfg.follow && fi.Mode()&os.ModeSymlink != 0 {
		target, tfi, terr := w.resolve(realChild, fi)
		if terr != nil {
			if len(rest) == 0 {
				// Broken/looping link addressed literally matches as a leaf.
				return w.emit(logicalChild, realChild, fi, fi, yield)
			}
			if !errors.Is(terr, os.ErrNotExist) && !errors.Is(terr, ErrSymlinkLoop) {
				return yield(GlobMatch{}, terr)
			}
			return true
		}
		realChild, fi = target, tfi
	}
	if len(rest) == 0 {
		return w.emit(logicalChild, realChild, fi, fi, yield)
	}
	if !fi.IsDir() {
		return true
	}
	return w.visit(logicalChild, realChild, fi, rest, yield)
}

// walkSingleStar handles one segment containing meta characters but not the
// full "**" segment.
func (w *globWalker) walkSingleStar(cur, real, seg string, rest []string, yield globYield) bool {
	return w.enumerate(real, yield, func(name string, efi os.FileInfo) bool {
		matched, err := Match(seg, name)
		if err != nil || !matched {
			return err == nil
		}
		child := joinChild(cur, name)
		childReal := joinChild(real, name)
		if len(rest) == 0 {
			return w.emit(child, childReal, efi, efi, yield)
		}
		childReal, dfi, listable, ok := w.descendInfo(child, childReal, efi, yield)
		if !ok || !listable {
			return ok
		}
		return w.visit(child, childReal, dfi, rest, yield)
	})
}

// walkDoubleStar handles a leading "**" segment. Its caller (starAt) is
// responsible for the ancestor-chain guard.
//
// The directory is read exactly once per frame. For every entry E it tries
// both ways "**" may align with E:
//
//   - zero additional segments: E matches the remainder directly, or begins
//     a bounded match of a multi-segment remainder;
//   - one or more additional segments: if E is a directory, re-enter a
//     guarded "**" frame at E.
func (w *globWalker) walkDoubleStar(cur, real string, fi os.FileInfo, segs []string, yield globYield) bool {
	rest := segs[1:]
	if len(rest) == 0 {
		// Pattern ends in "/**": the directory itself and every descendant.
		if !w.emit(cur, real, fi, fi, yield) {
			return false
		}
		return w.allDescendants(cur, real, yield)
	}

	return w.enumerate(real, yield, func(name string, efi os.FileInfo) bool {
		child := joinChild(cur, name)
		childReal, dfi, listable, ok := w.descendInfo(child, joinChild(real, name), efi, yield)
		if !ok {
			return true
		}

		if !w.matchAtEntry(child, childReal, dfi, efi, listable, rest, yield) {
			return false
		}
		if listable {
			// Re-enter the same "** rest" frame at the descendant. Guard the
			// real directory on the chain first, so a link back at cur or
			// any ancestor is refused entry; no per-entry slice allocation.
			if w.ancestors[childReal] > 0 {
				return true
			}
			w.ancestors[childReal]++
			defer func() { w.ancestors[childReal]-- }()
			return w.walkDoubleStar(child, childReal, dfi, segs, yield)
		}
		return true
	})
}

// matchAtEntry applies a non-"**" remainder to a single directory entry for
// the case where "**" consumes zero additional segments. With a one-segment
// remainder the entry itself is the match; with more segments the entry must
// be a directory into which the bounded remainder continues.
func (w *globWalker) matchAtEntry(child, keyReal string, dfi, efi os.FileInfo, listable bool, rest []string, yield globYield) bool {
	seg, more := rest[0], rest[1:]
	matched, err := Match(seg, path.Base(child))
	if err != nil || !matched {
		return err == nil
	}
	if len(more) == 0 {
		return w.emitKeyed(child, keyReal, dfi, efi, yield)
	}
	if !listable {
		return true
	}
	// rest[1] is a bounded continuation (it cannot be "**" because
	// consecutive stars are collapsed); visit never expands without bound.
	return w.visit(child, keyReal, dfi, more, yield)
}

// allDescendants emits every descendant entry, recursively.
func (w *globWalker) allDescendants(cur, real string, yield globYield) bool {
	return w.enumerate(real, yield, func(name string, efi os.FileInfo) bool {
		child := joinChild(cur, name)
		childReal, dfi, listable, ok := w.descendInfo(child, joinChild(real, name), efi, yield)
		if !ok {
			return true
		}
		if !w.emit(child, childReal, dfi, efi, yield) {
			return false
		}
		if listable && w.ancestors[childReal] == 0 {
			w.ancestors[childReal]++
			defer func() { w.ancestors[childReal]-- }()
			return w.allDescendants(child, childReal, yield)
		}
		return true
	})
}

// descendInfo decides whether child can be entered as a directory and, when
// needed to follow a link, resolves it.
//
// The returned real path is the canonical directory path used for cycle
// detection and identity de-duplication; listable reports whether the entry
// is a directory the walker may enter. ok is false when the entry must be
// skipped silently (broken link without follow, or a cycle). A non-fatal
// resolution error is delivered to the caller.
func (w *globWalker) descendInfo(child, childReal string, efi os.FileInfo, yield globYield) (string, os.FileInfo, bool, bool) {
	isLink := efi.Mode()&os.ModeSymlink != 0
	if !isLink {
		if efi.IsDir() {
			return childReal, efi, true, true
		}
		return childReal, efi, false, true
	}
	if !w.cfg.follow {
		return childReal, efi, false, true
	}
	target, tfi, err := w.resolve(childReal, efi)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrSymlinkLoop) {
			return childReal, efi, false, false
		}
		if !yield(GlobMatch{}, err) {
			return childReal, efi, false, false
		}
		return childReal, efi, false, false
	}
	if !tfi.IsDir() {
		return target, tfi, false, true
	}
	return target, tfi, true, true
}

// emit delivers one match, applying de-duplication. p is the returned path.
// identity is the canonical full path (equal to p unless p traversed a
// followed link) and is the GlobDedupByFile key. keyFi is the resolved
// metadata for identity checks; outFi is what the caller receives.
func (w *globWalker) emit(p, identity string, keyFi, outFi os.FileInfo, yield globYield) bool {
	return w.emitKeyed(p, identity, keyFi, outFi, yield)
}

// emitKeyed is emit with an explicit canonical full path.
func (w *globWalker) emitKeyed(p, keyPath string, keyFi, outFi os.FileInfo, yield globYield) bool {
	key := p
	if w.cfg.dedup == GlobDedupByFile {
		// Normalize so identities reached through "." vs the server's
		// canonical root collapse to one key. Symlinks were already
		// resolved by the caller into keyPath/keyFi when following.
		key = path.Clean(keyPath)
	}
	if w.seen[key] {
		return true
	}
	w.seen[key] = true
	return yield(GlobMatch{Path: p, Info: outFi}, nil)
}

// enumerate lists the directory at real path real one READDIR batch at a
// time, invoking fn per entry. The real path is used for the server request
// so that directories reached through symbolic links list correctly.
// Listing and traversal stop when fn returns false or the context expires.
// A directory read error is delivered to the caller and the subtree is
// skipped; sibling subtrees continue.
func (w *globWalker) enumerate(real string, yield globYield,
	fn func(name string, fi os.FileInfo) bool,
) bool {
	if err := w.ctx.Err(); err != nil {
		yield(GlobMatch{}, err)
		return false
	}
	stopped := false
	err := w.fs.readdir(w.ctx, real, func(name string, fi os.FileInfo) bool {
		cont := fn(name, fi)
		if !cont {
			stopped = true
		}
		return cont
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			yield(GlobMatch{}, err)
		} else {
			yield(GlobMatch{}, fmt.Errorf("sftp: readdir %q: %w", real, err))
		}
		return false
	}
	return !stopped
}

// resolve follows symbolic links starting at logical path p, whose current
// best canonical directory estimate is real. It returns the canonical target
// path and the target's metadata. Results are memoized per logical path.
//
// Cycles within the chain and chains longer than maxSymlinkHops yield
// ErrSymlinkLoop; a dangling final link yields os.ErrNotExist.
func (w *globWalker) resolve(p string, start os.FileInfo) (string, os.FileInfo, error) {
	if r, ok := w.resolved[p]; ok {
		return r.target, r.fi, r.err
	}
	target, fi, err := w.resolveUncached(p, start)
	w.resolved[p] = globResolved{target: target, fi: fi, err: err}
	return target, fi, err
}

func (w *globWalker) resolveUncached(p string, start os.FileInfo) (string, os.FileInfo, error) {
	cur := p
	fi := start
	seen := map[string]bool{p: start.Mode()&os.ModeSymlink != 0}
	for hop := 0; hop <= maxSymlinkHops; hop++ {
		if fi == nil || fi.Mode()&os.ModeSymlink == 0 {
			return cur, fi, nil
		}
		target, err := w.fs.readLink(w.ctx, cur)
		if err != nil {
			return cur, fi, err
		}
		if path.IsAbs(target) {
			cur = path.Clean(target)
		} else {
			cur = path.Join(path.Dir(cur), target)
		}
		if hop == maxSymlinkHops || seen[cur] {
			return cur, fi, ErrSymlinkLoop
		}
		seen[cur] = true
		next, err := w.fs.lstat(w.ctx, cur)
		if err != nil {
			return cur, fi, err
		}
		fi = next
	}
	return cur, fi, ErrSymlinkLoop
}

// prepareGlobPattern splits pattern into a literal root directory and the
// wildcard segments to match below it, validating every wildcard segment.
func prepareGlobPattern(pattern string) (root string, segs []string, err error) {
	if !hasMeta(pattern) {
		if pattern == "" {
			return ".", nil, nil
		}
		return path.Clean(pattern), nil, nil
	}

	cleaned := path.Clean(pattern)
	parts := splitSegments(cleaned)
	absolute := path.IsAbs(cleaned)

	rootParts := make([]string, 0, len(parts))
	for _, part := range parts {
		if hasMeta(part) {
			break
		}
		rootParts = append(rootParts, part)
	}

	if absolute {
		root = path.Join(rootParts...)
		if root == "" {
			root = "/"
		}
		if !path.IsAbs(root) {
			root = "/" + root
		}
	} else if len(rootParts) == 0 {
		root = "."
	} else {
		root = path.Join(rootParts...)
	}

	segs = parts[len(rootParts):]
	segs = collapseDoubleStar(segs)

	for _, seg := range segs {
		if seg == "**" {
			continue
		}
		if _, err := Match(seg, ""); err != nil {
			return "", nil, err
		}
	}
	return root, segs, nil
}

// splitSegments splits a cleaned slash separated pattern into segments,
// dropping the leading "" produced by an absolute path.
func splitSegments(p string) []string {
	raw := strings.Split(p, "/")
	out := raw[:0]
	for _, s := range raw {
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// collapseDoubleStar merges consecutive "**" segments.
func collapseDoubleStar(segs []string) []string {
	out := make([]string, 0, len(segs))
	prevStar := false
	for _, seg := range segs {
		if seg == "**" {
			if prevStar {
				continue
			}
			prevStar = true
		} else {
			prevStar = false
		}
		out = append(out, seg)
	}
	return out
}

// joinChild appends a single, slash-free entry name to an already cleaned
// directory path. Unlike path.Join it does not re-clean, which avoids an
// allocation proportional to the path length on every level of a deep tree.
func joinChild(dir, name string) string {
	if dir == "" || dir == "." {
		return name
	}
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}
