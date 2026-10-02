package bitset

import (
	"math/rand"
	"testing"
)

// checkIndexedEqual verifies the indexed wrapper against the naive BitSet
// used as the ground truth.
func checkIndexedEqual(t *testing.T, ix *IndexedBitSet, b *BitSet, rng *rand.Rand) {
	t.Helper()

	if ix.Len() != b.Len() {
		t.Fatalf("Len mismatch: indexed %d, naive %d", ix.Len(), b.Len())
	}
	if ix.Count() != b.Count() {
		t.Fatalf("Count mismatch: indexed %d, naive %d", ix.Count(), b.Count())
	}

	// Rank at random positions, including positions beyond the length.
	for i := 0; i < 8; i++ {
		idx := uint(rng.Intn(int(b.Len()) + 2))
		if got, want := ix.Rank(idx), b.Rank(idx); got != want {
			t.Fatalf("Rank(%d) mismatch: indexed %d, naive %d", idx, got, want)
		}
	}

	// Select around and beyond the valid range.
	count := b.Count()
	for i := 0; i < 8; i++ {
		j := uint(rng.Intn(int(count) + 3))
		got, ok := ix.Select(j)
		if j >= count {
			if ok {
				t.Fatalf("Select(%d) with Count=%d: expected ok=false, got (%d, true)", j, count, got)
			}
			continue
		}
		if !ok {
			t.Fatalf("Select(%d) with Count=%d: expected ok=true", j, count)
		}
		if want := b.Select(j); got != want {
			t.Fatalf("Select(%d) mismatch: indexed %d, naive %d", j, got, want)
		}
	}

	// Full content comparison.
	if !ix.Snapshot().Equal(b) {
		t.Fatalf("content mismatch: indexed %v, naive %v", ix.Snapshot(), b)
	}
}

// TestIndexedAgainstNaive applies a random sequence of mutating operations
// to both an IndexedBitSet and a plain BitSet and compares Rank, Select,
// Count, Len and the full content after every step.
func TestIndexedAgainstNaive(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	const maxLen = 70000 // crosses many index blocks (2048 bits each)

	ix := NewIndexed(0)
	b := New(0)

	for step := 0; step < 3000; step++ {
		i := uint(rng.Intn(maxLen))
		start := uint(rng.Intn(maxLen))
		end := start + uint(rng.Intn(3000))

		switch rng.Intn(12) {
		case 0, 1, 2:
			ix.Set(i)
			b.Set(i)
		case 3, 4:
			ix.Clear(i)
			b.Clear(i)
		case 5, 6:
			ix.Flip(i)
			b.Flip(i)
		case 7:
			v := rng.Intn(2) == 0
			ix.SetTo(i, v)
			b.SetTo(i, v)
		case 8:
			ix.SetRange(start, end)
			b.SetRange(start, end)
		case 9:
			ix.FlipRange(start, end)
			b.FlipRange(start, end)
		case 10:
			switch rng.Intn(4) {
			case 0:
				ix.ClearAll()
				b.ClearAll()
			case 1:
				ix.SetAll()
				b.SetAll()
			case 2:
				if b.Len() > 0 {
					last := uint(rng.Intn(int(b.Len())))
					ix.Shrink(last)
					b.Shrink(last)
				}
			case 3:
				ix.Compact()
				b.Compact()
			}
		default:
			// grow well past the current length
			g := uint(rng.Intn(maxLen * 2))
			ix.Set(g)
			b.Set(g)
		}

		checkIndexedEqual(t, ix, b, rng)
	}
}

// TestIndexedSelectOutOfRange checks that out-of-range Select queries are
// distinguishable from a bit set at position 0.
func TestIndexedSelectOutOfRange(t *testing.T) {
	var ix IndexedBitSet

	if got, ok := ix.Select(0); ok {
		t.Fatalf("empty set: Select(0) = (%d, true), want ok=false", got)
	}

	ix.Set(0)
	if got, ok := ix.Select(0); !ok || got != 0 {
		t.Fatalf("Select(0) = (%d, %v), want (0, true)", got, ok)
	}
	if got, ok := ix.Select(1); ok {
		t.Fatalf("Select(1) with Count=1 = (%d, true), want ok=false", got)
	}

	ix.Set(100).Set(5000)
	if got, ok := ix.Select(2); !ok || got != 5000 {
		t.Fatalf("Select(2) = (%d, %v), want (5000, true)", got, ok)
	}
	if got, ok := ix.Select(3); ok {
		t.Fatalf("Select(3) with Count=3 = (%d, true), want ok=false", got)
	}
	if got, ok := ix.Select(1000); ok {
		t.Fatalf("Select(1000) with Count=3 = (%d, true), want ok=false", got)
	}
}

// TestIndexedGrowthShrinkClearAll exercises length growth across block
// boundaries, the partial last word, Shrink, ClearAll and SetAll.
func TestIndexedGrowthShrinkClearAll(t *testing.T) {
	var ix IndexedBitSet // zero value must be usable
	b := New(0)

	// growth across word and block boundaries
	for _, i := range []uint{0, 63, 64, 2047, 2048, 2049, 4095, 4096, 65536} {
		ix.Set(i)
		b.Set(i)
	}
	rng := rand.New(rand.NewSource(7))
	checkIndexedEqual(t, &ix, b, rng)

	// SetAll with a length that is not a multiple of the word size:
	// the partial last word must be counted correctly.
	ix2 := NewIndexed(100)
	b2 := New(100)
	ix2.SetAll()
	b2.SetAll()
	if ix2.Count() != 100 {
		t.Fatalf("SetAll on length 100: Count = %d, want 100", ix2.Count())
	}
	checkIndexedEqual(t, ix2, b2, rng)

	// Shrink into the middle of a word clears the tail bits.
	ix2.Shrink(64) // new length 65, last word keeps a single bit
	b2.Shrink(64)
	if ix2.Count() != 65 {
		t.Fatalf("after Shrink(64) of all-ones: Count = %d, want 65", ix2.Count())
	}
	checkIndexedEqual(t, ix2, b2, rng)

	// ClearAll keeps the length but zeroes the index.
	ix2.ClearAll()
	b2.ClearAll()
	if ix2.Count() != 0 || ix2.Len() != 65 {
		t.Fatalf("after ClearAll: Count = %d, Len = %d, want 0 and 65", ix2.Count(), ix2.Len())
	}
	checkIndexedEqual(t, ix2, b2, rng)

	// The set must still be usable afterwards.
	ix2.Set(64)
	b2.Set(64)
	checkIndexedEqual(t, ix2, b2, rng)
}

// TestIndexedOwnership checks that callers cannot mutate the internal
// words: Snapshot returns a copy and IndexedFrom clones its argument.
func TestIndexedOwnership(t *testing.T) {
	b := New(128).Set(3).Set(70)
	ix := IndexedFrom(b)

	// Mutating the source after wrapping must not affect the index.
	b.Set(5)
	if ix.Count() != 2 {
		t.Fatalf("IndexedFrom did not clone: Count = %d, want 2", ix.Count())
	}

	// Mutating a snapshot must not affect the index either.
	snap := ix.Snapshot()
	snap.Set(9)
	snap.Words()[0] = ^uint64(0)
	if ix.Count() != 2 {
		t.Fatalf("Snapshot leaked internal words: Count = %d, want 2", ix.Count())
	}
	if got, ok := ix.Select(1); !ok || got != 70 {
		t.Fatalf("Select(1) = (%d, %v), want (70, true)", got, ok)
	}

	// Clone is independent.
	c := ix.Clone()
	c.Set(1000)
	if ix.Count() != 2 || c.Count() != 3 {
		t.Fatalf("Clone not independent: ix.Count = %d, c.Count = %d", ix.Count(), c.Count())
	}
}

// TestIndexedQueryWordReadsBound proves, by counting word accesses, that
// Rank and Select never scan more than a single index block of words,
// regardless of the total number of words.
func TestIndexedQueryWordReadsBound(t *testing.T) {
	const nbits = 1 << 21 // 2M bits = 32768 words = 1024 blocks
	rng := rand.New(rand.NewSource(99))

	ix := NewIndexed(nbits)
	b := New(nbits)
	for i := 0; i < 20000; i++ {
		pos := uint(rng.Intn(nbits))
		ix.Set(pos)
		b.Set(pos)
	}

	nwords := len(b.Words())
	if nwords != 32768 {
		t.Fatalf("expected 32768 words, got %d", nwords)
	}

	for i := 0; i < 1000; i++ {
		idx := uint(rng.Intn(nbits))
		if got, want := ix.Rank(idx), b.Rank(idx); got != want {
			t.Fatalf("Rank(%d) = %d, want %d", idx, got, want)
		}
		if ix.wordReads > indexBlockWords+1 {
			t.Fatalf("Rank(%d) read %d words (of %d), want at most %d",
				idx, ix.wordReads, nwords, indexBlockWords+1)
		}

		j := uint(rng.Intn(int(ix.Count())))
		got, ok := ix.Select(j)
		if want := b.Select(j); !ok || got != want {
			t.Fatalf("Select(%d) = (%d, %v), want (%d, true)", j, got, ok, want)
		}
		if ix.wordReads > indexBlockWords {
			t.Fatalf("Select(%d) read %d words (of %d), want at most %d",
				j, ix.wordReads, nwords, indexBlockWords)
		}
	}
}

func benchmarkIndexedSets(b *testing.B) (*BitSet, *IndexedBitSet, uint) {
	rng := rand.New(rand.NewSource(1))
	const nwords = 1 << 16 // 65536 words = 4M bits
	buf := make([]uint64, nwords)
	for i := range buf {
		buf[i] = rng.Uint64()
	}
	naive := From(buf)
	indexed := IndexedFrom(naive)
	return naive, indexed, naive.Count()
}

func BenchmarkNaiveRank(b *testing.B) {
	naive, _, _ := benchmarkIndexedSets(b)
	rng := rand.New(rand.NewSource(2))
	indices := make([]uint, 1024)
	for i := range indices {
		indices[i] = uint(rng.Intn(int(naive.Len())))
	}
	b.ResetTimer()
	var sum uint
	for i := 0; i < b.N; i++ {
		sum += naive.Rank(indices[i&1023])
	}
	_ = sum
}

func BenchmarkIndexedRank(b *testing.B) {
	_, indexed, _ := benchmarkIndexedSets(b)
	rng := rand.New(rand.NewSource(2))
	indices := make([]uint, 1024)
	for i := range indices {
		indices[i] = uint(rng.Intn(int(indexed.Len())))
	}
	b.ResetTimer()
	var sum uint
	for i := 0; i < b.N; i++ {
		sum += indexed.Rank(indices[i&1023])
	}
	_ = sum
}

func BenchmarkNaiveSelect(b *testing.B) {
	naive, _, count := benchmarkIndexedSets(b)
	rng := rand.New(rand.NewSource(3))
	js := make([]uint, 1024)
	for i := range js {
		js[i] = uint(rng.Intn(int(count)))
	}
	b.ResetTimer()
	var sum uint
	for i := 0; i < b.N; i++ {
		sum += naive.Select(js[i&1023])
	}
	_ = sum
}

func BenchmarkIndexedSelect(b *testing.B) {
	_, indexed, count := benchmarkIndexedSets(b)
	rng := rand.New(rand.NewSource(3))
	js := make([]uint, 1024)
	for i := range js {
		js[i] = uint(rng.Intn(int(count)))
	}
	b.ResetTimer()
	var sum uint
	for i := 0; i < b.N; i++ {
		v, ok := indexed.Select(js[i&1023])
		if ok {
			sum += v
		}
	}
	_ = sum
}
