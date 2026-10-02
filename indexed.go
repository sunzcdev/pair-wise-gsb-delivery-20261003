package bitset

import "math/bits"

// indexBlockShift is log2 of the number of 64-bit words grouped into one
// index block.
const indexBlockShift = 5

// indexBlockWords is the number of words covered by one index block,
// i.e. 32 words = 2048 bits.
const indexBlockWords = 1 << indexBlockShift

// An IndexedBitSet is a BitSet augmented with a block-level rank/select
// index, aimed at workloads with a high Rank/Select query rate.
//
// The index stores the number of set bits per block of indexBlockWords
// words, plus a Fenwick (binary indexed) tree over those counts. As a
// result Rank and Select only ever scan the words of a single block
// instead of the whole bitset, while Set, Clear, Flip and the range
// update operations correct the index incrementally.
//
// An IndexedBitSet owns its data: the underlying words are never exposed,
// so callers cannot bypass the index maintenance. Use Snapshot to obtain
// a plain BitSet copy of the current content.
//
// The zero value is an empty, ready-to-use IndexedBitSet.
// Like BitSet, an IndexedBitSet is not safe for concurrent use.
type IndexedBitSet struct {
	data BitSet
	// counts[k] is the number of set bits in block k (non-cumulative).
	counts []uint
	// tree is a Fenwick tree over counts, 1-indexed (tree[0] unused).
	tree []uint
	// total is the number of set bits (cached Count).
	total uint
	// wordReads counts word accesses performed by the last Rank/Select
	// query; it exists to demonstrate (and test) that queries do not
	// scan the whole word slice.
	wordReads uint
}

// NewIndexed creates an empty IndexedBitSet with a hint that length bits
// will be required, mirroring New.
func NewIndexed(length uint) *IndexedBitSet {
	ix := &IndexedBitSet{data: *New(length)}
	n := indexBlocksNeeded(len(ix.data.set))
	ix.counts = make([]uint, n)
	ix.tree = make([]uint, n+1)
	return ix
}

// IndexedFrom creates an IndexedBitSet holding a copy of b: the argument
// is cloned, so later changes to b do not affect the returned set and the
// returned set is the sole owner of its words.
func IndexedFrom(b *BitSet) *IndexedBitSet {
	ix := &IndexedBitSet{data: *b.Clone()}
	ix.rebuild()
	return ix
}

// indexBlocksNeeded returns the number of index blocks covering nwords words.
func indexBlocksNeeded(nwords int) int {
	return (nwords + indexBlockWords - 1) >> indexBlockShift
}

// indexBlockOf returns the index block holding bit i.
func indexBlockOf(i uint) int {
	return int(i >> log2WordSize >> indexBlockShift)
}

// fenwickBuild constructs the 1-indexed Fenwick tree over counts in O(n).
func fenwickBuild(tree, counts []uint) {
	for i, c := range counts {
		tree[i+1] = c
	}
	for i := 1; i < len(tree); i++ {
		if j := i + (i & -i); j < len(tree) {
			tree[j] += tree[i]
		}
	}
}

// fenwickAdd adds delta (which may be negative) to element i (0-based).
func fenwickAdd(tree []uint, i int, delta int) {
	for x := i + 1; x < len(tree); x += x & -x {
		if delta >= 0 {
			tree[x] += uint(delta)
		} else {
			tree[x] -= uint(-delta)
		}
	}
}

// fenwickPrefix returns the sum of the first i elements
// (i may range from 0 to len(tree)-1).
func fenwickPrefix(tree []uint, i int) (sum uint) {
	for x := i; x > 0; x -= x & -x {
		sum += tree[x]
	}
	return
}

// fenwickSelect returns the 0-based index of the element containing the
// j-th unit (0-based): the smallest i such that the prefix sum of the
// first i+1 elements exceeds j. It requires j to be smaller than the
// total sum.
func fenwickSelect(tree []uint, j uint) int {
	n := len(tree) - 1
	d := 1
	for d<<1 <= n {
		d <<= 1
	}
	pos := 0
	for ; d > 0; d >>= 1 {
		if next := pos + d; next <= n && tree[next] <= j {
			j -= tree[next]
			pos = next
		}
	}
	return pos
}

// ensureBlocks grows the index to cover the current words of the bitset.
// Newly covered words are all zero, so the new counts are zero and only
// the Fenwick tree has to be rebuilt.
func (ix *IndexedBitSet) ensureBlocks() {
	needed := indexBlocksNeeded(len(ix.data.set))
	if needed <= len(ix.counts) {
		return
	}
	for len(ix.counts) < needed {
		ix.counts = append(ix.counts, 0)
	}
	ix.tree = make([]uint, needed+1)
	fenwickBuild(ix.tree, ix.counts)
}

// addBlock applies a delta of +1 or -1 to the block holding bit i.
func (ix *IndexedBitSet) addBlock(i uint, delta int) {
	k := indexBlockOf(i)
	if delta >= 0 {
		ix.counts[k] += uint(delta)
		ix.total += uint(delta)
	} else {
		ix.counts[k] -= uint(-delta)
		ix.total -= uint(-delta)
	}
	fenwickAdd(ix.tree, k, delta)
}

// recomputeBlock recomputes the count of block k from the actual words
// and corrects the Fenwick tree and the total by the difference.
func (ix *IndexedBitSet) recomputeBlock(k int) {
	start := k << indexBlockShift
	end := start + indexBlockWords
	if end > len(ix.data.set) {
		end = len(ix.data.set)
	}
	c := uint(popcntSlice(ix.data.set[start:end]))
	old := ix.counts[k]
	if c == old {
		return
	}
	ix.counts[k] = c
	if c > old {
		ix.total += c - old
		fenwickAdd(ix.tree, k, int(c-old))
	} else {
		ix.total -= old - c
		fenwickAdd(ix.tree, k, -int(old-c))
	}
}

// recomputeBlocks recomputes the counts of blocks lo through hi (inclusive).
func (ix *IndexedBitSet) recomputeBlocks(lo, hi int) {
	for k := lo; k <= hi; k++ {
		ix.recomputeBlock(k)
	}
}

// rebuild recomputes the whole index from the current words. It is used
// after operations, such as Shrink, that change the number of words.
func (ix *IndexedBitSet) rebuild() {
	n := indexBlocksNeeded(len(ix.data.set))
	if cap(ix.counts) >= n {
		ix.counts = ix.counts[:n]
	} else {
		ix.counts = make([]uint, n)
	}
	var total uint
	for k := range ix.counts {
		start := k << indexBlockShift
		end := start + indexBlockWords
		if end > len(ix.data.set) {
			end = len(ix.data.set)
		}
		c := uint(popcntSlice(ix.data.set[start:end]))
		ix.counts[k] = c
		total += c
	}
	if cap(ix.tree) >= n+1 {
		ix.tree = ix.tree[:n+1]
	} else {
		ix.tree = make([]uint, n+1)
	}
	fenwickBuild(ix.tree, ix.counts)
	ix.total = total
}

// Len returns the number of bits in the set.
func (ix *IndexedBitSet) Len() uint {
	return ix.data.length
}

// Count returns the number of set bits. It is O(1): the total is
// maintained incrementally by every update.
func (ix *IndexedBitSet) Count() uint {
	return ix.total
}

// Test reports whether bit i is set.
func (ix *IndexedBitSet) Test(i uint) bool {
	return ix.data.Test(i)
}

// Set bit i to 1, growing the set (and the index) if needed.
func (ix *IndexedBitSet) Set(i uint) *IndexedBitSet {
	if i < ix.data.length && ix.data.Test(i) {
		return ix
	}
	ix.data.Set(i)
	ix.ensureBlocks()
	ix.addBlock(i, 1)
	return ix
}

// Clear bit i to 0.
func (ix *IndexedBitSet) Clear(i uint) *IndexedBitSet {
	if i >= ix.data.length || !ix.data.Test(i) {
		return ix
	}
	ix.data.Clear(i)
	ix.addBlock(i, -1)
	return ix
}

// SetTo sets bit i to value.
func (ix *IndexedBitSet) SetTo(i uint, value bool) *IndexedBitSet {
	if value {
		return ix.Set(i)
	}
	return ix.Clear(i)
}

// Flip bit at i, growing the set (and the index) if needed.
func (ix *IndexedBitSet) Flip(i uint) *IndexedBitSet {
	if i >= ix.data.length {
		return ix.Set(i)
	}
	if ix.data.Test(i) {
		ix.data.Clear(i)
		ix.addBlock(i, -1)
	} else {
		ix.data.Set(i)
		ix.addBlock(i, 1)
	}
	return ix
}

// SetRange sets bits in [start, end) to 1, growing the set if needed, and
// incrementally corrects the index of the affected blocks.
func (ix *IndexedBitSet) SetRange(start, end uint) *IndexedBitSet {
	if start >= end {
		return ix
	}
	ix.data.SetRange(start, end)
	ix.ensureBlocks()
	ix.recomputeBlocks(indexBlockOf(start), indexBlockOf(end-1))
	return ix
}

// FlipRange flips bits in [start, end), growing the set if needed, and
// incrementally corrects the index of the affected blocks.
func (ix *IndexedBitSet) FlipRange(start, end uint) *IndexedBitSet {
	if start >= end {
		return ix
	}
	ix.data.FlipRange(start, end)
	ix.ensureBlocks()
	ix.recomputeBlocks(indexBlockOf(start), indexBlockOf(end-1))
	return ix
}

// SetAll sets the entire set and corrects the index.
func (ix *IndexedBitSet) SetAll() *IndexedBitSet {
	ix.data.SetAll()
	if len(ix.counts) > 0 {
		ix.recomputeBlocks(0, len(ix.counts)-1)
	}
	return ix
}

// ClearAll clears the entire set and zeroes the index.
// It does not free the memory.
func (ix *IndexedBitSet) ClearAll() *IndexedBitSet {
	ix.data.ClearAll()
	for k := range ix.counts {
		ix.counts[k] = 0
	}
	for t := range ix.tree {
		ix.tree[t] = 0
	}
	ix.total = 0
	return ix
}

// Shrink shrinks the set so that lastbitindex is the last possible set
// value, exactly like BitSet.Shrink, and updates the index, including the
// block covering the new, possibly partial, last word.
func (ix *IndexedBitSet) Shrink(lastbitindex uint) *IndexedBitSet {
	ix.data.Shrink(lastbitindex)
	ix.rebuild()
	return ix
}

// Compact shrinks the set to preserve all set bits while minimizing
// memory usage, exactly like BitSet.Compact, and updates the index.
func (ix *IndexedBitSet) Compact() *IndexedBitSet {
	ix.data.Compact()
	ix.rebuild()
	return ix
}

// Rank returns the number of set bits up to and including the index,
// exactly like BitSet.Rank, but it only scans the words of a single
// index block instead of all words before the index.
func (ix *IndexedBitSet) Rank(index uint) (rank uint) {
	ix.wordReads = 0
	index++ // Rank is up to and including

	nwords := len(ix.data.set)
	word := int(index >> log2WordSize)
	if word > nwords {
		word = nwords
	}
	block := word >> indexBlockShift

	// set bits in all blocks before the one holding the index ...
	rank = fenwickPrefix(ix.tree, block)

	// ... plus the words of that block up to the index ...
	for w := block << indexBlockShift; w < word; w++ {
		rank += uint(bits.OnesCount64(ix.data.set[w]))
		ix.wordReads++
	}

	// ... plus the partial word at the index.
	if word < nwords {
		rank += uint(bits.OnesCount64(ix.data.set[word] << (64 - index&63)))
		ix.wordReads++
	}

	return
}

// Select returns the index of the jth set bit, exactly like BitSet.Select,
// but it only scans the words of a single index block instead of all
// words. The second return value reports whether j was in range: it is
// false (and the index is 0) when j >= Count, so out-of-range queries are
// distinguishable from a bit set at position 0.
func (ix *IndexedBitSet) Select(j uint) (uint, bool) {
	ix.wordReads = 0
	if j >= ix.total {
		return 0, false
	}
	block := fenwickSelect(ix.tree, j)
	leftover := j - fenwickPrefix(ix.tree, block)

	start := block << indexBlockShift
	end := start + indexBlockWords
	if end > len(ix.data.set) {
		end = len(ix.data.set)
	}
	for w := start; w < end; w++ {
		word := ix.data.set[w]
		ix.wordReads++
		c := uint(bits.OnesCount64(word))
		if c > leftover {
			return uint(w)<<log2WordSize + select64(word, leftover), true
		}
		leftover -= c
	}
	// Unreachable while the index is consistent with the data.
	return 0, false
}

// Snapshot returns a plain BitSet holding a copy of the current content.
// The result is owned by the caller: modifying it does not affect the
// IndexedBitSet, whose own words are never exposed.
func (ix *IndexedBitSet) Snapshot() *BitSet {
	return ix.data.Clone()
}

// Clone returns a new IndexedBitSet with the same content and an
// independent copy of the index.
func (ix *IndexedBitSet) Clone() *IndexedBitSet {
	return &IndexedBitSet{
		data:   *ix.data.Clone(),
		counts: append([]uint(nil), ix.counts...),
		tree:   append([]uint(nil), ix.tree...),
		total:  ix.total,
	}
}

// String returns a string representation of the set, like BitSet.String.
func (ix *IndexedBitSet) String() string {
	return ix.data.String()
}
