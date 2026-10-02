package reedsolomon

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"testing"
)

// genCodeword builds and encodes a deterministic random codeword.
func genCodeword(t *testing.T, r *reedSolomon, size int, seed int64) [][]byte {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	shards := make([][]byte, r.totalShards)
	for i := 0; i < r.dataShards; i++ {
		shards[i] = make([]byte, size)
		for b := range shards[i] {
			shards[i][b] = byte(rng.Intn(256))
		}
	}
	for i := r.dataShards; i < r.totalShards; i++ {
		shards[i] = make([]byte, size)
	}
	if err := r.Encode(shards); err != nil {
		t.Fatal(err)
	}
	return shards
}

func cloneShards(in [][]byte) [][]byte {
	out := make([][]byte, len(in))
	for i := range in {
		if in[i] != nil {
			out[i] = append([]byte(nil), in[i]...)
		}
	}
	return out
}

func flipRandom(t *testing.T, rng *rand.Rand, shard []byte) {
	t.Helper()
	for b := range shard {
		if rng.Intn(2) == 0 {
			shard[b] ^= byte(1 + rng.Intn(255))
		}
	}
	shard[0] ^= 0x01 // guarantee at least one changed byte
}

// TestCorrectFixedVector uses one fixed codeword and a fixed error/erasure
// pattern within the guarantee.
func TestCorrectFixedVector(t *testing.T) {
	const data, parity, size = 6, 6, 32
	enc, err := New(data, parity)
	if err != nil {
		t.Fatal(err)
	}
	r := enc.(*reedSolomon)
	orig := genCodeword(t, r, size, 42)

	shards := cloneShards(orig)
	erasures := []int{2, 8} // 1 data + 1 parity erased
	corrupt := []int{1, 7}  // 1 data + 1 parity corrupted
	for _, idx := range erasures {
		shards[idx] = nil
	}
	rng := rand.New(rand.NewSource(7))
	for _, idx := range corrupt {
		flipRandom(t, rng, shards[idx])
	}

	ec := enc.(ErrorCorrection)
	fixed, err := ec.CorrectErrors(shards)
	if err != nil {
		t.Fatalf("CorrectErrors: %v", err)
	}
	for i := range orig {
		if !bytes.Equal(shards[i], orig[i]) {
			t.Fatalf("shard %d not restored", i)
		}
	}
	sortInts(fixed)
	if fmt.Sprint(fixed) != "[1 7]" {
		t.Fatalf("fixed indices = %v, want [1 7]", fixed)
	}

	ok, err := enc.Verify(shards)
	if err != nil || !ok {
		t.Fatalf("repaired set does not verify: ok=%v err=%v", ok, err)
	}
}

// TestCorrectEnumerate exhaustively covers every error/erasure position
// combination for several small codes at the guaranteed boundary.
func TestCorrectEnumerate(t *testing.T) {
	cases := []struct{ data, parity int }{
		{3, 3},
		{5, 4},
		{4, 5},
		{7, 2},
		{2, 6},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%dx%d", tc.data, tc.parity), func(t *testing.T) {
			n := tc.data + tc.parity
			enc, err := New(tc.data, tc.parity)
			if err != nil {
				t.Fatal(err)
			}
			orig := genCodeword(t, enc.(*reedSolomon), 16, int64(tc.data*100+tc.parity))

			for e := 0; e <= tc.parity; e++ {
				for v := 0; 2*v+e <= tc.parity; v++ {
					for _, erasures := range combinations(n, e) {
						erased := map[int]bool{}
						for _, x := range erasures {
							erased[x] = true
						}
						remaining := make([]int, 0, n-e)
						for i := 0; i < n; i++ {
							if !erased[i] {
								remaining = append(remaining, i)
							}
						}
						for _, corrupt := range combinations(len(remaining), v) {
							corruptIdx := make([]int, v)
							for i, c := range corrupt {
								corruptIdx[i] = remaining[c]
							}
							shards := cloneShards(orig)
							for _, idx := range erasures {
								shards[idx] = nil
							}
							rng := rand.New(rand.NewSource(int64(1000 + e*97 + v*7)))
							for _, idx := range corruptIdx {
								flipRandom(t, rng, shards[idx])
							}
							fixed, err := enc.(ErrorCorrection).CorrectErrors(shards)
							if err != nil {
								t.Fatalf("e=%v v=%v bad=%v: %v", erasures, v, corruptIdx, err)
							}
							for i := range orig {
								if !bytes.Equal(shards[i], orig[i]) {
									t.Fatalf("e=%v bad=%v: shard %d mismatch", erasures, corruptIdx, i)
								}
							}
							fset := map[int]bool{}
							for _, x := range fixed {
								fset[x] = true
							}
							for _, idx := range corruptIdx {
								if !fset[idx] {
									t.Fatalf("e=%v bad=%v: corrected index %d not reported", erasures, corruptIdx, idx)
								}
							}
						}
					}
				}
			}
		})
	}
}

// TestCorrectShardZero covers the special evaluation-point-0 cases, including
// the reduced bound when shard 0 is missing.
func TestCorrectShardZero(t *testing.T) {
	enc, _ := New(6, 4)
	r := enc.(*reedSolomon)
	orig := genCodeword(t, r, 24, 11)

	// Shard 0 corrupted, no erasures: 2*1+0 <= p.
	shards := cloneShards(orig)
	flipRandom(t, rand.New(rand.NewSource(1)), shards[0])
	if fixed, err := enc.(ErrorCorrection).CorrectErrors(shards); err != nil {
		t.Fatalf("corrupt shard0: %v", err)
	} else if len(fixed) != 1 || fixed[0] != 0 {
		t.Fatalf("fixed=%v want [0]", fixed)
	}
	for i := range orig {
		if !bytes.Equal(shards[i], orig[i]) {
			t.Fatalf("shard %d mismatch", i)
		}
	}

	// Shard 0 erased plus one error: 2*1+1 = 3 <= p-1 = 3.
	shards = cloneShards(orig)
	shards[0] = nil
	flipRandom(t, rand.New(rand.NewSource(2)), shards[5])
	if _, err := enc.(ErrorCorrection).CorrectErrors(shards); err != nil {
		t.Fatalf("erase0 + 1 error: %v", err)
	}
	for i := range orig {
		if !bytes.Equal(shards[i], orig[i]) {
			t.Fatalf("shard %d mismatch", i)
		}
	}

	// Erasure-only fast scenario: shard 0 missing, no corruption.
	shards = cloneShards(orig)
	shards[0] = nil
	if _, err := enc.(ErrorCorrection).CorrectErrors(shards); err != nil {
		t.Fatalf("erase0 only: %v", err)
	}
	for i := range orig {
		if !bytes.Equal(shards[i], orig[i]) {
			t.Fatalf("shard %d mismatch", i)
		}
	}
}

// TestCorrectUntrustedPresentShards ensures a corrupted parity shard cannot
// be mistaken for truth when data shards are fine.
func TestCorrectUntrustedPresentShards(t *testing.T) {
	enc, _ := New(8, 4)
	r := enc.(*reedSolomon)
	orig := genCodeword(t, r, 48, 99)

	shards := cloneShards(orig)
	flipRandom(t, rand.New(rand.NewSource(3)), shards[10]) // corrupt a parity shard

	before := bytes.Equal(shards[10], orig[10])
	if before {
		t.Fatal("test setup failed to corrupt parity")
	}
	if _, err := enc.(ErrorCorrection).CorrectErrors(shards); err != nil {
		t.Fatalf("correct parity error: %v", err)
	}
	if !bytes.Equal(shards[10], orig[10]) {
		t.Fatal("corrupted parity shard was trusted instead of repaired")
	}
}

// TestCorrectBeyondGuarantee documents behaviour outside the bound: it must
// either return ErrUncorrectable or leave a codeword; it must never panic or
// silently report success on wrong bytes.
func TestCorrectBeyondGuarantee(t *testing.T) {
	enc, _ := New(8, 4)
	r := enc.(*reedSolomon)
	orig := genCodeword(t, r, 64, 5)

	// 3 errors, 0 erasures: 2*3 = 6 > p = 4.
	shards := cloneShards(orig)
	rng := rand.New(rand.NewSource(9))
	for _, idx := range []int{1, 5, 9} {
		flipRandom(t, rng, shards[idx])
	}
	_, err := enc.(ErrorCorrection).CorrectErrors(shards)
	if err != nil {
		if !errors.Is(err, ErrUncorrectable) {
			t.Fatalf("want ErrUncorrectable, got %v", err)
		}
		return
	}
	// If something was returned it must be a valid codeword, but it is allowed
	// to differ from the original; we only assert no panic and consistent state.
	ok, verr := enc.Verify(shards)
	if verr != nil {
		t.Fatal(verr)
	}
	if !ok {
		t.Fatal("returned nil error but shards do not form a codeword")
	}
}

// TestCorrectUnsupportedMatrix checks non-standard matrix forms are rejected
// rather than decoding with an unsupported guarantee.
func TestCorrectUnsupportedMatrix(t *testing.T) {
	opts := [][]Option{
		{WithJerasureMatrix()},
		{WithPAR1Matrix()},
		{WithCauchyMatrix()},
	}
	for i, o := range opts {
		enc, err := New(5, 2, o...)
		if err != nil {
			t.Fatal(err)
		}
		shards := genCodeword(t, enc.(*reedSolomon), 16, int64(i))
		_, err = enc.(ErrorCorrection).CorrectErrors(shards)
		if !errors.Is(err, ErrNotSupported) {
			t.Fatalf("opts %d: want ErrNotSupported, got %v", i, err)
		}
	}
	// Fast one-parity XOR matrix.
	enc, err := New(5, 1, WithFastOneParityMatrix())
	if err != nil {
		t.Fatal(err)
	}
	shards := genCodeword(t, enc.(*reedSolomon), 16, 123)
	if _, err := enc.(ErrorCorrection).CorrectErrors(shards); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("xor matrix: want ErrNotSupported, got %v", err)
	}
}

// TestCorrectRandom runs a broad randomized sweep within the guarantee.
func TestCorrectRandom(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	rng := rand.New(rand.NewSource(2026))
	for iter := 0; iter < 2000; iter++ {
		data := 1 + rng.Intn(40)
		parity := 1 + rng.Intn(12)
		if data+parity > 256 {
			continue
		}
		enc, err := New(data, parity)
		if err != nil {
			t.Fatal(err)
		}
		r := enc.(*reedSolomon)
		size := 1 + rng.Intn(128)
		orig := genCodeword(t, r, size, rng.Int63())

		e := rng.Intn(parity + 1)
		v := rng.Intn((parity-e)/2 + 1)
		perm := rng.Perm(data + parity)
		shards := cloneShards(orig)
		for _, idx := range perm[:e] {
			shards[idx] = nil
		}
		for _, idx := range perm[e : e+v] {
			flipRandom(t, rng, shards[idx])
		}
		if _, err := enc.(ErrorCorrection).CorrectErrors(shards); err != nil {
			t.Fatalf("iter %d data=%d parity=%d e=%d v=%d: %v", iter, data, parity, e, v, err)
		}
		for i := range orig {
			if !bytes.Equal(shards[i], orig[i]) {
				t.Fatalf("iter %d shard %d mismatch", iter, i)
			}
		}
	}
}

// TestCorrectErasureOnlyMatchesReconstruct ensures that when only shards are
// missing (no silent corruption), CorrectErrors restores exactly what the
// erasure-only fast path would and reports no corrected indices.
func TestCorrectErasureOnlyMatchesReconstruct(t *testing.T) {
	enc, _ := New(10, 4)
	r := enc.(*reedSolomon)
	orig := genCodeword(t, r, 2048, 77)

	missing := []int{2, 5, 12}
	a := cloneShards(orig)
	b := cloneShards(orig)
	for _, idx := range missing {
		a[idx] = nil
		b[idx] = nil
	}
	if err := enc.Reconstruct(a); err != nil {
		t.Fatal(err)
	}
	fixed, err := enc.(ErrorCorrection).CorrectErrors(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixed) != 0 {
		t.Fatalf("erasure-only reported corrected shards %v", fixed)
	}
	for i := range orig {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("shard %d differs from Reconstruct output", i)
		}
	}
}

// TestCorrectNoParity rejects encoders without parity shards.
func TestCorrectNoParity(t *testing.T) {
	enc, err := New(5, 0)
	if err != nil {
		t.Fatal(err)
	}
	shards := make([][]byte, 5)
	for i := range shards {
		shards[i] = make([]byte, 8)
	}
	_, err = enc.(ErrorCorrection).CorrectErrors(shards)
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("want ErrNotSupported, got %v", err)
	}
}

// combinations returns all length-r subsets of [0..n-1] in index order.
func combinations(n, r int) [][]int {
	if r < 0 || r > n {
		return nil
	}
	if r == 0 {
		return [][]int{{}}
	}
	var out [][]int
	current := make([]int, r)
	var rec func(start, depth int)
	rec = func(start, depth int) {
		if depth == r {
			cp := append([]int(nil), current...)
			out = append(out, cp)
			return
		}
		for i := start; i <= n-(r-depth); i++ {
			current[depth] = i
			rec(i+1, depth+1)
		}
	}
	rec(0, 0)
	return out
}

func sortInts(x []int) {
	for i := 1; i < len(x); i++ {
		for j := i; j > 0 && x[j-1] > x[j]; j-- {
			x[j-1], x[j] = x[j], x[j-1]
		}
	}
}
