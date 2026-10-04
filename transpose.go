package bitset

import (
	"errors"
	"fmt"
)

// ErrColumnOutOfRange indicates that a row contains a set bit at or above the
// logical number of columns supplied to RowsToColumns.
var ErrColumnOutOfRange = errors.New("bitset: bit set outside logical column count")

// RowsToColumns transposes rows into columns. Rows are BitSets whose set bits
// identify columns; the returned BitSets identify rows containing each column.
// Rows shorter than columnCount are padded with zero bits.
//
// When strict is false, set bits at or above columnCount are ignored. When
// strict is true, RowsToColumns returns ErrColumnOutOfRange before modifying
// columns. A nil row is treated as an empty row.
//
// The columns slice may reuse existing slice capacity and BitSets. Reused
// BitSets are cleared, so bits from a previous, larger result cannot remain.
// The destination BitSets must not alias the input rows.
func RowsToColumns(rows []*BitSet, columnCount uint, strict bool, columns []*BitSet) ([]*BitSet, error) {
	if strict {
		if err := checkRowsToColumnsBounds(rows, columnCount); err != nil {
			return columns, err
		}
	}

	columns = resetTransposeColumns(columns, columnCount, uint(len(rows)))

	var block [64]uint64
	for columnBase := uint(0); columnBase < columnCount; columnBase += wordSize {
		columnWidth := columnCount - columnBase
		if columnWidth > wordSize {
			columnWidth = wordSize
		}
		columnWord := int(columnBase >> log2WordSize)

		for rowBase := uint(0); rowBase < uint(len(rows)); rowBase += wordSize {
			rowHeight := uint(len(rows)) - rowBase
			if rowHeight > wordSize {
				rowHeight = wordSize
			}

			for offset := uint(0); offset < rowHeight; offset++ {
				row := rows[rowBase+offset]
				var word uint64
				if row != nil && columnWord < len(row.set) {
					word = row.set[columnWord]
				}

				width := columnWidth
				if row == nil || row.length <= columnBase {
					width = 0
				} else if row.length-columnBase < width {
					width = row.length - columnBase
				}
				if width < wordSize {
					word &= (uint64(1) << width) - 1
				}

				block[offset] = word
			}
			for offset := rowHeight; offset < wordSize; offset++ {
				block[offset] = 0
			}

			transpose64x64(&block)

			outputWord := int(rowBase >> log2WordSize)
			if rowHeight < wordSize {
				validRowsMask := (uint64(1) << rowHeight) - 1
				for offset := uint(0); offset < columnWidth; offset++ {
					block[offset] &= validRowsMask
				}
			}
			for offset := uint(0); offset < columnWidth; offset++ {
				columns[columnBase+offset].set[outputWord] = block[offset]
			}
		}
	}

	return columns, nil
}

func checkRowsToColumnsBounds(rows []*BitSet, columnCount uint) error {
	boundaryWord := int(columnCount >> log2WordSize)
	remainder := columnCount & wordMask

	for rowIndex, row := range rows {
		if row == nil {
			continue
		}

		if remainder == 0 {
			for wordIndex := boundaryWord; wordIndex < len(row.set); wordIndex++ {
				if row.set[wordIndex] != 0 {
					return fmt.Errorf("row %d: %w", rowIndex, ErrColumnOutOfRange)
				}
			}
			continue
		}

		if boundaryWord < len(row.set) {
			validMask := (uint64(1) << remainder) - 1
			if row.set[boundaryWord]&^validMask != 0 {
				return fmt.Errorf("row %d: %w", rowIndex, ErrColumnOutOfRange)
			}
		}
		for wordIndex := boundaryWord + 1; wordIndex < len(row.set); wordIndex++ {
			if row.set[wordIndex] != 0 {
				return fmt.Errorf("row %d: %w", rowIndex, ErrColumnOutOfRange)
			}
		}
	}

	return nil
}

func resetTransposeColumns(columns []*BitSet, columnCount, rowCount uint) []*BitSet {
	if uint(cap(columns)) < columnCount {
		reused := columns
		columns = make([]*BitSet, columnCount)
		copy(columns, reused)
	} else {
		columns = columns[:cap(columns)]
		for index := columnCount; index < uint(cap(columns)); index++ {
			if columns[index] != nil {
				columns[index].ClearAll()
			}
		}
		columns = columns[:columnCount]
	}

	neededWords := wordsNeeded(rowCount)
	for index := range columns {
		if columns[index] == nil {
			columns[index] = &BitSet{
				length: rowCount,
				set:    make([]uint64, neededWords),
			}
			continue
		}

		column := columns[index]
		if cap(column.set) >= neededWords {
			storage := column.set[:cap(column.set)]
			for wordIndex := range storage {
				storage[wordIndex] = 0
			}
			column.set = storage[:neededWords]
		} else {
			column.set = make([]uint64, neededWords)
		}
		column.length = rowCount
	}

	return columns
}

func transpose64x64(block *[64]uint64) {
	masks := [6]uint64{
		0x00000000ffffffff,
		0x0000ffff0000ffff,
		0x00ff00ff00ff00ff,
		0x0f0f0f0f0f0f0f0f,
		0x3333333333333333,
		0x5555555555555555,
	}

	for step := 0; step < len(masks); step++ {
		blockSize := uint(wordSize >> uint(step))
		half := blockSize >> 1
		mask := masks[step]

		for base := uint(0); base < wordSize; base += blockSize {
			for offset := uint(0); offset < half; offset++ {
				low := base + offset
				high := low + half
				delta := ((block[low] >> half) ^ block[high]) & mask
				block[low] ^= delta << half
				block[high] ^= delta
			}
		}
	}
}
