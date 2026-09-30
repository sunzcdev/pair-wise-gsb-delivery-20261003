/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package z

import (
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	wordlist1 [][]byte
	n         = 1 << 16
	bf        *Bloom
)

func TestMain(m *testing.M) {
	wordlist1 = make([][]byte, n)
	for i := range wordlist1 {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		wordlist1[i] = b
	}
	fmt.Println("\n###############\nbbloom_test.go")
	fmt.Print("Benchmarks relate to 2**16 OP. --> output/65536 op/ns\n###############\n\n")

	m.Run()
}

func TestM_NumberOfWrongs(t *testing.T) {
	bf = NewBloomFilter(float64(n*10), float64(7))

	cnt := 0
	for i := range wordlist1 {
		hash := MemHash(wordlist1[i])
		if !bf.AddIfNotHas(hash) {
			cnt++
		}
	}
	//nolint:lll
	fmt.Printf("Bloomfilter New(7* 2**16, 7) (-> size=%v bit): \n            Check for 'false positives': %v wrong positive 'Has' results on 2**16 entries => %v %%\n", len(bf.bitset)<<6, cnt, float64(cnt)/float64(n))

}

func TestM_JSON(t *testing.T) {
	const shallBe = int(1 << 16)

	bf = NewBloomFilter(float64(n*10), float64(7))

	cnt := 0
	for i := range wordlist1 {
		hash := MemHash(wordlist1[i])
		if !bf.AddIfNotHas(hash) {
			cnt++
		}
	}

	Json := bf.JSONMarshal()

	// create new bloomfilter from bloomfilter's JSON representation
	bf2, err := JSONUnmarshal(Json)
	require.NoError(t, err)

	cnt2 := 0
	for i := range wordlist1 {
		hash := MemHash(wordlist1[i])
		if !bf2.AddIfNotHas(hash) {
			cnt2++
		}
	}
	require.Equal(t, shallBe, cnt2)
}

func TestBloomSnapshotRestore(t *testing.T) {
	orig := NewBloomFilter(float64(n*10), float64(7))
	keys := make(map[uint64]bool, 1024)
	for i := 0; i < 1024; i++ {
		h := uint64(i*2654435761) ^ uint64(i)
		keys[h] = true
		orig.Add(h)
	}

	snap := orig.Snapshot()
	require.Equal(t, orig.SizeExp(), snap.SizeExp)
	require.Equal(t, orig.SetLocs(), snap.SetLocs)

	restored, err := RestoreBloom(snap)
	require.NoError(t, err)

	for h := range keys {
		require.True(t, restored.Has(h), "restored filter lost key %d", h)
	}

	// Mutating the snapshot must not affect the restored filter.
	snap.Bits[0] ^= 0xff
	for h := range keys {
		require.True(t, restored.Has(h))
	}
}

func TestBloomRestoreRejects(t *testing.T) {
	_, err := RestoreBloom(BloomSnapshot{Bits: make([]byte, 64), SizeExp: 8, SetLocs: 7})
	require.Error(t, err)
	_, err = RestoreBloom(BloomSnapshot{Bits: make([]byte, 128), SizeExp: 10, SetLocs: 0})
	require.Error(t, err)
	_, err = RestoreBloom(BloomSnapshot{Bits: make([]byte, 127), SizeExp: 10, SetLocs: 7})
	require.Error(t, err)
}

func BenchmarkM_New(b *testing.B) {
	for r := 0; r < b.N; r++ {
		_ = NewBloomFilter(float64(n*10), float64(7))
	}
}

func BenchmarkM_Clear(b *testing.B) {
	bf = NewBloomFilter(float64(n*10), float64(7))
	for i := range wordlist1 {
		hash := MemHash(wordlist1[i])
		bf.Add(hash)
	}
	b.ResetTimer()
	for r := 0; r < b.N; r++ {
		bf.Clear()
	}
}

func BenchmarkM_Add(b *testing.B) {
	bf = NewBloomFilter(float64(n*10), float64(7))
	b.ResetTimer()
	for r := 0; r < b.N; r++ {
		for i := range wordlist1 {
			hash := MemHash(wordlist1[i])
			bf.Add(hash)
		}
	}

}

func BenchmarkM_Has(b *testing.B) {
	b.ResetTimer()
	for r := 0; r < b.N; r++ {
		for i := range wordlist1 {
			hash := MemHash(wordlist1[i])
			bf.Has(hash)
		}
	}
}
