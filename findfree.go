package bitset

import "errors"

// Errors returned by FindFreeRange and ClaimFreeRange.
var (
	// ErrZeroLengthRange is returned when a range of length zero is requested.
	ErrZeroLengthRange = errors.New("bitset: cannot search for a zero-length range")
	// ErrInvalidAlignment is returned when the requested alignment is not a
	// power of two (alignment zero is rejected as well).
	ErrInvalidAlignment = errors.New("bitset: alignment must be a power of two")
	// ErrNoFreeRange is returned when no free range satisfying the
	// constraints exists within the logical length of the bit set.
	ErrNoFreeRange = errors.New("bitset: no free range found")
)

// alignUp returns the smallest multiple of alignment greater than or equal
// to i. alignment must be a power of two. The second return value is false
// if the result would overflow uint.
func alignUp(i, alignment uint) (uint, bool) {
	rem := i & (alignment - 1)
	if rem == 0 {
		return i, true
	}
	add := alignment - rem
	if i > ^uint(0)-add {
		return 0, false
	}
	return i + add, true
}

// FindFreeRange returns the first half-open interval [lo, hi) of length
// consecutive clear bits within the logical length of the bit set, such
// that hi-lo == length, lo >= start and lo is a multiple of alignment.
// The interval never extends beyond Len(): a range is never spliced
// together from the end and the beginning of the set.
//
// The search skips directly from an occupied bit to the next aligned
// candidate instead of probing every aligned offset.
//
// If wrap is true and no interval is found at or after start, the search
// continues from the beginning of the set, considering only intervals
// that end at or before start (clamped to the logical length), so the two
// search passes never overlap.
//
// length must be greater than zero and alignment must be a power of two
// (use 1 for no alignment constraint); otherwise ErrZeroLengthRange or
// ErrInvalidAlignment is returned. If no suitable interval exists,
// ErrNoFreeRange is returned. The bit set is not modified.
func (b *BitSet) FindFreeRange(length, start, alignment uint, wrap bool) (lo, hi uint, err error) {
	return b.findFreeRange(length, start, alignment, wrap)
}

// ClaimFreeRange behaves like FindFreeRange, but on success it also sets
// every bit of the returned interval in a single pass, marking the range
// as occupied. On failure the bit set is left untouched.
func (b *BitSet) ClaimFreeRange(length, start, alignment uint, wrap bool) (lo, hi uint, err error) {
	lo, hi, err = b.findFreeRange(length, start, alignment, wrap)
	if err != nil {
		return 0, 0, err
	}
	// hi <= Len(), so SetRange cannot grow the set here.
	b.SetRange(lo, hi)
	return lo, hi, nil
}

func (b *BitSet) findFreeRange(length, start, alignment uint, wrap bool) (lo, hi uint, err error) {
	if length == 0 {
		return 0, 0, ErrZeroLengthRange
	}
	if alignment == 0 || alignment&(alignment-1) != 0 {
		return 0, 0, ErrInvalidAlignment
	}
	if length > b.length {
		return 0, 0, ErrNoFreeRange
	}

	limit := b.length
	candidate, ok := alignUp(start, alignment)
	for {
		// length <= limit, so limit-length cannot underflow and
		// candidate+length cannot overflow.
		for ok && candidate <= limit-length {
			end := candidate + length
			next, isSet := b.NextSet(candidate)
			if !isSet || next >= end {
				return candidate, end, nil
			}
			// Jump past the occupied bit to the next aligned candidate.
			candidate, ok = alignUp(next+1, alignment)
		}
		if !wrap {
			break
		}
		wrap = false
		if start < limit {
			limit = start
		}
		if length > limit {
			break
		}
		candidate, ok = 0, true
	}
	return 0, 0, ErrNoFreeRange
}
