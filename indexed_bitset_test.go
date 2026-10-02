package bitset

import (
	"reflect"
	"testing"
)

func TestIndexedBitSetMatchesBitSet(t *testing.T) {
	seed := uint64(0x9e3779b97f4a7c15)
	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}

	indexed := NewIndexedBitSet(0)
	reference := New(0)
	for step := 0; step < 5000; step++ {
		length := reference.Len()
		var n uint
		if length > 0 {
			n = uint(next() % uint64(length+32))
		}
		start := uint(next() % 160)
		end := start + uint(next()%160)

		switch next() % 7 {
		case 0:
			indexed.Set(n)
			reference.Set(n)
		case 1:
			indexed.Clear(n)
			reference.Clear(n)
		case 2:
			indexed.Flip(n)
			reference.Flip(n)
		case 3:
			indexed.SetTo(n, next()%2 == 0)
			reference.SetTo(n, indexed.Test(n))
		case 4:
			indexed.SetRange(start, end)
			reference.SetRange(start, end)
		case 5:
			indexed.ClearRange(start, end)
			if start < reference.Len() {
				for i := start; i < reference.Len() && i < end; i++ {
					reference.Clear(i)
				}
			}
		case 6:
			indexed.FlipRange(start, end)
			reference.FlipRange(start, end)
		}

		assertIndexedMatches(t, indexed, reference, step)
	}

	for indexed.Len() > 1 {
		newLast := uint(next() % uint64(indexed.Len()))
		indexed.Shrink(newLast)
		reference.Shrink(newLast)
		assertIndexedMatches(t, indexed, reference, -1)
	}

	indexed.ClearAll()
	reference.ClearAll()
	assertIndexedMatches(t, indexed, reference, -1)
}

func TestIndexedBitSetPartialLastWordUpdates(t *testing.T) {
	indexed := NewIndexedBitSet(100)
	reference := New(100)

	for _, i := range []uint{63, 64, 65, 97, 98, 99} {
		indexed.Set(i)
		reference.Set(i)
	}
	assertIndexedMatches(t, indexed, reference, -1)

	indexed.SetRange(60, 95)
	reference.SetRange(60, 95)
	assertIndexedMatches(t, indexed, reference, -1)

	indexed.FlipRange(64, 100)
	reference.FlipRange(64, 100)
	assertIndexedMatches(t, indexed, reference, -1)

	indexed.ClearRange(62, 99)
	for i := uint(62); i < 99; i++ {
		reference.Clear(i)
	}
	assertIndexedMatches(t, indexed, reference, -1)
}

func TestIndexedBitSetGrowthAndShrink(t *testing.T) {
	indexed := NewIndexedBitSet(65)
	reference := New(65)

	indexed.SetRange(0, 65)
	reference.SetRange(0, 65)
	assertIndexedMatches(t, indexed, reference, -1)

	indexed.Set(511)
	reference.Set(511)
	assertIndexedMatches(t, indexed, reference, -1)

	indexed.Flip(1023)
	reference.Flip(1023)
	assertIndexedMatches(t, indexed, reference, -1)

	indexed.Shrink(99)
	reference.Shrink(99)
	assertIndexedMatches(t, indexed, reference, -1)

	indexed.FlipRange(90, 100)
	reference.FlipRange(90, 100)
	assertIndexedMatches(t, indexed, reference, -1)

	indexed.Shrink(64)
	reference.Shrink(64)
	assertIndexedMatches(t, indexed, reference, -1)
}

func TestIndexedBitSetSelectOutOfRange(t *testing.T) {
	b := NewIndexedBitSet(64)
	if position, ok := b.Select(0); ok || position != 64 {
		t.Fatalf("expected (64, false), got (%d, %v)", position, ok)
	}

	b.Set(63)
	if position, ok := b.Select(1); ok || position != 64 {
		t.Fatalf("expected (64, false), got (%d, %v)", position, ok)
	}

	position, ok := b.Select(0)
	if !ok || position != 63 {
		t.Fatalf("expected (63, true), got (%d, %v)", position, ok)
	}
}

func TestIndexedBitSetDoesNotExposeWords(t *testing.T) {
	typ := reflect.TypeOf((*IndexedBitSet)(nil))
	for _, name := range []string{"Words", "Bytes"} {
		if _, ok := typ.MethodByName(name); ok {
			t.Fatalf("IndexedBitSet must not expose %s", name)
		}
	}

	structType := typ.Elem()
	for i := 0; i < structType.NumField(); i++ {
		if field := structType.Field(i); field.PkgPath == "" {
			t.Fatalf("IndexedBitSet field %s must be unexported", field.Name)
		}
	}
}

func BenchmarkRankIndexedVsNaive(b *testing.B) {
	const words = 1 << 14
	set := New(words * wordSize)
	for i := uint(0); i < set.Len(); i += 17 {
		set.Set(i)
	}
	indexed := NewIndexedBitSet(set.Len())
	indexed.SetRange(0, indexed.Len())
	indexed.ClearAll()
	for i := uint(0); i < indexed.Len(); i += 17 {
		indexed.Set(i)
	}
	last := set.Len() - 1

	b.Run("naive", func(b *testing.B) {
		var rank uint
		for i := 0; i < b.N; i++ {
			rank = set.Rank(uint(i) % set.Len())
		}
		sinkRank = rank
	})
	b.Run("naive/last", func(b *testing.B) {
		var rank uint
		for i := 0; i < b.N; i++ {
			rank = set.Rank(last)
		}
		sinkRank = rank
	})
	b.Run("indexed", func(b *testing.B) {
		var rank uint
		for i := 0; i < b.N; i++ {
			rank = indexed.Rank(uint(i) % indexed.Len())
		}
		sinkRank = rank
	})
	b.Run("indexed/last", func(b *testing.B) {
		var rank uint
		for i := 0; i < b.N; i++ {
			rank = indexed.Rank(last)
		}
		sinkRank = rank
	})
}

func BenchmarkSelectIndexedVsNaive(b *testing.B) {
	const words = 1 << 14
	set := New(words * wordSize)
	for i := uint(0); i < set.Len(); i += 17 {
		set.Set(i)
	}
	indexed := NewIndexedBitSet(set.Len())
	for i := uint(0); i < indexed.Len(); i += 17 {
		indexed.Set(i)
	}
	last := indexed.Count() - 1

	b.Run("naive", func(b *testing.B) {
		var selected uint
		for i := 0; i < b.N; i++ {
			selected = set.Select(uint(i) % indexed.Count())
		}
		sinkSelect = selected
	})
	b.Run("naive/last", func(b *testing.B) {
		var selected uint
		for i := 0; i < b.N; i++ {
			selected = set.Select(last)
		}
		sinkSelect = selected
	})
	b.Run("indexed", func(b *testing.B) {
		var selected uint
		for i := 0; i < b.N; i++ {
			selected, _ = indexed.Select(uint(i) % indexed.Count())
		}
		sinkSelect = selected
	})
	b.Run("indexed/last", func(b *testing.B) {
		var selected uint
		for i := 0; i < b.N; i++ {
			selected, _ = indexed.Select(last)
		}
		sinkSelect = selected
	})
}

var (
	sinkRank   uint
	sinkSelect uint
)

func assertIndexedMatches(t *testing.T, indexed *IndexedBitSet, reference *BitSet, step int) {
	t.Helper()

	if indexed.Len() != reference.Len() {
		t.Fatalf("step %d: length = %d, want %d", step, indexed.Len(), reference.Len())
	}
	if indexed.Count() != reference.Count() {
		t.Fatalf("step %d: count = %d, want %d", step, indexed.Count(), reference.Count())
	}

	length := indexed.Len()
	for i := uint(0); i < length; i++ {
		if indexed.Test(i) != reference.Test(i) {
			t.Fatalf("step %d: bit %d mismatch", step, i)
		}
	}
	for i := uint(0); i < length; i += 7 {
		if got := indexed.Rank(i); got != reference.Rank(i) {
			t.Fatalf("step %d: rank(%d) = %d, want %d", step, i, got, reference.Rank(i))
		}
	}
	if got := indexed.Rank(length); got != reference.Count() {
		t.Fatalf("step %d: rank(len) = %d, want %d", step, got, reference.Count())
	}

	for j := uint(0); j < reference.Count(); j++ {
		position, ok := indexed.Select(j)
		if !ok || position != reference.Select(j) {
			t.Fatalf("step %d: select(%d) = (%d, %v), want %d", step, j, position, ok, reference.Select(j))
		}
	}
	if position, ok := indexed.Select(reference.Count()); ok || position != reference.Len() {
		t.Fatalf("step %d: out-of-range select = (%d, %v), want (%d, false)", step, position, ok, reference.Len())
	}
}
