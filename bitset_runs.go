package bitset

import "math/bits"

// A RunCursor enumerates the maximal runs of consecutive identical bits of a
// BitSet as half-open intervals [start, end), always strictly contained in
// the logical length of the set. A cursor enumerates either runs of set bits
// or runs of clear bits, chosen at creation time.
//
// The cursor scans whole 64-bit words and skips entirely uninteresting words
// without inspecting individual bits, so sparse sets are traversed in time
// proportional to the number of runs, not the number of bits.
//
// A RunCursor borrows the BitSet's live word slice; it does NOT take a
// snapshot. Every call re-reads the underlying words and the current logical
// length, so mutating the BitSet while iterating is reflected in subsequent
// calls: runs may appear, vanish, shrink or grow, and previously returned
// intervals are not re-validated. Already returned intervals remain valid
// values but may no longer describe the set. For a stable view of the data,
// iterate over a Clone instead. As usual, concurrent mutation from another
// goroutine requires external synchronization.
//
// The forward (Next) and backward (Prev) states are independent: each cursor
// may be advanced in both directions, and a full forward traversal yields the
// same intervals as a full backward traversal in reverse order.
type RunCursor struct {
	b     *BitSet
	ones  bool // true: enumerate runs of set bits; false: runs of clear bits
	next  uint // next forward search position
	prev  uint // next backward search position, inclusive
	hasPr bool // whether prev holds a valid position
}

// Runs returns a RunCursor over the runs of consecutive set bits of b if
// ones is true, or over the runs of consecutive clear bits if ones is false.
// The search range of every call is strictly limited to [0, b.Len()).
func (b *BitSet) Runs(ones bool) *RunCursor {
	c := &RunCursor{b: b, ones: ones}
	if b.length > 0 {
		c.prev = b.length - 1
		c.hasPr = true
	}
	return c
}

// NextFrom resets the forward position of the cursor to pos and returns the
// first run intersecting [pos, b.Len()). If pos lies inside a run, only the
// in-range part [pos, runEnd) is returned. It returns ok == false when no
// run exists at or after pos within the logical length.
func (c *RunCursor) NextFrom(pos uint) (start, end uint, ok bool) {
	c.next = pos
	return c.Next()
}

// Next returns the next run at or after the current forward position and
// advances the cursor past it.
func (c *RunCursor) Next() (start, end uint, ok bool) {
	b := c.b
	pos := c.next
	if b.length == 0 || pos >= b.length {
		return 0, 0, false
	}
	lastWord := int((b.length - 1) >> log2WordSize)
	x := int(pos >> log2WordSize)

	// process first (partial) word, then skip whole uninteresting words
	w := c.targetWord(x, lastWord) & (allBits << wordsIndex(pos))
	for w == 0 {
		x++
		if x > lastWord {
			c.next = b.length
			return 0, 0, false
		}
		w = c.targetWord(x, lastWord)
	}
	start = uint(x<<log2WordSize) + uint(bits.TrailingZeros64(w))
	end = c.runEndForward(x, start, lastWord)
	c.next = end
	return start, end, true
}

// PrevFrom resets the backward position of the cursor to pos (clamped to
// b.Len()-1 if pos is beyond the logical length) and returns the last run
// intersecting [0, pos]. If pos lies inside a run, only the in-range part
// [runStart, pos+1) is returned. It returns ok == false when no run exists
// at or before pos.
func (c *RunCursor) PrevFrom(pos uint) (start, end uint, ok bool) {
	c.prev = pos
	c.hasPr = true
	return c.Prev()
}

// Prev returns the previous run at or before the current backward position
// and moves the cursor before it.
func (c *RunCursor) Prev() (start, end uint, ok bool) {
	b := c.b
	if b.length == 0 || !c.hasPr {
		return 0, 0, false
	}
	pos := c.prev
	if pos >= b.length {
		pos = b.length - 1
	}
	lastWord := int((b.length - 1) >> log2WordSize)
	x := int(pos >> log2WordSize)

	// process first (partial) word, then skip whole uninteresting words
	w := c.targetWord(x, lastWord) & (allBits >> (wordMask - wordsIndex(pos)))
	for w == 0 {
		x--
		if x < 0 {
			c.hasPr = false
			return 0, 0, false
		}
		w = c.targetWord(x, lastWord)
	}
	end = uint(x<<log2WordSize) + uint(bits.Len64(w))
	start = c.runStartBackward(x, end, lastWord)
	if start == 0 {
		c.hasPr = false
	} else {
		c.prev = start - 1
	}
	return start, end, true
}

// targetWord returns the word at index x with the bits of interest set to 1,
// masked so that bits at or beyond the logical length read as 0.
func (c *RunCursor) targetWord(x, lastWord int) uint64 {
	w := c.b.set[x]
	if !c.ones {
		w = ^w
	}
	if x == lastWord && !c.b.isLenExactMultiple() {
		w &= allBits >> (wordSize - wordsIndex(c.b.length))
	}
	return w
}

// runEndForward returns the exclusive end of the run containing start,
// scanning whole words forward from word x. The result never exceeds the
// logical length.
func (c *RunCursor) runEndForward(x int, start uint, lastWord int) uint {
	inv := ^c.targetWord(x, lastWord) & (allBits << wordsIndex(start))
	for inv == 0 {
		x++
		if x > lastWord {
			return c.b.length
		}
		inv = ^c.targetWord(x, lastWord)
	}
	return uint(x<<log2WordSize) + uint(bits.TrailingZeros64(inv))
}

// runStartBackward returns the inclusive start of the run whose exclusive
// end is end, scanning whole words backward from word x.
func (c *RunCursor) runStartBackward(x int, end uint, lastWord int) uint {
	inv := ^c.targetWord(x, lastWord) & (allBits >> (wordMask - wordsIndex(end-1)))
	for inv == 0 {
		x--
		if x < 0 {
			return 0
		}
		inv = ^c.targetWord(x, lastWord)
	}
	return uint(x<<log2WordSize) + uint(bits.Len64(inv))
}
