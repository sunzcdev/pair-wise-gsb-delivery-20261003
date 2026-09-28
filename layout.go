// Layout conversion between redundancy schemes.

package reedsolomon

import (
	"errors"
	"unsafe"
)

// ErrIncompatibleLayouts is returned when the source and destination
// layouts cannot be used for a direct layout conversion.
var ErrIncompatibleLayouts = errors.New("layouts are incompatible for conversion")

// ErrInvalidLayout is returned when the layout description given to
// NewLayoutConverter is invalid, for instance out-of-range or
// duplicated shard indices.
var ErrInvalidLayout = errors.New("invalid layout description")

// ErrNotFullRank is returned when the available source shards do not
// form a full-rank transform, even if there are enough of them.
var ErrNotFullRank = errors.New("available shards do not form a full-rank transform")

// ErrShardOverlap is returned when destination shards share backing
// memory with source shards.
var ErrShardOverlap = errors.New("source and destination shards must not overlap")

// LayoutConverter converts encoded shards of one redundancy layout
// directly into the parity shards of another layout, without
// reconstructing the original data in memory.
//
// Both layouts must use the matrix encoding form (the default),
// must have the same number of data shards and the shards must
// have the same size. Leopard based encoders are not supported.
//
// Create with NewLayoutConverter.
type LayoutConverter struct {
	src        *reedSolomon
	available  []int  // Source shard indices, order matches Convert input.
	selected   []int  // Positions within available used for the transform.
	transform  matrix // dst parity shards x data shards.
	dstParity  int
	dataShards int
}

// NewLayoutConverter creates a converter that produces the parity shards
// of the dst layout from encoded shards of the src layout.
//
// The src and dst encoders must both use the matrix encoding form and
// have the same number of data shards. Shard sizes must be identical
// when converting.
//
// available lists the indices of the source shards the caller can
// supply, in any order. Indices refer to the source shard set:
// [0, DataShards) are data shards, the rest are parity shards.
// At least DataShards indices must be given, and the corresponding
// rows of the source encoding matrix must form a full-rank transform;
// ErrNotFullRank is returned when the count is sufficient but the
// shards are linearly dependent.
func NewLayoutConverter(src, dst Encoder, available []int) (*LayoutConverter, error) {
	srcRS, ok := src.(*reedSolomon)
	if !ok {
		return nil, ErrNotSupported
	}
	dstRS, ok := dst.(*reedSolomon)
	if !ok {
		return nil, ErrNotSupported
	}
	if srcRS.dataShards != dstRS.dataShards || dstRS.parityShards == 0 {
		return nil, ErrIncompatibleLayouts
	}
	k := srcRS.dataShards
	if len(available) < k {
		return nil, ErrTooFewShards
	}
	seen := make(map[int]struct{}, len(available))
	avail := make([]int, len(available))
	for i, idx := range available {
		if idx < 0 || idx >= srcRS.totalShards {
			return nil, ErrInvalidLayout
		}
		if _, dup := seen[idx]; dup {
			return nil, ErrInvalidLayout
		}
		seen[idx] = struct{}{}
		avail[i] = idx
	}

	// Pick a full-rank subset of the available source rows.
	selected, err := selectBasis(srcRS.m, avail, k)
	if err != nil {
		return nil, err
	}

	// Invert the selected source rows: D = inv * S_selected.
	sub, _ := newMatrix(k, k)
	for row, pos := range selected {
		copy(sub[row], srcRS.m[avail[pos]])
	}
	inv, err := sub.Invert()
	if err != nil {
		return nil, ErrNotFullRank
	}

	// New parity = dstParityRows * D = (dstParityRows * inv) * S_selected.
	transform, err := matrix(dstRS.parity).Multiply(inv)
	if err != nil {
		return nil, err
	}

	return &LayoutConverter{
		src:        srcRS,
		available:  avail,
		selected:   selected,
		transform:  transform,
		dstParity:  dstRS.parityShards,
		dataShards: k,
	}, nil
}

// Selected returns the source shard indices that are actually consumed
// by Convert, as a subset of the available indices given to
// NewLayoutConverter.
func (c *LayoutConverter) Selected() []int {
	out := make([]int, len(c.selected))
	for i, pos := range c.selected {
		out[i] = c.available[pos]
	}
	return out
}

// Convert produces the parity shards of the destination layout.
//
// src must contain exactly the shards listed in the available indices
// given to NewLayoutConverter, in the same order. dst must contain
// ParityShards of the destination layout and will be overwritten.
// All shards must have the same, non-zero size.
//
// The source shards are only read, never modified, and destination
// shards must not share memory with them. The conversion is executed
// in blocks, so intermediate memory stays bounded by the transform
// matrix and internal work buffers, independent of the shard size.
func (c *LayoutConverter) Convert(src, dst [][]byte) error {
	if len(src) != len(c.available) || len(dst) != c.dstParity {
		return ErrTooFewShards
	}
	if err := checkShards(src, false); err != nil {
		return err
	}
	if err := checkShards(dst, false); err != nil {
		return err
	}
	size := len(src[0])
	if len(dst[0]) != size {
		return ErrShardSize
	}
	for _, d := range dst {
		for _, s := range src {
			if shardsOverlap(d, s) {
				return ErrShardOverlap
			}
		}
	}

	inputs := make([][]byte, c.dataShards)
	for j, pos := range c.selected {
		inputs[j] = src[pos]
	}
	c.src.codeSomeShards(c.transform, inputs, dst, size, true)
	return nil
}

// shardsOverlap reports whether the backing arrays of a and b intersect.
func shardsOverlap(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	pa := uintptr(unsafe.Pointer(unsafe.SliceData(a)))
	pb := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	return pa < pb+uintptr(len(b)) && pb < pa+uintptr(len(a))
}

// selectBasis returns positions within available whose source matrix
// rows form a full-rank k x k matrix. ErrNotFullRank is returned if
// the available rows span less than k dimensions.
func selectBasis(m matrix, available []int, k int) ([]int, error) {
	type basisRow struct {
		pivot int
		row   []byte
	}
	basis := make([]basisRow, 0, k)
	selected := make([]int, 0, k)
	for pos, idx := range available {
		row := append([]byte(nil), m[idx]...)
		// Reduce against the current basis, kept in reduced
		// row echelon form with normalized pivots.
		for _, b := range basis {
			if row[b.pivot] != 0 {
				scale := row[b.pivot]
				for col := b.pivot; col < k; col++ {
					row[col] ^= galMultiply(scale, b.row[col])
				}
			}
		}
		pivot := -1
		for col := 0; col < k; col++ {
			if row[col] != 0 {
				pivot = col
				break
			}
		}
		if pivot < 0 {
			// Linearly dependent on the current basis.
			continue
		}
		if row[pivot] != 1 {
			scale := galOneOver(row[pivot])
			for col := pivot; col < k; col++ {
				row[col] = galMultiply(row[col], scale)
			}
		}
		// Back-substitute so existing basis rows stay in RREF.
		for _, b := range basis {
			if b.row[pivot] != 0 {
				scale := b.row[pivot]
				for col := pivot; col < k; col++ {
					b.row[col] ^= galMultiply(scale, row[col])
				}
			}
		}
		basis = append(basis, basisRow{pivot: pivot, row: row})
		selected = append(selected, pos)
		if len(selected) == k {
			return selected, nil
		}
	}
	return nil, ErrNotFullRank
}
