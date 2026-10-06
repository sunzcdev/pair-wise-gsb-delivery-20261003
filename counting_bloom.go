package bloom

import (
	"errors"
	"sort"
)

const maxCounterValue = 1<<8 - 1

var (
	// ErrCounterLimit is returned when an Add would increment a counter
	// beyond maxCounterValue. The Add is rejected and no counter changes.
	ErrCounterLimit = errors.New("counting bloom filter: counter limit reached")

	// ErrInvalidReceipt is returned by Remove for a receipt from another
	// filter, an already removed receipt, or a receipt invalidated by ClearAll.
	ErrInvalidReceipt = errors.New("counting bloom filter: invalid receipt")
)

// Receipt represents one successful Add. A copy of a receipt refers to the
// same addition; removing one copy consumes the receipt for every copy.
type Receipt struct {
	filter     *byte
	id         uint64
	generation uint64
}

type counterIncrement struct {
	index uint
	delta byte
}

// CountingBloomFilter is a Bloom filter whose bits are replaced by byte
// counters. Additions can be removed only with the Receipt returned by Add;
// there is intentionally no key-based remove operation.
//
// The filter retains one small private record for every receipt that is still
// valid. Remove deletes that record, and ClearAll replaces the receipt table
// and counter slice, making their old backing storage eligible for garbage
// collection once the runtime collects it. A Receipt value held by the caller
// is itself collected when it becomes unreachable.
//
// CountingBloomFilter is not safe for concurrent use.
type CountingBloomFilter struct {
	m          uint
	k          uint
	counters   []byte
	receipts   map[uint64][]counterIncrement
	nextID     uint64
	generation uint64
	identity   *byte
}

// NewCountingBloomFilter creates a CountingBloomFilter with m byte counters
// and k hashing functions.
func NewCountingBloomFilter(m uint, k uint) *CountingBloomFilter {
	return &CountingBloomFilter{
		m:          max(1, m),
		k:          max(1, k),
		counters:   make([]byte, max(1, m)),
		receipts:   make(map[uint64][]counterIncrement),
		generation: 1,
		identity:   new(byte),
	}
}

// Cap returns the number of counters in the filter.
func (f *CountingBloomFilter) Cap() uint {
	return f.m
}

// K returns the number of hashing functions used by the filter.
func (f *CountingBloomFilter) K() uint {
	return f.k
}

func (f *CountingBloomFilter) location(h [4]uint64, i uint) uint {
	return uint(location(h, i) % uint64(f.m))
}

func (f *CountingBloomFilter) increments(data []byte) ([]counterIncrement, error) {
	h := baseHashes(data)
	counts := make(map[uint]uint, f.k)
	for i := uint(0); i < f.k; i++ {
		counts[f.location(h, i)]++
	}

	indices := make([]uint, 0, len(counts))
	for index := range counts {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })

	increments := make([]counterIncrement, 0, len(indices))
	for _, index := range indices {
		delta := counts[index]
		if delta > maxCounterValue-uint(f.counters[index]) {
			return nil, ErrCounterLimit
		}
		increments = append(increments, counterIncrement{index: index, delta: byte(delta)})
	}
	return increments, nil
}

// Add increments the counters for data and returns a receipt for that exact
// addition. Repeated calls for the same data return independent receipts. If
// any counter would overflow, Add returns a zero receipt and ErrCounterLimit
// without modifying any counter.
func (f *CountingBloomFilter) Add(data []byte) (Receipt, error) {
	increments, err := f.increments(data)
	if err != nil {
		return Receipt{}, err
	}

	for _, increment := range increments {
		f.counters[increment.index] += increment.delta
	}

	id := f.nextID
	f.nextID++
	f.receipts[id] = increments
	return Receipt{filter: f.identity, id: id, generation: f.generation}, nil
}

// AddString is equivalent to Add([]byte(data)).
func (f *CountingBloomFilter) AddString(data string) (Receipt, error) {
	return f.Add([]byte(data))
}

// Test reports whether all counters for data are nonzero. A false result means
// data is definitely absent; a true result may be a false positive.
func (f *CountingBloomFilter) Test(data []byte) bool {
	h := baseHashes(data)
	seen := make(map[uint]struct{}, f.k)
	for i := uint(0); i < f.k; i++ {
		index := f.location(h, i)
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}
		if f.counters[index] == 0 {
			return false
		}
	}
	return true
}

// TestString is equivalent to Test([]byte(data)).
func (f *CountingBloomFilter) TestString(data string) bool {
	return f.Test([]byte(data))
}

// Remove decrements exactly the counters associated with one successful Add.
// Receipts from other filters, duplicate removals, and receipts old enough to
// predate ClearAll are rejected without changing the filter.
func (f *CountingBloomFilter) Remove(receipt Receipt) error {
	if receipt.filter != f.identity || receipt.generation != f.generation {
		return ErrInvalidReceipt
	}

	increments, ok := f.receipts[receipt.id]
	if !ok {
		return ErrInvalidReceipt
	}

	for _, increment := range increments {
		if uint(f.counters[increment.index]) < uint(increment.delta) {
			return ErrInvalidReceipt
		}
	}
	for _, increment := range increments {
		f.counters[increment.index] -= increment.delta
	}
	delete(f.receipts, receipt.id)
	return nil
}

// ClearAll removes all counters and invalidates every receipt issued before
// the call. The old counter slice and receipt table become eligible for garbage
// collection.
func (f *CountingBloomFilter) ClearAll() *CountingBloomFilter {
	f.counters = make([]byte, f.m)
	f.receipts = make(map[uint64][]counterIncrement)
	f.generation++
	return f
}
