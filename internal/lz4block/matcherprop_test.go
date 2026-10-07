package lz4block

import (
	crand "crypto/rand"
	"testing"
)

// decoderMatch simulates an LZ4 decoder copying with the given offset and
// returns how many bytes are produced equal to src[si:].
func decoderMatch(src []byte, si, off int) int {
	n := len(src)
	out := make([]byte, n-si)
	for k := range out {
		if k < off {
			out[k] = src[si+k-off]
		} else {
			out[k] = out[k-off]
		}
		if out[k] != src[si+k] {
			return k
		}
	}
	return n - si
}

func TestMatcherMatchesDecoder(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		n := 2000
		src := make([]byte, n)
		crand.Read(src)
		// Inject patterns of various periods.
		switch iter % 4 {
		case 0:
			for i := range src {
				src[i] = byte(i % 3)
			}
		case 1:
			copy(src, make([]byte, n))
		case 2:
			for i := 1000; i < n; i++ {
				src[i] = src[i-20]
			}
		}
		for si := 1; si < n-20; si++ {
			for _, off := range []int{1, 2, 3, 5, 20, 33, 100, 1000, si} {
				if off > si {
					continue
				}
				got := matchFullLength(src, si-off, si)
				want := decoderMatch(src, si, off)
				if got != want {
					t.Fatalf("iter=%d si=%d off=%d got=%d want=%d", iter, si, off, got, want)
				}
			}
		}
	}
}
