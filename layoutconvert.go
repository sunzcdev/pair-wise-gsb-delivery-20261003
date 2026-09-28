package reedsolomon

import "errors"

// ErrLayoutIncompatible is returned by NewLayoutConverter when the source
// and target layouts cannot be combined into a conversion, for instance
// when the data shard counts differ or the target layout has no parity
// shards to produce.
var ErrLayoutIncompatible = errors.New("encoder layouts are incompatible")

// ErrInsufficientRank is returned by NewLayoutConverter when the available
// source shards cannot express the original data, i.e. the corresponding
// coding matrix rows do not have full rank. Having enough shards by count
// alone is not sufficient.
var ErrInsufficientRank = errors.New("available shards do not form a full-rank transform")

// ErrInvalidShardIndex is returned by NewLayoutConverter when the list of
// available source shards contains negative, out of range or duplicate
// indices.
var ErrInvalidShardIndex = errors.New("invalid shard index")

// ErrShardOverlap is returned by LayoutConverter.Convert if an output shard
// overlaps a source shard. Source shards are never overwritten.
var ErrShardOverlap = errors.New("output shard overlaps a source shard")

// LayoutConverter converts the parity shards of one erasure coded layout
// directly into the parity shards of another layout, without
// reconstructing the original data in memory.
//
// Both layouts must be matrix based and use the same number of data
// shards and the same shard size. NewLayoutConverter combines a
// full-rank subset of the available source shards into a single
// transform matrix, and Convert applies it block by block, so
// intermediate memory use stays bounded regardless of the shard size.
type LayoutConverter struct {
	enc        *reedSolomon // Source encoder, provides the coding kernels.
	srcIndices []int        // Selected source shard indices.
	transform  matrix       // dstParity x dataShards transform matrix.
	srcTotal   int          // Total shards of the source layout.
	dstParity  int          // Parity shards of the target layout.
	blockSize  int          // Block size used by Convert.
}

// NewLayoutConverter creates a converter that produces the parity shards
// of the dst layout directly from encoded shards of the src layout.
//
// Both encoders must have been created with the same number of data
// shards and must be matrix based; leopard encoders are rejected with
// ErrNotSupported. "available" lists the indices of the source shards
// that can be read during conversion; indices refer to the full source
// shard set (data shards followed by parity shards).
//
// The available shards must contain a full-rank set of dataShards coding
// rows, otherwise ErrInsufficientRank is returned. Note that simply
// having dataShards or more shards available is not a guarantee.
func NewLayoutConverter(src, dst Encoder, available []int) (*LayoutConverter, error) {
	s, ok := src.(*reedSolomon)
	if !ok {
		return nil, ErrNotSupported
	}
	d, ok := dst.(*reedSolomon)
	if !ok {
		return nil, ErrNotSupported
	}
	if s.dataShards != d.dataShards || d.parityShards == 0 {
		return nil, ErrLayoutIncompatible
	}
	if len(available) < s.dataShards {
		return nil, ErrTooFewShards
	}
	seen := make([]bool, s.totalShards)
	rows := make([][]byte, len(available))
	for i, idx := range available {
		if idx < 0 || idx >= s.totalShards || seen[idx] {
			return nil, ErrInvalidShardIndex
		}
		seen[idx] = true
		rows[i] = s.codingRow(idx)
	}
	sel, ok := selectFullRankRows(rows, s.dataShards)
	if !ok {
		return nil, ErrInsufficientRank
	}
	// B maps the original data to the selected source shards.
	b, err := newMatrix(s.dataShards, s.dataShards)
	if err != nil {
		return nil, err
	}
	for i, sidx := range sel {
		copy(b[i], rows[sidx])
	}
	bInv, err := b.Invert()
	if err != nil {
		return nil, err
	}
	// The target parity is obtained by applying the target parity rows to
	// the data recovered through bInv. Composing both steps yields a
	// single transform directly from source shards to target parity.
	dstParity := matrix(d.m[s.dataShards : s.dataShards+d.parityShards])
	transform, err := dstParity.Multiply(bInv)
	if err != nil {
		return nil, err
	}
	srcIndices := make([]int, len(sel))
	for i, sidx := range sel {
		srcIndices[i] = available[sidx]
	}
	block := s.o.perRound
	if block <= 0 {
		block = 1 << 20
	}
	return &LayoutConverter{
		enc:        s,
		srcIndices: srcIndices,
		transform:  transform,
		srcTotal:   s.totalShards,
		dstParity:  d.parityShards,
		blockSize:  block,
	}, nil
}

// SourceIndices returns the indices of the source shards that Convert
// will read, in the order they are combined.
func (c *LayoutConverter) SourceIndices() []int {
	return append([]int(nil), c.srcIndices...)
}

// Convert computes the parity shards of the target layout and writes
// them to dstParity.
//
// src must be the full source shard set (len == source TotalShards);
// shards not selected by NewLayoutConverter are ignored and may be nil.
// dstParity must have one shard per target parity shard, each the same
// size as the source shards.
//
// The conversion is performed block by block and source shards are never
// written to. An output shard overlapping a source shard is rejected
// with ErrShardOverlap.
func (c *LayoutConverter) Convert(src [][]byte, dstParity [][]byte) error {
	if len(src) != c.srcTotal || len(dstParity) != c.dstParity {
		return ErrTooFewShards
	}
	inputs := make([][]byte, len(c.srcIndices))
	size := -1
	for i, idx := range c.srcIndices {
		shard := src[idx]
		if len(shard) == 0 {
			return ErrTooFewShards
		}
		if size < 0 {
			size = len(shard)
		} else if len(shard) != size {
			return ErrShardSize
		}
		inputs[i] = shard
	}
	for _, out := range dstParity {
		if len(out) != size {
			return ErrShardSize
		}
		for _, in := range inputs {
			if &out[0] == &in[0] {
				return ErrShardOverlap
			}
		}
	}
	in := make([][]byte, len(inputs))
	out := make([][]byte, len(dstParity))
	for start := 0; start < size; start += c.blockSize {
		end := start + c.blockSize
		if end > size {
			end = size
		}
		for i := range inputs {
			in[i] = inputs[i][start:end]
		}
		for i := range dstParity {
			out[i] = dstParity[i][start:end]
		}
		c.enc.codeSomeShards(c.transform, in, out, end-start, true)
	}
	return nil
}

// codingRow returns row i of the encoder's coding matrix.
// Encoders without parity shards have no matrix stored; their rows
// are the identity.
func (r *reedSolomon) codingRow(i int) []byte {
	if r.m == nil {
		row := make([]byte, r.dataShards)
		row[i] = 1
		return row
	}
	return r.m[i]
}

// selectFullRankRows returns the indices of dim rows of rows that form an
// invertible dim x dim matrix, or false if the rows have rank < dim.
func selectFullRankRows(rows [][]byte, dim int) ([]int, bool) {
	basis := make([][]byte, dim) // basis[c] has leading 1 in column c.
	sel := make([]int, 0, dim)
	for idx, r := range rows {
		row := append([]byte(nil), r...)
		for c := 0; c < dim; c++ {
			if row[c] == 0 {
				continue
			}
			if basis[c] == nil {
				inv := galOneOver(row[c])
				for j := c; j < dim; j++ {
					row[j] = galMultiply(row[j], inv)
				}
				basis[c] = row
				sel = append(sel, idx)
				break
			}
			f := row[c]
			for j := c; j < dim; j++ {
				row[j] ^= galMultiply(f, basis[c][j])
			}
		}
		if len(sel) == dim {
			return sel, true
		}
	}
	return nil, false
}
