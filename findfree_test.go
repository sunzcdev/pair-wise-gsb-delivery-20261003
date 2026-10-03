package bitset

import (
	"errors"
	"math/rand"
	"testing"
)

// naiveFindFreeRange is a deliberately simple reference implementation of
// FindFreeRange: it probes every aligned candidate and tests every bit of
// every candidate interval. It is only suitable for small bit sets.
func naiveFindFreeRange(b *BitSet, length, start, alignment uint, wrap bool) (uint, uint, error) {
	if length == 0 {
		return 0, 0, ErrZeroLengthRange
	}
	if alignment == 0 || alignment&(alignment-1) != 0 {
		return 0, 0, ErrInvalidAlignment
	}

	probe := func(lo, limit uint) (uint, uint, bool) {
		if length > limit {
			return 0, 0, false
		}
		for ; lo <= limit-length; lo += alignment {
			free := true
			for i := lo; i < lo+length; i++ {
				if b.Test(i) {
					free = false
					break
				}
			}
			if free {
				return lo, lo + length, true
			}
		}
		return 0, 0, false
	}

	first := start
	if rem := first & (alignment - 1); rem != 0 {
		first += alignment - rem // test values never overflow here
	}
	if lo, hi, ok := probe(first, b.Len()); ok {
		return lo, hi, nil
	}
	if wrap {
		limit := b.Len()
		if start < limit {
			limit = start
		}
		if lo, hi, ok := probe(0, limit); ok {
			return lo, hi, nil
		}
	}
	return 0, 0, ErrNoFreeRange
}

func TestFindFreeRangeEmpty(t *testing.T) {
	b := New(256)
	lo, hi, err := b.FindFreeRange(8, 0, 1, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lo != 0 || hi != 8 {
		t.Fatalf("got [%d, %d), want [0, 8)", lo, hi)
	}
}

func TestFindFreeRangeAlignment(t *testing.T) {
	b := New(256)
	b.Set(0).Set(8).Set(16)
	lo, hi, err := b.FindFreeRange(8, 0, 8, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lo != 24 || hi != 32 {
		t.Fatalf("got [%d, %d), want [24, 32)", lo, hi)
	}
}

func TestFindFreeRangeSkipsOccupied(t *testing.T) {
	b := New(1024)
	// Occupy one bit inside each of the first ten aligned windows.
	for i := uint(0); i < 10; i++ {
		b.Set(i*64 + 7)
	}
	lo, hi, err := b.FindFreeRange(64, 0, 64, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lo != 640 || hi != 704 {
		t.Fatalf("got [%d, %d), want [640, 704)", lo, hi)
	}
}

func TestFindFreeRangeStartAndLimit(t *testing.T) {
	b := New(100)
	lo, hi, err := b.FindFreeRange(10, 5, 1, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lo != 5 || hi != 15 {
		t.Fatalf("got [%d, %d), want [5, 15)", lo, hi)
	}

	// No room for 10 bits aligned to 32 at or after 64: 64+10 <= 100 works,
	// so occupy it and expect failure.
	b.SetRange(64, 74)
	if _, _, err := b.FindFreeRange(10, 64, 32, false); !errors.Is(err, ErrNoFreeRange) {
		t.Fatalf("got %v, want ErrNoFreeRange", err)
	}
}

func TestFindFreeRangeWrap(t *testing.T) {
	b := New(128)
	b.SetRange(64, 128) // second half fully occupied
	// Without wrap there is still room in the first half.
	lo, hi, err := b.FindFreeRange(16, 64, 1, false)
	if !errors.Is(err, ErrNoFreeRange) {
		t.Fatalf("got [%d, %d) err=%v, want ErrNoFreeRange", lo, hi, err)
	}
	// With wrap the search continues from the beginning.
	lo, hi, err = b.FindFreeRange(16, 64, 1, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lo != 0 || hi != 16 {
		t.Fatalf("got [%d, %d), want [0, 16)", lo, hi)
	}

	// A wrapped interval must end at or before start: the free bits at
	// [56, 64) cannot be spliced with anything across the set boundary.
	c := New(128)
	c.SetRange(0, 56)
	c.SetRange(64, 128)
	if _, _, err := c.FindFreeRange(16, 64, 1, true); !errors.Is(err, ErrNoFreeRange) {
		t.Fatalf("got %v, want ErrNoFreeRange", err)
	}
}

func TestFindFreeRangeInvalidInput(t *testing.T) {
	b := New(64)
	if _, _, err := b.FindFreeRange(0, 0, 1, false); !errors.Is(err, ErrZeroLengthRange) {
		t.Fatalf("got %v, want ErrZeroLengthRange", err)
	}
	for _, alignment := range []uint{0, 3, 5, 6, 7, 24, 48} {
		if _, _, err := b.FindFreeRange(1, 0, alignment, false); !errors.Is(err, ErrInvalidAlignment) {
			t.Fatalf("alignment %d: got %v, want ErrInvalidAlignment", alignment, err)
		}
	}
	if _, _, err := b.FindFreeRange(65, 0, 1, false); !errors.Is(err, ErrNoFreeRange) {
		t.Fatalf("got %v, want ErrNoFreeRange", err)
	}
}

func TestFindFreeRangeOverflow(t *testing.T) {
	maxUint := ^uint(0)
	empty := New(64)
	// A start near the top of uint must not wrap around to small candidates.
	if _, _, err := empty.FindFreeRange(1, maxUint, 1, false); !errors.Is(err, ErrNoFreeRange) {
		t.Fatalf("got %v, want ErrNoFreeRange", err)
	}
	if _, _, err := empty.FindFreeRange(1, maxUint-3, 4, false); !errors.Is(err, ErrNoFreeRange) {
		t.Fatalf("got %v, want ErrNoFreeRange", err)
	}
	// Rounding maxUint up to an even alignment overflows uint.
	if _, _, err := empty.FindFreeRange(1, maxUint, 2, false); !errors.Is(err, ErrNoFreeRange) {
		t.Fatalf("got %v, want ErrNoFreeRange", err)
	}
	// Huge alignment must not cause an infinite loop or a wrapped result.
	full := New(64)
	full.SetAll()
	if _, _, err := full.FindFreeRange(1, 0, 1<<20, true); !errors.Is(err, ErrNoFreeRange) {
		t.Fatalf("got %v, want ErrNoFreeRange", err)
	}
	if _, _, err := full.FindFreeRange(1, maxUint, 1, true); !errors.Is(err, ErrNoFreeRange) {
		t.Fatalf("got %v, want ErrNoFreeRange", err)
	}
}

func TestFindFreeRangeDoesNotModify(t *testing.T) {
	b := New(256)
	b.Set(3).Set(100).Set(255)
	clone := b.Clone()
	if _, _, err := b.FindFreeRange(4, 0, 4, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !b.Equal(clone) {
		t.Fatal("FindFreeRange modified the bit set")
	}
}

func TestClaimFreeRange(t *testing.T) {
	b := New(128)
	lo, hi, err := b.ClaimFreeRange(16, 0, 16, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lo != 0 || hi != 16 {
		t.Fatalf("got [%d, %d), want [0, 16)", lo, hi)
	}
	if b.Count() != 16 {
		t.Fatalf("count = %d, want 16", b.Count())
	}
	for i := lo; i < hi; i++ {
		if !b.Test(i) {
			t.Fatalf("bit %d not set after claim", i)
		}
	}
	// The next claim must not overlap the first one.
	lo, hi, err = b.ClaimFreeRange(16, 0, 16, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lo != 16 || hi != 32 {
		t.Fatalf("got [%d, %d), want [16, 32)", lo, hi)
	}
	// A failed claim leaves the set untouched.
	full := New(64)
	full.SetAll()
	if _, _, err := full.ClaimFreeRange(1, 0, 1, true); !errors.Is(err, ErrNoFreeRange) {
		t.Fatalf("got %v, want ErrNoFreeRange", err)
	}
	if full.Count() != 64 {
		t.Fatalf("failed claim modified the bit set")
	}
}

func TestFindFreeRangeAgainstNaive(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alignments := []uint{1, 2, 4, 8, 16, 32, 64}
	for trial := 0; trial < 2000; trial++ {
		size := uint(1 + rng.Intn(200))
		b := New(size)
		// Mix sparse and dense occupancy patterns.
		density := rng.Float64()
		for i := uint(0); i < size; i++ {
			if rng.Float64() < density {
				b.Set(i)
			}
		}
		length := uint(rng.Intn(int(size) + 2))
		if length == 0 {
			length = 1
		}
		start := uint(rng.Intn(int(size) + 2))
		alignment := alignments[rng.Intn(len(alignments))]
		wrap := rng.Intn(2) == 0

		lo, hi, err := b.FindFreeRange(length, start, alignment, wrap)
		nlo, nhi, nerr := naiveFindFreeRange(b, length, start, alignment, wrap)
		if (err == nil) != (nerr == nil) {
			t.Fatalf("trial %d: size=%d length=%d start=%d alignment=%d wrap=%v: err=%v, naive err=%v",
				trial, size, length, start, alignment, wrap, err, nerr)
		}
		if err == nil && (lo != nlo || hi != nhi) {
			t.Fatalf("trial %d: size=%d length=%d start=%d alignment=%d wrap=%v: got [%d, %d), naive [%d, %d)",
				trial, size, length, start, alignment, wrap, lo, hi, nlo, nhi)
		}
		if err == nil {
			if hi-lo != length || lo%alignment != 0 || hi > size {
				t.Fatalf("trial %d: invalid interval [%d, %d)", trial, lo, hi)
			}
			for i := lo; i < hi; i++ {
				if b.Test(i) {
					t.Fatalf("trial %d: bit %d of returned interval is set", trial, i)
				}
			}
		}
	}
}

func TestClaimFreeRangeAgainstNaive(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for trial := 0; trial < 500; trial++ {
		size := uint(1 + rng.Intn(128))
		b := New(size)
		for i := uint(0); i < size; i++ {
			if rng.Float64() < 0.4 {
				b.Set(i)
			}
		}
		// Repeatedly claim until the set is exhausted.
		for {
			length := uint(1 + rng.Intn(8))
			start := uint(rng.Intn(int(size)))
			alignment := uint(1) << uint(rng.Intn(4))
			wrap := rng.Intn(2) == 0

			wantLo, wantHi, wantErr := naiveFindFreeRange(b, length, start, alignment, wrap)
			before := b.Clone()
			lo, hi, err := b.ClaimFreeRange(length, start, alignment, wrap)
			if (err == nil) != (wantErr == nil) {
				t.Fatalf("trial %d: err=%v, naive err=%v", trial, err, wantErr)
			}
			if err != nil {
				if !b.Equal(before) {
					t.Fatalf("trial %d: failed claim modified the bit set", trial)
				}
				break
			}
			if lo != wantLo || hi != wantHi {
				t.Fatalf("trial %d: got [%d, %d), naive [%d, %d)", trial, lo, hi, wantLo, wantHi)
			}
			for i := lo; i < hi; i++ {
				if !b.Test(i) {
					t.Fatalf("trial %d: bit %d not set after claim", trial, i)
				}
			}
			if b.Count() != before.Count()+length {
				t.Fatalf("trial %d: count = %d, want %d", trial, b.Count(), before.Count()+length)
			}
		}
	}
}
