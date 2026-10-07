package btree

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

func validateBTree(t *testing.T, tr *BTreeG[testKind]) {
	t.Helper()
	if tr == nil {
		t.Fatal("nil tree")
	}
	if tr.root == nil {
		if tr.count != 0 {
			t.Fatalf("nil root with count %d", tr.count)
		}
		return
	}
	leafDepth := btreeLeafDepth(tr.root, 0)
	if got := btreeValidateNode(tr, tr.root, 0, leafDepth, true, nil, nil); got != tr.count {
		t.Fatalf("root count %d, tree count %d", got, tr.count)
	}
}

func btreeLeafDepth(n *node[testKind], depth int) int {
	if n.leaf() {
		return depth
	}
	return btreeLeafDepth((*n.children)[0], depth+1)
}

func btreeValidateNode(tr *BTreeG[testKind], n *node[testKind], depth,
	leafDepth int, root bool, min, max *testKind,
) int {
	if len(n.items) == 0 && (!root || !n.leaf()) {
		panic("empty non-root or internal node")
	}
	if len(n.items) > tr.max {
		panic(fmt.Sprintf("node has %d items, max %d", len(n.items), tr.max))
	}
	if !root && depth > 0 && len(n.items) < tr.min {
		panic(fmt.Sprintf("node has %d items, min %d", len(n.items), tr.min))
	}
	if n.leaf() != (depth == leafDepth) {
		panic("inconsistent leaf depth")
	}
	for i := 1; i < len(n.items); i++ {
		if !tr.less(n.items[i-1], n.items[i]) {
			panic("items are not ordered")
		}
	}
	if min != nil && (len(n.items) == 0 || tr.less(n.items[0], *min)) {
		panic("item below node minimum")
	}
	if max != nil && (len(n.items) == 0 || tr.less(*max, n.items[len(n.items)-1])) {
		panic("item above node maximum")
	}
	count := len(n.items)
	if n.leaf() {
		if n.children != nil {
			panic("leaf has children")
		}
	} else {
		if len(*n.children) != len(n.items)+1 {
			panic("child/item count mismatch")
		}
		for i := range *n.children {
			childMin := min
			childMax := max
			if i > 0 {
				childMin = &n.items[i-1]
			}
			if i < len(n.items) {
				childMax = &n.items[i]
			}
			count += btreeValidateNode(tr, (*n.children)[i], depth+1,
				leafDepth, false, childMin, childMax)
		}
	}
	if count != n.count {
		panic(fmt.Sprintf("stored count %d, actual %d", n.count, count))
	}
	return count
}

func keysForSplit(keys []testKind, pivot testKind, equalToRight bool,
) (left, right []testKind) {
	for _, key := range keys {
		if key < pivot || (key == pivot && !equalToRight) {
			left = append(left, key)
		} else {
			right = append(right, key)
		}
	}
	return left, right
}

func TestGenericSplitJoinRandom(t *testing.T) {
	for degree := 2; degree <= 8; degree++ {
		for trial := 0; trial < 30; trial++ {
			n := 200 + rand.Intn(800)
			tr := NewBTreeGOptions(testLess, Options{Degree: degree})
			want := map[testKind]bool{}
			for _, key := range rand.Perm(n) {
				if rand.Intn(5) != 0 {
					tr.Set(testKind(key))
					want[testKind(key)] = true
				}
			}
			for key := range want {
				if rand.Intn(4) == 0 {
					tr.Delete(key)
					delete(want, key)
				}
			}

			original := tr.Items()
			pivot := testKind(-1 + rand.Intn(n+2))
			equalToRight := rand.Intn(2) == 0
			beforeSnapshot := tr.Copy()
			left, right := tr.Split(pivot, equalToRight)

			validateBTree(t, tr)
			validateBTree(t, left)
			validateBTree(t, right)
			if got := tr.Items(); !kindsAreEqual(got, original) {
				t.Fatalf("split changed source: %v", degree)
			}
			if got := beforeSnapshot.Items(); !kindsAreEqual(got, original) {
				t.Fatal("split changed an existing snapshot")
			}

			wantLeft, wantRight := keysForSplit(original, pivot, equalToRight)
			if got := left.Items(); !kindsAreEqual(got, wantLeft) {
				t.Fatalf("left mismatch degree=%d pivot=%d equalRight=%v",
					degree, pivot, equalToRight)
			}
			if got := right.Items(); !kindsAreEqual(got, wantRight) {
				t.Fatalf("right mismatch degree=%d pivot=%d equalRight=%v",
					degree, pivot, equalToRight)
			}
			if left.Len()+right.Len() != tr.Len() {
				t.Fatal("split counts do not add up")
			}

			joined, err := left.Join(right)
			if err != nil {
				t.Fatal(err)
			}
			validateBTree(t, joined)
			if got := joined.Items(); !kindsAreEqual(got, original) {
				t.Fatalf("joined mismatch degree=%d pivot=%d", degree, pivot)
			}

		}
	}
}

func TestGenericSplitJoinBoundariesAndEmpty(t *testing.T) {
	for degree := 2; degree <= 6; degree++ {
		tr := NewBTreeGOptions(testLess, Options{Degree: degree})
		for _, key := range rand.Perm(250) {
			tr.Set(testKind(key))
		}
		for _, tc := range []struct {
			pivot testKind
			right bool
		}{
			{-1, false}, {-1, true}, {0, false}, {0, true},
			{249, false}, {249, true}, {250, false}, {250, true},
		} {
			original := tr.Items()
			left, right := tr.Split(tc.pivot, tc.right)
			joined, err := left.Join(right)
			if err != nil {
				t.Fatal(err)
			}
			if got := joined.Items(); !kindsAreEqual(got, original) {
				t.Fatalf("boundary join failed: %+v", tc)
			}
		}

		empty := NewBTreeGOptions(testLess, Options{Degree: degree})
		el, er := empty.Split(10, true)
		if el.Len() != 0 || er.Len() != 0 {
			t.Fatal("splitting an empty tree must be empty")
		}
		full := tr
		joined, err := el.Join(er)
		if err != nil || joined.Len() != 0 {
			t.Fatal("joining two empty trees failed")
		}
		joined, err = el.Join(full)
		if err != nil || joined.Len() != full.Len() {
			t.Fatal("joining empty left failed")
		}
		joined, err = full.Join(er)
		if err != nil || joined.Len() != full.Len() {
			t.Fatal("joining empty right failed")
		}
	}
}

func TestGenericSplitJoinDifferentHeights(t *testing.T) {
	for degree := 2; degree <= 6; degree++ {
		for leftN := 0; leftN < 350; leftN += 37 {
			for rightN := 0; rightN < 350; rightN += 41 {
				left := NewBTreeGOptions(testLess, Options{Degree: degree})
				right := NewBTreeGOptions(testLess, Options{Degree: degree})
				var want []testKind
				for key := 0; key < leftN; key++ {
					left.Set(testKind(key))
					want = append(want, testKind(key))
				}
				for key := 0; key < rightN; key++ {
					right.Set(testKind(10000 + key))
					want = append(want, testKind(10000+key))
				}
				joined, err := left.Join(right)
				if err != nil {
					t.Fatal(err)
				}
				if joined.root != nil && !joined.root.leaf() &&
					len(*joined.root.children) != len(joined.root.items)+1 {
					t.Fatalf("bad root degree=%d l=%d r=%d items=%d children=%d",
						degree, leftN, rightN, len(joined.root.items),
						len(*joined.root.children))
				}
				validateBTree(t, joined)
				sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
				if got := joined.Items(); !kindsAreEqual(got, want) {
					t.Fatalf("height join mismatch degree=%d left=%d right=%d",
						degree, leftN, rightN)
				}
			}
		}
	}
}

func TestGenericJoinValidation(t *testing.T) {
	left := NewBTreeGOptions(testLess, Options{Degree: 4})
	right := NewBTreeGOptions(testLess, Options{Degree: 4})
	left.Set(1)
	right.Set(2)
	if _, err := left.Join(right); err != nil {
		t.Fatal(err)
	}

	overlap := NewBTreeGOptions(testLess, Options{Degree: 4})
	overlap.Set(1)
	_, err := left.Join(overlap)
	if !errors.Is(err, ErrJoinRange) {
		t.Fatalf("got %v, want ErrJoinRange", err)
	}

	differentDegree := NewBTreeGOptions(testLess, Options{Degree: 5})
	differentDegree.Set(2)
	_, err = left.Join(differentDegree)
	if !errors.Is(err, ErrIncompatibleJoin) {
		t.Fatalf("got %v, want ErrIncompatibleJoin", err)
	}

	differentLess := NewBTreeGOptions(func(a, b testKind) bool {
		return a > b
	}, Options{Degree: 4})
	differentLess.Set(2)
	_, err = left.Join(differentLess)
	if !errors.Is(err, ErrIncompatibleJoin) {
		t.Fatalf("got %v, want ErrIncompatibleJoin", err)
	}

	leftBefore := left.Items()
	rightBefore := right.Items()
	_, _ = left.Join(overlap)
	if got := left.Items(); !kindsAreEqual(got, leftBefore) {
		t.Fatal("failed join changed left tree")
	}
	if got := overlap.Items(); !kindsAreEqual(got, []testKind{1}) {
		t.Fatal("failed join changed right tree")
	}
	_ = rightBefore
}

func TestGenericSplitJoinComparisonsAreLogarithmic(t *testing.T) {
	const n = 10_000
	var comparisons int
	less := func(a, b testKind) bool {
		comparisons++
		return a < b
	}
	tr := NewBTreeGOptions(less, Options{})
	for _, key := range rand.Perm(n) {
		tr.Set(testKind(key))
	}

	comparisons = 0
	left, right := tr.Split(n/2, false)
	splitComparisons := comparisons
	if splitComparisons >= n/4 {
		t.Fatalf("split used %d comparisons for %d items", splitComparisons, n)
	}

	comparisons = 0
	joined, err := left.Join(right)
	if err != nil {
		t.Fatal(err)
	}
	joinComparisons := comparisons
	if joinComparisons >= n/4 {
		t.Fatalf("join used %d comparisons for %d items", joinComparisons, n)
	}
	t.Logf("n=%d split comparisons=%d join comparisons=%d height=%d",
		n, splitComparisons, joinComparisons, joined.Height())
}

type isoSplitItem struct {
	key int
}

func (item *isoSplitItem) IsoCopy() *isoSplitItem {
	copied := *item
	return &copied
}

func TestGenericSplitIsoCopySnapshot(t *testing.T) {
	less := func(a, b *isoSplitItem) bool { return a.key < b.key }
	tr := NewBTreeGOptions(less, Options{Degree: 4})
	for i := 0; i < 200; i++ {
		tr.Set(&isoSplitItem{key: i})
	}
	snapshot := tr.Copy()
	left, right := tr.Split(&isoSplitItem{key: 100}, false)

	tr.Set(&isoSplitItem{key: 1000})
	left.Set(&isoSplitItem{key: -1000})
	right.Set(&isoSplitItem{key: 2000})

	if tr.Len() != 201 || left.Len() != 102 || right.Len() != 100 {
		t.Fatalf("bad counts: %d/%d/%d", tr.Len(), left.Len(), right.Len())
	}
	if snapshot.Len() != 200 {
		t.Fatalf("snapshot changed: %d", snapshot.Len())
	}
	joined, err := left.Join(right)
	if err != nil {
		t.Fatal(err)
	}
	var keys []int
	joined.Scan(func(item *isoSplitItem) bool {
		keys = append(keys, item.key)
		return true
	})
	if len(keys) != 202 || keys[0] != -1000 || keys[len(keys)-1] != 2000 {
		t.Fatalf("bad joined keys: %v", keys)
	}
}

func TestSplitJoinLegacyWrapper(t *testing.T) {
	tr := New(func(a, b any) bool { return a.(int) < b.(int) })
	for _, key := range rand.Perm(100) {
		tr.Set(key)
	}
	left, right := tr.Split(50, false)
	if left.Len() != 51 || right.Len() != 49 || tr.Len() != 100 {
		t.Fatalf("bad split counts: %d/%d/%d", left.Len(), right.Len(), tr.Len())
	}
	joined, err := left.Join(right)
	if err != nil {
		t.Fatal(err)
	}
	if joined.Len() != 100 {
		t.Fatalf("bad joined count: %d", joined.Len())
	}
	if value := joined.Get(50); value != 50 {
		t.Fatalf("got %v", value)
	}
}

func collectTestNodePointers(n *node[testKind], nodes map[*node[testKind]]bool) {
	if n == nil || nodes[n] {
		return
	}
	nodes[n] = true
	if !n.leaf() {
		for _, child := range *n.children {
			collectTestNodePointers(child, nodes)
		}
	}
}

func TestGenericJoinReusesInputNodes(t *testing.T) {
	tr := NewBTreeG(testLess)
	for _, key := range rand.Perm(1000) {
		tr.Set(testKind(key))
	}
	left, right := tr.Split(500, false)

	leftNodes := map[*node[testKind]]bool{}
	rightNodes := map[*node[testKind]]bool{}
	collectTestNodePointers(left.root, leftNodes)
	collectTestNodePointers(right.root, rightNodes)

	joined, err := left.Join(right)
	if err != nil {
		t.Fatal(err)
	}
	joinedNodes := map[*node[testKind]]bool{}
	collectTestNodePointers(joined.root, joinedNodes)
	reused := 0
	for n := range leftNodes {
		if joinedNodes[n] {
			reused++
		}
	}
	for n := range rightNodes {
		if joinedNodes[n] {
			reused++
		}
	}
	if reused == 0 {
		t.Fatal("join rebuilt every node")
	}
}
