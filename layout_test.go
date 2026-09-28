package reedsolomon

import (
	"bytes"
	"errors"
	"math/rand"
	"runtime"
	"testing"
)

// fillShards allocates data+parity shards of the given size and
// fills the data shards with deterministic pseudo-random content.
func fillShards(data, parity, size int) ([][]byte, [][]byte) {
	rng := rand.New(rand.NewSource(int64(data*1000 + parity)))
	shards := make([][]byte, data+parity)
	for i := range data {
		shards[i] = make([]byte, size)
		_, _ = rng.Read(shards[i])
	}
	for i := data; i < data+parity; i++ {
		shards[i] = make([]byte, size)
	}
	return shards[:data], shards
}

func checksumShards(shards [][]byte) []byte {
	var sum []byte
	for _, s := range shards {
		for _, b := range s {
			sum = append(sum, b)
		}
	}
	return sum
}

// runLayoutConvertCase encodes data with the source layout, drops the
// given shard indices and converts the remaining shards to the
// destination layout, comparing against direct encoding of the data.
func runLayoutConvertCase(t *testing.T, data, srcParity, dstParity, size int, drop []int, srcOpts, dstOpts []Option) {
	t.Helper()
	srcEnc, err := New(data, srcParity, srcOpts...)
	if err != nil {
		t.Fatal(err)
	}
	dstEnc, err := New(data, dstParity, dstOpts...)
	if err != nil {
		t.Fatal(err)
	}

	dataShards, all := fillShards(data, srcParity, size)
	if err := srcEnc.Encode(all); err != nil {
		t.Fatal(err)
	}

	// Reference: encode the original data directly with the dst layout.
	ref := make([][]byte, data+dstParity)
	for i := range ref {
		if i < data {
			ref[i] = dataShards[i]
		} else {
			ref[i] = make([]byte, size)
		}
	}
	if err := dstEnc.Encode(ref); err != nil {
		t.Fatal(err)
	}

	dropped := make(map[int]bool, len(drop))
	for _, idx := range drop {
		dropped[idx] = true
	}
	var available []int
	var src [][]byte
	for idx := 0; idx < data+srcParity; idx++ {
		if dropped[idx] {
			continue
		}
		available = append(available, idx)
		src = append(src, all[idx])
	}

	conv, err := NewLayoutConverter(srcEnc, dstEnc, available)
	if err != nil {
		t.Fatal(err)
	}
	sel := conv.Selected()
	if len(sel) != data {
		t.Fatalf("selected %d shards, want %d", len(sel), data)
	}
	for _, idx := range sel {
		if dropped[idx] {
			t.Fatalf("selected dropped shard %d", idx)
		}
	}

	before := checksumShards(src)
	dst := make([][]byte, dstParity)
	for i := range dst {
		dst[i] = make([]byte, size)
	}
	if err := conv.Convert(src, dst); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, checksumShards(src)) {
		t.Fatal("source shards were modified by Convert")
	}

	for i := range dst {
		if !bytes.Equal(dst[i], ref[data+i]) {
			t.Fatalf("converted parity shard %d does not match direct encoding", i)
		}
	}
}

func TestLayoutConvert(t *testing.T) {
	cases := []struct {
		data, srcParity, dstParity, size int
		drop                             []int
	}{
		{10, 3, 3, 4096, nil},
		{10, 3, 4, 4096, []int{0, 1}},         // original data shards incomplete
		{10, 4, 2, 4096, []int{2, 3, 10}},     // data and source parity missing
		{12, 3, 6, 4097, []int{0, 4, 8}},      // odd size, several data shards missing
		{3, 2, 2, 128, []int{0}},              // only one missing data shard
		{10, 5, 3, 64, []int{11, 12, 13, 14}}, // only data shards available
	}
	for _, tc := range cases {
		runLayoutConvertCase(t, tc.data, tc.srcParity, tc.dstParity, tc.size, tc.drop, testOptions(), testOptions())
	}
}

func TestLayoutConvertMatrixForms(t *testing.T) {
	// Converting between different supported matrix encoding forms.
	runLayoutConvertCase(t, 8, 3, 4, 2048, []int{0, 5},
		testOptions(WithJerasureMatrix()),
		testOptions(WithCauchyMatrix()))
	runLayoutConvertCase(t, 8, 2, 3, 2048, []int{1},
		testOptions(WithPAR1Matrix()),
		testOptions(WithJerasureMatrix()))
}

func TestLayoutConvertIncompatible(t *testing.T) {
	// Different data shard counts.
	srcEnc, err := New(10, 3, testOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	dstEnc, err := New(11, 3, testOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	available := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	if _, err := NewLayoutConverter(srcEnc, dstEnc, available); !errors.Is(err, ErrIncompatibleLayouts) {
		t.Fatalf("got %v, want ErrIncompatibleLayouts", err)
	}

	// Leopard encoders are not supported.
	leop, err := New(10, 3, WithLeopardGF16(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLayoutConverter(leop, dstEnc, available); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("got %v, want ErrNotSupported", err)
	}
	if _, err := NewLayoutConverter(srcEnc, leop, available); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("got %v, want ErrNotSupported", err)
	}

	// Rank deficient custom matrix: two parity rows are identical,
	// so two parity shards alone do not form a full-rank transform
	// even though the count equals the number of data shards.
	def, err := New(2, 2, WithCustomMatrix([][]byte{{1, 1}, {1, 1}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLayoutConverter(def, def, []int{2, 3}); !errors.Is(err, ErrNotFullRank) {
		t.Fatalf("got %v, want ErrNotFullRank", err)
	}
	// Providing a data shard as well restores full rank.
	if _, err := NewLayoutConverter(def, def, []int{2, 3, 0}); err != nil {
		t.Fatalf("expected full rank with data shard, got %v", err)
	}
}

func TestLayoutConvertInvalidLayout(t *testing.T) {
	srcEnc, err := New(10, 3, testOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	dstEnc, err := New(10, 4, testOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, available := range [][]int{
		nil,
		{0, 1, 2},                       // too few
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 13}, // out of range
		{0, 1, 2, 3, 4, 5, 6, 7, 8, -1}, // negative
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 8},  // duplicate
	} {
		if _, err := NewLayoutConverter(srcEnc, dstEnc, available); err == nil {
			t.Fatalf("expected error for available %v", available)
		}
	}
	if _, err := NewLayoutConverter(srcEnc, dstEnc, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLayoutConvertBadInput(t *testing.T) {
	const data, parity, size = 6, 3, 256
	srcEnc, _ := New(data, parity, testOptions()...)
	dstEnc, _ := New(data, 4, testOptions()...)
	available := []int{0, 1, 2, 3, 4, 5}
	conv, err := NewLayoutConverter(srcEnc, dstEnc, available)
	if err != nil {
		t.Fatal(err)
	}

	goodSrc := make([][]byte, len(available))
	for i := range goodSrc {
		goodSrc[i] = make([]byte, size)
	}
	goodDst := make([][]byte, 4)
	for i := range goodDst {
		goodDst[i] = make([]byte, size)
	}

	if err := conv.Convert(goodSrc[:5], goodDst); !errors.Is(err, ErrTooFewShards) {
		t.Fatalf("got %v, want ErrTooFewShards", err)
	}
	if err := conv.Convert(goodSrc, goodDst[:3]); !errors.Is(err, ErrTooFewShards) {
		t.Fatalf("got %v, want ErrTooFewShards", err)
	}
	badSize := make([][]byte, len(goodSrc))
	for i := range badSize {
		badSize[i] = make([]byte, size-1)
	}
	if err := conv.Convert(badSize, goodDst); !errors.Is(err, ErrShardSize) {
		t.Fatalf("got %v, want ErrShardSize", err)
	}
	// Destination must not overlap source.
	overlapDst := make([][]byte, 4)
	copy(overlapDst, goodDst)
	overlapDst[1] = goodSrc[0]
	if err := conv.Convert(goodSrc, overlapDst); !errors.Is(err, ErrShardOverlap) {
		t.Fatalf("got %v, want ErrShardOverlap", err)
	}
}

func TestLayoutConvertMemory(t *testing.T) {
	const data, srcParity, dstParity = 10, 4, 6
	const size = 4 << 20
	srcEnc, _ := New(data, srcParity, testOptions()...)
	dstEnc, _ := New(data, dstParity, testOptions()...)

	_, all := fillShards(data, srcParity, size)
	if err := srcEnc.Encode(all); err != nil {
		t.Fatal(err)
	}
	// Two data shards are missing, parity shards stand in.
	available := []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	src := make([][]byte, len(available))
	for i, idx := range available {
		src[i] = all[idx]
	}
	conv, err := NewLayoutConverter(srcEnc, dstEnc, available)
	if err != nil {
		t.Fatal(err)
	}
	dst := make([][]byte, dstParity)
	for i := range dst {
		dst[i] = make([]byte, size)
	}

	// Warm up internal pools and generated matrices.
	if err := conv.Convert(src, dst); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := conv.Convert(src, dst); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)

	intermediate := int64(after.TotalAlloc - before.TotalAlloc)
	t.Logf("intermediate memory for converting %d shards of %d bytes: %d bytes",
		len(src), size, intermediate)
	// Must stay far below a single shard: the original data
	// (data*size = 40 MiB) is never materialized.
	if intermediate > int64(size) {
		t.Fatalf("intermediate memory %d bytes exceeds one shard (%d)", intermediate, size)
	}
}

func BenchmarkLayoutConvert(b *testing.B) {
	const data, srcParity, dstParity, size = 10, 4, 6, 1 << 20
	srcEnc, _ := New(data, srcParity, testOptions()...)
	dstEnc, _ := New(data, dstParity, testOptions()...)
	_, all := fillShards(data, srcParity, size)
	if err := srcEnc.Encode(all); err != nil {
		b.Fatal(err)
	}
	available := []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	src := make([][]byte, len(available))
	for i, idx := range available {
		src[i] = all[idx]
	}
	conv, err := NewLayoutConverter(srcEnc, dstEnc, available)
	if err != nil {
		b.Fatal(err)
	}
	dst := make([][]byte, dstParity)
	for i := range dst {
		dst[i] = make([]byte, size)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(src) * size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := conv.Convert(src, dst); err != nil {
			b.Fatal(err)
		}
	}
}
