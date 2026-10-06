package bloom

import (
	"errors"
	"sort"
)

const maxCounterValue = uint64(^uint32(0))

var (
	// ErrInvalidReceipt is returned when a receipt was not issued by a
	// counting Bloom filter.
	ErrInvalidReceipt = errors.New("invalid counting Bloom filter receipt")

	// ErrWrongFilter is returned when a receipt is presented to a different
	// filter than the one that issued it.
	ErrWrongFilter = errors.New("receipt belongs to a different counting Bloom filter")

	// ErrReceiptExpired is returned when a receipt was issued before ClearAll.
	ErrReceiptExpired = errors.New("receipt expired after ClearAll")

	// ErrReceiptUsed is returned when a receipt has already removed its add.
	ErrReceiptUsed = errors.New("receipt has already been used")

	// ErrCounterLimit is returned when an add would overflow a counter. The
	// rejected add leaves the filter unchanged.
	ErrCounterLimit = errors.New("counting Bloom filter counter limit reached")
)

// Receipt represents one successful Add call on a CountingBloomFilter. It can
// be used only once to remove that add.
type Receipt interface {
	isCountingReceipt()
}

// CountingBloomFilter is a Bloom filter backed by counters instead of bits. It
// supports removing an add when the caller retains the receipt returned by
// Add.
type CountingBloomFilter struct {
	m        uint
	k        uint
	counters []uint32

	generation uint64
}

type countingReceipt struct {
	filter     *CountingBloomFilter
	generation uint64
	h          [4]uint64
	used       bool
}

func (*countingReceipt) isCountingReceipt() {}

// NewCounting creates a counting Bloom filter with m counters and k hashing
// functions.
func NewCounting(m uint, k uint) *CountingBloomFilter {
	size := max(1, m)
	return &CountingBloomFilter{
		m:          size,
		k:          max(1, k),
		counters:   make([]uint32, size),
		generation: 1,
	}
}

// NewCountingWithEstimates creates a counting Bloom filter for approximately n
// items with the requested false-positive rate.
func NewCountingWithEstimates(n uint, fp float64) *CountingBloomFilter {
	m, k := EstimateParameters(n, fp)
	return NewCounting(m, k)
}

// Cap returns the number of counters in the filter.
func (f *CountingBloomFilter) Cap() uint {
	return f.m
}

// K returns the number of hashing functions used by the filter.
func (f *CountingBloomFilter) K() uint {
	return f.k
}

// Add increments the counters for data and returns a receipt for removing that
// particular add. Repeated adds of the same key return independent receipts.
// If any counter would overflow, Add returns ErrCounterLimit without changing
// the filter.
func (f *CountingBloomFilter) Add(data []byte) (Receipt, error) {
	h := baseHashes(data)
	locations := f.locations(h)
	sort.Slice(locations, func(i, j int) bool {
		return locations[i] < locations[j]
	})

	for start := 0; start < len(locations); {
		location := locations[start]
		end := start + 1
		for end < len(locations) && locations[end] == location {
			end++
		}

		if uint64(f.counters[location])+uint64(end-start) > maxCounterValue {
			return nil, ErrCounterLimit
		}
		start = end
	}

	for _, location := range locations {
		f.counters[location]++
	}

	return &countingReceipt{
		filter:     f,
		generation: f.generation,
		h:          h,
	}, nil
}

// AddString adds a string and returns a receipt for that add.
func (f *CountingBloomFilter) AddString(data string) (Receipt, error) {
	return f.Add([]byte(data))
}

// Remove decrements exactly the counters from the add represented by receipt.
// A receipt can be used only once and only on the filter that issued it.
func (f *CountingBloomFilter) Remove(receipt Receipt) error {
	r, ok := receipt.(*countingReceipt)
	if !ok || r == nil {
		return ErrInvalidReceipt
	}
	if r.filter != f {
		return ErrWrongFilter
	}
	if r.generation != f.generation {
		return ErrReceiptExpired
	}
	if r.used {
		return ErrReceiptUsed
	}

	locations := f.locations(r.h)
	sort.Slice(locations, func(i, j int) bool {
		return locations[i] < locations[j]
	})

	for start := 0; start < len(locations); {
		location := locations[start]
		end := start + 1
		for end < len(locations) && locations[end] == location {
			end++
		}

		if uint64(f.counters[location]) < uint64(end-start) {
			return ErrInvalidReceipt
		}
		start = end
	}

	for start := 0; start < len(locations); {
		location := locations[start]
		end := start + 1
		for end < len(locations) && locations[end] == location {
			end++
		}

		f.counters[location] -= uint32(end - start)
		start = end
	}

	r.used = true
	r.h = [4]uint64{}
	return nil
}

// Test returns true if all counters for data are greater than zero. A true
// result may be a false positive; a false result means data is definitely not
// present.
func (f *CountingBloomFilter) Test(data []byte) bool {
	h := baseHashes(data)
	for i := uint(0); i < f.k; i++ {
		if f.counters[f.location(h, i)] == 0 {
			return false
		}
	}
	return true
}

// TestString returns true if the string may be in the filter.
func (f *CountingBloomFilter) TestString(data string) bool {
	return f.Test([]byte(data))
}

// ClearAll removes all keys and invalidates every receipt issued before the
// call. It also releases the filter-side storage for those receipts.
func (f *CountingBloomFilter) ClearAll() *CountingBloomFilter {
	for i := range f.counters {
		f.counters[i] = 0
	}
	f.generation++
	return f
}

func (f *CountingBloomFilter) location(h [4]uint64, i uint) uint {
	return uint(location(h, i) % uint64(f.m))
}

func (f *CountingBloomFilter) locations(h [4]uint64) []uint {
	locations := make([]uint, f.k)
	for i := uint(0); i < f.k; i++ {
		locations[i] = f.location(h, i)
	}
	return locations
}
