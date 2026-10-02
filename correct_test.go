package reedsolomon

import (
	"bytes"
	"errors"
	"math/rand"
	"reflect"
	"testing"
)

// fixedVectorShards returns the fixed encoded shard set used by the
// fixed-vector tests: 4 data + 3 parity shards of 8 bytes.
func fixedVectorShards() [][]byte {
	return [][]byte{
		{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		{0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01},
		{0xff, 0x00, 0xff, 0x00, 0xff, 0x00, 0xff, 0x00},
		{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88},
		{0x1a, 0xf0, 0x16, 0x39, 0xdf, 0xd5, 0x33, 0xb6},
		{0x7b, 0x27, 0x9f, 0xbb, 0x03, 0x87, 0x3f, 0x9e},
		{0x8f, 0xd7, 0xec, 0x2f, 0x14, 0xec, 0xd7, 0xc2},
	}
}

func shardSetEqual(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// One corrupted shard and one erasure, exactly at the 2e+s <= p bound.
func TestCorrectErrorsFixedVector(t *testing.T) {
	enc, err := New(4, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := fixedVectorShards()

	shards := fixedVectorShards()
	// Corrupt shard 1.
	for i := range shards[1] {
		shards[1][i] ^= 0x5a
	}
	// Erase shard 5.
	shards[5] = nil

	corrupted, err := enc.(ErrorCorrection).CorrectErrors(shards)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(corrupted, []int{1}) {
		t.Fatalf("expected corrupted=[1], got %v", corrupted)
	}
	if !shardSetEqual(shards, want) {
		t.Fatalf("shards not restored:\ngot  %v\nwant %v", shards, want)
	}
}

// Erasure-only correction through CorrectErrors.
func TestCorrectErrorsFixedVectorErasureOnly(t *testing.T) {
	enc, err := New(4, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := fixedVectorShards()

	shards := fixedVectorShards()
	shards[0] = nil
	shards[6] = shards[6][:0]

	corrupted, err := enc.(ErrorCorrection).CorrectErrors(shards)
	if err != nil {
		t.Fatal(err)
	}
	if len(corrupted) != 0 {
		t.Fatalf("expected no corrupted shards, got %v", corrupted)
	}
	if !shardSetEqual(shards, want) {
		t.Fatalf("shards not restored:\ngot  %v\nwant %v", shards, want)
	}
}

// A consistent set must be left untouched, with no corrupted shards reported.
func TestCorrectErrorsFixedVectorClean(t *testing.T) {
	enc, err := New(4, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := fixedVectorShards()
	shards := fixedVectorShards()

	corrupted, err := enc.(ErrorCorrection).CorrectErrors(shards)
	if err != nil {
		t.Fatal(err)
	}
	if len(corrupted) != 0 {
		t.Fatalf("expected no corrupted shards, got %v", corrupted)
	}
	if !shardSetEqual(shards, want) {
		t.Fatal("clean shard set was modified")
	}
}

// Missing shards whose original content is zero take the all-zero
// syndrome path.
func TestCorrectErrorsZeroContent(t *testing.T) {
	enc, err := New(4, 3)
	if err != nil {
		t.Fatal(err)
	}
	shards := make([][]byte, 7)
	for i := range shards {
		shards[i] = make([]byte, 16)
	}
	if err := enc.Encode(shards); err != nil {
		t.Fatal(err)
	}
	shards[2] = nil
	shards[5] = nil
	corrupted, err := enc.(ErrorCorrection).CorrectErrors(shards)
	if err != nil {
		t.Fatal(err)
	}
	if len(corrupted) != 0 {
		t.Fatalf("expected no corrupted shards, got %v", corrupted)
	}
	for i := range shards {
		if !isZero(shards[i]) || len(shards[i]) != 16 {
			t.Fatalf("shard %d not restored to zeros", i)
		}
	}
}

// combinations returns all size-k subsets of {0..n-1} not intersecting excl.
func combinations(n, k int, excl map[int]bool) [][]int {
	var out [][]int
	cur := make([]int, 0, k)
	var walk func(start int)
	walk = func(start int) {
		if len(cur) == k {
			out = append(out, append([]int(nil), cur...))
			return
		}
		for i := start; i < n; i++ {
			if excl[i] {
				continue
			}
			cur = append(cur, i)
			walk(i + 1)
			cur = cur[:len(cur)-1]
		}
	}
	walk(0)
	return out
}

// Enumerate all small error and erasure position sets at and below the
// 2e+s <= p bound; every one must be located and fully repaired.
func TestCorrectErrorsEnumerate(t *testing.T) {
	const dataShards, parityShards = 4, 4
	const total = dataShards + parityShards
	const size = 64

	enc, err := New(dataShards, parityShards)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	original := make([][]byte, total)
	for i := range original {
		original[i] = make([]byte, size)
		rng.Read(original[i])
	}
	if err := enc.Encode(original); err != nil {
		t.Fatal(err)
	}

	for nErr := 0; 2*nErr <= parityShards; nErr++ {
		for _, errSet := range combinations(total, nErr, nil) {
			excl := make(map[int]bool, len(errSet))
			for _, e := range errSet {
				excl[e] = true
			}
			for nEra := 0; nEra+2*nErr <= parityShards; nEra++ {
				for _, eraSet := range combinations(total, nEra, excl) {
					shards := make([][]byte, total)
					for i := range original {
						shards[i] = append([]byte(nil), original[i]...)
					}
					for _, e := range errSet {
						for j := range shards[e] {
							shards[e][j] ^= 0xff
						}
					}
					for _, s := range eraSet {
						shards[s] = nil
					}
					corrupted, err := enc.(ErrorCorrection).CorrectErrors(shards)
					if err != nil {
						t.Fatalf("errors=%v erasures=%v: %v", errSet, eraSet, err)
					}
					wantCorrupted := append([]int(nil), errSet...)
					if len(wantCorrupted) == 0 && len(corrupted) == 0 {
						// ok
					} else if !reflect.DeepEqual(corrupted, wantCorrupted) {
						t.Fatalf("errors=%v erasures=%v: corrupted=%v", errSet, eraSet, corrupted)
					}
					if !shardSetEqual(shards, original) {
						t.Fatalf("errors=%v erasures=%v: shards not restored", errSet, eraSet)
					}
				}
			}
		}
	}
}

// Beyond the guaranteed bound the decoder must never claim a guaranteed
// result: it either fails, or returns a consistent codeword. A plain
// re-encode check on such a result is not a proof of correctness.
func TestCorrectErrorsBeyondBound(t *testing.T) {
	enc, err := New(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	ec := enc.(ErrorCorrection)

	// More erasures than parity shards.
	shards := make([][]byte, 6)
	for i := range shards {
		shards[i] = bytes.Repeat([]byte{byte(i + 1)}, 32)
	}
	if err := enc.Encode(shards); err != nil {
		t.Fatal(err)
	}
	tooMany := make([][]byte, 6)
	copy(tooMany, shards)
	tooMany[0], tooMany[1], tooMany[2] = nil, nil, nil
	if _, err := ec.CorrectErrors(tooMany); !errors.Is(err, ErrTooFewShards) {
		t.Fatalf("expected ErrTooFewShards, got %v", err)
	}

	// Two corrupted shards with two parity shards (2e=4 > p=2).
	// Must fail deterministically and leave the input unmodified.
	broken := make([][]byte, 6)
	for i := range shards {
		broken[i] = append([]byte(nil), shards[i]...)
	}
	for j := range broken[0] {
		broken[0][j] ^= 0xff
	}
	for j := range broken[1] {
		broken[1][j] ^= 0xa5
	}
	snapshot := make([][]byte, 6)
	for i := range broken {
		snapshot[i] = append([]byte(nil), broken[i]...)
	}
	if _, err := ec.CorrectErrors(broken); !errors.Is(err, ErrCannotCorrect) {
		t.Fatalf("expected ErrCannotCorrect, got %v", err)
	}
	if !shardSetEqual(broken, snapshot) {
		t.Fatal("shards were modified on failure")
	}

	// Enumerate small beyond-bound cases: any successful outcome must be a
	// consistent codeword, and failures must not modify the input.
	rng := rand.New(rand.NewSource(2))
	for nErr := 0; nErr <= 3; nErr++ {
		for _, errSet := range combinations(6, nErr, nil) {
			excl := make(map[int]bool, len(errSet))
			for _, e := range errSet {
				excl[e] = true
			}
			for nEra := 0; nEra <= 3; nEra++ {
				if nEra+2*nErr <= 2 || nEra > 2 {
					continue
				}
				for _, eraSet := range combinations(6, nEra, excl) {
					test := make([][]byte, 6)
					for i := range shards {
						test[i] = append([]byte(nil), shards[i]...)
					}
					for _, e := range errSet {
						for j := range test[e] {
							test[e][j] ^= byte(rng.Intn(255) + 1)
						}
					}
					for _, s := range eraSet {
						test[s] = nil
					}
					snap := make([][]byte, 6)
					for i := range test {
						snap[i] = append([]byte(nil), test[i]...)
					}
					_, err := ec.CorrectErrors(test)
					if err != nil {
						if !shardSetEqual(test, snap) {
							t.Fatalf("errors=%v erasures=%v: modified on failure", errSet, eraSet)
						}
						continue
					}
					ok, verr := enc.Verify(test)
					if verr != nil || !ok {
						t.Fatalf("errors=%v erasures=%v: success but inconsistent", errSet, eraSet)
					}
				}
			}
		}
	}
}

// Only the default Vandermonde-based matrix supports error correction.
func TestCorrectErrorsUnsupportedMatrix(t *testing.T) {
	custom := make([][]byte, 2)
	for i := range custom {
		custom[i] = []byte{1, 2, 3, 4}
	}
	opts := [][]Option{
		{WithCauchyMatrix()},
		{WithPAR1Matrix()},
		{WithJerasureMatrix()},
		{WithCustomMatrix(custom)},
	}
	for i, opt := range opts {
		enc, err := New(4, 2, opt...)
		if err != nil {
			t.Fatal(err)
		}
		shards := make([][]byte, 6)
		for j := range shards {
			shards[j] = make([]byte, 16)
		}
		if _, err := enc.(ErrorCorrection).CorrectErrors(shards); !errors.Is(err, ErrNotSupported) {
			t.Fatalf("option set %d: expected ErrNotSupported, got %v", i, err)
		}
	}

	// Single-parity XOR matrix.
	enc, err := New(4, 1, WithFastOneParityMatrix())
	if err != nil {
		t.Fatal(err)
	}
	shards := make([][]byte, 5)
	for j := range shards {
		shards[j] = make([]byte, 16)
	}
	if _, err := enc.(ErrorCorrection).CorrectErrors(shards); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("fastOneParity: expected ErrNotSupported, got %v", err)
	}

	// No parity at all.
	enc, err = New(4, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.(ErrorCorrection).CorrectErrors(shards[:4]); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("no parity: expected ErrNotSupported, got %v", err)
	}

	// Leopard encoders do not implement the interface.
	leo, err := New(4, 2, WithLeopardGF(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := leo.(ErrorCorrection); ok {
		t.Fatal("leopard encoder must not implement ErrorCorrection")
	}
}

// The error at shard index 0 (evaluation point 0) is a degenerate case for
// locator polynomials; make sure it is found.
func TestCorrectErrorsShardZero(t *testing.T) {
	enc, err := New(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	shards := make([][]byte, 6)
	rng := rand.New(rand.NewSource(3))
	for i := range shards {
		shards[i] = make([]byte, 48)
		rng.Read(shards[i])
	}
	if err := enc.Encode(shards); err != nil {
		t.Fatal(err)
	}
	want := make([][]byte, 6)
	for i := range shards {
		want[i] = append([]byte(nil), shards[i]...)
	}
	for j := range shards[0] {
		shards[0][j] ^= 0x77
	}
	corrupted, err := enc.(ErrorCorrection).CorrectErrors(shards)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(corrupted, []int{0}) {
		t.Fatalf("expected corrupted=[0], got %v", corrupted)
	}
	if !shardSetEqual(shards, want) {
		t.Fatal("shards not restored")
	}
}
