package sftp

import (
	"io"
	"sort"
	"syscall"

	"github.com/pkg/sftp/internal/sparse"
)

// sparseMemFile wraps the example in-memory file with explicit hole tracking.
//
// Holes are recorded independently from the dense content buffer, so a region
// that contains genuine allocated zeroes is not confused with a hole. Ranges
// past the end of a file are implicitly holes, which also preserves the
// logical length when a file ends in a hole.
type sparseMemFile struct {
	*memFile
}

// WriteAt writes data and clears any holes overlapping the written range:
// data explicitly written by the client is allocated, even if it happens to
// consist entirely of zero bytes.
func (f *sparseMemFile) WriteAt(b []byte, off int64) (int, error) {
	n, err := f.memFile.WriteAt(b, off)
	if n > 0 {
		f.memFile.hmu.Lock()
		f.memFile.holes = subtractHole(f.memFile.holes, sparse.Extent{
			Offset: uint64(off),
			Length: uint64(n),
		})
		f.memFile.hmu.Unlock()
	}
	return n, err
}

// SparseInMemHandler returns the example in-memory Handlers with full sparse
// support: it can both discover data extents and create holes.
func SparseInMemHandler() Handlers {
	sr := &sparseRoot{root: InMemHandler().FileGet.(*root), caps: sparse.CapAll}
	return Handlers{
		FileGet:  sr,
		FilePut:  sr,
		FileCmd:  sr,
		FileList: sr,
	}
}

type sparseRoot struct {
	*root
	caps sparse.Capability
}

func (fs *sparseRoot) SparseCapabilities() sparse.Capability { return fs.caps }

func (fs *sparseRoot) Fileread(r *Request) (io.ReaderAt, error) {
	rd, err := fs.root.Fileread(r)
	if err != nil {
		return nil, err
	}
	return wrapSparse(rd).(io.ReaderAt), nil
}

func (fs *sparseRoot) Filewrite(r *Request) (io.WriterAt, error) {
	wr, err := fs.root.Filewrite(r)
	if err != nil {
		return nil, err
	}
	return wrapSparse(wr).(io.WriterAt), nil
}

func (fs *sparseRoot) OpenFile(r *Request) (WriterAtReaderAt, error) {
	rw, err := fs.root.OpenFile(r)
	if err != nil {
		return nil, err
	}
	return wrapSparse(rw).(WriterAtReaderAt), nil
}

var (
	_ FileReader = (*sparseRoot)(nil)
	_ FileWriter = (*sparseRoot)(nil)
)

// wrapSparse upgrades a *memFile handle to a sparseMemFile. Handles returned by
// the example backend are always *memFile.
func wrapSparse(v any) any {
	if mf, ok := v.(*memFile); ok {
		return &sparseMemFile{memFile: mf}
	}
	return v
}

// DataExtents implements SparseDataReaderAt.
func (f *sparseMemFile) DataExtents(off, length int64) ([]DataExtent, error) {
	f.memFile.mu.Lock()
	size := f.size()
	f.memFile.mu.Unlock()

	f.memFile.hmu.RLock()
	holes := append([]sparse.Extent(nil), f.memFile.holes...)
	f.memFile.hmu.RUnlock()

	end := size
	if length > 0 && off+length < end {
		end = off + length
	}
	if off < 0 || off > size {
		return nil, syscall.EINVAL
	}
	if off == end {
		return nil, nil
	}

	var extents []DataExtent
	cursor := off
	for _, h := range holes {
		hs, he := int64(h.Offset), int64(h.Offset+h.Length)
		if hs >= end {
			break
		}
		if he <= cursor {
			continue
		}
		if hs > cursor {
			extents = append(extents, DataExtent{Offset: uint64(cursor), Length: uint64(min64(hs, end) - cursor)})
		}
		if he > cursor {
			cursor = he
		}
		if cursor >= end {
			return extents, nil
		}
	}
	if cursor < end {
		extents = append(extents, DataExtent{Offset: uint64(cursor), Length: uint64(end - cursor)})
	}

	// Merge adjacent extents for canonical output.
	sort.Slice(extents, func(i, j int) bool { return extents[i].Offset < extents[j].Offset })
	merged := extents[:0]
	for _, e := range extents {
		if n := len(merged); n > 0 && merged[n-1].Offset+merged[n-1].Length == e.Offset {
			merged[n-1].Length += e.Length
			continue
		}
		merged = append(merged, e)
	}
	return merged, nil
}

// PunchHole implements SparseHoleWriterAt. It does not change the logical
// length; the example content buffer stays dense but the range is recorded so
// that subsequent extent queries skip it.
func (f *sparseMemFile) PunchHole(off, length int64) error {
	if off < 0 || length < 0 {
		return syscall.EINVAL
	}
	if length == 0 {
		return nil
	}

	f.memFile.hmu.Lock()
	defer f.memFile.hmu.Unlock()

	f.memFile.holes = addHole(f.memFile.holes, sparse.Extent{Offset: uint64(off), Length: uint64(length)})
	return nil
}

// addHole inserts e into a sorted, disjoint hole list, merging overlaps.
func addHole(holes []sparse.Extent, e sparse.Extent) []sparse.Extent {
	holes = append(holes, e)
	sort.Slice(holes, func(i, j int) bool { return holes[i].Offset < holes[j].Offset })

	merged := holes[:0]
	for _, h := range holes {
		if n := len(merged); n > 0 && h.Offset <= merged[n-1].Offset+merged[n-1].Length {
			stop := h.Offset + h.Length
			if end := merged[n-1].Offset + merged[n-1].Length; stop > end {
				merged[n-1].Length = stop - merged[n-1].Offset
			}
			continue
		}
		merged = append(merged, h)
	}
	return merged
}

// subtractHole removes the half-open range e from the sorted, disjoint hole
// list, splitting an entry when the range lands inside it.
func subtractHole(holes []sparse.Extent, e sparse.Extent) []sparse.Extent {
	if e.Length == 0 {
		return holes
	}

	eStart, eEnd := e.Offset, e.Offset+e.Length
	var out []sparse.Extent
	for _, h := range holes {
		hStart, hEnd := h.Offset, h.Offset+h.Length
		switch {
		case hEnd <= eStart || hStart >= eEnd:
			out = append(out, h) // disjoint
		case eStart <= hStart && eEnd >= hEnd:
			// whole hole covered: drop it
		case eStart > hStart && eEnd < hEnd:
			// range splits the hole into a head and a tail
			out = append(out,
				sparse.Extent{Offset: hStart, Length: eStart - hStart},
				sparse.Extent{Offset: eEnd, Length: hEnd - eEnd},
			)
		case eStart > hStart:
			out = append(out, sparse.Extent{Offset: hStart, Length: eStart - hStart})
		default:
			out = append(out, sparse.Extent{Offset: eEnd, Length: hEnd - eEnd})
		}
	}
	return out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
