package reedsolomon

import "testing"

func benchmarkCorrectErasures(b *testing.B, dataShards, parityShards, shardSize, missing int) {
	enc, err := New(dataShards, parityShards)
	if err != nil {
		b.Fatal(err)
	}
	r := enc.(*reedSolomon)
	orig := make([][]byte, r.totalShards)
	for i := 0; i < r.dataShards; i++ {
		orig[i] = make([]byte, shardSize)
		for j := range orig[i] {
			orig[i][j] = byte(i + j)
		}
	}
	for i := r.dataShards; i < r.totalShards; i++ {
		orig[i] = make([]byte, shardSize)
	}
	if err := r.Encode(orig); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(r.totalShards * shardSize))
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		shards := cloneShards(orig)
		for i := 0; i < missing; i++ {
			shards[r.dataShards+i] = nil
		}
		if _, err := enc.(ErrorCorrection).CorrectErrors(shards); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCorrectErasures10x4x1M(b *testing.B) {
	benchmarkCorrectErasures(b, 10, 4, 1<<20, 2)
}

func BenchmarkCorrectErasures16x16x1M(b *testing.B) {
	benchmarkCorrectErasures(b, 16, 16, 1<<20, 8)
}
