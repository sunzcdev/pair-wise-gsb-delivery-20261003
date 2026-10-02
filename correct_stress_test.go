package reedsolomon

import (
	"bytes"
	"math/rand"
	"testing"
	"time"
)

func TestCorrectStressLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	rng := rand.New(rand.NewSource(909))
	for iter := 0; iter < 120; iter++ {
		start := time.Now()
		parity := 8 + rng.Intn(60)
		data := 1 + rng.Intn(256-parity)
		n := data + parity
		enc, err := New(data, parity)
		if err != nil {
			t.Fatal(err)
		}
		r := enc.(*reedSolomon)
		size := 1 + rng.Intn(200)
		orig := genCodeword(t, r, size, rng.Int63())

		e := rng.Intn(parity + 1)
		v := rng.Intn((parity-e)/2 + 1)
		perm := rng.Perm(n)
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
		t.Logf("iter %d data=%d parity=%d e=%d v=%d took %v", iter, data, parity, e, v, time.Since(start))
		for i := range orig {
			if !bytes.Equal(shards[i], orig[i]) {
				t.Fatalf("iter %d shard %d mismatch", iter, i)
			}
		}
	}
}
