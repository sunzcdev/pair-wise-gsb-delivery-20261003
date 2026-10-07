package lz4block

import (
	"math"
	"sort"

	"github.com/pierrec/lz4/v4/internal/lz4errors"
)

// UncompressBlockSlices uncompresses src into the logical concatenation of
// dsts. Unlike decoding into each destination separately, matches may start in
// an earlier destination, in bytes decoded during this call, or in dict.
//
// Destination slices must not overlap. On success, the decoded byte count is
// returned. On error, the returned count is the complete logical prefix that
// was written; only destination bytes covered by that prefix may have been
// modified.
func UncompressBlockSlices(src []byte, dsts [][]byte, dict []byte) (n int, err error) {
	total, ok := sliceBytes(dsts)
	if !ok {
		return 0, lz4errors.ErrInvalidSourceShortBuffer
	}
	if len(src) == 0 {
		return 0, nil
	}

	d := &scatterDst{
		dsts:  make([][]byte, 0, len(dsts)),
		total: total,
	}
	for i := range dsts {
		if len(dsts[i]) > 0 {
			d.dsts = append(d.dsts, dsts[i])
		}
	}
	d.ends = make([]int, len(d.dsts))
	for i := range d.dsts {
		d.ends[i] = total
	}
	for i := len(d.dsts) - 2; i >= 0; i-- {
		d.ends[i] = d.ends[i+1] - len(d.dsts[i+1])
	}

	defer func() {
		if recover() != nil {
			n = d.pos
			err = lz4errors.ErrInvalidSourceShortBuffer
		}
	}()

	n, ok = d.decode(src, dict)
	if !ok {
		return n, lz4errors.ErrInvalidSourceShortBuffer
	}
	return n, nil
}

func sliceBytes(dsts [][]byte) (int, bool) {
	var total int
	for _, dst := range dsts {
		if len(dst) > math.MaxInt-total {
			return 0, false
		}
		total += len(dst)
	}
	return total, true
}

type scatterDst struct {
	dsts  [][]byte
	ends  []int
	total int
	pos   int
}

func (d *scatterDst) decode(src, dict []byte) (int, bool) {
	for si := 0; si < len(src); {
		token := src[si]
		si++

		lLen := int(token >> 4)
		if lLen == 0xF {
			var ok bool
			lLen, si, ok = readLength(src, si, lLen)
			if !ok {
				return d.pos, false
			}
		}
		if lLen > 0 {
			if si+lLen < si || si+lLen > len(src) || d.pos+lLen < d.pos || d.pos+lLen > d.total {
				return d.pos, false
			}
			d.copyLiterals(src[si : si+lLen])
			si += lLen
		}

		mLen := int(token&0xF) + minMatch
		if si == len(src) && token&0xF == 0 {
			break
		}
		if si+2 < si || si+2 > len(src) {
			return d.pos, false
		}
		offset := int(u16(src[si:]))
		si += 2
		if len(dict) > math.MaxInt-d.pos || offset == 0 || offset > len(dict)+d.pos {
			return d.pos, false
		}

		if token&0xF == 0xF {
			var ok bool
			mLen, si, ok = readLength(src, si, mLen)
			if !ok {
				return d.pos, false
			}
		}
		if d.pos+mLen < d.pos || d.pos+mLen > d.total {
			return d.pos, false
		}
		d.copyMatch(dict, offset, mLen)
	}
	return d.pos, true
}

func readLength(src []byte, si, length int) (int, int, bool) {
	for {
		if si >= len(src) {
			return length, si, false
		}
		x := int(src[si])
		si++
		if length > math.MaxInt-x {
			return length, si, false
		}
		length += x
		if x != 0xFF {
			return length, si, true
		}
	}
}

func (d *scatterDst) segment(pos int) (int, int) {
	i := sort.Search(len(d.ends), func(i int) bool { return pos < d.ends[i] })
	return i, pos - d.ends[i] + len(d.dsts[i])
}

func (d *scatterDst) copyLiterals(src []byte) {
	di, off := d.segment(d.pos)
	for len(src) > 0 {
		dst := d.dsts[di]
		n := copy(dst[off:], src)
		d.pos += n
		src = src[n:]
		off += n
		if off == len(dst) {
			di++
			off = 0
		}
	}
}

func (d *scatterDst) copyMatch(dict []byte, offset, length int) {
	start := d.pos
	template := min(offset, length)
	d.copyFrom(len(dict)+start-offset, start, template, dict)

	for remaining := length - template; remaining > 0; {
		n := min(offset, remaining)
		d.copyFrom(len(dict)+start, d.pos, n, dict)
		remaining -= n
		if remaining == 0 {
			break
		}
		if offset > remaining || offset > math.MaxInt-offset {
			offset = remaining
		} else {
			offset *= 2
		}
	}
}

func (d *scatterDst) copyFrom(srcPos, dstPos, length int, dict []byte) {
	var si, srcOff int
	if srcPos < len(dict) {
		si = -1
		srcOff = srcPos
	} else {
		si, srcOff = d.segment(srcPos - len(dict))
	}

	di, dstOff := d.segment(dstPos)
	for length > 0 {
		var n int
		if si < 0 {
			n = min(length, len(dict)-srcOff, len(d.dsts[di])-dstOff)
			copy(d.dsts[di][dstOff:dstOff+n], dict[srcOff:srcOff+n])
		} else {
			n = min(length, len(d.dsts[si])-srcOff, len(d.dsts[di])-dstOff)
			copy(d.dsts[di][dstOff:dstOff+n], d.dsts[si][srcOff:srcOff+n])
		}

		length -= n
		d.pos += n
		dstOff += n
		if dstOff == len(d.dsts[di]) && length > 0 {
			di++
			dstOff = 0
		}

		if si < 0 {
			srcOff += n
			if srcOff == len(dict) && length > 0 {
				si = 0
				srcOff = 0
			}
		} else {
			srcOff += n
			if srcOff == len(d.dsts[si]) && length > 0 {
				si++
				srcOff = 0
			}
		}
	}
}
