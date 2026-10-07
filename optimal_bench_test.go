package lz4_test

import (
	"bytes"
	crand "crypto/rand"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"
)

// TestOptimalComparison reports compression ratio and time for Fast, HC9 and
// Optimal on the three requested shapes: a long repeated prefix, a match far
// from its reference, and incompressible data. It is a comparison test, not a
// claim that Optimal wins on every input: incompressible data is stored raw
// by all three, for example.
func TestOptimalComparison(t *testing.T) {
	const size = 256 << 10

	mk := map[string][]byte{}

	// Repeated prefix: a phrase repeated thousands of times.
	phrase := []byte("error: failed to connect to upstream service after 30s; retrying with backoff\n")
	rep := bytes.Repeat(phrase, (size+len(phrase)-1)/len(phrase))[:size]
	mk["repeated-prefix"] = rep

	// Far match: the same block appears once near the start and once near
	// the end, with high-entropy padding between them.
	far := make([]byte, size)
	// Incompressible padding, so the only useful references are the two
	// embedded chunks ~50kiB apart.
	if _, err := crand.Read(far); err != nil {
		t.Fatal(err)
	}
	// A 16kiB structured region placed twice, about 48kiB apart (inside the
	// 64kiB window). The second copy is far enough that greedy parsers do
	// not connect it to the first across the incompressible gap.
	region := make([]byte, 8<<10)
	if _, err := crand.Read(region); err != nil {
		t.Fatal(err)
	}
	first, second := 1<<10, 50<<10
	copy(far[first:first+len(region)], region)
	copy(far[second:second+len(region)], region)
	mk["far-match"] = far

	// Incompressible: cryptographically-ish random bytes.
	inc := make([]byte, size)
	rng2 := rand.New(rand.NewSource(7))
	rng2.Read(inc)
	mk["incompressible"] = inc

	var fast lz4.Compressor
	var hc9 lz4.CompressorHC
	hc9.Level = lz4.Level9
	var opt lz4.CompressorOptimal

	type result struct {
		name string
		size int
		dur  time.Duration
	}
	compress := func(name string, fn func(src, dst []byte) int, src []byte) result {
		dst := make([]byte, lz4.CompressBlockBound(len(src)))
		fn(src, dst) // warm pools and caches
		start := time.Now()
		n0 := fn(src, dst)
		return result{name, n0, time.Since(start)}
	}

	for name, src := range mk {
		obuf := make([]byte, lz4.CompressBlockBound(len(src)))
		rf := compress("fast", func(src, dst []byte) int { n, _ := fast.CompressBlock(src, dst); return n }, src)
		rh := compress("hc9", func(src, dst []byte) int { n, _ := hc9.CompressBlock(src, dst); return n }, src)
		opt.CompressBlock(src, obuf) // warm
		ostart := time.Now()
		osize, oerr := opt.CompressBlock(src, obuf)
		if oerr != nil {
			t.Fatal(oerr)
		}
		ro := result{"optimal", osize, time.Since(ostart)}
		pct := func(r result) string {
			if r.size == 0 {
				return "raw (0)"
			}
			return fmt.Sprintf("%d (%.2f%%)", r.size, 100*float64(r.size)/float64(len(src)))
		}
		speed := func(r result) string {
			return fmt.Sprintf("%.1fMB/s", float64(len(src))/r.dur.Seconds()/1e6)
		}
		t.Logf("%-16s input=%d\n  fast    size=%s time=%v (%s)\n  hc9     size=%s time=%v (%s)\n  optimal size=%s time=%v (%s)",
			name, len(src),
			pct(rf), rf.dur, speed(rf),
			pct(rh), rh.dur, speed(rh),
			pct(ro), ro.dur, speed(ro))
		// Sanity: round-trip the optimal output.
		if ro.size > 0 {
			out := make([]byte, len(src))
			dn, err := lz4.UncompressBlock(obuf[:ro.size], out)
			if err != nil || dn != len(src) || !bytes.Equal(out, src) {
				t.Fatalf("%s: optimal round trip failed: %v %d", name, err, dn)
			}
		}
	}
}

// BenchmarkOptimalComparison reports throughput and size for each compressor
// on each of the three shapes. Run with, e.g.:
//
//	go test -run '^$' -bench '^BenchmarkOptimalComparison' -benchtime 2s .
func BenchmarkOptimalComparison(b *testing.B) {
	const size = 256 << 10
	cases := []struct {
		name  string
		make_ func() []byte
	}{
		{"repeated-prefix", func() []byte {
			phrase := []byte("error: failed to connect to upstream service after 30s; retrying with backoff\n")
			return bytes.Repeat(phrase, (size+len(phrase)-1)/len(phrase))[:size]
		}},
		{"incompressible", func() []byte {
			buf := make([]byte, size)
			rand.New(rand.NewSource(7)).Read(buf)
			return buf
		}},
		{"far-match", func() []byte {
			buf := make([]byte, size)
			crand.Read(buf)
			region := make([]byte, 8<<10)
			crand.Read(region)
			first, second := 1<<10, 50<<10
			copy(buf[first:first+len(region)], region)
			copy(buf[second:second+len(region)], region)
			return buf
		}},
	}

	for _, tc := range cases {
		src := tc.make_()
		for _, mode := range []struct {
			name string
			fn   func(src, dst []byte) int
		}{
			{"fast", func(src, dst []byte) int {
				var c lz4.Compressor
				n, _ := c.CompressBlock(src, dst)
				return n
			}},
			{"hc9", func(src, dst []byte) int {
				c := lz4.CompressorHC{Level: lz4.Level9}
				n, _ := c.CompressBlock(src, dst)
				return n
			}},
			{"optimal", func(src, dst []byte) int {
				var c lz4.CompressorOptimal
				n, _ := c.CompressBlock(src, dst)
				return n
			}},
		} {
			dst := make([]byte, lz4.CompressBlockBound(size))
			total := mode.fn(src, dst)
			b.Run(mode.name+"/"+tc.name, func(b *testing.B) {
				b.SetBytes(size)
				b.ReportMetric(100*float64(total)/float64(size), "%size")
				for i := 0; i < b.N; i++ {
					mode.fn(src, dst)
				}
			})
		}
	}
}
