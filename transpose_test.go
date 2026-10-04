package bitset

import (
	"math/rand"
	"strings"
	"testing"
)

func makeRandomRows(rng *rand.Rand, nrows, cols uint) []*BitSet {
	rows := make([]*BitSet, nrows)
	for i := range rows {
		rows[i] = New(cols)
		for j := uint(0); j < cols; j++ {
			if rng.Intn(2) == 0 {
				rows[i].Set(j)
			}
		}
	}
	return rows
}

func checkTransposeMapping(t *testing.T, rows []*BitSet, cols uint, colSets []*BitSet) {
	t.Helper()
	if uint(len(colSets)) != cols {
		t.Fatalf("expected %d column sets, got %d", cols, len(colSets))
	}
	for j := uint(0); j < cols; j++ {
		if colSets[j].Len() != uint(len(rows)) {
			t.Fatalf("column %d has length %d, expected %d", j, colSets[j].Len(), len(rows))
		}
		for i := uint(0); i < uint(len(rows)); i++ {
			want := rows[i].Test(j)
			if got := colSets[j].Test(i); got != want {
				t.Fatalf("mismatch at row %d, column %d: got %v, want %v", i, j, got, want)
			}
		}
	}
}

func TestTransposeMapping(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	// Non-square shapes crossing 64-bit word boundaries in both directions.
	shapes := [][2]uint{
		{1, 1},
		{1, 200},
		{200, 1},
		{3, 130},
		{130, 3},
		{64, 64},
		{65, 63},
		{63, 65},
		{100, 257},
		{257, 100},
	}
	for _, shape := range shapes {
		nrows, cols := shape[0], shape[1]
		rows := makeRandomRows(rng, nrows, cols)
		colSets, err := Transpose(rows, cols, true, nil)
		if err != nil {
			t.Fatalf("shape %dx%d: unexpected error: %v", nrows, cols, err)
		}
		checkTransposeMapping(t, rows, cols, colSets)
	}
}

func TestTransposeTwiceRestores(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	nrows, cols := uint(150), uint(130)
	rows := makeRandomRows(rng, nrows, cols)

	colSets, err := Transpose(rows, cols, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	back, err := Transpose(colSets, nrows, true, nil)
	if err != nil {
		t.Fatalf("unexpected error on second transpose: %v", err)
	}
	if uint(len(back)) != nrows {
		t.Fatalf("expected %d rows after double transpose, got %d", nrows, len(back))
	}
	for i := uint(0); i < nrows; i++ {
		if back[i].Len() != cols {
			t.Fatalf("row %d has length %d, expected %d", i, back[i].Len(), cols)
		}
		for j := uint(0); j < cols; j++ {
			if back[i].Test(j) != rows[i].Test(j) {
				t.Fatalf("double transpose mismatch at row %d, column %d", i, j)
			}
		}
	}
}

func TestTransposeShortRowsPadded(t *testing.T) {
	short := New(4).Set(1)    // shorter than cols
	long := New(300).Set(129) // longer than cols, bit inside range
	empty := New(0)           // zero-length row
	rows := []*BitSet{short, long, empty}

	colSets, err := Transpose(rows, 130, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !colSets[1].Test(0) {
		t.Fatal("column 1 should have bit 0 set")
	}
	if !colSets[129].Test(1) {
		t.Fatal("column 129 should have bit 1 set")
	}
	total := uint(0)
	for _, c := range colSets {
		if c.Len() != 3 {
			t.Fatalf("column length %d, expected 3", c.Len())
		}
		total += c.Count()
	}
	if total != 2 {
		t.Fatalf("expected 2 bits set across all columns, got %d", total)
	}
}

func TestTransposeStrictRejectsOverflow(t *testing.T) {
	rows := []*BitSet{
		New(10).Set(3),
		New(200).Set(150), // bit beyond 64 columns
	}
	_, err := Transpose(rows, 64, true, nil)
	if err == nil {
		t.Fatal("expected strict mode to reject bits beyond the column count")
	}
	if !strings.Contains(err.Error(), "row 1") || !strings.Contains(err.Error(), "150") {
		t.Fatalf("error should name the row and bit, got: %v", err)
	}

	// Non-strict mode silently drops the overflowing bits.
	colSets, err := Transpose(rows, 64, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uint(len(colSets)) != 64 {
		t.Fatalf("expected 64 column sets, got %d", len(colSets))
	}
	total := uint(0)
	for _, c := range colSets {
		total += c.Count()
	}
	if total != 1 || !colSets[3].Test(0) {
		t.Fatal("only bit 3 of row 0 should survive truncation")
	}
}

func TestTransposeReuseClearsDirtyBits(t *testing.T) {
	rng := rand.New(rand.NewSource(99))

	// First transpose a large matrix.
	bigRows := makeRandomRows(rng, 200, 150)
	dst, err := Transpose(bigRows, 150, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Reuse the result for a smaller matrix: no stale bits may survive.
	smallRows := makeRandomRows(rng, 5, 40)
	dst, err = Transpose(smallRows, 40, true, dst)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uint(len(dst)) != 40 {
		t.Fatalf("expected 40 column sets after reuse, got %d", len(dst))
	}
	checkTransposeMapping(t, smallRows, 40, dst)
	total := uint(0)
	for _, c := range dst {
		if c.Len() != 5 {
			t.Fatalf("column length %d after reuse, expected 5", c.Len())
		}
		total += c.Count()
	}
	want := uint(0)
	for _, r := range smallRows {
		want += r.Count()
	}
	if total != want {
		t.Fatalf("dirty bits after reuse: got %d bits, want %d", total, want)
	}
}

func TestTransposeEdgeCases(t *testing.T) {
	// Empty matrix: no rows, columns requested.
	colSets, err := Transpose(nil, 10, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(colSets) != 10 {
		t.Fatalf("expected 10 empty column sets, got %d", len(colSets))
	}
	for j, c := range colSets {
		if c.Len() != 0 || c.Count() != 0 {
			t.Fatalf("column %d should be an empty set of length 0", j)
		}
	}

	// Zero columns.
	colSets, err = Transpose([]*BitSet{New(5).Set(2)}, 0, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(colSets) != 0 {
		t.Fatalf("expected empty result for 0 columns, got %d", len(colSets))
	}

	// Strict mode with zero columns rejects any set bit.
	if _, err = Transpose([]*BitSet{New(5).Set(2)}, 0, true, nil); err == nil {
		t.Fatal("expected strict mode to reject bits with 0 columns")
	}

	// Single row, single column.
	colSets, err = Transpose([]*BitSet{New(1).Set(0)}, 1, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(colSets) != 1 || !colSets[0].Test(0) || colSets[0].Len() != 1 {
		t.Fatal("1x1 transpose failed")
	}

	// Nil rows are treated as empty rows.
	colSets, err = Transpose([]*BitSet{nil, New(3).Set(2), nil}, 3, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !colSets[2].Test(1) || colSets[2].Count() != 1 {
		t.Fatal("nil rows should be treated as empty")
	}
}

func TestTranspose64(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 100; trial++ {
		var a, orig [wordSize]uint64
		for i := range a {
			a[i] = rng.Uint64()
		}
		orig = a
		transpose64(&a)
		for i := uint(0); i < wordSize; i++ {
			for j := uint(0); j < wordSize; j++ {
				if (a[j]>>i)&1 != (orig[i]>>j)&1 {
					t.Fatalf("transpose64 mismatch at (%d, %d)", i, j)
				}
			}
		}
		// Transposing again restores the input.
		transpose64(&a)
		if a != orig {
			t.Fatal("transpose64 is not an involution")
		}
	}
}
