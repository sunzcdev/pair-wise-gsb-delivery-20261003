package bitset

// alignUp rounds p up to the next multiple of align, which must be a
// power of two. ok is false if the rounded value overflows uint.
func alignUp(p, align uint) (rounded uint, ok bool) {
	mask := align - 1
	if p > ^uint(0)-mask {
		return 0, false
	}
	return (p + mask) &^ mask, true
}

// scanFreeRange looks for a run of length consecutive clear bits
// starting at an aligned position p with p0 <= p < pMax and the whole
// run [p, p+length) contained in [0, bound). p0 must be a multiple of
// align and pMax must not exceed bound, which must not exceed the
// logical length of the bitset.
//
// Rather than testing every aligned candidate, the scan jumps
// directly past the first set bit of each failed candidate: a set bit
// at position s invalidates every aligned start in (s-length, s], so
// the next possible start is the next aligned position after s.
func (b *BitSet) scanFreeRange(length, p0, pMax, bound, align uint) (uint, bool) {
	p := p0
	// p < pMax <= bound keeps bound-p and p+length free of overflow.
	for p < pMax && length <= bound-p {
		next, ok := b.NextSet(p)
		if !ok || next >= p+length {
			return p, true
		}
		p, ok = alignUp(next+1, align)
		if !ok {
			return 0, false
		}
	}
	return 0, false
}

// FindFreeRange searches for the first run of length consecutive
// clear bits, considering only runs whose start is a multiple of
// align, and returns it as the half-open interval [lo, hi). The
// search begins at bit start; align must be a power of two (0 and 1
// mean no alignment constraint). The bitset is not modified.
//
// If wrap is true and no run is found at or after start, the search
// resumes from bit 0. A returned interval is always fully contained
// in [0, b.Len()): it never wraps across the end of the bitset.
//
// FindFreeRange panics with an Error if length is zero or align is
// not a power of two. Arithmetic that would overflow uint (huge
// start, length or align values) is detected and reported as
// "not found" rather than wrapping around silently.
func (b *BitSet) FindFreeRange(length, start, align uint, wrap bool) (lo, hi uint, ok bool) {
	if length == 0 {
		panic(Error("bitset: FindFreeRange requires length > 0"))
	}
	if align == 0 {
		align = 1
	}
	if align&(align-1) != 0 {
		panic(Error("bitset: FindFreeRange requires align to be a power of two"))
	}

	bound := b.length

	// First pass: aligned starts at or after start.
	p0, valid := alignUp(start, align)
	if valid {
		if p, found := b.scanFreeRange(length, p0, bound, bound, align); found {
			return p, p + length, true
		}
	}

	if !wrap {
		return 0, 0, false
	}

	// Second pass: aligned starts below the first pass. If alignUp
	// overflowed, no aligned start exists at or after start and the
	// whole bitset remains to be searched.
	stop := p0
	if !valid || stop > bound {
		stop = bound
	}
	if p, found := b.scanFreeRange(length, 0, stop, bound, align); found {
		return p, p + length, true
	}
	return 0, 0, false
}

// ClaimFreeRange behaves like FindFreeRange but, on success, sets all
// bits of the returned interval in a single operation, marking the
// run as used. On failure the bitset is left untouched.
func (b *BitSet) ClaimFreeRange(length, start, align uint, wrap bool) (lo, hi uint, ok bool) {
	lo, hi, ok = b.FindFreeRange(length, start, align, wrap)
	if !ok {
		return 0, 0, false
	}
	b.SetRange(lo, hi)
	return lo, hi, true
}
