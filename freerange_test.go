package bitset

import (
	"math/rand"
	"testing"
)

// naiveFindFreeRange is a deliberately simple reference
// implementation: it tests every aligned candidate position one by
// one. It is only suitable for small bitsets.
func naiveFindFreeRange(b *BitSet, length, start, align uint, wrap bool) (uint, uint, bool) {
	if align == 0 {
		align = 1
	}
	bound := b.Len()
	try := func(from, stop uint) (uint, uint, bool) {
		for p := from; p < stop && length <= bound-p; p += align {
			clear := true
			for i := p; i < p+length; i++ {
				if b.Test(i) {
					clear = false
					break
				}
			}
			if clear {
				return p, p + length, true
			}
		}
		return 0, 0, false
	}
	first := (start + align - 1) / align * align
	if lo, hi, ok := try(first, bound); ok {
		return lo, hi, true
	}
	if !wrap {
		return 0, 0, false
	}
	stop := first
	if stop > bound {
		stop = bound
	}
	return try(0, stop)
}

func TestFindFreeRangeBasic(t *testing.T) {
	b := New(256)
	b.SetRange(0, 10).SetRange(64, 70).SetRange(200, 256)

	lo, hi, ok := b.FindFreeRange(8, 0, 1, false)
	if !ok || lo != 10 || hi != 18 {
		t.Fatalf("got [%d,%d),%v, want [10,18),true", lo, hi, ok)
	}

	// alignment pushes the start to the next multiple of 16
	lo, hi, ok = b.FindFreeRange(8, 0, 16, false)
	if !ok || lo != 16 || hi != 24 {
		t.Fatalf("got [%d,%d),%v, want [16,24),true", lo, hi, ok)
	}

	// start is rounded up to alignment, but bits 0-9 are set, so the
	// search jumps past the set bit to the next aligned position
	lo, hi, ok = b.FindFreeRange(4, 3, 8, false)
	if !ok || lo != 16 || hi != 20 {
		t.Fatalf("got [%d,%d),%v, want [16,20),true", lo, hi, ok)
	}

	// no room left at or after start
	if _, _, ok = b.FindFreeRange(16, 220, 1, false); ok {
		t.Fatal("expected no result without wrap")
	}

	// wrap finds a run before start, never crossing the end
	lo, hi, ok = b.FindFreeRange(16, 220, 1, true)
	if !ok || lo != 10 || hi != 26 {
		t.Fatalf("got [%d,%d),%v, want [10,26),true", lo, hi, ok)
	}

	// a run may end exactly at the bitset length
	lo, hi, ok = b.FindFreeRange(56, 144, 1, false)
	if !ok || lo != 144 || hi != 200 {
		t.Fatalf("got [%d,%d),%v, want [144,200),true", lo, hi, ok)
	}
}

func TestFindFreeRangeDoesNotModify(t *testing.T) {
	b := New(128)
	b.Set(5).Set(90)
	snapshot := b.Clone()
	if _, _, ok := b.FindFreeRange(4, 0, 4, true); !ok {
		t.Fatal("expected a result")
	}
	if !b.Equal(snapshot) {
		t.Fatal("FindFreeRange modified the bitset")
	}
}

func TestFindFreeRangeInvalidParameters(t *testing.T) {
	assertPanic := func(name string, f func()) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("%s: expected panic", name)
			} else if _, ok := r.(Error); !ok {
				t.Fatalf("%s: expected bitset.Error, got %v", name, r)
			}
		}()
		f()
	}
	b := New(64)
	assertPanic("zero length", func() { b.FindFreeRange(0, 0, 1, false) })
	assertPanic("zero length claim", func() { b.ClaimFreeRange(0, 0, 1, false) })
	assertPanic("align 3", func() { b.FindFreeRange(1, 0, 3, false) })
	assertPanic("align 6", func() { b.FindFreeRange(1, 0, 6, false) })
	assertPanic("align 6 claim", func() { b.ClaimFreeRange(1, 0, 6, false) })
}

func TestFindFreeRangeOverflow(t *testing.T) {
	b := New(128)
	maxUint := ^uint(0)

	// huge length: not found, must not hang or wrap around
	if _, _, ok := b.FindFreeRange(maxUint, 0, 1, true); ok {
		t.Fatal("expected no result for huge length")
	}
	// huge start: first pass empty, wrap searches from 0
	lo, hi, ok := b.FindFreeRange(4, maxUint-3, 4, true)
	if !ok || lo != 0 || hi != 4 {
		t.Fatalf("got [%d,%d),%v, want [0,4),true", lo, hi, ok)
	}
	// huge start without wrap: not found
	if _, _, ok := b.FindFreeRange(4, maxUint-3, 4, false); ok {
		t.Fatal("expected no result for huge start without wrap")
	}
	// huge power-of-two align with huge start: alignUp overflows
	// (on 32-bit platforms the shift yields 0, i.e. no alignment,
	// which is equally out of range for this start)
	if _, _, ok := b.FindFreeRange(4, maxUint-3, uint(1)<<63, false); ok {
		t.Fatal("expected no result for overflowing alignment")
	}
}

func TestClaimFreeRange(t *testing.T) {
	b := New(128)
	b.SetRange(0, 32)

	lo, hi, ok := b.ClaimFreeRange(16, 0, 16, false)
	if !ok || lo != 32 || hi != 48 {
		t.Fatalf("got [%d,%d),%v, want [32,48),true", lo, hi, ok)
	}
	for i := lo; i < hi; i++ {
		if !b.Test(i) {
			t.Fatalf("bit %d not set after claim", i)
		}
	}

	// the claimed range is skipped by the next claim
	lo, hi, ok = b.ClaimFreeRange(16, 0, 16, false)
	if !ok || lo != 48 || hi != 64 {
		t.Fatalf("got [%d,%d),%v, want [48,64),true", lo, hi, ok)
	}

	// failed claim leaves the bitset untouched
	snapshot := b.Clone()
	if _, _, ok = b.ClaimFreeRange(128, 0, 1, false); ok {
		t.Fatal("expected claim to fail")
	}
	if !b.Equal(snapshot) {
		t.Fatal("failed claim modified the bitset")
	}
}

func TestFindFreeRangeAgainstNaive(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 2000; trial++ {
		size := uint(1 + rng.Intn(200))
		b := New(size)
		for i := uint(0); i < size; i++ {
			if rng.Intn(2) == 0 {
				b.Set(i)
			}
		}
		length := uint(1 + rng.Intn(20))
		start := uint(rng.Intn(int(size) + 4))
		align := uint(1) << uint(rng.Intn(5))
		wrap := rng.Intn(2) == 0

		lo, hi, ok := b.FindFreeRange(length, start, align, wrap)
		nlo, nhi, nok := naiveFindFreeRange(b, length, start, align, wrap)
		if ok != nok || lo != nlo || hi != nhi {
			t.Fatalf("trial %d: size=%d length=%d start=%d align=%d wrap=%v: got [%d,%d),%v, naive [%d,%d),%v",
				trial, size, length, start, align, wrap, lo, hi, ok, nlo, nhi, nok)
		}
		if ok && hi > size {
			t.Fatalf("trial %d: result [%d,%d) crosses bitset end %d", trial, lo, hi, size)
		}
	}
}
