// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

func TestTreap_InsertRankSelect(t *testing.T) {
	rng := &xorshift{state: 1}
	var root *tNode

	const n = 2000
	keys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		k := keyFor(i)
		keys = append(keys, k)
		root = tInsert(root, []byte(k), k, rng)
	}

	if got := tSize(root); got != n {
		t.Fatalf("size = %d, want %d", got, n)
	}

	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)

	for i, k := range sorted {
		wantRank := i + 1
		if got := tCountLess(root, []byte(k)) + 1; got != wantRank {
			t.Fatalf("rank(%q) = %d, want %d", k, got, wantRank)
		}
		node := tKth(root, wantRank)
		if node == nil || node.val.(string) != k {
			t.Fatalf("select(%d) = %v, want %q", wantRank, node, k)
		}
	}

	// Out-of-range select returns nil.
	if tKth(root, 0) != nil || tKth(root, n+1) != nil {
		t.Fatal("expected nil for out of range select")
	}
}

func TestTreap_DeleteKeepsRankConsistent(t *testing.T) {
	rng := &xorshift{state: 7}
	var root *tNode

	all := make([]int, 500)
	for i := range all {
		all[i] = i
	}
	for _, i := range all {
		k := keyFor(i)
		root = tInsert(root, []byte(k), k, rng)
	}

	prng := rand.New(rand.NewSource(42))
	prng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })

	deleted := make(map[int]bool)
	for _, i := range all {
		root = tDelete(root, []byte(keyFor(i)))
		deleted[i] = true

		remaining := make([]string, 0)
		for j := 0; j < 500; j++ {
			if !deleted[j] {
				remaining = append(remaining, keyFor(j))
			}
		}
		sort.Strings(remaining)

		if got := tSize(root); got != len(remaining) {
			t.Fatalf("after deleting %d: size = %d, want %d", i, got, len(remaining))
		}
		for pos, k := range remaining {
			if got := tCountLess(root, []byte(k)) + 1; got != pos+1 {
				t.Fatalf("rank(%q) = %d, want %d", k, got, pos+1)
			}
			if node := tKth(root, pos+1); node == nil || node.val.(string) != k {
				t.Fatalf("select(%d) = %v, want %q", pos+1, node, k)
			}
		}
	}

	if root != nil {
		t.Fatalf("root should be empty, got %#v", root)
	}
}

func TestTreap_PersistenceDoesNotMutateOldRoot(t *testing.T) {
	rng := &xorshift{state: 3}
	var root *tNode
	root = tInsert(root, []byte("b"), "b", rng)
	snap := root
	root = tInsert(root, []byte("a"), "a", rng)
	root = tInsert(root, []byte("c"), "c", rng)

	if tSize(snap) != 1 {
		t.Fatalf("snapshot size = %d, want 1", tSize(snap))
	}
	if _, ok := tGet(snap, []byte("a")); ok {
		t.Fatal("old snapshot unexpectedly contains later insert")
	}

	afterDelete := tDelete(root, []byte("b"))
	if tSize(root) != 3 {
		t.Fatalf("delete mutated shared root: size = %d, want 3", tSize(root))
	}
	if tSize(afterDelete) != 2 {
		t.Fatalf("new root size = %d, want 2", tSize(afterDelete))
	}
}

func TestTreap_DuplicateKeyReplacesValue(t *testing.T) {
	rng := &xorshift{state: 11}
	var root *tNode
	root = tInsert(root, []byte("k"), "v1", rng)
	root = tInsert(root, []byte("k"), "v2", rng)
	if tSize(root) != 1 {
		t.Fatalf("size = %d, want 1", tSize(root))
	}
	if v, _ := tGet(root, []byte("k")); v != "v2" {
		t.Fatalf("value = %v, want v2", v)
	}
}

func keyFor(i int) string {
	// Zero-padded decimal numbers so lexical order matches numeric order.
	return fmt.Sprintf("k%04d", i)
}
