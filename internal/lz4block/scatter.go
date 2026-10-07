package lz4block

import (
	"math"

	"github.com/pierrec/lz4/v4/internal/lz4errors"
)

// scatterWriter writes a byte stream into a fixed set of fragments that are
// logically contiguous but need not be adjacent in memory. It tracks two
// cursors over the same logical space: a write cursor and, for match copies,
// a read cursor that may point at bytes already written in earlier fragments
// or at the external dictionary.
//
// The only per-decode allocation is start, a table of fragment offsets with
// one entry per fragment: output is produced directly in the fragments and
// never staged in a contiguous buffer.
type scatterWriter struct {
	parts [][]byte
	// start[i] is the logical offset of parts[i]'s first byte; start has a
	// final sentinel entry holding the total length.
	start []int

	// Write cursor.
	wi int // fragment index
	wo int // offset within parts[wi]

	// Read cursor for match copies.
	ri int // fragment index, or -1 when the cursor is in the dictionary
	ro int // offset within parts[ri], or offset into the dictionary
}

// newScatterWriter caps each fragment at its own length so no copy can spill
// into the caller's capacity past it, and returns a writer positioned at the
// start of the first non-empty fragment.
func newScatterWriter(parts [][]byte) *scatterWriter {
	w := &scatterWriter{parts: parts, start: make([]int, len(parts)+1), ri: -1}
	for i, p := range parts {
		w.parts[i] = p[:len(p):len(p)]
		w.start[i+1] = w.start[i] + len(p)
	}
	w.advanceWrite()
	return w
}

// total returns the combined length of the fragments.
func (w *scatterWriter) total() int { return w.start[len(w.parts)] }

// written returns the logical offset of the next byte to be written.
func (w *scatterWriter) written() int {
	if w.wi >= len(w.parts) {
		return w.total()
	}
	return w.start[w.wi] + w.wo
}

// advanceWrite skips empty fragments and keeps the write cursor at the first
// byte available for writing.
func (w *scatterWriter) advanceWrite() {
	for w.wi < len(w.parts) && w.wo == len(w.parts[w.wi]) {
		w.wi++
		w.wo = 0
	}
}

// write copies src into the fragments at the write cursor. It is used for
// literals, whose source never overlaps the output.
func (w *scatterWriter) write(src []byte) {
	for len(src) > 0 {
		if w.wi >= len(w.parts) {
			// Out of destination room: index out of range panics, and
			// decodeBlockScatter recovers it as an invalid-block error.
			_ = w.parts[w.wi][0]
		}
		n := copy(w.parts[w.wi][w.wo:], src)
		src = src[n:]
		w.wo += n
		w.advanceWrite()
	}
}

// positionRead places the read cursor at logical offset pos within the
// concatenation of dict (occupying negative offsets) and the fragments.
func (w *scatterWriter) positionRead(pos int, dictLen int) bool {
	if pos < 0 {
		if -pos > dictLen {
			return false
		}
		w.ri = -1
		w.ro = dictLen + pos
		return true
	}
	// Locate the fragment containing pos. Empty fragments are skipped because
	// the search lands on the first fragment whose end exceeds pos.
	lo, hi := 0, len(w.parts)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if w.start[mid+1] <= pos {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	w.ri = lo
	if w.ri >= len(w.parts) {
		return false
	}
	w.ro = pos - w.start[w.ri]
	return true
}

// readByte returns the byte at the read cursor and advances it one position,
// moving from the dictionary into the first fragment and then across
// fragments. The read cursor never passes the write cursor.
func (w *scatterWriter) readByte(dict []byte) byte {
	if w.ri < 0 {
		b := dict[w.ro]
		w.ro++
		if w.ro == len(dict) {
			w.ri, w.ro = 0, 0
			// Skip a leading empty first fragment.
			for w.ri < len(w.parts) && len(w.parts[w.ri]) == 0 {
				w.ri++
			}
		}
		return b
	}
	b := w.parts[w.ri][w.ro]
	w.ro++
	for w.ri < len(w.parts) && w.ro == len(w.parts[w.ri]) {
		w.ri++
		w.ro = 0
	}
	return b
}

// copyMatch copies mLen bytes starting mOff bytes before the write cursor.
// The match may begin in the dictionary, may span fragment boundaries, and
// may overlap bytes it is itself writing (a repeated or tiling copy).
func (w *scatterWriter) copyMatch(dict []byte, mOff, mLen int) bool {
	if !w.positionRead(w.written()-mOff, len(dict)) {
		return false
	}
	for i := 0; i < mLen; i++ {
		b := w.readByte(dict)
		if w.wi >= len(w.parts) {
			return false
		}
		w.parts[w.wi][w.wo] = b
		w.wo++
		w.advanceWrite()
	}
	return true
}

// UncompressBlockScatter uncompresses the LZ4 block src into the fragments
// dsts, which together form one contiguous logical destination, and returns
// the number of bytes written.
//
// Literals and matches may span fragment boundaries, and a match may refer to
// bytes written earlier in the same call, including bytes in a previous
// fragment. The fragments themselves are the only output storage; no
// contiguous buffer of the output size is allocated.
//
// dict is an optional external dictionary consulted for matches whose start
// precedes the beginning of the output, as in UncompressBlock.
//
// The sum of the fragment lengths is checked for integer overflow before any
// byte is written. On error, the returned count is the number of bytes
// produced so far: logical offsets [0, count) may have been modified, while
// bytes at offsets >= count and capacity past each fragment's length are
// untouched.
func UncompressBlockScatter(src []byte, dsts [][]byte, dict []byte) (int, error) {
	var total int64
	for _, p := range dsts {
		total += int64(len(p))
		if total > math.MaxInt {
			return 0, lz4errors.ErrInvalidSourceShortBuffer
		}
	}
	if len(src) == 0 {
		return 0, nil
	}
	w := newScatterWriter(dsts)
	if decodeBlockScatter(w, src, dict) < 0 {
		return w.written(), lz4errors.ErrInvalidSourceShortBuffer
	}
	return w.written(), nil
}

// decodeBlockScatter is the portable scatter decoder. It mirrors
// decodeBlockGo but performs every write through w, so neither the 16-byte
// literal over-copy nor any other access touches bytes past the logical
// output produced so far. It returns -2 on invalid input or a truncated
// destination; bounds-check panics are recovered the same way.
func decodeBlockScatter(w *scatterWriter, src, dict []byte) (ret int) {
	const hasError = -2

	src = src[:len(src):len(src)]

	defer func() {
		if recover() != nil {
			ret = hasError
		}
	}()

	var si uint
	for si < uint(len(src)) {
		// Literals and match lengths (token).
		b := uint(src[si])
		si++

		// Literals.
		if lLen := b >> 4; lLen > 0 {
			if lLen == 0xF {
				for {
					x := uint(src[si])
					if lLen += x; int(lLen) < 0 {
						return hasError
					}
					si++
					if x != 0xFF {
						break
					}
				}
			}
			end := si + lLen
			if end < si || end > uint(len(src)) {
				return hasError
			}
			w.write(src[si:end])
			si = end
		}

		// Match.
		mLen := b & 0xF
		if si == uint(len(src)) && mLen == 0 {
			break
		} else if si+2 > uint(len(src)) {
			return hasError
		}
		mLen += minMatch

		offset := u16(src[si:])
		if offset == 0 {
			return hasError
		}
		si += 2

		if mLen >= 15+minMatch {
			for {
				x := uint(src[si])
				if mLen += x; int(mLen) < 0 {
					return hasError
				}
				si++
				if x != 0xFF {
					break
				}
			}
		}

		if si > uint(len(src)) {
			return hasError
		}
		if !w.copyMatch(dict, int(offset), int(mLen)) {
			return hasError
		}
	}

	return w.written()
}
