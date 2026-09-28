package reedsolomon

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// encodeLayout creates a full shard set for enc with random data shards
// and encoded parity shards.
func encodeLayout(t *testing.T, enc Encoder, shardSize int, seed int64) [][]byte {
	t.Helper()
	ext := enc.(Extensions)
	shards := make([][]byte, ext.TotalShards())
	for i := range shards {
		shards[i] = make([]byte, shardSize)
		if i < ext.DataShards() {
			fillRandom(shards[i], seed+int64(i))
		}
	}
	if err := enc.Encode(shards); err != nil {
		t.Fatal(err)
	}
	return shards
}

// cloneShards returns a deep copy of shards.
func cloneShards(shards [][]byte) [][]byte {
	out := make([][]byte, len(shards))
	for i, s := range shards {
		out[i] = append([]byte(nil), s...)
	}
	return out
}

func intRange(start, end int) []int {
	out := make([]int, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, i)
	}
	return out
}

func TestLayoutConverter(t *testing.T) {
	const shardSize = 99991 // Odd size, forces a tail block.
	tests := []struct {
		data, srcParity, dstParity int
		srcOpts, dstOpts           []Option
	}{
		{data: 10, srcParity: 4, dstParity: 6},
		{data: 10, srcParity: 6, dstParity: 4},
		{data: 4, srcParity: 4, dstParity: 4},
		{data: 5, srcParity: 3, dstParity: 3, srcOpts: []Option{WithCauchyMatrix()}},
		{data: 6, srcParity: 3, dstParity: 5, dstOpts: []Option{WithCauchyMatrix()}},
		{data: 4, srcParity: 2, dstParity: 3, srcOpts: []Option{WithJerasureMatrix()}, dstOpts: []Option{WithPAR1Matrix()}},
		{data: 3, srcParity: 1, dstParity: 2, srcOpts: []Option{WithFastOneParityMatrix()}},
		{data: 8, srcParity: 2, dstParity: 2, dstOpts: []Option{WithCustomMatrix([][]byte{
			{1, 2, 3, 4, 5, 6, 7, 8},
			{8, 7, 6, 5, 4, 3, 2, 1},
		})}},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("%d+%d-to-%d+%d", test.data, test.srcParity, test.data, test.dstParity), func(t *testing.T) {
			src, err := New(test.data, test.srcParity, test.srcOpts...)
			if err != nil {
				t.Fatal(err)
			}
			dst, err := New(test.data, test.dstParity, test.dstOpts...)
			if err != nil {
				t.Fatal(err)
			}
			srcShards := encodeLayout(t, src, shardSize, 1)
			want := encodeLayout(t, dst, shardSize, 1)
			srcBackup := cloneShards(srcShards)

			total := test.data + test.srcParity
			availSets := map[string][]int{
				"all":       intRange(0, total),
				"data-only": intRange(0, test.data),
				"mixed":     append(intRange(1, test.data), intRange(test.data, total)...),
				"reversed":  intRange(0, test.data),
			}
			// Reverse the "reversed" set.
			for i, j := 0, len(availSets["reversed"])-1; i < j; i, j = i+1, j-1 {
				availSets["reversed"][i], availSets["reversed"][j] = availSets["reversed"][j], availSets["reversed"][i]
			}
			if test.srcParity >= test.data {
				availSets["parity-only"] = intRange(test.data, test.data+test.data)
			}
			for name, avail := range availSets {
				t.Run(name, func(t *testing.T) {
					conv, err := NewLayoutConverter(src, dst, avail)
					if err != nil {
						t.Fatal(err)
					}
					out := make([][]byte, test.dstParity)
					for i := range out {
						out[i] = make([]byte, shardSize)
					}
					if err := conv.Convert(srcShards, out); err != nil {
						t.Fatal(err)
					}
					for i := range out {
						if !bytes.Equal(out[i], want[test.data+i]) {
							t.Fatalf("parity shard %d mismatch", i)
						}
					}
					// Source shards must be untouched.
					for i := range srcShards {
						if !bytes.Equal(srcShards[i], srcBackup[i]) {
							t.Fatalf("source shard %d was modified", i)
						}
					}
				})
			}
		})
	}
}

func TestLayoutConverterMissingData(t *testing.T) {
	const shardSize = 65536
	src, err := New(10, 4)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := New(10, 2)
	if err != nil {
		t.Fatal(err)
	}
	srcShards := encodeLayout(t, src, shardSize, 7)
	want := encodeLayout(t, dst, shardSize, 7)

	// Data shards 0 and 1 are gone; two parity shards take their place.
	avail := append(intRange(2, 10), 10, 11)
	conv, err := NewLayoutConverter(src, dst, avail)
	if err != nil {
		t.Fatal(err)
	}
	// The shards not selected by the converter may be nil.
	partial := make([][]byte, 14)
	for _, idx := range conv.SourceIndices() {
		partial[idx] = srcShards[idx]
	}
	out := make([][]byte, 2)
	for i := range out {
		out[i] = make([]byte, shardSize)
	}
	if err := conv.Convert(partial, out); err != nil {
		t.Fatal(err)
	}
	for i := range out {
		if !bytes.Equal(out[i], want[10+i]) {
			t.Fatalf("parity shard %d mismatch", i)
		}
	}
}

func TestLayoutConverterIncompatible(t *testing.T) {
	src, err := New(10, 4)
	if err != nil {
		t.Fatal(err)
	}
	// Different data shard count.
	other, err := New(11, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLayoutConverter(src, other, intRange(0, 14)); !errors.Is(err, ErrLayoutIncompatible) {
		t.Fatalf("expected ErrLayoutIncompatible, got %v", err)
	}
	// Target layout without parity shards.
	noParity, err := New(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLayoutConverter(src, noParity, intRange(0, 14)); !errors.Is(err, ErrLayoutIncompatible) {
		t.Fatalf("expected ErrLayoutIncompatible, got %v", err)
	}
	// Leopard encoders are not matrix based.
	leo, err := New(10, 4, WithLeopardGF(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLayoutConverter(leo, src, intRange(0, 14)); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
	if _, err := NewLayoutConverter(src, leo, intRange(0, 14)); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestLayoutConverterInvalidLayout(t *testing.T) {
	src, err := New(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := New(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		avail []int
		want  error
	}{
		{"empty", nil, ErrTooFewShards},
		{"too-few", []int{0, 1, 2}, ErrTooFewShards},
		{"negative", []int{0, 1, 2, -1}, ErrInvalidShardIndex},
		{"out-of-range", []int{0, 1, 2, 6}, ErrInvalidShardIndex},
		{"duplicate", []int{0, 1, 2, 2}, ErrInvalidShardIndex},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewLayoutConverter(src, dst, tc.avail); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
		})
	}
}

func TestLayoutConverterInsufficientRank(t *testing.T) {
	// Custom matrix with duplicated rows: enough shards by count,
	// but the coding rows are linearly dependent.
	custom := [][]byte{
		{1, 0, 0, 0},
		{1, 0, 0, 0},
		{0, 1, 0, 0},
		{0, 1, 0, 0},
	}
	src, err := New(4, 4, WithCustomMatrix(custom))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := New(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	// 4 shards, but only rank 2.
	if _, err := NewLayoutConverter(src, dst, []int{0, 4, 5, 6}); !errors.Is(err, ErrInsufficientRank) {
		t.Fatalf("expected ErrInsufficientRank, got %v", err)
	}
	// 4 shards, rank 3: one row short of a basis.
	if _, err := NewLayoutConverter(src, dst, []int{0, 1, 2, 4}); !errors.Is(err, ErrInsufficientRank) {
		t.Fatalf("expected ErrInsufficientRank, got %v", err)
	}
	// Sanity check: the identity rows do have full rank.
	if _, err := NewLayoutConverter(src, dst, []int{0, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
}

func TestLayoutConverterBadInput(t *testing.T) {
	const shardSize = 1024
	src, err := New(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := New(4, 3)
	if err != nil {
		t.Fatal(err)
	}
	srcShards := encodeLayout(t, src, shardSize, 3)
	conv, err := NewLayoutConverter(src, dst, intRange(0, 6))
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]byte, 3)
	for i := range out {
		out[i] = make([]byte, shardSize)
	}

	if err := conv.Convert(srcShards[:5], out); !errors.Is(err, ErrTooFewShards) {
		t.Fatalf("expected ErrTooFewShards, got %v", err)
	}
	if err := conv.Convert(srcShards, out[:2]); !errors.Is(err, ErrTooFewShards) {
		t.Fatalf("expected ErrTooFewShards, got %v", err)
	}
	// A shard promised to be available is missing.
	broken := cloneShards(srcShards)
	broken[conv.SourceIndices()[0]] = nil
	if err := conv.Convert(broken, out); !errors.Is(err, ErrTooFewShards) {
		t.Fatalf("expected ErrTooFewShards, got %v", err)
	}
	// Source shard sizes differ.
	broken = cloneShards(srcShards)
	broken[1] = broken[1][:shardSize-1]
	if err := conv.Convert(broken, out); !errors.Is(err, ErrShardSize) {
		t.Fatalf("expected ErrShardSize, got %v", err)
	}
	// Output shard has the wrong size.
	badOut := cloneShards(out)
	badOut[1] = badOut[1][:shardSize-1]
	if err := conv.Convert(srcShards, badOut); !errors.Is(err, ErrShardSize) {
		t.Fatalf("expected ErrShardSize, got %v", err)
	}
	// Output aliases a source shard.
	badOut = cloneShards(out)
	badOut[0] = srcShards[0]
	if err := conv.Convert(srcShards, badOut); !errors.Is(err, ErrShardOverlap) {
		t.Fatalf("expected ErrShardOverlap, got %v", err)
	}
}

// BenchmarkLayoutConverter measures conversion speed and, via
// ReportAllocs, the intermediate memory used by Convert. The conversion
// works block by block, so allocations stay tiny compared to the shard
// data, independent of the shard size.
func BenchmarkLayoutConverter(b *testing.B) {
	const (
		dataShards = 10
		srcParity  = 4
		dstParity  = 6
		shardSize  = 4 << 20
	)
	src, err := New(dataShards, srcParity)
	if err != nil {
		b.Fatal(err)
	}
	dst, err := New(dataShards, dstParity)
	if err != nil {
		b.Fatal(err)
	}
	srcShards := make([][]byte, dataShards+srcParity)
	for i := range srcShards {
		srcShards[i] = make([]byte, shardSize)
		fillRandom(srcShards[i], int64(i))
	}
	if err := src.Encode(srcShards); err != nil {
		b.Fatal(err)
	}
	// Convert from a mix of data and parity shards.
	avail := append(intRange(2, dataShards), intRange(dataShards, dataShards+2)...)
	conv, err := NewLayoutConverter(src, dst, avail)
	if err != nil {
		b.Fatal(err)
	}
	out := make([][]byte, dstParity)
	for i := range out {
		out[i] = make([]byte, shardSize)
	}
	b.SetBytes(int64(dstParity * shardSize))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := conv.Convert(srcShards, out); err != nil {
			b.Fatal(err)
		}
	}
}
