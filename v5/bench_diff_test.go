package jsonpatch

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// buildShuffleDocs returns a pair of documents, each holding a keyed array of
// size elements. The modified document is a full deterministic reshuffle of
// the original, with every tenth element's "weight" field changed.
func buildShuffleDocs(size int) ([]byte, []byte) {
	type entry struct {
		id     int
		weight int
	}
	entries := make([]entry, size)
	for i := range entries {
		entries[i] = entry{id: i, weight: i}
	}

	var orig strings.Builder
	orig.WriteString(`{"items":[`)
	for i, e := range entries {
		if i > 0 {
			orig.WriteByte(',')
		}
		fmt.Fprintf(&orig, `{"id":"id-%06d","weight":%d,"label":"item-%06d"}`, e.id, e.weight, e.id)
	}
	orig.WriteString(`]}`)

	rng := rand.New(rand.NewSource(42))
	rng.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })

	var mod strings.Builder
	mod.WriteString(`{"items":[`)
	for i, e := range entries {
		if i > 0 {
			mod.WriteByte(',')
		}
		weight := e.weight
		if i%10 == 0 {
			weight++
		}
		fmt.Fprintf(&mod, `{"id":"id-%06d","weight":%d,"label":"item-%06d"}`, e.id, weight, e.id)
	}
	mod.WriteString(`]}`)

	return []byte(orig.String()), []byte(mod.String())
}

func BenchmarkCreatePatchShuffled(b *testing.B) {
	for _, size := range []int{100, 1000, 5000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			original, modified := buildShuffleDocs(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				patch, err := CreatePatch(original, modified, WithArrayIdentity("/items", "id"))
				if err != nil {
					b.Fatal(err)
				}
				if len(patch) == 0 {
					b.Fatal("expected non-empty patch")
				}
			}
		})
	}
}

func BenchmarkCreatePatchPositionalShuffled(b *testing.B) {
	for _, size := range []int{100, 1000, 5000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			original, modified := buildShuffleDocs(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := CreatePatch(original, modified); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
