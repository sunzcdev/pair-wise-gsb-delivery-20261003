// Copyright 2020 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package btree

import (
	"reflect"
	"sync"
)

type BTreeG[T any] struct {
	isoid        uint64
	mu           *sync.RWMutex
	root         *node[T]
	count        int
	locks        bool
	copyItems    bool
	isoCopyItems bool
	less         func(a, b T) bool
	empty        T
	max          int
	min          int
}

type node[T any] struct {
	isoid    uint64
	count    int
	items    []T
	children *[]*node[T]
}

// PathHint is a utility type used with the *Hint() functions. Hints provide
// faster operations for clustered keys.
type PathHint struct {
	used [8]bool
	path [8]uint8
}

// Options for passing to New when creating a new BTree.
type Options struct {
	// Degree is used to define how many items and children each internal node
	// can contain before it must branch. For example, a degree of 2 will
	// create a 2-3-4 tree, where each node may contains 1-3 items and
	// 2-4 children. See https://en.wikipedia.org/wiki/2–3–4_tree.
	// Default is 32
	Degree int
	// NoLocks will disable locking. Otherwide a sync.RWMutex is used to
	// ensure all operations are safe across multiple goroutines.
	NoLocks bool
}

// New returns a new BTree
func NewBTreeG[T any](less func(a, b T) bool) *BTreeG[T] {
	return NewBTreeGOptions(less, Options{})
}

func NewBTreeGOptions[T any](less func(a, b T) bool, opts Options) *BTreeG[T] {
	tr := new(BTreeG[T])
	tr.isoid = newIsoID()
	tr.locks = !opts.NoLocks
	if tr.locks {
		tr.mu = new(sync.RWMutex)
	}
	tr.less = less
	tr.init(opts.Degree)
	return tr
}

func (tr *BTreeG[T]) init(degree int) {
	if tr.min != 0 {
		return
	}
	tr.min, tr.max = degreeToMinMax(degree)
	_, tr.copyItems = ((interface{})(tr.empty)).(copier[T])
	if !tr.copyItems {
		_, tr.isoCopyItems = ((interface{})(tr.empty)).(isoCopier[T])
	}
}

// Less is a convenience function that performs a comparison of two items
// using the same "less" function provided to New.
func (tr *BTreeG[T]) Less(a, b T) bool {
	return tr.less(a, b)
}

func (tr *BTreeG[T]) newNode(leaf bool) *node[T] {
	n := &node[T]{isoid: tr.isoid}
	if !leaf {
		n.children = new([]*node[T])
	}
	return n
}

// leaf returns true if the node is a leaf.
func (n *node[T]) leaf() bool {
	return n.children == nil
}

func (tr *BTreeG[T]) bsearch(n *node[T], key T) (index int, found bool) {
	low, high := 0, len(n.items)
	for low < high {
		h := int(uint(low+high) >> 1) // avoid overflow when computing h
		if !tr.less(key, n.items[h]) {
			low = h + 1
		} else {
			high = h
		}
	}
	if low > 0 && !tr.less(n.items[low-1], key) {
		return low - 1, true
	}
	return low, false
}

func (tr *BTreeG[T]) find(n *node[T], key T, hint *PathHint, depth int,
) (index int, found bool) {
	if hint == nil {
		return tr.bsearch(n, key)
	}
	return tr.hintsearch(n, key, hint, depth)
}

func (tr *BTreeG[T]) hintsearch(n *node[T], key T, hint *PathHint, depth int,
) (index int, found bool) {
	// Best case finds the exact match, updates the hint and returns.
	// Worst case, updates the low and high bounds to binary search between.
	low := 0
	high := len(n.items) - 1
	if depth < 8 && hint.used[depth] {
		index = int(hint.path[depth])
		if index >= len(n.items) {
			// tail item
			if tr.Less(n.items[len(n.items)-1], key) {
				index = len(n.items)
				goto path_match
			}
			index = len(n.items) - 1
		}
		if tr.Less(key, n.items[index]) {
			if index == 0 || tr.Less(n.items[index-1], key) {
				goto path_match
			}
			high = index - 1
		} else if tr.Less(n.items[index], key) {
			low = index + 1
		} else {
			found = true
			goto path_match
		}
	}

	// Do a binary search between low and high
	// keep on going until low > high, where the guarantee on low is that
	// key >= items[low - 1]
	for low <= high {
		mid := low + ((high+1)-low)/2
		// if key >= n.items[mid], low = mid + 1
		// which implies that key >= everything below low
		if !tr.Less(key, n.items[mid]) {
			low = mid + 1
		} else {
			high = mid - 1
		}
	}

	// if low > 0, n.items[low - 1] >= key,
	// we have from before that key >= n.items[low - 1]
	// therefore key = n.items[low - 1],
	// and we have found the entry for key.
	// Otherwise we must keep searching for the key in index `low`.
	if low > 0 && !tr.Less(n.items[low-1], key) {
		index = low - 1
		found = true
	} else {
		index = low
		found = false
	}

path_match:
	if depth < 8 {
		hint.used[depth] = true
		var pathIndex uint8
		if n.leaf() && found {
			pathIndex = uint8(index + 1)
		} else {
			pathIndex = uint8(index)
		}
		if pathIndex != hint.path[depth] {
			hint.path[depth] = pathIndex
			for i := depth + 1; i < 8; i++ {
				hint.used[i] = false
			}
		}
	}
	return index, found
}

// SetHint sets or replace a value for a key using a path hint
func (tr *BTreeG[T]) SetHint(item T, hint *PathHint) (prev T, replaced bool) {
	if tr.locks {
		tr.mu.Lock()
		prev, replaced = tr.setHint(item, hint)
		tr.mu.Unlock()
	} else {
		prev, replaced = tr.setHint(item, hint)
	}
	return prev, replaced
}

func (tr *BTreeG[T]) setHint(item T, hint *PathHint) (prev T, replaced bool) {
	if tr.root == nil {
		tr.init(0)
		tr.root = tr.newNode(true)
		tr.root.items = append([]T{}, item)
		tr.root.count = 1
		tr.count = 1
		return tr.empty, false
	}
	prev, replaced, split := tr.nodeSet(&tr.root, item, hint, 0)
	if split {
		left := tr.isoLoad(&tr.root, true)
		right, median := tr.nodeSplit(left)
		tr.root = tr.newNode(false)
		*tr.root.children = make([]*node[T], 0, tr.max+1)
		*tr.root.children = append([]*node[T]{}, left, right)
		tr.root.items = append([]T{}, median)
		tr.root.updateCount()
		return tr.setHint(item, hint)
	}
	if replaced {
		return prev, true
	}
	tr.count++
	return tr.empty, false
}

// Set or replace a value for a key
func (tr *BTreeG[T]) Set(item T) (T, bool) {
	return tr.SetHint(item, nil)
}

func (tr *BTreeG[T]) nodeSplit(n *node[T]) (right *node[T], median T) {
	i := tr.max / 2
	median = n.items[i]

	// right node
	right = tr.newNode(n.leaf())
	right.items = n.items[i+1:]
	if !n.leaf() {
		*right.children = (*n.children)[i+1:]
	}
	right.updateCount()

	// left node
	n.items[i] = tr.empty
	n.items = n.items[:i:i]
	if !n.leaf() {
		*n.children = (*n.children)[: i+1 : i+1]
	}
	n.updateCount()

	return right, median
}

func (n *node[T]) updateCount() {
	n.count = len(n.items)
	if !n.leaf() {
		for i := 0; i < len(*n.children); i++ {
			n.count += (*n.children)[i].count
		}
	}
}

// Copy the node for safe isolation.
func (tr *BTreeG[T]) copy(n *node[T]) *node[T] {
	n2 := new(node[T])
	n2.isoid = tr.isoid
	n2.count = n.count
	n2.items = make([]T, len(n.items), cap(n.items))
	copy(n2.items, n.items)
	if tr.copyItems {
		for i := 0; i < len(n2.items); i++ {
			n2.items[i] = ((interface{})(n2.items[i])).(copier[T]).Copy()
		}
	} else if tr.isoCopyItems {
		for i := 0; i < len(n2.items); i++ {
			n2.items[i] = ((interface{})(n2.items[i])).(isoCopier[T]).IsoCopy()
		}
	}
	if !n.leaf() {
		n2.children = new([]*node[T])
		*n2.children = make([]*node[T], len(*n.children), tr.max+1)
		copy(*n2.children, *n.children)
	}
	return n2
}

// isoLoad loads the provided node and, if needed, performs a copy-on-write.
func (tr *BTreeG[T]) isoLoad(cn **node[T], mut bool) *node[T] {
	if mut && (*cn).isoid != tr.isoid {
		*cn = tr.copy(*cn)
	}
	return *cn
}

func (tr *BTreeG[T]) nodeSet(cn **node[T], item T,
	hint *PathHint, depth int,
) (prev T, replaced bool, split bool) {
	if (*cn).isoid != tr.isoid {
		*cn = tr.copy(*cn)
	}
	n := *cn
	var i int
	var found bool
	if hint == nil {
		i, found = tr.bsearch(n, item)
	} else {
		i, found = tr.hintsearch(n, item, hint, depth)
	}
	if found {
		prev = n.items[i]
		n.items[i] = item
		return prev, true, false
	}
	if n.leaf() {
		if len(n.items) == tr.max {
			return tr.empty, false, true
		}
		n.items = append(n.items, tr.empty)
		copy(n.items[i+1:], n.items[i:])
		n.items[i] = item
		n.count++
		return tr.empty, false, false
	}
	prev, replaced, split = tr.nodeSet(&(*n.children)[i], item, hint, depth+1)
	if split {
		if len(n.items) == tr.max {
			return tr.empty, false, true
		}
		right, median := tr.nodeSplit((*n.children)[i])
		*n.children = append(*n.children, nil)
		copy((*n.children)[i+1:], (*n.children)[i:])
		(*n.children)[i+1] = right
		n.items = append(n.items, tr.empty)
		copy(n.items[i+1:], n.items[i:])
		n.items[i] = median
		return tr.nodeSet(&n, item, hint, depth)
	}
	if !replaced {
		n.count++
	}
	return prev, replaced, false
}

func (tr *BTreeG[T]) Scan(iter func(item T) bool) {
	tr.scan(iter, false)
}
func (tr *BTreeG[T]) ScanMut(iter func(item T) bool) {
	tr.scan(iter, true)
}

func (tr *BTreeG[T]) scan(iter func(item T) bool, mut bool) {
	if tr.lock(mut) {
		defer tr.unlock(mut)
	}
	if tr.root == nil {
		return
	}
	tr.nodeScan(&tr.root, iter, mut)
}

func (tr *BTreeG[T]) nodeScan(cn **node[T], iter func(item T) bool, mut bool,
) bool {
	n := tr.isoLoad(cn, mut)
	if n.leaf() {
		for i := 0; i < len(n.items); i++ {
			if !iter(n.items[i]) {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(n.items); i++ {
		if !tr.nodeScan(&(*n.children)[i], iter, mut) {
			return false
		}
		if !iter(n.items[i]) {
			return false
		}
	}
	return tr.nodeScan(&(*n.children)[len(*n.children)-1], iter, mut)
}

// Get a value for key
func (tr *BTreeG[T]) Get(key T) (T, bool) {
	return tr.getHint(key, nil, false)
}

func (tr *BTreeG[T]) GetMut(key T) (T, bool) {
	return tr.getHint(key, nil, true)
}

// GetHint gets a value for key using a path hint
func (tr *BTreeG[T]) GetHint(key T, hint *PathHint) (value T, ok bool) {
	return tr.getHint(key, hint, false)
}
func (tr *BTreeG[T]) GetHintMut(key T, hint *PathHint) (value T, ok bool) {
	return tr.getHint(key, hint, true)
}

// GetHint gets a value for key using a path hint
func (tr *BTreeG[T]) getHint(key T, hint *PathHint, mut bool) (T, bool) {
	if tr.lock(mut) {
		defer tr.unlock(mut)
	}
	if tr.root == nil {
		return tr.empty, false
	}
	n := tr.isoLoad(&tr.root, mut)
	depth := 0
	for {
		i, found := tr.find(n, key, hint, depth)
		if found {
			return n.items[i], true
		}
		if n.children == nil {
			return tr.empty, false
		}
		n = tr.isoLoad(&(*n.children)[i], mut)
		depth++
	}
}

// Action for DeleteAscend
type Action int

const (
	Stop Action = iota
	Keep
	Delete
)

// DeleteAscend ascends over tree within the range [pivot, last].
// Return Delete to delete the item.
// Return Keep to keep the item, avoiding deletion.
// Return Stop to stop iterating
func (tr *BTreeG[T]) DeleteAscend(pivot T, iter func(item T) Action) {
	if tr.lock(true) {
		defer tr.unlock(true)
	}
	var hint PathHint
	type stackItem struct {
		node  *node[T]
		index int
	}
	var stack []stackItem
restart:
	if tr.root == nil {
		return
	}
	n := tr.isoLoad(&tr.root, true)
	stack = append(stack, stackItem{n, 0})
	for {
		i, found := tr.find(n, pivot, &hint, len(stack)-1)
	next:
		if n.children != nil {
			if found {
				act := iter(n.items[i])
				switch act {
				case Delete:
					pivot = n.items[i]
					tr.deleteHint(n.items[i], &hint)
					stack = stack[:0]
					goto restart
				case Keep:
					n = tr.isoLoad(&(*n.children)[i+1], true)
					stack = append(stack, stackItem{n, i + 1})
					i = 0
					for !n.leaf() {
						n = tr.isoLoad(&(*n.children)[i], true)
						stack = append(stack, stackItem{n, i})
					}
				default:
					// user stop requested
					return
				}
			} else {
				// at branch, continue to leaf
				n = tr.isoLoad(&(*n.children)[i], true)
				stack = append(stack, stackItem{n, i})
				continue
			}
		}
		// at leaf
		maxbulk := len(n.items) - i - tr.min
		if maxbulk > 1 {
			var act Action
			j := 0
			for ; j < maxbulk; j++ {
				act = iter(n.items[i+j])
				if act != Delete {
					break
				}
			}
			copy(n.items[i:], n.items[i+j:])
			for k := len(n.items) - j; k < len(n.items); k++ {
				n.items[k] = tr.empty
			}
			n.items = n.items[:len(n.items)-j]
			for k := 0; k < len(stack); k++ {
				stack[k].node.count -= j
			}
			tr.count -= j
			if act == Stop {
				return
			}
			if act == Keep {
				i++
			}
		}
		for ; i < len(n.items); i++ {
			act := iter(n.items[i])
			switch act {
			case Delete:
				if len(n.items) > tr.min {
					copy(n.items[i:], n.items[i+1:])
					n.items[len(n.items)-1] = tr.empty
					n.items = n.items[:len(n.items)-1]
					for j := 0; j < len(stack); j++ {
						stack[j].node.count--
					}
					tr.count--
					i--
				} else {
					pivot = n.items[i]
					tr.deleteHint(n.items[i], &hint)
					stack = stack[:0]
					goto restart
				}
			case Keep:
			default:
				// user stop requested
				return
			}
		}
		// end of leaf items. traverse upwards
		for {
			i = stack[len(stack)-1].index
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				// end of tree
				return
			}
			n = stack[len(stack)-1].node
			if i < len(n.items) {
				found = true
				goto next
			}
		}
	}
}

type eitem[T any] struct {
	item T
	node *node[T]
}

// List of deleted items from the DeleteRange function.
type List[T any] struct {
	less  func(a, b T) bool // comparator
	count int               // total number of items
	np    int               // number of pop eitems
	q     []eitem[T]        // priority queue or reversed/popped array
}

func (e *List[T]) push(eitem eitem[T]) {
	e.q = append(e.q, eitem)
	i := len(e.q) - 1
	parent := (i - 1) / 2
	for i != 0 && e.less(e.q[i].item, e.q[parent].item) {
		e.q[parent], e.q[i] = e.q[i], e.q[parent]
		i = parent
		parent = (i - 1) / 2
	}
}

func (e *List[T]) pop() (eitem[T], bool) {
	if len(e.q) == 0 {
		return eitem[T]{}, false
	}
	var n eitem[T]
	n, e.q[0] = e.q[0], e.q[len(e.q)-1]
	e.q = e.q[:len(e.q)-1]
	i := 0
	for {
		smallest := i
		left := i*2 + 1
		right := i*2 + 2
		if left < len(e.q) && !e.less(e.q[smallest].item, e.q[left].item) {
			smallest = left
		}
		if right < len(e.q) && !e.less(e.q[smallest].item, e.q[right].item) {
			smallest = right
		}
		if smallest == i {
			break
		}
		e.q[smallest], e.q[i] = e.q[i], e.q[smallest]
		i = smallest
	}
	return n, true
}

func (e *List[T]) append(item T, n *node[T]) {
	e.count++
	if n != nil {
		e.count += n.count
	}
	e.push(eitem[T]{item, n})
}

func extractNodeScan[T any](n *node[T], iter func(item T) bool) bool {
	if n.leaf() {
		for i := 0; i < len(n.items); i++ {
			if !iter(n.items[i]) {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(n.items); i++ {
		if !extractNodeScan((*n.children)[i], iter) {
			return false
		}
		if !iter(n.items[i]) {
			return false
		}
	}
	return extractNodeScan((*n.children)[len(*n.children)-1], iter)
}

// Len returns the number of items in the list
func (e *List[T]) Len() int {
	return e.count
}

// Scan performs an ordered scan of all items in the list.
func (e *List[T]) Scan(iter func(item T) bool) {
	pitems := e.q[:cap(e.q)]
	for i := 0; i < e.np; i++ {
		eitem := pitems[cap(pitems)-i-1]
		if !iter(eitem.item) {
			return
		}
		if eitem.node != nil {
			if !extractNodeScan(eitem.node, iter) {
				return
			}
		}
	}
	for {
		// pop the next item
		eitem, ok := e.pop()
		if !ok {
			return
		}
		// Place item in reverse order for the extract can be scanned again.
		e.q[:cap(e.q)][cap(e.q)-e.np-1] = eitem
		e.np++
		if !iter(eitem.item) {
			return
		}
		if eitem.node != nil {
			if !extractNodeScan(eitem.node, iter) {
				return
			}
		}
	}
}

func (e *List[T]) Clear() {
	e.less = nil
	e.q = e.q[:0]
	e.count = 0
	e.np = 0
}

// Options for passing to the DeleteRange function
type DeleteRangeOptions struct {
	// Do not return the deleted items.
	// Default false
	NoReturn bool
	// Make the max inclusive.
	// Default false
	MaxInclusive bool
}

// DeleteRange will delete all items within the provided min (inclusive) and
// max (exclusive) sub-range.
// Returns the deleted items as an ordered list that can be iterated over using
// the list.Scan() method.
func (tr *BTreeG[T]) DeleteRange(min, max T, opts *DeleteRangeOptions) (deleted List[T]) {
	return tr.DeleteRangeReuse(min, max, opts, &deleted)
}

// DeleteRangeReuse is the same as DeleteRange, but it takes a List as an argument to
// avoid allocating/growing a new List on each call to DeleteRange. It is unsafe to use
// the same List across concurrent calls to DeleteRange.
//
// The deleted paramter needs to be a pointer as the go runtime can allocate a new backing memory on
// append to a slice, and that change needs to be reflected in the caller.
func (tr *BTreeG[T]) DeleteRangeReuse(min, max T, opts *DeleteRangeOptions, deleted *List[T]) List[T] {
	if tr.lock(true) {
		defer tr.unlock(true)
	}
	extract := opts == nil || !opts.NoReturn
	maxincl := opts != nil && opts.MaxInclusive

	if deleted == nil {
		deleted = &List[T]{}
	}

	// Clear just in case it hasn't been cleared yet.
	deleted.Clear()

	maxstop := func(item T) bool {
		if maxincl {
			return tr.less(max, item)
		}
		return !tr.less(item, max)
	}
	if extract {
		deleted.less = tr.less
	}
	var hint PathHint
	type stackItem struct {
		node  *node[T]
		index int
	}
	pivot := min
	var stack []stackItem
restart:
	if tr.root == nil {
		return *deleted
	}
	n := tr.isoLoad(&tr.root, true)
	stack = append(stack, stackItem{n, 0})
	for {
		i, found := tr.find(n, pivot, &hint, len(stack)-1)
	next:
		if n.children != nil {
			// Can the item and child to the right be completely deleted?
			if i < len(n.items)-1 && (len(n.items) > tr.min ||
				(len(n.items) > 1 && len(stack) == 1)) &&
				tr.less(n.items[i+1], max) {
				// Yes, delete both without traversing
				ditem := n.items[i]
				copy(n.items[i:], n.items[i+1:])
				n.items[len(n.items)-1] = tr.empty
				n.items = n.items[:len(n.items)-1]
				dnode := (*n.children)[i+1]
				copy((*n.children)[i+1:], (*n.children)[i+2:])
				(*n.children)[len(*n.children)-1] = nil
				*n.children = (*n.children)[:len(*n.children)-1]
				for k := 0; k < len(stack); k++ {
					stack[k].node.count -= dnode.count + 1
				}
				tr.count -= dnode.count + 1
				if extract {
					deleted.append(ditem, dnode)
				}
				if found {
					goto next
				}
				continue
			}
			if found {
				if maxstop(n.items[i]) {
					// stop
					return *deleted
				}
				pivot = n.items[i]
				if extract {
					deleted.append(n.items[i], nil)
				}
				tr.deleteHint(n.items[i], &hint)
				stack = stack[:0]
				goto restart
			}
			// at branch, continue to leaf
			n = tr.isoLoad(&(*n.children)[i], true)
			stack = append(stack, stackItem{n, i})
			continue
		}
		// at leaf
		maxbulk := len(n.items) - i - tr.min
		if maxbulk > 1 {
			var stop bool
			j := 0
			for ; j < maxbulk; j++ {
				stop := maxstop(n.items[i+j])
				if stop {
					break
				}
				if extract {
					deleted.append(n.items[i+j], nil)
				}
			}
			copy(n.items[i:], n.items[i+j:])
			for k := len(n.items) - j; k < len(n.items); k++ {
				n.items[k] = tr.empty
			}
			n.items = n.items[:len(n.items)-j]
			for k := 0; k < len(stack); k++ {
				stack[k].node.count -= j
			}
			tr.count -= j
			if stop {
				return *deleted
			}
		}
		for ; i < len(n.items); i++ {
			if maxstop(n.items[i]) {
				// stop
				return *deleted
			}
			if len(n.items) > tr.min {
				if extract {
					deleted.append(n.items[i], nil)
				}
				copy(n.items[i:], n.items[i+1:])
				n.items[len(n.items)-1] = tr.empty
				n.items = n.items[:len(n.items)-1]
				for j := 0; j < len(stack); j++ {
					stack[j].node.count--
				}
				tr.count--
				i--
			} else {
				pivot = n.items[i]
				if extract {
					deleted.append(n.items[i], nil)
				}
				tr.deleteHint(n.items[i], &hint)
				stack = stack[:0]
				goto restart
			}
		}
		// end of leaf items. traverse upwards
		for {
			i = stack[len(stack)-1].index
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				// end of tree
				return *deleted
			}
			n = stack[len(stack)-1].node
			if i < len(n.items) {
				found = true
				goto next
			}
		}
	}
}

// Len returns the number of items in the tree
func (tr *BTreeG[T]) Len() int {
	return tr.count
}

// Delete a value for a key and returns the deleted value.
// Returns false if there was no value by that key found.
func (tr *BTreeG[T]) Delete(key T) (T, bool) {
	return tr.DeleteHint(key, nil)
}

// DeleteHint deletes a value for a key using a path hint and returns the
// deleted value.
// Returns false if there was no value by that key found.
func (tr *BTreeG[T]) DeleteHint(key T, hint *PathHint) (T, bool) {
	if tr.lock(true) {
		defer tr.unlock(true)
	}
	return tr.deleteHint(key, hint)
}

func (tr *BTreeG[T]) deleteHint(key T, hint *PathHint) (T, bool) {
	if tr.root == nil {
		return tr.empty, false
	}
	prev, deleted := tr.delete(&tr.root, false, key, hint, 0)
	if !deleted {
		return tr.empty, false
	}
	if len(tr.root.items) == 0 && !tr.root.leaf() {
		tr.root = (*tr.root.children)[0]
	}
	tr.count--
	if tr.count == 0 {
		tr.root = nil
	}
	return prev, true
}

func (tr *BTreeG[T]) delete(cn **node[T], max bool, key T,
	hint *PathHint, depth int,
) (T, bool) {
	n := tr.isoLoad(cn, true)
	var i int
	var found bool
	if max {
		i, found = len(n.items)-1, true
	} else {
		i, found = tr.find(n, key, hint, depth)
	}
	if n.leaf() {
		if found {
			// found the items at the leaf, remove it and return.
			prev := n.items[i]
			copy(n.items[i:], n.items[i+1:])
			n.items[len(n.items)-1] = tr.empty
			n.items = n.items[:len(n.items)-1]
			n.count--
			return prev, true
		}
		return tr.empty, false
	}

	var prev T
	var deleted bool
	if found {
		if max {
			i++
			prev, deleted = tr.delete(&(*n.children)[i], true, tr.empty, nil, 0)
		} else {
			prev = n.items[i]
			maxItem, _ := tr.delete(&(*n.children)[i], true, tr.empty, nil, 0)
			deleted = true
			n.items[i] = maxItem
		}
	} else {
		prev, deleted = tr.delete(&(*n.children)[i], max, key, hint, depth+1)
	}
	if !deleted {
		return tr.empty, false
	}
	n.count--
	if len((*n.children)[i].items) < tr.min {
		tr.nodeRebalance(n, i)
	}
	return prev, true
}

// nodeRebalance rebalances the child nodes following a delete operation.
// Provide the index of the child node with the number of items that fell
// below minItems.
func (tr *BTreeG[T]) nodeRebalance(n *node[T], i int) {
	if i == len(n.items) {
		i--
	}

	// ensure copy-on-write
	left := tr.isoLoad(&(*n.children)[i], true)
	right := tr.isoLoad(&(*n.children)[i+1], true)

	if len(left.items)+len(right.items) < tr.max {
		// Merges the left and right children nodes together as a single node
		// that includes (left,item,right), and places the contents into the
		// existing left node. Delete the right node altogether and move the
		// following items and child nodes to the left by one slot.

		// merge (left,item,right)
		left.items = append(left.items, n.items[i])
		left.items = append(left.items, right.items...)
		if !left.leaf() {
			*left.children = append(*left.children, *right.children...)
		}
		left.count += right.count + 1

		// move the items over one slot
		copy(n.items[i:], n.items[i+1:])
		n.items[len(n.items)-1] = tr.empty
		n.items = n.items[:len(n.items)-1]

		// move the children over one slot
		copy((*n.children)[i+1:], (*n.children)[i+2:])
		(*n.children)[len(*n.children)-1] = nil
		(*n.children) = (*n.children)[:len(*n.children)-1]
	} else if len(left.items) > len(right.items) {
		// move left -> right over one slot

		// Move the item of the parent node at index into the right-node first
		// slot, and move the left-node last item into the previously moved
		// parent item slot.
		right.items = append(right.items, tr.empty)
		copy(right.items[1:], right.items)
		right.items[0] = n.items[i]
		right.count++
		n.items[i] = left.items[len(left.items)-1]
		left.items[len(left.items)-1] = tr.empty
		left.items = left.items[:len(left.items)-1]
		left.count--

		if !left.leaf() {
			// move the left-node last child into the right-node first slot
			*right.children = append(*right.children, nil)
			copy((*right.children)[1:], *right.children)
			(*right.children)[0] = (*left.children)[len(*left.children)-1]
			(*left.children)[len(*left.children)-1] = nil
			(*left.children) = (*left.children)[:len(*left.children)-1]
			left.count -= (*right.children)[0].count
			right.count += (*right.children)[0].count
		}
	} else {
		// move left <- right over one slot

		// Same as above but the other direction
		left.items = append(left.items, n.items[i])
		left.count++
		n.items[i] = right.items[0]
		copy(right.items, right.items[1:])
		right.items[len(right.items)-1] = tr.empty
		right.items = right.items[:len(right.items)-1]
		right.count--

		if !left.leaf() {
			*left.children = append(*left.children, (*right.children)[0])
			copy(*right.children, (*right.children)[1:])
			(*right.children)[len(*right.children)-1] = nil
			*right.children = (*right.children)[:len(*right.children)-1]
			left.count += (*left.children)[len(*left.children)-1].count
			right.count -= (*left.children)[len(*left.children)-1].count
		}
	}
}

// Ascend the tree within the range [pivot, last]
// Pass nil for pivot to scan all item in ascending order
// Return false to stop iterating
func (tr *BTreeG[T]) Ascend(pivot T, iter func(item T) bool) {
	tr.ascend(pivot, iter, false, nil)
}
func (tr *BTreeG[T]) AscendMut(pivot T, iter func(item T) bool) {
	tr.ascend(pivot, iter, true, nil)
}
func (tr *BTreeG[T]) ascend(pivot T, iter func(item T) bool, mut bool,
	hint *PathHint,
) {
	if tr.lock(mut) {
		defer tr.unlock(mut)
	}
	if tr.root == nil {
		return
	}
	tr.nodeAscend(&tr.root, pivot, hint, 0, iter, mut)
}
func (tr *BTreeG[T]) AscendHint(pivot T, iter func(item T) bool, hint *PathHint,
) {
	tr.ascend(pivot, iter, false, hint)
}
func (tr *BTreeG[T]) AscendHintMut(pivot T, iter func(item T) bool,
	hint *PathHint,
) {
	tr.ascend(pivot, iter, true, hint)
}

// The return value of this function determines whether we should keep iterating
// upon this functions return.
func (tr *BTreeG[T]) nodeAscend(cn **node[T], pivot T, hint *PathHint,
	depth int, iter func(item T) bool, mut bool,
) bool {
	n := tr.isoLoad(cn, mut)
	i, found := tr.find(n, pivot, hint, depth)
	if !found {
		if !n.leaf() {
			if !tr.nodeAscend(&(*n.children)[i], pivot, hint, depth+1, iter,
				mut) {
				return false
			}
		}
	}
	// We are either in the case that
	// - node is found, we should iterate through it starting at `i`,
	//   the index it was located at.
	// - node is not found, and TODO: fill in.
	for ; i < len(n.items); i++ {
		if !iter(n.items[i]) {
			return false
		}
		if !n.leaf() {
			if !tr.nodeScan(&(*n.children)[i+1], iter, mut) {
				return false
			}
		}
	}
	return true
}

func (tr *BTreeG[T]) Reverse(iter func(item T) bool) {
	tr.reverse(iter, false)
}
func (tr *BTreeG[T]) ReverseMut(iter func(item T) bool) {
	tr.reverse(iter, true)
}
func (tr *BTreeG[T]) reverse(iter func(item T) bool, mut bool) {
	if tr.lock(mut) {
		defer tr.unlock(mut)
	}
	if tr.root == nil {
		return
	}
	tr.nodeReverse(&tr.root, iter, mut)
}

func (tr *BTreeG[T]) nodeReverse(cn **node[T], iter func(item T) bool, mut bool,
) bool {
	n := tr.isoLoad(cn, mut)
	if n.leaf() {
		for i := len(n.items) - 1; i >= 0; i-- {
			if !iter(n.items[i]) {
				return false
			}
		}
		return true
	}
	if !tr.nodeReverse(&(*n.children)[len(*n.children)-1], iter, mut) {
		return false
	}
	for i := len(n.items) - 1; i >= 0; i-- {
		if !iter(n.items[i]) {
			return false
		}
		if !tr.nodeReverse(&(*n.children)[i], iter, mut) {
			return false
		}
	}
	return true
}

// Descend the tree within the range [pivot, first]
// Pass nil for pivot to scan all item in descending order
// Return false to stop iterating
func (tr *BTreeG[T]) Descend(pivot T, iter func(item T) bool) {
	tr.descend(pivot, iter, false, nil)
}
func (tr *BTreeG[T]) DescendMut(pivot T, iter func(item T) bool) {
	tr.descend(pivot, iter, true, nil)
}
func (tr *BTreeG[T]) descend(pivot T, iter func(item T) bool, mut bool,
	hint *PathHint,
) {
	if tr.lock(mut) {
		defer tr.unlock(mut)
	}
	if tr.root == nil {
		return
	}
	tr.nodeDescend(&tr.root, pivot, hint, 0, iter, mut)
}

func (tr *BTreeG[T]) DescendHint(pivot T, iter func(item T) bool,
	hint *PathHint,
) {
	tr.descend(pivot, iter, false, hint)
}
func (tr *BTreeG[T]) DescendHintMut(pivot T, iter func(item T) bool,
	hint *PathHint,
) {
	tr.descend(pivot, iter, true, hint)
}

func (tr *BTreeG[T]) nodeDescend(cn **node[T], pivot T, hint *PathHint,
	depth int, iter func(item T) bool, mut bool,
) bool {
	n := tr.isoLoad(cn, mut)
	i, found := tr.find(n, pivot, hint, depth)
	if !found {
		if !n.leaf() {
			if !tr.nodeDescend(&(*n.children)[i], pivot, hint, depth+1, iter,
				mut) {
				return false
			}
		}
		i--
	}
	for ; i >= 0; i-- {
		if !iter(n.items[i]) {
			return false
		}
		if !n.leaf() {
			if !tr.nodeReverse(&(*n.children)[i], iter, mut) {
				return false
			}
		}
	}
	return true
}

// Load is for bulk loading pre-sorted items
func (tr *BTreeG[T]) Load(item T) (T, bool) {
	if tr.lock(true) {
		defer tr.unlock(true)
	}
	if tr.root == nil {
		return tr.setHint(item, nil)
	}
	n := tr.isoLoad(&tr.root, true)
	for {
		n.count++ // optimistically update counts
		if n.leaf() {
			if len(n.items) < tr.max {
				if tr.Less(n.items[len(n.items)-1], item) {
					n.items = append(n.items, item)
					tr.count++
					return tr.empty, false
				}
			}
			break
		}
		n = tr.isoLoad(&(*n.children)[len(*n.children)-1], true)
	}
	// revert the counts
	n = tr.root
	for {
		n.count--
		if n.leaf() {
			break
		}
		n = (*n.children)[len(*n.children)-1]
	}
	return tr.setHint(item, nil)
}

// Min returns the minimum item in tree.
// Returns nil if the treex has no items.
func (tr *BTreeG[T]) Min() (T, bool) {
	return tr.minMut(false)
}

func (tr *BTreeG[T]) MinMut() (T, bool) {
	return tr.minMut(true)
}

func (tr *BTreeG[T]) minMut(mut bool) (T, bool) {
	if tr.lock(mut) {
		defer tr.unlock(mut)
	}
	if tr.root == nil {
		return tr.empty, false
	}
	n := tr.isoLoad(&tr.root, mut)
	for {
		if n.leaf() {
			return n.items[0], true
		}
		n = tr.isoLoad(&(*n.children)[0], mut)
	}
}

// Max returns the maximum item in tree.
// Returns nil if the tree has no items.
func (tr *BTreeG[T]) Max() (T, bool) {
	return tr.maxMut(false)
}

func (tr *BTreeG[T]) MaxMut() (T, bool) {
	return tr.maxMut(true)
}

func (tr *BTreeG[T]) maxMut(mut bool) (T, bool) {
	if tr.lock(mut) {
		defer tr.unlock(mut)
	}
	if tr.root == nil {
		return tr.empty, false
	}
	n := tr.isoLoad(&tr.root, mut)
	for {
		if n.leaf() {
			return n.items[len(n.items)-1], true
		}
		n = tr.isoLoad(&(*n.children)[len(*n.children)-1], mut)
	}
}

// PopMin removes the minimum item in tree and returns it.
// Returns nil if the tree has no items.
func (tr *BTreeG[T]) PopMin() (T, bool) {
	if tr.lock(true) {
		defer tr.unlock(true)
	}
	if tr.root == nil {
		return tr.empty, false
	}
	n := tr.isoLoad(&tr.root, true)
	var item T
	for {
		n.count-- // optimistically update counts
		if n.leaf() {
			item = n.items[0]
			if len(n.items) == tr.min {
				break
			}
			copy(n.items[:], n.items[1:])
			n.items[len(n.items)-1] = tr.empty
			n.items = n.items[:len(n.items)-1]
			tr.count--
			if tr.count == 0 {
				tr.root = nil
			}
			return item, true
		}
		n = tr.isoLoad(&(*n.children)[0], true)
	}
	// revert the counts
	n = tr.root
	for {
		n.count++
		if n.leaf() {
			break
		}
		n = (*n.children)[0]
	}
	return tr.deleteHint(item, nil)
}

// PopMax removes the maximum item in tree and returns it.
// Returns nil if the tree has no items.
func (tr *BTreeG[T]) PopMax() (T, bool) {
	if tr.lock(true) {
		defer tr.unlock(true)
	}
	if tr.root == nil {
		return tr.empty, false
	}
	n := tr.isoLoad(&tr.root, true)
	var item T
	for {
		n.count-- // optimistically update counts
		if n.leaf() {
			item = n.items[len(n.items)-1]
			if len(n.items) == tr.min {
				break
			}
			n.items[len(n.items)-1] = tr.empty
			n.items = n.items[:len(n.items)-1]
			tr.count--
			if tr.count == 0 {
				tr.root = nil
			}
			return item, true
		}
		n = tr.isoLoad(&(*n.children)[len(*n.children)-1], true)
	}
	// revert the counts
	n = tr.root
	for {
		n.count++
		if n.leaf() {
			break
		}
		n = (*n.children)[len(*n.children)-1]
	}
	return tr.deleteHint(item, nil)
}

// GetAt returns the value at index.
// Return nil if the tree is empty or the index is out of bounds.
func (tr *BTreeG[T]) GetAt(index int) (T, bool) {
	return tr.getAt(index, false)
}
func (tr *BTreeG[T]) GetAtMut(index int) (T, bool) {
	return tr.getAt(index, true)
}
func (tr *BTreeG[T]) getAt(index int, mut bool) (T, bool) {
	if tr.lock(mut) {
		defer tr.unlock(mut)
	}
	if tr.root == nil || index < 0 || index >= tr.count {
		return tr.empty, false
	}
	n := tr.isoLoad(&tr.root, mut)
	for {
		if n.leaf() {
			return n.items[index], true
		}
		i := 0
		for ; i < len(n.items); i++ {
			if index < (*n.children)[i].count {
				break
			} else if index == (*n.children)[i].count {
				return n.items[i], true
			}
			index -= (*n.children)[i].count + 1
		}
		n = tr.isoLoad(&(*n.children)[i], mut)
	}
}

// DeleteAt deletes the item at index.
// Return nil if the tree is empty or the index is out of bounds.
func (tr *BTreeG[T]) DeleteAt(index int) (T, bool) {
	if tr.lock(true) {
		defer tr.unlock(true)
	}
	if tr.root == nil || index < 0 || index >= tr.count {
		return tr.empty, false
	}
	var pathbuf [8]uint8 // track the path
	path := pathbuf[:0]
	var item T
	n := tr.isoLoad(&tr.root, true)
outer:
	for {
		n.count-- // optimistically update counts
		if n.leaf() {
			// the index is the item position
			item = n.items[index]
			if len(n.items) == tr.min {
				path = append(path, uint8(index))
				break outer
			}
			copy(n.items[index:], n.items[index+1:])
			n.items[len(n.items)-1] = tr.empty
			n.items = n.items[:len(n.items)-1]
			tr.count--
			if tr.count == 0 {
				tr.root = nil
			}
			return item, true
		}
		i := 0
		for ; i < len(n.items); i++ {
			if index < (*n.children)[i].count {
				break
			} else if index == (*n.children)[i].count {
				item = n.items[i]
				path = append(path, uint8(i))
				break outer
			}
			index -= (*n.children)[i].count + 1
		}
		path = append(path, uint8(i))
		n = tr.isoLoad(&(*n.children)[i], true)
	}
	// revert the counts
	var hint PathHint
	n = tr.root
	for i := 0; i < len(path); i++ {
		if i < len(hint.path) {
			hint.path[i] = uint8(path[i])
			hint.used[i] = true
		}
		n.count++
		if !n.leaf() {
			n = (*n.children)[uint8(path[i])]
		}
	}
	return tr.deleteHint(item, &hint)
}

// Height returns the height of the tree.
// Returns zero if tree has no items.
func (tr *BTreeG[T]) Height() int {
	if tr.lock(false) {
		defer tr.unlock(false)
	}
	var height int
	if tr.root != nil {
		n := tr.root
		for {
			height++
			if n.leaf() {
				break
			}
			n = (*n.children)[0]
		}
	}
	return height
}

// Walk iterates over all items in tree, in order.
// The items param will contain one or more items.
func (tr *BTreeG[T]) Walk(iter func(item []T) bool) {
	tr.walk(iter, false)
}
func (tr *BTreeG[T]) WalkMut(iter func(item []T) bool) {
	tr.walk(iter, true)
}
func (tr *BTreeG[T]) walk(iter func(item []T) bool, mut bool) {
	if tr.lock(mut) {
		defer tr.unlock(mut)
	}
	if tr.root == nil {
		return
	}
	tr.nodeWalk(&tr.root, iter, mut)
}

func (tr *BTreeG[T]) nodeWalk(cn **node[T], iter func(item []T) bool, mut bool,
) bool {
	n := tr.isoLoad(cn, mut)
	if n.leaf() {
		if !iter(n.items) {
			return false
		}
	} else {
		for i := 0; i < len(n.items); i++ {
			if !tr.nodeWalk(&(*n.children)[i], iter, mut) {
				return false
			}
			if !iter(n.items[i : i+1]) {
				return false
			}
		}
		if !tr.nodeWalk(&(*n.children)[len(n.items)], iter, mut) {
			return false
		}
	}
	return true
}

// Copy the tree. This is a copy-on-write operation and is very fast because
// it only performs a shadowed copy.
func (tr *BTreeG[T]) Copy() *BTreeG[T] {
	return tr.IsoCopy()
}

func (tr *BTreeG[T]) IsoCopy() *BTreeG[T] {
	var mu *sync.RWMutex
	if tr.lock(true) {
		mu = new(sync.RWMutex)
		defer tr.unlock(true)
	}
	tr.isoid = newIsoID()
	tr2 := new(BTreeG[T])
	*tr2 = *tr
	tr2.mu = mu
	tr2.isoid = newIsoID()
	return tr2
}

func (tr *BTreeG[T]) lock(write bool) bool {
	if tr.locks {
		if write {
			tr.mu.Lock()
		} else {
			tr.mu.RLock()
		}
	}
	return tr.locks
}

func (tr *BTreeG[T]) unlock(write bool) {
	if write {
		tr.mu.Unlock()
	} else {
		tr.mu.RUnlock()
	}
}

// Iter represents an iterator
type IterG[T any] struct {
	tr      *BTreeG[T]
	mut     bool
	locked  bool
	seeked  bool
	atstart bool
	atend   bool
	stack0  [4]iterStackItemG[T]
	stack   []iterStackItemG[T]
	item    T
}

type iterStackItemG[T any] struct {
	n *node[T]
	i int
}

// Iter returns a read-only iterator.
// The Release method must be called finished with iterator.
func (tr *BTreeG[T]) Iter() IterG[T] {
	return tr.iter(false)
}

func (tr *BTreeG[T]) IterMut() IterG[T] {
	return tr.iter(true)
}

func (tr *BTreeG[T]) iter(mut bool) IterG[T] {
	var iter IterG[T]
	iter.tr = tr
	iter.mut = mut
	iter.locked = tr.lock(iter.mut)
	iter.stack = iter.stack0[:0]
	return iter
}

// Seek to item greater-or-equal-to key.
// Returns false if there was no item found.
func (iter *IterG[T]) Seek(key T) bool {
	return iter.seek(key, nil)
}

func (iter *IterG[T]) SeekHint(key T, hint *PathHint) bool {
	return iter.seek(key, hint)
}

func (iter *IterG[T]) seek(key T, hint *PathHint) bool {
	if iter.tr == nil {
		return false
	}
	iter.seeked = true
	iter.stack = iter.stack[:0]
	if iter.tr.root == nil {
		return false
	}
	n := iter.tr.isoLoad(&iter.tr.root, iter.mut)
	var depth int
	for {
		i, found := iter.tr.find(n, key, hint, depth)
		iter.stack = append(iter.stack, iterStackItemG[T]{n, i})
		if found {
			iter.item = n.items[i]
			return true
		}
		if n.leaf() {
			iter.stack[len(iter.stack)-1].i--
			return iter.Next()
		}
		n = iter.tr.isoLoad(&(*n.children)[i], iter.mut)
		depth++
	}
}

// First moves iterator to first item in tree.
// Returns false if the tree is empty.
func (iter *IterG[T]) First() bool {
	if iter.tr == nil {
		return false
	}
	iter.atend = false
	iter.atstart = false
	iter.seeked = true
	iter.stack = iter.stack[:0]
	if iter.tr.root == nil {
		return false
	}
	n := iter.tr.isoLoad(&iter.tr.root, iter.mut)
	for {
		iter.stack = append(iter.stack, iterStackItemG[T]{n, 0})
		if n.leaf() {
			break
		}
		n = iter.tr.isoLoad(&(*n.children)[0], iter.mut)
	}
	s := &iter.stack[len(iter.stack)-1]
	iter.item = s.n.items[s.i]
	return true
}

// Last moves iterator to last item in tree.
// Returns false if the tree is empty.
func (iter *IterG[T]) Last() bool {
	if iter.tr == nil {
		return false
	}
	iter.seeked = true
	iter.stack = iter.stack[:0]
	if iter.tr.root == nil {
		return false
	}
	n := iter.tr.isoLoad(&iter.tr.root, iter.mut)
	for {
		iter.stack = append(iter.stack, iterStackItemG[T]{n, len(n.items)})
		if n.leaf() {
			iter.stack[len(iter.stack)-1].i--
			break
		}
		n = iter.tr.isoLoad(&(*n.children)[len(n.items)], iter.mut)
	}
	s := &iter.stack[len(iter.stack)-1]
	iter.item = s.n.items[s.i]
	return true
}

// Release the iterator.
func (iter *IterG[T]) Release() {
	if iter.tr == nil {
		return
	}
	if iter.locked {
		iter.tr.unlock(iter.mut)
		iter.locked = false
	}

	// Preserve the backing memory for the stack, so that the iterator can be re-used without
	// allocating.
	stack := iter.stack[:0]
	*iter = IterG[T]{}
	iter.stack = stack
}

// Init is used to initialize an existing iterator with a new tree. Release must've
// been called on the iterator before re-using it using Init.
func (iter *IterG[T]) Init(tr *BTreeG[T], mut bool) {
	// Re-use the stack, but 0 out the rest of the memory.
	stack := iter.stack[:0]
	*iter = IterG[T]{}
	iter.stack = stack

	iter.tr = tr
	iter.mut = mut

	iter.locked = tr.lock(iter.mut)
	if iter.stack == nil {
		iter.stack = iter.stack0[:0]
	} else {
		iter.stack = iter.stack[:0]
	}
}

// Next moves iterator to the next item in iterator.
// Returns false if the tree is empty or the iterator is at the end of
// the tree.
func (iter *IterG[T]) Next() bool {
	if iter.tr == nil {
		return false
	}
	if !iter.seeked {
		return iter.First()
	}
	if len(iter.stack) == 0 {
		if iter.atstart {
			return iter.First() && iter.Next()
		}
		return false
	}
	s := &iter.stack[len(iter.stack)-1]
	s.i++
	if s.n.leaf() {
		if s.i == len(s.n.items) {
			for {
				iter.stack = iter.stack[:len(iter.stack)-1]
				if len(iter.stack) == 0 {
					iter.atend = true
					return false
				}
				s = &iter.stack[len(iter.stack)-1]
				if s.i < len(s.n.items) {
					break
				}
			}
		}
	} else {
		n := iter.tr.isoLoad(&(*s.n.children)[s.i], iter.mut)
		for {
			iter.stack = append(iter.stack, iterStackItemG[T]{n, 0})
			if n.leaf() {
				break
			}
			n = iter.tr.isoLoad(&(*n.children)[0], iter.mut)
		}
	}
	s = &iter.stack[len(iter.stack)-1]
	iter.item = s.n.items[s.i]
	return true
}

// Prev moves iterator to the previous item in iterator.
// Returns false if the tree is empty or the iterator is at the beginning of
// the tree.
func (iter *IterG[T]) Prev() bool {
	if iter.tr == nil {
		return false
	}
	if !iter.seeked {
		return false
	}
	if len(iter.stack) == 0 {
		if iter.atend {
			return iter.Last() && iter.Prev()
		}
		return false
	}
	s := &iter.stack[len(iter.stack)-1]
	if s.n.leaf() {
		s.i--
		if s.i == -1 {
			for {
				iter.stack = iter.stack[:len(iter.stack)-1]
				if len(iter.stack) == 0 {
					iter.atstart = true
					return false
				}
				s = &iter.stack[len(iter.stack)-1]
				s.i--
				if s.i > -1 {
					break
				}
			}
		}
	} else {
		n := iter.tr.isoLoad(&(*s.n.children)[s.i], iter.mut)
		for {
			iter.stack = append(iter.stack, iterStackItemG[T]{n, len(n.items)})
			if n.leaf() {
				iter.stack[len(iter.stack)-1].i--
				break
			}
			n = iter.tr.isoLoad(&(*n.children)[len(n.items)], iter.mut)
		}
	}
	s = &iter.stack[len(iter.stack)-1]
	iter.item = s.n.items[s.i]
	return true
}

// Item returns the current iterator item.
func (iter *IterG[T]) Item() T {
	return iter.item
}

// Items returns all the items in order.
func (tr *BTreeG[T]) Items() []T {
	return tr.items(false)
}

func (tr *BTreeG[T]) ItemsMut() []T {
	return tr.items(true)
}

func (tr *BTreeG[T]) items(mut bool) []T {
	if tr.lock(mut) {
		defer tr.unlock(mut)
	}
	items := make([]T, 0, tr.Len())
	if tr.root != nil {
		items = tr.nodeItems(&tr.root, items, mut)
	}
	return items
}

func (tr *BTreeG[T]) nodeItems(cn **node[T], items []T, mut bool) []T {
	n := tr.isoLoad(cn, mut)
	if n.leaf() {
		return append(items, n.items...)
	}
	for i := 0; i < len(n.items); i++ {
		items = tr.nodeItems(&(*n.children)[i], items, mut)
		items = append(items, n.items[i])
	}
	return tr.nodeItems(&(*n.children)[len(*n.children)-1], items, mut)
}

// DiffKind identifies the kind of difference reported by the DiffG
// iterator.
type DiffKind int

const (
	// DiffAdded indicates an item that only exists in the other tree,
	// the one that was passed to Diff.
	DiffAdded DiffKind = iota
	// DiffDeleted indicates an item that only exists in the original
	// tree, the one that Diff was called on.
	DiffDeleted
	// DiffModified indicates that both trees contain an item with the
	// same key, but the two items are not equal according to the
	// equality function that was provided to Diff.
	DiffModified
)

// DiffG is an ordered iterator over the differences between two B-trees.
// Use BTreeG.Diff to create a new iterator, and call Release when finished.
type DiffG[T any] struct {
	less    func(a, b T) bool
	eq      func(a, b T) bool
	a, b    *BTreeG[T]
	ca, cb  diffCursorG[T]
	kind    DiffKind
	item    T // current item of tree a (DiffDeleted and DiffModified)
	oitem   T // current item of tree b (DiffAdded and DiffModified)
	lockedA bool
	lockedB bool
}

type diffStackItemG[T any] struct {
	n *node[T]
	i int
}

// diffCursorG is a read-only, node-aware cursor over the items of a tree.
// It follows the same stack conventions as IterG: for a leaf frame, i is
// the index of the current item; for an internal frame, i is the index of
// the child subtree currently being traversed, and items[i] is yielded
// once that subtree is exhausted.
type diffCursorG[T any] struct {
	stack []diffStackItemG[T]
	item  T
	valid bool
}

// first positions the cursor on the first item of the tree.
func (c *diffCursorG[T]) first(tr *BTreeG[T]) bool {
	n := tr.root
	if n == nil {
		return false
	}
	for {
		c.stack = append(c.stack, diffStackItemG[T]{n, 0})
		if n.leaf() {
			break
		}
		n = (*n.children)[0]
	}
	c.item = n.items[0]
	return true
}

// ascend pops exhausted frames and positions the cursor on the next
// available item. Returns false when the tree has been fully consumed.
func (c *diffCursorG[T]) ascend() bool {
	for len(c.stack) > 0 {
		s := &c.stack[len(c.stack)-1]
		if s.i < len(s.n.items) {
			c.item = s.n.items[s.i]
			return true
		}
		c.stack = c.stack[:len(c.stack)-1]
	}
	return false
}

// next advances the cursor to the next item in the tree.
func (c *diffCursorG[T]) next() bool {
	if len(c.stack) == 0 {
		return false
	}
	s := &c.stack[len(c.stack)-1]
	s.i++
	if s.n.leaf() {
		return c.ascend()
	}
	n := (*s.n.children)[s.i]
	for {
		c.stack = append(c.stack, diffStackItemG[T]{n, 0})
		if n.leaf() {
			break
		}
		n = (*n.children)[0]
	}
	c.item = n.items[0]
	return true
}

// Diff returns an iterator that yields the ordered differences between the
// receiver and the provided tree. Differences are reported in ascending
// key order: items that only exist in the receiver are reported as
// DiffDeleted, items that only exist in the other tree are reported as
// DiffAdded, and items that exist in both trees with equal keys but
// unequal values are reported as DiffModified.
//
// The eq function reports whether two items with equal keys are considered
// equal. This allows for items that contain more data than what the less
// comparator looks at, such as a key-value pair where less only compares
// the key. If eq is nil, any two items with equal keys are considered
// equal, and DiffModified is never reported.
//
// The two trees must use the same less comparator. Because comparators are
// function values, compatibility is checked by comparing their underlying
// code pointers, which is best-effort: distinct closures that share the
// same code are considered compatible. If the comparators are incompatible,
// Diff returns false and the iterator must not be used.
//
// Diff takes advantage of the copy-on-write node sharing performed by
// Copy: subtrees that are shared between both trees are skipped wholesale,
// without comparing their items, so the amount of work is proportional to
// the number of differences rather than the size of the trees. This relies
// on the following contract:
//
//   - Items must be treated as immutable while they are stored in either
//     tree. Replacing an item with Set is fine because it copies the
//     affected nodes, but mutating the contents of an item in place (for
//     example through a pointer stored in the item) leaves the shared
//     nodes untouched, and such changes will not be reported.
//   - Neither tree may be modified while the iterator is in use. Read
//     locks are held on both trees until Release is called.
//
// The Release method must be called when finished with the iterator.
func (tr *BTreeG[T]) Diff(other *BTreeG[T], eq func(a, b T) bool,
) (DiffG[T], bool) {
	var iter DiffG[T]
	if tr.less == nil || other.less == nil ||
		reflect.ValueOf(tr.less).Pointer() !=
			reflect.ValueOf(other.less).Pointer() {
		return iter, false
	}
	iter.less = tr.less
	if eq == nil {
		iter.eq = func(a, b T) bool {
			return !tr.less(a, b) && !tr.less(b, a)
		}
	} else {
		iter.eq = eq
	}
	iter.a, iter.b = tr, other
	iter.lockedA = tr.lock(false)
	if other != tr {
		iter.lockedB = other.lock(false)
	}
	iter.ca.valid = iter.ca.first(tr)
	iter.cb.valid = iter.cb.first(other)
	return iter, true
}

// skipShared advances both cursors past subtrees that are shared between
// the two trees. When both cursors are positioned at the same offset
// within the same shared node, the remainder of that node's subtree is
// identical in both trees and can be skipped without further comparison.
func (iter *DiffG[T]) skipShared() {
	for iter.ca.valid && iter.cb.valid {
		ia := len(iter.ca.stack) - 1
		ib := len(iter.cb.stack) - 1
		for ia >= 0 && ib >= 0 &&
			iter.ca.stack[ia].n == iter.cb.stack[ib].n &&
			iter.ca.stack[ia].i == iter.cb.stack[ib].i {
			ia--
			ib--
		}
		if ia == len(iter.ca.stack)-1 {
			return // no shared frames
		}
		// The matching frames form an identical suffix of both stacks:
		// both cursors are at the same position within a shared subtree.
		// Drop the shared frames and continue right after the subtree.
		iter.ca.stack = iter.ca.stack[:ia+1]
		iter.cb.stack = iter.cb.stack[:ib+1]
		iter.ca.valid = iter.ca.ascend()
		iter.cb.valid = iter.cb.ascend()
	}
}

// Next moves the iterator to the next difference between the two trees.
// Returns false when there are no more differences.
func (iter *DiffG[T]) Next() bool {
	for iter.ca.valid && iter.cb.valid {
		iter.skipShared()
		if !iter.ca.valid || !iter.cb.valid {
			break
		}
		ai, bi := iter.ca.item, iter.cb.item
		if iter.less(ai, bi) {
			iter.kind = DiffDeleted
			iter.item = ai
			iter.ca.valid = iter.ca.next()
			return true
		}
		if iter.less(bi, ai) {
			iter.kind = DiffAdded
			iter.oitem = bi
			iter.cb.valid = iter.cb.next()
			return true
		}
		if !iter.eq(ai, bi) {
			iter.kind = DiffModified
			iter.item = ai
			iter.oitem = bi
			iter.ca.valid = iter.ca.next()
			iter.cb.valid = iter.cb.next()
			return true
		}
		iter.ca.valid = iter.ca.next()
		iter.cb.valid = iter.cb.next()
	}
	if iter.ca.valid {
		iter.kind = DiffDeleted
		iter.item = iter.ca.item
		iter.ca.valid = iter.ca.next()
		return true
	}
	if iter.cb.valid {
		iter.kind = DiffAdded
		iter.oitem = iter.cb.item
		iter.cb.valid = iter.cb.next()
		return true
	}
	return false
}

// Kind returns the kind of the current difference.
func (iter *DiffG[T]) Kind() DiffKind {
	return iter.kind
}

// Item returns the current item of the original tree, the one that Diff
// was called on. It is only meaningful when Kind returns DiffDeleted or
// DiffModified.
func (iter *DiffG[T]) Item() T {
	return iter.item
}

// OtherItem returns the current item of the other tree, the one that was
// passed to Diff. It is only meaningful when Kind returns DiffAdded or
// DiffModified.
func (iter *DiffG[T]) OtherItem() T {
	return iter.oitem
}

// Release the iterator.
func (iter *DiffG[T]) Release() {
	if iter.lockedA {
		iter.a.unlock(false)
		iter.lockedA = false
	}
	if iter.lockedB {
		iter.b.unlock(false)
		iter.lockedB = false
	}
}

// Clear will delete all items.
func (tr *BTreeG[T]) Clear() {
	if tr.lock(true) {
		defer tr.unlock(true)
	}
	tr.root = nil
	tr.count = 0
}

// Generic BTree
//
// Deprecated: use BTreeG
type Generic[T any] struct {
	*BTreeG[T]
}

// NewGeneric returns a generic BTree
//
// Deprecated: use NewBTreeG
func NewGeneric[T any](less func(a, b T) bool) *Generic[T] {
	return &Generic[T]{NewBTreeGOptions(less, Options{})}
}

// NewGenericOptions returns a generic BTree
//
// Deprecated: use NewBTreeGOptions
func NewGenericOptions[T any](less func(a, b T) bool, opts Options,
) *Generic[T] {
	return &Generic[T]{NewBTreeGOptions(less, opts)}
}

func (tr *Generic[T]) Copy() *Generic[T] {
	return &Generic[T]{tr.BTreeG.Copy()}
}
