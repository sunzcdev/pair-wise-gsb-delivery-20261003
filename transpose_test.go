package bitset

import (
	"errors"
	"testing"
)

func TestRowsToColumnsCoordinates(t *testing.T) {
	const rowCount = 70
	const columnCount = 100

	matrix := make([][]bool, rowCount)
	rows := make([]*BitSet, rowCount)
	for row := range matrix {
		matrix[row] = make([]bool, columnCount)
		rows[row] = New(columnCount)
	}

	for row := uint(0); row < rowCount; row++ {
		for column := uint(0); column < columnCount; column++ {
			if (row*31+column*17)%7 == 0 {
				rows[row].Set(column)
				matrix[row][column] = true
			}
		}
	}

	rows[5] = New(20)
	rows[5].Set(3)
	rows[5].Set(17)
	for column := range matrix[5] {
		matrix[5][column] = false
	}
	matrix[5][3] = true
	matrix[5][17] = true

	rows[60] = nil
	for column := range matrix[60] {
		matrix[60][column] = false
	}

	columns, err := RowsToColumns(rows, columnCount, true, nil)
	if err != nil {
		t.Fatalf("RowsToColumns returned error: %v", err)
	}
	if len(columns) != columnCount {
		t.Fatalf("column count = %d, want %d", len(columns), columnCount)
	}

	for column := uint(0); column < columnCount; column++ {
		if columns[column].Len() != rowCount {
			t.Fatalf("column %d length = %d, want %d", column, columns[column].Len(), rowCount)
		}
		for row := uint(0); row < rowCount; row++ {
			if columns[column].Test(row) != matrix[row][column] {
				t.Fatalf("coordinate (%d,%d) = %v, want %v", row, column, columns[column].Test(row), matrix[row][column])
			}
		}
	}

	transposedRows, err := RowsToColumns(columns, rowCount, true, nil)
	if err != nil {
		t.Fatalf("second RowsToColumns returned error: %v", err)
	}
	if len(transposedRows) != rowCount {
		t.Fatalf("restored row count = %d, want %d", len(transposedRows), rowCount)
	}
	for row := uint(0); row < rowCount; row++ {
		if transposedRows[row].Len() != columnCount {
			t.Fatalf("restored row %d length = %d, want %d", row, transposedRows[row].Len(), columnCount)
		}
		for column := uint(0); column < columnCount; column++ {
			if transposedRows[row].Test(column) != matrix[row][column] {
				t.Fatalf("restored coordinate (%d,%d) differs", row, column)
			}
		}
	}
}

func TestRowsToColumnsDegenerateShapes(t *testing.T) {
	tests := []struct {
		name        string
		rows        []*BitSet
		columnCount uint
	}{
		{"empty matrix", nil, 0},
		{"no rows with columns", nil, 5},
		{"no columns with row", []*BitSet{New(0)}, 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			columns, err := RowsToColumns(test.rows, test.columnCount, true, nil)
			if err != nil {
				t.Fatalf("RowsToColumns returned error: %v", err)
			}
			if len(columns) != int(test.columnCount) {
				t.Fatalf("column count = %d, want %d", len(columns), test.columnCount)
			}
			for column, bits := range columns {
				if bits.Len() != uint(len(test.rows)) {
					t.Fatalf("column %d length = %d, want %d", column, bits.Len(), len(test.rows))
				}
				if bits.Count() != 0 {
					t.Fatalf("column %d has %d set bits, want 0", column, bits.Count())
				}
			}
		})
	}

	oneRow := New(100)
	for _, column := range []uint{0, 63, 64, 99} {
		oneRow.Set(column)
	}
	columns, err := RowsToColumns([]*BitSet{oneRow}, 100, true, nil)
	if err != nil {
		t.Fatalf("one-row RowsToColumns returned error: %v", err)
	}
	for column := uint(0); column < 100; column++ {
		want := column == 0 || column == 63 || column == 64 || column == 99
		if columns[column].Len() != 1 || columns[column].Test(0) != want {
			t.Fatalf("one-row column %d = %v with length %d, want %v with length 1", column, columns[column].Test(0), columns[column].Len(), want)
		}
	}

	rows := make([]*BitSet, 70)
	for row := range rows {
		rows[row] = New(1)
	}
	for _, row := range []uint{0, 63, 64, 69} {
		rows[row].Set(0)
	}
	columns, err = RowsToColumns(rows, 1, true, nil)
	if err != nil {
		t.Fatalf("one-column RowsToColumns returned error: %v", err)
	}
	if len(columns) != 1 || columns[0].Len() != 70 {
		t.Fatalf("one-column result has %d columns and length %d, want 1 column with length 70", len(columns), columns[0].Len())
	}
	for row := uint(0); row < 70; row++ {
		want := row == 0 || row == 63 || row == 64 || row == 69
		if columns[0].Test(row) != want {
			t.Fatalf("one-column row %d = %v, want %v", row, columns[0].Test(row), want)
		}
	}
}

func TestRowsToColumnsReuseClearsLargerResult(t *testing.T) {
	const oldRows = 130
	const oldColumns = 130
	oldMatrix := make([]*BitSet, oldRows)
	for row := range oldMatrix {
		oldMatrix[row] = New(oldColumns)
		oldMatrix[row].SetRange(0, oldColumns)
	}
	reused, err := RowsToColumns(oldMatrix, oldColumns, true, nil)
	if err != nil {
		t.Fatalf("building larger result: %v", err)
	}
	reused[0] = &BitSet{length: oldRows, set: make([]uint64, wordsNeeded(oldRows), 5)}
	reused[0].SetRange(0, oldRows)
	for word := range reused[0].set {
		reused[0].set[word] = ^uint64(0)
	}
	for word := wordsNeeded(oldRows); word < cap(reused[0].set); word++ {
		reused[0].set[:cap(reused[0].set)][word] = ^uint64(0)
	}

	newRows := []*BitSet{New(5), New(5), New(5)}
	newRows[0].Set(0)
	newRows[0].Set(4)
	newRows[1].Set(2)
	originalPointers := make([]*BitSet, len(reused))
	copy(originalPointers, reused)

	columns, err := RowsToColumns(newRows, 5, true, reused)
	if err != nil {
		t.Fatalf("RowsToColumns returned error: %v", err)
	}

	for column := uint(0); column < 5; column++ {
		if columns[column] != originalPointers[column] {
			t.Fatalf("column %d was not reused", column)
		}
		if columns[column].Len() != 3 {
			t.Fatalf("column %d length = %d, want 3", column, columns[column].Len())
		}
		for word := len(columns[column].set); word < cap(columns[column].set); word++ {
			if columns[column].set[:cap(columns[column].set)][word] != 0 {
				t.Fatalf("column %d has stale hidden word %d", column, word)
			}
		}
	}

	want := [][]uint{{0}, nil, {1}, nil, {0}}
	for column, rows := range want {
		for row := uint(0); row < 3; row++ {
			expected := false
			for _, wantedRow := range rows {
				if row == wantedRow {
					expected = true
				}
			}
			if columns[column].Test(row) != expected {
				t.Fatalf("coordinate (%d,%d) = %v, want %v", row, column, columns[column].Test(row), expected)
			}
		}
	}

	for column := 5; column < oldColumns; column++ {
		cleared := reused[column]
		if cleared != originalPointers[column] {
			t.Fatalf("dropped column %d pointer changed", column)
		}
		for word, value := range cleared.set {
			if value != 0 {
				t.Fatalf("dropped column %d has stale word %d: %#x", column, word, value)
			}
		}
	}
}

func TestRowsToColumnsStrictBounds(t *testing.T) {
	tests := []struct {
		name        string
		columnCount uint
		setColumn   uint
		wantError   bool
	}{
		{"inside boundary word", 64, 63, false},
		{"at 64-column boundary", 64, 64, true},
		{"inside partial boundary word", 65, 64, false},
		{"above partial boundary word", 65, 65, true},
		{"far outside boundary", 65, 128, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			row := New(test.setColumn + 1)
			row.Set(test.setColumn)
			destination := []*BitSet{New(1)}
			destination[0].Set(0)
			original := destination[0]

			columns, err := RowsToColumns([]*BitSet{row}, test.columnCount, true, destination)
			if test.wantError {
				if !errors.Is(err, ErrColumnOutOfRange) {
					t.Fatalf("error = %v, want ErrColumnOutOfRange", err)
				}
				if len(columns) != 1 || columns[0] != original || !columns[0].Test(0) {
					t.Fatal("strict error modified destination")
				}
				return
			}
			if err != nil {
				t.Fatalf("RowsToColumns returned error: %v", err)
			}
			if len(columns) != int(test.columnCount) {
				t.Fatalf("column count = %d, want %d", len(columns), test.columnCount)
			}
		})
	}
}

func TestRowsToColumnsIgnoresBitsOutsideColumns(t *testing.T) {
	row := New(100)
	row.Set(1)
	row.Set(64)
	row.Set(99)

	columns, err := RowsToColumns([]*BitSet{row}, 64, false, nil)
	if err != nil {
		t.Fatalf("RowsToColumns returned error: %v", err)
	}
	if len(columns) != 64 {
		t.Fatalf("column count = %d, want 64", len(columns))
	}
	for column, result := range columns {
		want := column == 1
		if result.Test(0) != want {
			t.Fatalf("column %d = %v, want %v", column, result.Test(0), want)
		}
	}
}

func TestRowsToColumnsIgnoresInvalidTailBits(t *testing.T) {
	row := FromWithLength(1, []uint64{3})

	columns, err := RowsToColumns([]*BitSet{row}, 1, false, nil)
	if err != nil {
		t.Fatalf("RowsToColumns returned error: %v", err)
	}
	if len(columns) != 1 || columns[0].Len() != 1 {
		t.Fatalf("unexpected result shape: %d columns, length %d", len(columns), columns[0].Len())
	}
	if !columns[0].Test(0) {
		t.Fatal("logical bit was not transposed")
	}
	if columns[0].set[0] != 1 {
		t.Fatalf("invalid tail bit leaked into result: %#x", columns[0].set[0])
	}

	if _, err := RowsToColumns([]*BitSet{row}, 1, true, nil); !errors.Is(err, ErrColumnOutOfRange) {
		t.Fatalf("strict error = %v, want ErrColumnOutOfRange", err)
	}

	shortRow := FromWithLength(1, []uint64{1<<63 | 1})
	columns, err = RowsToColumns([]*BitSet{shortRow}, 64, false, nil)
	if err != nil {
		t.Fatalf("short-row RowsToColumns returned error: %v", err)
	}
	if len(columns) != 64 || !columns[0].Test(0) {
		t.Fatalf("short row was not correctly zero-padded")
	}
	for column := 1; column < 64; column++ {
		if columns[column].Any() {
			t.Fatalf("short-row tail bit leaked into column %d", column)
		}
	}
}
