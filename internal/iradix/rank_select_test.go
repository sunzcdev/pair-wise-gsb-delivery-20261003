package iradix

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// checkRankSelect verifies Rank, Select and subtree sizes against a sorted
// reference list of keys.
func checkRankSelect(t *testing.T, tree *Tree, keys []string) {
	t.Helper()
	root := tree.Root()
	if root.Len() != len(keys) {
		t.Fatalf("bad root size: got %d, want %d", root.Len(), len(keys))
	}
	if tree.Len() != len(keys) {
		t.Fatalf("bad tree size: got %d, want %d", tree.Len(), len(keys))
	}
	for i, k := range keys {
		if got := root.Rank([]byte(k)); got != i {
			t.Fatalf("bad rank for %q: got %d, want %d", k, got, i)
		}
		key, val, ok := root.Select(i)
		if !ok {
			t.Fatalf("select %d failed", i)
		}
		if string(key) != k || val != k {
			t.Fatalf("bad select at %d: got (%q, %v), want %q", i, key, val, k)
		}
	}
	// Ranks of keys that are not present must still be their sorted position.
	for _, probe := range []string{"", "!", "zzzzzz", "m"} {
		want := sort.SearchStrings(keys, probe)
		if got := root.Rank([]byte(probe)); got != want {
			t.Fatalf("bad rank for probe %q: got %d, want %d", probe, got, want)
		}
	}
	// Out of range selects fail.
	if _, _, ok := root.Select(-1); ok {
		t.Fatalf("select -1 should fail")
	}
	if _, _, ok := root.Select(len(keys)); ok {
		t.Fatalf("select %d should fail", len(keys))
	}
}

func TestRankSelect_RandomOps(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	tree := New()
	ref := make(map[string]struct{})

	sortedRef := func() []string {
		keys := make([]string, 0, len(ref))
		for k := range ref {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}

	// Apply random operations in multi-operation transactions to exercise
	// the writable-node reuse paths within a single transaction.
	for round := 0; round < 500; round++ {
		txn := tree.Txn()
		ops := 1 + rng.Intn(50)
		for op := 0; op < ops; op++ {
			key := fmt.Sprintf("key-%06d", rng.Intn(3000))
			switch rng.Intn(4) {
			case 0, 1:
				// Insert (or update) a key.
				txn.Insert([]byte(key), key)
				ref[key] = struct{}{}
			case 2:
				// Delete a key.
				txn.Delete([]byte(key))
				delete(ref, key)
			case 3:
				// Delete a whole prefix.
				prefix := fmt.Sprintf("key-%03d", rng.Intn(1000))
				txn.DeletePrefix([]byte(prefix))
				for k := range ref {
					if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
						delete(ref, k)
					}
				}
			}
		}
		tree = txn.Commit()

		if round%25 == 0 {
			checkRankSelect(t, tree, sortedRef())
		}
	}
	checkRankSelect(t, tree, sortedRef())

	// An old snapshot must keep answering with its own sizes while the
	// newer tree keeps mutating.
	snap := tree
	snapKeys := sortedRef()
	txn := tree.Txn()
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("key-%06d", rng.Intn(3000))
		txn.Insert([]byte(key), key)
	}
	tree = txn.Commit()
	checkRankSelect(t, snap, snapKeys)
}

func TestRankSelect_Sequential(t *testing.T) {
	tree := New()
	var keys []string
	for i := 0; i < 1000; i++ {
		k := fmt.Sprintf("%08d", i)
		tree, _, _ = tree.Insert([]byte(k), k)
		keys = append(keys, k)
	}
	checkRankSelect(t, tree, keys)

	// Delete every other key.
	kept := keys[:0]
	for i, k := range keys {
		if i%2 == 0 {
			tree, _, _ = tree.Delete([]byte(k))
		} else {
			kept = append(kept, k)
		}
	}
	checkRankSelect(t, tree, kept)
}

func benchmarkTree(b *testing.B, n int) (*Tree, []string) {
	rng := rand.New(rand.NewSource(42))
	tree := New()
	keys := make([]string, 0, n)
	txn := tree.Txn()
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("key-%06d", rng.Intn(n*10))
		if _, ok := txn.Get([]byte(k)); ok {
			continue
		}
		txn.Insert([]byte(k), k)
		keys = append(keys, k)
	}
	tree = txn.Commit()
	return tree, keys
}

func BenchmarkRank(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000, 1000000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			tree, keys := benchmarkTree(b, n)
			root := tree.Root()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				root.Rank([]byte(keys[i%len(keys)]))
			}
		})
	}
}

func BenchmarkSelect(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000, 1000000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			tree, keys := benchmarkTree(b, n)
			root := tree.Root()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				root.Select(i % len(keys))
			}
		})
	}
}
