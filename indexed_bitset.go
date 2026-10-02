package bitset

import (
	"math/bits"
	"sort"
)

const indexedBitSetBlockWords = 16
const indexedBitSetSuperBlockWords = 64
const indexedBitSetBlocksPerSuperBlock = indexedBitSetSuperBlockWords / indexedBitSetBlockWords

// IndexedBitSet is a BitSet with block population counts for Rank and Select.
//
// The underlying BitSet is owned by IndexedBitSet and is not exposed. Updates
// must go through the IndexedBitSet methods so that the block counts stay
// consistent.
type IndexedBitSet struct {
	bits        *BitSet
	blockCounts []uint
	superCounts []uint
	count       uint
}

// NewIndexedBitSet creates an indexed BitSet with room for length bits.
func NewIndexedBitSet(length uint) *IndexedBitSet {
	wordCount := wordsNeeded(length)
	return &IndexedBitSet{
		bits:        New(length),
		blockCounts: make([]uint, indexedBlockCount(wordCount)),
		superCounts: make([]uint, indexedSuperBlockCount(wordCount)),
	}
}

// Len returns the number of bits in the indexed BitSet.
func (b *IndexedBitSet) Len() uint {
	if b == nil || b.bits == nil {
		return 0
	}
	return b.bits.Len()
}

// Test reports whether bit i is set.
func (b *IndexedBitSet) Test(i uint) bool {
	if b == nil || b.bits == nil {
		return false
	}
	return b.bits.Test(i)
}

// Count returns the number of set bits.
func (b *IndexedBitSet) Count() uint {
	if b == nil {
		return 0
	}
	return b.count
}

// Set sets bit i to one and updates the block index.
func (b *IndexedBitSet) Set(i uint) *IndexedBitSet {
	return b.mutateBit(i, b.bits.Set)
}

// Clear sets bit i to zero and updates the block index.
func (b *IndexedBitSet) Clear(i uint) *IndexedBitSet {
	return b.mutateBit(i, b.bits.Clear)
}

// Flip toggles bit i and updates the block index.
func (b *IndexedBitSet) Flip(i uint) *IndexedBitSet {
	return b.mutateBit(i, b.bits.Flip)
}

// SetTo sets bit i to value and updates the block index.
func (b *IndexedBitSet) SetTo(i uint, value bool) *IndexedBitSet {
	if value {
		return b.Set(i)
	}
	return b.Clear(i)
}

// SetRange sets bits in [start, end) to one and updates affected block counts.
func (b *IndexedBitSet) SetRange(start, end uint) *IndexedBitSet {
	if start >= end {
		return b
	}
	b.ensureInitialized()
	firstWord := int(start >> log2WordSize)
	lastWord := int((end - 1) >> log2WordSize)
	b.bits.SetRange(start, end)
	b.resizeIndex(len(b.bits.set))
	b.refreshWordRange(firstWord, lastWord)
	return b
}

// ClearRange sets bits in [start, end) to zero and updates affected block counts.
func (b *IndexedBitSet) ClearRange(start, end uint) *IndexedBitSet {
	if start >= end || start >= b.Len() {
		return b
	}
	b.ensureInitialized()
	if end > b.bits.length {
		end = b.bits.length
	}

	firstWord := int(start >> log2WordSize)
	lastWord := int((end - 1) >> log2WordSize)
	firstMask := ^uint64(0) << wordsIndex(start)
	lastMask := ^uint64(0) >> wordsIndex(-end)

	if firstWord == lastWord {
		b.bits.set[firstWord] &^= firstMask & lastMask
	} else {
		b.bits.set[firstWord] &^= firstMask
		for word := firstWord + 1; word < lastWord; word++ {
			b.bits.set[word] = 0
		}
		b.bits.set[lastWord] &^= lastMask
	}

	b.refreshWordRange(firstWord, lastWord)
	return b
}

// FlipRange toggles bits in [start, end) and updates affected block counts.
func (b *IndexedBitSet) FlipRange(start, end uint) *IndexedBitSet {
	if start >= end {
		return b
	}
	b.ensureInitialized()
	firstWord := int(start >> log2WordSize)
	lastWord := int((end - 1) >> log2WordSize)
	b.bits.FlipRange(start, end)
	b.resizeIndex(len(b.bits.set))
	b.refreshWordRange(firstWord, lastWord)
	return b
}

// Shrink truncates the indexed BitSet and updates affected block counts.
func (b *IndexedBitSet) Shrink(lastBitIndex uint) *IndexedBitSet {
	if b == nil || b.bits == nil {
		return b
	}
	wordCount := wordsNeeded(lastBitIndex + 1)
	if wordCount > len(b.bits.set) {
		return b
	}

	b.bits.Shrink(lastBitIndex)
	b.resizeIndex(wordCount)
	if wordCount > 0 {
		b.refreshBlock((wordCount - 1) / indexedBitSetBlockWords)
	}
	return b
}

// ClearAll clears all bits while retaining the current length.
func (b *IndexedBitSet) ClearAll() *IndexedBitSet {
	if b == nil || b.bits == nil {
		return b
	}
	b.bits.ClearAll()
	for i := range b.blockCounts {
		b.blockCounts[i] = 0
	}
	for i := range b.superCounts {
		b.superCounts[i] = 0
	}
	b.count = 0
	return b
}

// Rank returns the number of set bits up to and including index.
func (b *IndexedBitSet) Rank(index uint) uint {
	if b == nil || b.bits == nil || b.count == 0 {
		return 0
	}
	length := b.bits.Len()
	if index >= length {
		return b.count
	}

	limit := index + 1
	wordEnd := int(limit >> log2WordSize)
	word := wordEnd
	var rank uint
	if bitOffset := limit & wordMask; bitOffset != 0 {
		word++
	}
	word--

	if word < 0 {
		return 0
	}

	superBlock := word / indexedBitSetSuperBlockWords
	rank = b.superCounts[superBlock]
	firstBlock := superBlock * indexedBitSetBlocksPerSuperBlock
	endBlock := word / indexedBitSetBlockWords
	for block := firstBlock; block < endBlock; block++ {
		rank += b.blockCounts[block]
	}

	firstWord := endBlock * indexedBitSetBlockWords
	lastWord := word
	if bitOffset := limit & wordMask; bitOffset != 0 {
		lastWord--
	}
	for scannedWord := firstWord; scannedWord <= lastWord; scannedWord++ {
		rank += uint(bits.OnesCount64(b.bits.set[scannedWord]))
	}
	if bitOffset := limit & wordMask; bitOffset != 0 {
		rank += uint(bits.OnesCount64(b.bits.set[word] << (wordSize - bitOffset)))
	}
	return rank
}

// Select returns the zero-based jth set bit. The boolean result is false when
// j is out of range, unlike BitSet.Select, which returns the bitset length.
func (b *IndexedBitSet) Select(j uint) (uint, bool) {
	if b == nil || b.bits == nil || j >= b.count {
		return b.Len(), false
	}

	wordCount := len(b.bits.set)
	superBlock := sort.Search(len(b.superCounts)-1, func(i int) bool {
		return b.superCounts[i+1] > j
	})
	j -= b.superCounts[superBlock]
	{
		firstBlock := superBlock * indexedBitSetBlocksPerSuperBlock
		endBlock := firstBlock + indexedBitSetBlocksPerSuperBlock
		if endBlock > len(b.blockCounts) {
			endBlock = len(b.blockCounts)
		}
		for block := firstBlock; block < endBlock; block++ {
			blockCount := b.blockCounts[block]
			if j >= blockCount {
				j -= blockCount
				continue
			}

			firstWord := block * indexedBitSetBlockWords
			lastWord := firstWord + indexedBitSetBlockWords
			if lastWord > wordCount {
				lastWord = wordCount
			}
			for wordIndex := firstWord; wordIndex < lastWord; wordIndex++ {
				wordBits := b.bits.set[wordIndex]
				wordPopCount := uint(bits.OnesCount64(wordBits))
				if wordPopCount > j {
					return uint(wordIndex)*wordSize + select64(wordBits, j), true
				}
				j -= wordPopCount
			}
		}
	}
	return b.bits.Len(), false
}

func (b *IndexedBitSet) ensureInitialized() {
	if b.bits == nil {
		b.bits = new(BitSet)
	}
}

func (b *IndexedBitSet) mutateBit(i uint, mutate func(uint) *BitSet) *IndexedBitSet {
	b.ensureInitialized()
	wordIndex := int(i >> log2WordSize)
	var oldWord uint64
	if wordIndex < len(b.bits.set) {
		oldWord = b.bits.set[wordIndex]
	}

	mutate(i)
	b.resizeIndex(len(b.bits.set))
	if wordIndex >= len(b.bits.set) {
		return b
	}
	newWord := b.bits.set[wordIndex]
	delta := int(bits.OnesCount64(newWord)) - int(bits.OnesCount64(oldWord))
	block := wordIndex / indexedBitSetBlockWords
	b.blockCounts[block] = addSigned(b.blockCounts[block], delta)
	b.count = addSigned(b.count, delta)
	superBlock := block / indexedBitSetBlocksPerSuperBlock
	for i := superBlock + 1; i < len(b.superCounts); i++ {
		b.superCounts[i] = addSigned(b.superCounts[i], delta)
	}
	return b
}

func (b *IndexedBitSet) resizeIndex(wordCount int) {
	blockCount := indexedBlockCount(wordCount)
	if blockCount < len(b.blockCounts) {
		for block := blockCount; block < len(b.blockCounts); block++ {
			b.count -= b.blockCounts[block]
		}
		b.blockCounts = b.blockCounts[:blockCount]
	} else if blockCount > cap(b.blockCounts) {
		newCounts := make([]uint, blockCount)
		copy(newCounts, b.blockCounts)
		b.blockCounts = newCounts
	} else {
		existing := len(b.blockCounts)
		b.blockCounts = b.blockCounts[:blockCount]
		for block := existing; block < blockCount; block++ {
			b.blockCounts[block] = 0
		}
	}

	superBlockCount := indexedSuperBlockCount(wordCount)
	if superBlockCount < len(b.superCounts) {
		b.superCounts = b.superCounts[:superBlockCount]
		return
	}
	if superBlockCount > cap(b.superCounts) {
		newCounts := make([]uint, superBlockCount)
		copy(newCounts, b.superCounts)
		b.superCounts = newCounts
		return
	}
	existingSuperBlocks := len(b.superCounts)
	b.superCounts = b.superCounts[:superBlockCount]
	for superBlock := existingSuperBlocks; superBlock < superBlockCount; superBlock++ {
		b.superCounts[superBlock] = b.count
	}
}

func (b *IndexedBitSet) refreshWordRange(firstWord, lastWord int) {
	firstBlock := firstWord / indexedBitSetBlockWords
	lastBlock := lastWord / indexedBitSetBlockWords
	for block := firstBlock; block <= lastBlock; block++ {
		b.refreshBlock(block)
	}
}

func (b *IndexedBitSet) refreshBlock(block int) {
	oldBlockCount := b.blockCounts[block]
	firstWord := block * indexedBitSetBlockWords
	lastWord := firstWord + indexedBitSetBlockWords
	if lastWord > len(b.bits.set) {
		lastWord = len(b.bits.set)
	}

	var count uint
	for word := firstWord; word < lastWord; word++ {
		count += uint(bits.OnesCount64(b.bits.set[word]))
	}
	b.blockCounts[block] = count
	delta := int(count) - int(oldBlockCount)
	b.count = addSigned(b.count, delta)

	superBlock := block / indexedBitSetBlocksPerSuperBlock
	for i := superBlock + 1; i < len(b.superCounts); i++ {
		b.superCounts[i] = addSigned(b.superCounts[i], delta)
	}
}

func indexedBlockCount(wordCount int) int {
	return (wordCount + indexedBitSetBlockWords - 1) / indexedBitSetBlockWords
}

func indexedSuperBlockCount(wordCount int) int {
	return (wordCount + indexedBitSetSuperBlockWords - 1) / indexedBitSetSuperBlockWords
}

func addSigned(value uint, delta int) uint {
	if delta >= 0 {
		return value + uint(delta)
	}
	return value - uint(-delta)
}
