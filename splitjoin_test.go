package btree

import (
	"math/rand"
	"sort"
	"sync/atomic"
	"testing"
)

// checkNodeIntegrity recursively verifies the B-tree invariants of the
// subtree rooted at n: node occupancy, item ordering, child counts, the
// counted b-tree fields, and that all leaves are at the same depth.
// It returns the number of items in the subtree.
func checkNodeIntegrity(t *testing.T, tr *BTreeG[testKind],
	n *node[testKind], root bool, depth int, leafDepth *int,
) int {
	t.Helper()
	if root {
		if len(n.items) < 1 || len(n.items) > tr.max {
			t.Fatalf("root node has %d items, max is %d", len(n.items),
				tr.max)
		}
	} else if len(n.items) < tr.min || len(n.items) > tr.max {
		t.Fatalf("node has %d items, expected within [%d, %d]",
			len(n.items), tr.min, tr.max)
	}
	for i := 1; i < len(n.items); i++ {
		if !tr.less(n.items[i-1], n.items[i]) {
			t.Fatalf("node items out of order")
		}
	}
	if n.leaf() {
		if *leafDepth == -1 {
			*leafDepth = depth
		} else if *leafDepth != depth {
			t.Fatalf("leaves at different depths: %d and %d",
				*leafDepth, depth)
		}
		return len(n.items)
	}
	if len(*n.children) != len(n.items)+1 {
		t.Fatalf("node has %d items and %d children", len(n.items),
			len(*n.children))
	}
	count := len(n.items)
	for _, child := range *n.children {
		count += checkNodeIntegrity(t, tr, child, false, depth+1,
			leafDepth)
	}
	if n.count != count {
		t.Fatalf("node count is %d, expected %d", n.count, count)
	}
	return count
}

// checkIntegrity verifies all structural invariants of the tree and that a
// full scan returns tr.Len() items in strictly ascending order.
func checkIntegrity(t *testing.T, tr *BTreeG[testKind]) {
	t.Helper()
	if tr.root == nil {
		if tr.count != 0 {
			t.Fatalf("empty tree has count %d", tr.count)
		}
		return
	}
	leafDepth := -1
	count := checkNodeIntegrity(t, tr, tr.root, true, 1, &leafDepth)
	if count != tr.count {
		t.Fatalf("tree count is %d, expected %d", tr.count, count)
	}
	if tr.Len() != tr.count {
		t.Fatalf("tree Len() is %d, expected %d", tr.Len(), tr.count)
	}
	var prev testKind
	n := 0
	tr.Scan(func(item testKind) bool {
		if n > 0 && !tr.less(prev, item) {
			t.Fatalf("scan out of order at item %d", n)
		}
		prev = item
		n++
		return true
	})
	if n != tr.count {
		t.Fatalf("scan returned %d items, expected %d", n, tr.count)
	}
}

// sortedItems returns all items in ascending order.
func sortedItems[T any](tr *BTreeG[T]) []T {
	var items []T
	tr.Scan(func(item T) bool {
		items = append(items, item)
		return true
	})
	return items
}

func itemsAreEqual[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSplitJoinRandom builds trees with random inserts and deletes, then
// repeatedly splits them at random keys and joins the parts back together,
// verifying key order, node occupancy, and item counts at every step.
func TestSplitJoinRandom(t *testing.T) {
	for _, degree := range []int{2, 3, 7, 0} { // 0 means the default of 32
		for iter := 0; iter < 20; iter++ {
			tr := NewBTreeGOptions(testLess, Options{Degree: degree})
			// Random inserts and deletes over a dense key space.
			space := 1 + rand.Intn(2000)
			for i := 0; i < space*2; i++ {
				key := testMakeItem(rand.Intn(space))
				if rand.Intn(100) < 60 {
					tr.Set(key)
				} else {
					tr.Delete(key)
				}
			}
			checkIntegrity(t, tr)
			expected := sortedItems(tr)
			// Snapshot the tree to ensure split/join do not disturb
			// existing copies.
			snap := tr.Copy()
			// Repeatedly split and join.
			for round := 0; round < 5; round++ {
				key := testMakeItem(rand.Intn(space + 2))
				right := tr.Split(key)
				// Everything < key on the left, >= key on the right.
				pivot := sort.Search(len(expected), func(i int) bool {
					return !testLess(expected[i], key)
				})
				if !itemsAreEqual(sortedItems(tr), expected[:pivot]) {
					t.Fatalf("deg %d iter %d round %d: bad left split",
						degree, iter, round)
				}
				if !itemsAreEqual(sortedItems(right), expected[pivot:]) {
					t.Fatalf("deg %d iter %d round %d: bad right split",
						degree, iter, round)
				}
				if tr.Len()+right.Len() != len(expected) {
					t.Fatalf("deg %d iter %d round %d: count mismatch",
						degree, iter, round)
				}
				checkIntegrity(t, tr)
				checkIntegrity(t, right)
				// The snapshot must be untouched.
				if !itemsAreEqual(sortedItems(snap), expected) {
					t.Fatalf("deg %d iter %d round %d: snapshot "+
						"changed", degree, iter, round)
				}
				// Join the parts back together.
				if err := tr.Join(right); err != nil {
					t.Fatalf("deg %d iter %d round %d: join: %v",
						degree, iter, round, err)
				}
				if right.Len() != 0 {
					t.Fatalf("joined tree not emptied")
				}
				if !itemsAreEqual(sortedItems(tr), expected) {
					t.Fatalf("deg %d iter %d round %d: bad join",
						degree, iter, round)
				}
				checkIntegrity(t, tr)
			}
			checkIntegrity(t, snap)
		}
	}
}

// TestSplitBoundaries tests splits of empty trees, single-item trees, and
// splits at, below, and above the boundary keys.
func TestSplitBoundaries(t *testing.T) {
	// Empty tree.
	tr := testNewBTree()
	right := tr.Split(testMakeItem(50))
	checkIntegrity(t, tr)
	checkIntegrity(t, right)
	if tr.Len() != 0 || right.Len() != 0 {
		t.Fatalf("split of empty tree produced items")
	}
	if err := tr.Join(right); err != nil {
		t.Fatalf("join of empty trees: %v", err)
	}

	// Single item tree.
	for _, key := range []int{0, 50, 100} {
		tr := testNewBTree()
		tr.Set(testMakeItem(50))
		right := tr.Split(testMakeItem(key))
		checkIntegrity(t, tr)
		checkIntegrity(t, right)
		var wantLeft, wantRight int
		if key <= 50 {
			wantRight = 1
		} else {
			wantLeft = 1
		}
		if tr.Len() != wantLeft || right.Len() != wantRight {
			t.Fatalf("split at %d: got %d/%d items", key, tr.Len(),
				right.Len())
		}
		if err := tr.Join(right); err != nil {
			t.Fatalf("join: %v", err)
		}
		if tr.Len() != 1 {
			t.Fatalf("join at %d: got %d items", key, tr.Len())
		}
		checkIntegrity(t, tr)
	}

	// Split at, below, and above the boundary keys of a larger tree.
	N := 5000
	for _, key := range []int{-1, 0, 1, N / 2, N - 1, N, N + 1} {
		tr := testNewBTree()
		for _, k := range randKeys(N) {
			tr.Set(k)
		}
		right := tr.Split(testMakeItem(key))
		lo, hi := key, key
		if lo < 0 {
			lo = 0
		}
		if lo > N {
			lo = N
		}
		if hi < 0 {
			hi = 0
		}
		if hi > N {
			hi = N
		}
		if tr.Len() != lo || right.Len() != N-hi {
			t.Fatalf("split at %d: got %d/%d items, expected %d/%d",
				key, tr.Len(), right.Len(), lo, N-hi)
		}
		if key > 0 && key <= N {
			if max, _ := tr.Max(); max != testMakeItem(key-1) {
				t.Fatalf("split at %d: left max is %v", key, max)
			}
		}
		if key >= 0 && key < N {
			if min, _ := right.Min(); min != testMakeItem(key) {
				t.Fatalf("split at %d: right min is %v", key, min)
			}
		}
		checkIntegrity(t, tr)
		checkIntegrity(t, right)
		if err := tr.Join(right); err != nil {
			t.Fatalf("join at %d: %v", key, err)
		}
		if tr.Len() != N {
			t.Fatalf("join at %d: got %d items", key, tr.Len())
		}
		checkIntegrity(t, tr)
	}
}

// TestJoinDifferentHeights joins trees with very different heights.
func TestJoinDifferentHeights(t *testing.T) {
	for _, sizes := range [][2]int{
		{1, 10000}, {10000, 1}, {2, 5000}, {5000, 2},
		{63, 4097}, {4097, 63}, {1, 1}, {1, 2}, {2, 1},
	} {
		for _, degree := range []int{2, 0} {
			left := NewBTreeGOptions(testLess, Options{Degree: degree})
			right := NewBTreeGOptions(testLess, Options{Degree: degree})
			total := sizes[0] + sizes[1]
			for _, k := range rand.Perm(sizes[0]) {
				left.Set(testMakeItem(k))
			}
			for _, k := range rand.Perm(sizes[1]) {
				right.Set(testMakeItem(sizes[0] + k))
			}
			var expected []testKind
			for i := 0; i < total; i++ {
				expected = append(expected, testMakeItem(i))
			}
			if err := left.Join(right); err != nil {
				t.Fatalf("sizes %v degree %d: %v", sizes, degree, err)
			}
			if right.Len() != 0 {
				t.Fatalf("sizes %v: other tree not emptied", sizes)
			}
			if !itemsAreEqual(sortedItems(left), expected) {
				t.Fatalf("sizes %v degree %d: bad join result", sizes,
					degree)
			}
			checkIntegrity(t, left)
		}
	}
}

// TestJoinValidation tests that Join rejects incompatible trees.
func TestJoinValidation(t *testing.T) {
	// Self join and nil join.
	tr := testNewBTree()
	tr.Set(testMakeItem(1))
	if err := tr.Join(tr); err == nil {
		t.Fatalf("expected error joining tree with itself")
	}
	if err := tr.Join(nil); err == nil {
		t.Fatalf("expected error joining with nil tree")
	}

	// Different comparators.
	tr1 := testNewBTree()
	tr2 := NewBTreeG(func(a, b testKind) bool { return a < b })
	tr1.Set(testMakeItem(1))
	tr2.Set(testMakeItem(2))
	if err := tr1.Join(tr2); err == nil {
		t.Fatalf("expected error joining trees with different " +
			"comparators")
	}
	if tr1.Len() != 1 || tr2.Len() != 1 {
		t.Fatalf("failed join modified the trees")
	}

	// Different degrees.
	tr1 = NewBTreeGOptions(testLess, Options{Degree: 2})
	tr2 = NewBTreeGOptions(testLess, Options{Degree: 16})
	tr1.Set(testMakeItem(1))
	tr2.Set(testMakeItem(2))
	if err := tr1.Join(tr2); err == nil {
		t.Fatalf("expected error joining trees with different degrees")
	}
	if tr1.Len() != 1 || tr2.Len() != 1 {
		t.Fatalf("failed join modified the trees")
	}

	// Overlapping key ranges.
	tr1 = testNewBTree()
	tr2 = testNewBTree()
	for i := 0; i < 100; i++ {
		tr1.Set(testMakeItem(i))
		tr2.Set(testMakeItem(i + 50))
	}
	if err := tr1.Join(tr2); err == nil {
		t.Fatalf("expected error joining overlapping trees")
	}
	if tr1.Len() != 100 || tr2.Len() != 100 {
		t.Fatalf("failed join modified the trees")
	}

	// Reversed join order is also an overlap.
	if err := tr2.Join(tr1); err == nil {
		t.Fatalf("expected error joining overlapping trees")
	}

	// Touching key ranges (equal boundary keys) are rejected too.
	tr1 = testNewBTree()
	tr2 = testNewBTree()
	tr1.Set(testMakeItem(1))
	tr1.Set(testMakeItem(2))
	tr2.Set(testMakeItem(2))
	tr2.Set(testMakeItem(3))
	if err := tr1.Join(tr2); err == nil {
		t.Fatalf("expected error joining trees with touching ranges")
	}

	// Joining with an empty tree is fine.
	tr2 = testNewBTree()
	if err := tr1.Join(tr2); err != nil {
		t.Fatalf("join with empty tree: %v", err)
	}
	if tr1.Len() != 2 {
		t.Fatalf("join with empty tree changed the tree")
	}
	if err := tr2.Join(tr1); err != nil {
		t.Fatalf("join into empty tree: %v", err)
	}
	if tr2.Len() != 2 || tr1.Len() != 0 {
		t.Fatalf("join into empty tree failed")
	}
	checkIntegrity(t, tr2)
}

// TestSplitJoinIsolation verifies that split and join preserve the
// copy-on-write sharing and isolation conventions of Copy and IsoCopy.
func TestSplitJoinIsolation(t *testing.T) {
	N := 20000
	tr := testNewBTree()
	for _, k := range randKeys(N) {
		tr.Set(k)
	}
	snap := tr.IsoCopy()
	expected := sortedItems(tr)

	// Split the original; the snapshot must not change.
	right := tr.Split(testMakeItem(N / 2))
	if !itemsAreEqual(sortedItems(snap), expected) {
		t.Fatalf("snapshot changed after split")
	}

	// Mutate the right tree; neither the left tree nor the snapshot
	// may change.
	leftItems := sortedItems(tr)
	for i := 0; i < 1000; i++ {
		right.Set(testMakeItem(N + i))
		right.Delete(testMakeItem(N/2 + i))
	}
	if !itemsAreEqual(sortedItems(tr), leftItems) {
		t.Fatalf("left tree changed after mutating right tree")
	}
	if !itemsAreEqual(sortedItems(snap), expected) {
		t.Fatalf("snapshot changed after mutating right tree")
	}

	// Mutate the left tree; the right tree and snapshot must not change.
	rightItems := sortedItems(right)
	for i := 0; i < 1000; i++ {
		tr.Set(testMakeItem(-i - 1))
		tr.Delete(testMakeItem(i))
	}
	if !itemsAreEqual(sortedItems(right), rightItems) {
		t.Fatalf("right tree changed after mutating left tree")
	}
	if !itemsAreEqual(sortedItems(snap), expected) {
		t.Fatalf("snapshot changed after mutating left tree")
	}

	// A copy of the right tree must stay intact when the right tree is
	// joined back into the left tree.
	rightSnap := right.Copy()
	leftItems = sortedItems(tr)
	if err := tr.Join(right); err != nil {
		t.Fatalf("join: %v", err)
	}
	if !itemsAreEqual(sortedItems(rightSnap), rightItems) {
		t.Fatalf("copy of right tree changed after join")
	}
	if !itemsAreEqual(sortedItems(snap), expected) {
		t.Fatalf("snapshot changed after join")
	}
	if !itemsAreEqual(sortedItems(tr),
		append(leftItems, rightItems...)) {
		t.Fatalf("bad join result")
	}
	checkIntegrity(t, tr)
	checkIntegrity(t, snap)
	checkIntegrity(t, rightSnap)
}

// collectNodes adds all nodes in the subtree rooted at n to the set.
func collectNodes[T any](n *node[T], set map[*node[T]]bool) {
	if n == nil {
		return
	}
	set[n] = true
	if !n.leaf() {
		for _, child := range *n.children {
			collectNodes(child, set)
		}
	}
}

// TestSplitJoinNodeReuse proves that split and join are structural
// operations: the vast majority of nodes are reused rather than rebuilt,
// and only O(log n) new nodes are created.
func TestSplitJoinNodeReuse(t *testing.T) {
	N := 100000
	tr := testNewBTree()
	for _, k := range randKeys(N) {
		tr.Set(k)
	}
	before := make(map[*node[testKind]]bool)
	collectNodes(tr.root, before)

	right := tr.Split(testMakeItem(N / 2))

	after := make(map[*node[testKind]]bool)
	collectNodes(tr.root, after)
	collectNodes(right.root, after)

	reused := 0
	for n := range after {
		if before[n] {
			reused++
		}
	}
	newNodes := len(after) - reused
	height := tr.Height()
	if right.Height() > height {
		height = right.Height()
	}
	// Only the nodes along the split path and the join spines may be
	// new, which is O(height) nodes.
	if newNodes > 100*height {
		t.Fatalf("split created %d new nodes, height is %d", newNodes,
			height)
	}
	reuseRatio := float64(reused) / float64(len(before))
	if reuseRatio < 0.98 {
		t.Fatalf("split reused %.2f%% of nodes, expected at least 98%%",
			reuseRatio*100)
	}

	// Joining the parts back together must also reuse the nodes.
	if err := tr.Join(right); err != nil {
		t.Fatalf("join: %v", err)
	}
	joined := make(map[*node[testKind]]bool)
	collectNodes(tr.root, joined)
	reused = 0
	for n := range joined {
		if before[n] {
			reused++
		}
	}
	reuseRatio = float64(reused) / float64(len(joined))
	if reuseRatio < 0.90 {
		t.Fatalf("join reused %.2f%% of nodes, expected at least 90%%",
			reuseRatio*100)
	}
	if tr.Len() != N {
		t.Fatalf("joined tree has %d items, expected %d", tr.Len(), N)
	}
	checkIntegrity(t, tr)
}

// TestSplitJoinCompareCount proves that split and join do not rebuild the
// tree by counting the number of key comparisons they perform. A rebuild
// would require O(n log n) comparisons; a structural operation only needs
// O(log n) comparisons for the search paths.
func TestSplitJoinCompareCount(t *testing.T) {
	var compares int64
	less := func(a, b testKind) bool {
		atomic.AddInt64(&compares, 1)
		return a < b
	}
	N := 100000
	tr := NewBTreeG(less)
	for _, k := range randKeys(N) {
		tr.Set(k)
	}

	atomic.StoreInt64(&compares, 0)
	right := tr.Split(testMakeItem(N / 2))
	splitCompares := atomic.LoadInt64(&compares)

	atomic.StoreInt64(&compares, 0)
	if err := tr.Join(right); err != nil {
		t.Fatalf("join: %v", err)
	}
	joinCompares := atomic.LoadInt64(&compares)

	// The search paths of a split and a join are O(height*log2(max))
	// comparisons, which for 100k items is well under a hundred. Use a
	// generous bound that is still far below the O(n) a rebuild needs.
	limit := int64(N / 100)
	if splitCompares > limit {
		t.Fatalf("split performed %d comparisons, limit is %d",
			splitCompares, limit)
	}
	if joinCompares > limit {
		t.Fatalf("join performed %d comparisons, limit is %d",
			joinCompares, limit)
	}
	t.Logf("split: %d compares, join: %d compares, for %d items",
		splitCompares, joinCompares, N)
	if tr.Len() != N {
		t.Fatalf("joined tree has %d items, expected %d", tr.Len(), N)
	}
	checkIntegrity(t, tr)
}

// TestSplitJoinBTree is a smoke test for the non-generic BTree wrapper.
func TestSplitJoinBTree(t *testing.T) {
	tr := New(func(a, b any) bool { return a.(int) < b.(int) })
	for _, k := range rand.Perm(1000) {
		tr.Set(k)
	}
	right := tr.Split(500)
	if tr.Len() != 500 || right.Len() != 500 {
		t.Fatalf("got %d/%d items", tr.Len(), right.Len())
	}
	if tr.Max().(int) != 499 || right.Min().(int) != 500 {
		t.Fatalf("bad split boundaries")
	}
	if err := tr.Join(right); err != nil {
		t.Fatalf("join: %v", err)
	}
	if tr.Len() != 1000 {
		t.Fatalf("got %d items", tr.Len())
	}
	if err := tr.Join(New(func(a, b any) bool {
		return a.(int) > b.(int)
	})); err == nil {
		t.Fatalf("expected error joining with different comparator")
	}
}
