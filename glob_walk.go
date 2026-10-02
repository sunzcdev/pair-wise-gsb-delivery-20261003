package sftp

import (
	"os"
	"path"
	"strconv"
)

// readdirBatch issues a single READDIR request for fr and returns one batch
// of entries (io.EOF is normalised by the caller).
func (g *GlobMatches) readdirBatch(fr *globFrame) ([]os.FileInfo, error) {
	id := g.client.nextID()
	typ, data, err := g.client.sendPacket(g.ctx(), nil, &sshFxpReaddirPacket{
		ID:     id,
		Handle: fr.handle,
	})
	if err != nil {
		return nil, err
	}
	switch typ {
	case sshFxpName:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return nil, &unexpectedIDErr{id, sid}
		}
		count, data := unmarshalUint32(data)
		entries := make([]os.FileInfo, 0, count)
		for i := uint32(0); i < count; i++ {
			var filename string
			filename, data = unmarshalString(data)
			_, data = unmarshalString(data) // longname
			attr, rest, err := unmarshalAttrs(data)
			if err != nil {
				return nil, err
			}
			data = rest
			if filename == "." || filename == ".." {
				continue
			}
			entries = append(entries, fileInfoFromStat(attr, path.Base(filename)))
		}
		return entries, nil
	case sshFxpStatus:
		return nil, normaliseError(unmarshalStatus(id, data))
	default:
		return nil, unimplementedPacketErr(typ)
	}
}

// entryKind describes how a directory entry relates to the file system.
type entryKind int

const (
	kindFile    entryKind = iota
	kindDir               // real directory
	kindLinkDir           // symlink whose target is a directory
	kindLink              // symlink whose target is not a directory / unresolvable
)

// classify determines whether entry is a directory (following symlinks when
// enabled) and returns the physical path used for cycle/dedupe purposes.
func (g *GlobMatches) classify(fr *globFrame, entry os.FileInfo) (entryKind, string) {
	name := entry.Name()
	isSymlink := entry.Mode()&os.ModeSymlink != 0

	switch {
	case isSymlink && g.cfg.follow:
		target, targetIsDir, err := g.resolveLink(path.Join(fr.logical, name))
		if err != nil || !targetIsDir {
			return kindLink, ""
		}
		return kindLinkDir, target
	case isSymlink:
		return kindLink, ""
	case entry.IsDir():
		return kindDir, path.Join(fr.physical, name)
	default:
		return kindFile, path.Join(fr.physical, name)
	}
}

// handleEntry processes one entry of fr: yields matches and/or pushes frames.
func (g *GlobMatches) handleEntry(fr *globFrame, entry os.FileInfo) {
	name := entry.Name()
	if name == "." || name == ".." {
		return
	}
	logical := path.Join(fr.logical, name)
	kind, physical := g.classify(fr, entry)
	isDir := kind == kindDir || kind == kindLinkDir

	seg := g.segAt(fr.segIndex)
	if seg == nil {
		return
	}

	if !seg.star {
		matched, err := Match(seg.part, name)
		if err != nil || !matched {
			return
		}
		if fr.segIndex == len(g.segments)-1 {
			g.yield(logical, entry, physical, kind)
			return
		}
		if isDir {
			g.enter(globFrame{
				logical:  logical,
				physical: physical,
				segIndex: fr.segIndex + 1,
			})
		}
		return
	}

	// seg == "**". Two independent interpretations:
	//
	// 1) consume: keep segIndex on "**" and descend into directories.
	// 2) skip-zero: match the next segment against this entry.
	//
	next := g.segAt(fr.segIndex + 1)
	if next == nil {
		// Trailing "**": everything below is a match.
		g.yield(logical, entry, physical, kind)
		if isDir {
			g.enter(globFrame{
				logical:  logical,
				physical: physical,
				segIndex: fr.segIndex,
			})
		}
		return
	}
	matched, err := Match(next.part, name)
	if err != nil || !matched {
		// Not a zero-segment match: "**" may still consume this directory.
		if isDir {
			g.enter(globFrame{
				logical:  logical,
				physical: physical,
				segIndex: fr.segIndex,
			})
		}
		return
	}
	if fr.segIndex+1 == len(g.segments)-1 {
		g.yield(logical, entry, physical, kind)
		if isDir {
			// A directory matching the final segment is reported; still
			// descend so deeper matches through "**" are found.
			g.enter(globFrame{
				logical:  logical,
				physical: physical,
				segIndex: fr.segIndex,
			})
		}
		return
	}
	if isDir {
		// skip-zero (segIndex+2) must be explored before the generic
		// consume subtree, so push the consume frame first (LIFO -> later)
		// and the skip-zero frame second (LIFO -> next).
		g.enter(globFrame{
			logical:  logical,
			physical: physical,
			segIndex: fr.segIndex,
		})
		g.enter(globFrame{
			logical:  logical,
			physical: physical,
			segIndex: fr.segIndex + 2,
		})
	}
}

// enter pushes a directory frame after validating it against cycles (active
// descent path) and ByFile physical dedupe.
func (g *GlobMatches) enter(child globFrame) {
	if child.physical == "" {
		// Non-followed symlink never reaches here as a directory.
		return
	}

	// Cycle guard: re-entering a directory already on the current descent
	// path (via symlinks) must be rejected regardless of pattern position.
	if _, looping := g.active[child.physical]; looping {
		return
	}

	stateKey := stateKey(child.physical, child.segIndex)
	if _, ok := g.states[stateKey]; ok {
		// This (directory, pattern-position) state has already been explored,
		// so re-entering would only repeat READDIR traffic.
		return
	}
	g.states[stateKey] = struct{}{}

	fr := child
	fr.pins = true
	g.stack = append(g.stack, &fr)
	g.active[fr.physical] = struct{}{}
	if g.cfg.dedupe == DedupeByFile {
		g.seen[fr.physical] = struct{}{}
	}
}

func stateKey(physical string, segIndex int) string {
	return physical + "\x00" + strconv.Itoa(segIndex)
}

func (g *GlobMatches) segAt(i int) *globSegment {
	if i < 0 || i >= len(g.segments) {
		return nil
	}
	return &g.segments[i]
}

// yield records a match unless it duplicates one already reported under the
// configured DedupeMode.
func (g *GlobMatches) yield(logical string, info os.FileInfo, physical string, kind entryKind) {
	switch g.cfg.dedupe {
	case DedupeByPath:
		if _, ok := g.emitted[logical]; ok {
			return
		}
		g.emitted[logical] = struct{}{}
	case DedupeByFile:
		key := physical
		if key == "" {
			key = logical
		}
		if _, ok := g.emitted[key]; ok {
			return
		}
		g.emitted[key] = struct{}{}
	}
	g.pending = append(g.pending, globMatch{path: logical, info: info})
}
