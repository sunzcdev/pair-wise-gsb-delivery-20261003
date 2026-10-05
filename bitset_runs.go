package bitset

import "math/bits"

// A RunCursor is a bidirectional cursor over the maximal runs of
// consecutive equal bits of a BitSet: runs of set bits when created with
// [BitSet.Runs], runs of clear bits when created with [BitSet.ClearRuns].
//
// Each run is reported as a half-open interval [start, end): start is the
// index of the first bit of the run and end is one past its last bit.
// Intervals never extend beyond the logical length of the BitSet: bits at
// or after [BitSet.Len] are never reported, even for clear runs and even
// if the last word is only partially used.
//
// Next and Prev search in opposite directions over the remaining range
// [lo, hi) of the cursor and meet in the middle: a full forward traversal
// and a full backward traversal report the same intervals in reverse
// order. SeekForward and SeekBackward reposition the cursor to search
// forward or backward from an arbitrary position; when the position falls
// in the middle of a run, only the part of the run inside the search
// range is reported.
//
// The cursor skips whole words that contain no bit of the requested kind,
// so its cost is proportional to the number of words actually covered by
// runs, not to the number of bits.
//
// A RunCursor borrows its BitSet: it reads the BitSet's current words on
// every call and does NOT take a snapshot. Mutating the BitSet from the
// same goroutine between calls is allowed: subsequent calls observe the
// new contents and intervals are always clipped to the BitSet's current
// length. Note however that the cursor frontiers are not reset by
// mutations, so bits inserted behind a frontier are not revisited. As
// with BitSet itself, mutating a BitSet concurrently while a cursor is
// reading it is a data race.
//
// The zero value of RunCursor is not usable; obtain a cursor with
// [BitSet.Runs] or [BitSet.ClearRuns].
type RunCursor struct {
	b    *BitSet
	ones bool // true: runs of set bits, false: runs of clear bits
	lo   uint // Next reports runs intersecting [lo, hi)
	hi   uint // Prev reports runs intersecting [lo, hi)
}

// Runs returns a cursor over the runs of consecutive set bits of b,
// covering the whole range [0, b.Len()). See [RunCursor] for details.
func (b *BitSet) Runs() RunCursor {
	return RunCursor{b: b, ones: true, lo: 0, hi: b.length}
}

// ClearRuns returns a cursor over the runs of consecutive clear bits of
// b, covering the whole range [0, b.Len()). Bits at or after b.Len() are
// never reported. See [RunCursor] for details.
func (b *BitSet) ClearRuns() RunCursor {
	return RunCursor{b: b, ones: false, lo: 0, hi: b.length}
}

// SeekForward repositions the cursor so that subsequent Next calls
// enumerate the runs intersecting [pos, b.Len()): the first reported run
// is clipped to start at pos if pos falls inside it. Positions at or
// beyond b.Len() exhaust the forward direction.
func (c *RunCursor) SeekForward(pos uint) {
	c.lo = pos
	c.hi = c.b.length
}

// SeekBackward repositions the cursor so that subsequent Prev calls
// enumerate the runs intersecting [0, pos+1): the first reported run is
// clipped to end at pos+1 if pos falls inside it. Positions at or beyond
// b.Len()-1 cover the whole bitset.
func (c *RunCursor) SeekBackward(pos uint) {
	c.lo = 0
	if pos >= c.b.length {
		c.hi = c.b.length
	} else {
		c.hi = pos + 1
	}
}

// Next advances the cursor to the next run in forward direction and
// returns its half-open interval [start, end). It reports ok == false
// when no run remains in the cursor range. If the run starts before the
// cursor frontier (e.g. after a SeekForward into the middle of a run),
// only the part inside the range is returned.
func (c *RunCursor) Next() (start, end uint, ok bool) {
	c.clamp()
	start, end, ok = c.b.nextRun(c.ones, c.lo, c.hi)
	if ok {
		c.lo = end
	}
	return start, end, ok
}

// Prev moves the cursor to the next run in backward direction and returns
// its half-open interval [start, end). It reports ok == false when no run
// remains in the cursor range. If the run extends past the cursor
// frontier (e.g. after a SeekBackward into the middle of a run), only the
// part inside the range is returned.
func (c *RunCursor) Prev() (start, end uint, ok bool) {
	c.clamp()
	start, end, ok = c.b.prevRun(c.ones, c.lo, c.hi)
	if ok {
		c.hi = start
	}
	return start, end, ok
}

// clamp restricts the cursor range to the current logical length of the
// BitSet, so results never exceed it even if the BitSet was shrunk after
// the cursor was created.
func (c *RunCursor) clamp() {
	if c.lo > c.b.length {
		c.lo = c.b.length
	}
	if c.hi > c.b.length {
		c.hi = c.b.length
	}
}

// runWord returns the word at index x, complemented for clear runs.
// Words beyond the allocated storage read as zero.
func (b *BitSet) runWord(ones bool, x int) uint64 {
	var w uint64
	if x < len(b.set) {
		w = b.set[x]
	}
	if !ones {
		w = ^w
	}
	return w
}

// lowMask returns a mask keeping bits 0..k (k in 0..63).
func lowMask(k uint) uint64 {
	return allBits >> (wordMask - k)
}

// nextRun finds the first run of target bits intersecting [from, hi),
// clipped to that range. Whole words without a target bit are skipped.
func (b *BitSet) nextRun(ones bool, from, hi uint) (start, end uint, ok bool) {
	if from >= hi {
		return 0, 0, false
	}
	last := int((hi - 1) >> log2WordSize)
	x := int(from >> log2WordSize)

	// locate the first word holding a target bit, skipping empty words
	w := b.runWord(ones, x) & (allBits << wordsIndex(from))
	if x == last {
		w &= lowMask(wordsIndex(hi - 1))
	}
	for w == 0 {
		x++
		if x > last {
			return 0, 0, false
		}
		w = b.runWord(ones, x)
		if x == last {
			w &= lowMask(wordsIndex(hi - 1))
		}
	}

	start = uint(x<<log2WordSize) + uint(bits.TrailingZeros64(w))

	// end of the run within this word
	n := uint(bits.TrailingZeros64(^(w >> wordsIndex(start))))
	if max := wordSize - wordsIndex(start); n > max {
		n = max
	}
	end = start + n

	// the run may continue across whole words
	for end < hi && wordsIndex(end) == 0 {
		w = b.runWord(ones, int(end>>log2WordSize))
		n = uint(bits.TrailingZeros64(^w))
		end += n
		if n < wordSize {
			break
		}
	}
	if end > hi {
		end = hi
	}
	return start, end, true
}

// prevRun finds the last run of target bits intersecting [lo, hi),
// clipped to that range. Whole words without a target bit are skipped.
func (b *BitSet) prevRun(ones bool, lo, hi uint) (start, end uint, ok bool) {
	if lo >= hi {
		return 0, 0, false
	}
	first := int(lo >> log2WordSize)
	x := int((hi - 1) >> log2WordSize)

	// locate the last word holding a target bit, skipping empty words
	w := b.runWord(ones, x) & lowMask(wordsIndex(hi-1))
	if x == first {
		w &= allBits << wordsIndex(lo)
	}
	for w == 0 {
		x--
		if x < first {
			return 0, 0, false
		}
		w = b.runWord(ones, x)
		if x == first {
			w &= allBits << wordsIndex(lo)
		}
	}

	end = uint(x<<log2WordSize) + uint(bits.Len64(w))

	// start of the run within this word
	n := uint(bits.LeadingZeros64(^(w << (wordSize - uint(bits.Len64(w))))))
	if max := uint(bits.Len64(w)); n > max {
		n = max
	}
	start = end - n

	// the run may continue across whole words
	for start > lo && wordsIndex(start) == 0 {
		w = b.runWord(ones, int(start>>log2WordSize)-1)
		n = uint(bits.LeadingZeros64(^w))
		start -= n
		if n < wordSize {
			break
		}
	}
	if start < lo {
		start = lo
	}
	return start, end, true
}
