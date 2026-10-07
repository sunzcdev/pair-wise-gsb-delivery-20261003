package lz4_test

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/pierrec/lz4/v4"
)

func benchmarkCostData() map[string][]byte {
	rng := rand.New(rand.NewSource(31))
	random := make([]byte, 256<<10)
	rng.Read(random)
	far := make([]byte, 128<<10)
	for i := range far {
		far[i] = byte(i * 13)
	}
	copy(far, "far-reference-abcdefghijklmnop")
	copy(far[70000:], "far-reference-abcdefghijklmnop")
	return map[string][]byte{
		"repeated-prefix": bytes.Repeat([]byte("offline logs are repetitive enough to benefit from cost parsing; "), 4096),
		"far-match":       far,
		"incompressible":  random,
	}
}

func BenchmarkCompressCostCompare(b *testing.B) {
	for name, src := range benchmarkCostData() {
		b.Run(name+"/hc-level9", func(b *testing.B) {
			dst := make([]byte, lz4.CompressBlockBound(len(src)))
			var compressor lz4.CompressorHC
			compressor.Level = lz4.Level9
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				n, _ := compressor.CompressBlock(src, dst)
				if i == 0 {
					b.ReportMetric(float64(n), "compressed-bytes")
				}
			}
		})
		b.Run(name+"/cost", func(b *testing.B) {
			dst := make([]byte, lz4.CompressBlockBound(len(src)))
			compressor := lz4.CompressorCost{Options: lz4.CostOptions{MaxCandidates: 64}}
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				n, _ := compressor.CompressBlock(src, dst)
				if i == 0 {
					b.ReportMetric(float64(n), "compressed-bytes")
				}
			}
		})
	}
}
