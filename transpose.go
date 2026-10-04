package bitset

import (
	"fmt"
	"math/bits"
)

// Transpose converts a matrix, given as one *BitSet per row, into one
// *BitSet per column. It is a building block for inverted index
// construction: on return, column j has bit i set if and only if
// rows[i] has bit j set.
//
// The caller supplies the logical number of columns cols. Rows shorter
// than cols are treated as padded with zeros. Set bits at or beyond
// position cols are ignored, unless strict is true, in which case
// Transpose returns an error naming the offending row.
//
// The result always holds exactly cols BitSets, each of length
// len(rows). Edge cases are well defined: with no rows the result is
// cols empty BitSets, and with cols == 0 the result is empty.
//
// dst may be supplied to reuse a previous result (slices and BitSets),
// avoiding allocations when transposing repeatedly. Any state left in
// dst from an earlier, larger matrix is cleared. Pass nil to allocate
// a fresh result.
//
// A nil row is treated as an empty row.
func Transpose(rows []*BitSet, cols uint, strict bool, dst []*BitSet) ([]*BitSet, error) {
	nrows := uint(len(rows))
	if strict {
		for i, row := range rows {
			if row == nil {
				continue
			}
			if bit, ok := firstSetAtOrBeyond(row, cols); ok {
				return nil, fmt.Errorf("bitset: row %d has bit %d set beyond %d columns", i, bit, cols)
			}
		}
	}

	if uint(cap(dst)) < cols {
		dst = make([]*BitSet, cols)
	} else {
		dst = dst[:cols]
	}
	needed := wordsNeeded(nrows)
	for j := range dst {
		b := dst[j]
		if b == nil {
			b = New(nrows)
			dst[j] = b
			continue
		}
		if cap(b.set) < needed {
			b.set = make([]uint64, needed)
		} else {
			b.set = b.set[:needed]
		}
		for k := range b.set {
			b.set[k] = 0
		}
		b.length = nrows
	}
	if nrows == 0 || cols == 0 {
		return dst, nil
	}

	colWords := wordsNeeded(cols)
	var block [wordSize]uint64
	for base := uint(0); base < nrows; base += wordSize {
		for w := 0; w < colWords; w++ {
			for r := uint(0); r < wordSize; r++ {
				var v uint64
				if i := base + r; i < nrows {
					if row := rows[i]; row != nil && w < len(row.set) {
						v = row.set[w]
					}
				}
				block[r] = v
			}
			if rem := cols & wordMask; w == colWords-1 && rem != 0 {
				keep := uint64(1)<<rem - 1
				for r := range block {
					block[r] &= keep
				}
			}
			transpose64(&block)
			out := base >> log2WordSize
			for r := uint(0); r < wordSize; r++ {
				if block[r] == 0 {
					continue
				}
				c := uint(w)<<log2WordSize + r
				if c >= cols {
					break
				}
				dst[c].set[out] = block[r]
			}
		}
	}
	return dst, nil
}

// firstSetAtOrBeyond returns the first bit set at or beyond position
// from, or false if there is none.
func firstSetAtOrBeyond(b *BitSet, from uint) (uint, bool) {
	w := from >> log2WordSize
	if w < uint(len(b.set)) {
		if word := b.set[w] >> wordsIndex(from); word != 0 {
			return from + uint(bits.TrailingZeros64(word)), true
		}
		w++
	}
	for ; w < uint(len(b.set)); w++ {
		if b.set[w] != 0 {
			return w<<log2WordSize + uint(bits.TrailingZeros64(b.set[w])), true
		}
	}
	return 0, false
}

// transpose64 transposes a 64x64 bit matrix in place: on return,
// a[j] bit i holds the original a[i] bit j, with bit 0 the least
// significant bit. It is the Hacker's Delight recursive block
// transpose, mirrored so that LSB-indexed bits transpose in place.
func transpose64(a *[wordSize]uint64) {
	mask := uint64(0x00000000ffffffff)
	for j := uint(32); j != 0; {
		for k := uint(0); k < wordSize; k = (k + j + 1) &^ j {
			t := ((a[k] >> j) ^ a[k+j]) & mask
			a[k] ^= t << j
			a[k+j] ^= t
		}
		j >>= 1
		mask ^= mask << j
	}
}
