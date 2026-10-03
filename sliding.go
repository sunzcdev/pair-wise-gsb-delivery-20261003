package hyperloglog

import (
	"errors"
	"fmt"
	"time"
)

// maxWindowBuckets bounds the number of buckets a SlidingWindow keeps. Each
// bucket owns a dense Sketch of 2^precision registers, so the ring size is
// the main driver of the window's worst-case memory usage.
const maxWindowBuckets = 1 << 20

// ErrorInvalidWindow is returned by NewSlidingWindow when the window, bucket
// width, or sketch precision fail validation.
var ErrorInvalidWindow = errors.New("hyperloglog: invalid sliding window parameters")

// SlidingWindow is a fixed-width sliding window built on top of Sketch. Events
// are routed into buckets derived from their caller-supplied event time, and a
// query merges the buckets that still overlap the window ending at the query
// time.
//
// Buckets that fall out of the window are cleared in place; their counts are
// never subtracted from an aggregate Sketch, which avoids the error that
// subtracting two HyperLogLog estimates would introduce. The state is held in
// a fixed-size ring, so memory stays bounded no matter how far event or query
// times jump ahead.
//
// Event time is supplied on every call; SlidingWindow never reads a wall
// clock and never mutates the times a caller passes in.
type SlidingWindow struct {
	precision uint8
	width     int64 // bucket width in nanoseconds
	window    int64 // window length in nanoseconds
	numBucket int64 // window / width, the number of covered bucket intervals

	// ring holds numBucket+1 slots: one extra slot is needed because a window
	// aligned away from a bucket boundary overlaps numBucket+1 buckets.
	slots []*Sketch

	// head is the most recent bucket ID observed (on insert or query); started
	// reports whether any time has been observed yet.
	head    int64
	started bool

	scratch *Sketch // dense merge target reused by EstimateAt
}

// NewSlidingWindow returns a SlidingWindow whose buckets are wide bucketWidth
// and whose query window covers window of event time. Each bucket keeps a
// dense Sketch with 2^precision registers (precision must be 4..18).
//
// bucketWidth must be positive, window must cover at least one bucket, and the
// resulting number of buckets must not exceed 2^20; otherwise
// ErrorInvalidWindow is returned, possibly wrapping ErrorInvalidPrecision.
func NewSlidingWindow(window time.Duration, bucketWidth time.Duration, precision uint8) (*SlidingWindow, error) {
	if err := checkPrecision(precision); err != nil {
		return nil, err
	}
	width := int64(bucketWidth)
	if width <= 0 {
		return nil, fmt.Errorf("hyperloglog: bucket width must be positive: %w", ErrorInvalidWindow)
	}
	w := int64(window)
	if w < width {
		return nil, fmt.Errorf("hyperloglog: window must be at least one bucket wide: %w", ErrorInvalidWindow)
	}
	numBucket := floorDiv(w-1, width) + 1
	if numBucket <= 0 || numBucket > maxWindowBuckets {
		return nil, fmt.Errorf("hyperloglog: window spans too many buckets: %w", ErrorInvalidWindow)
	}

	capacity := int(numBucket + 1)
	return &SlidingWindow{
		precision: precision,
		width:     width,
		window:    w,
		numBucket: numBucket,
		slots:     make([]*Sketch, capacity),
		scratch:   newDense(precision),
	}, nil
}

func newDense(precision uint8) *Sketch {
	return newSketchNoError(precision, false)
}

// bucketID maps an event time to its fixed-width bucket. Buckets are intervals
// [id*width, (id+1)*width), including for times before the epoch, so IDs use
// floor division rather than Go's truncated division.
func (sw *SlidingWindow) bucketID(t time.Time) int64 {
	return floorDiv(t.UnixNano(), sw.width)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b) != 0 && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func ringIndex(id, size int64) int {
	idx := id % size
	if idx < 0 {
		idx += size
	}
	return int(idx)
}

// advance moves the most-recent bucket forward to id and clears every bucket
// that has dropped out of the window since the previous head. Slots skipped by
// a jump of several buckets are cleared as well, so a reused slot can never be
// mistaken for the bucket that now maps to it.
func (sw *SlidingWindow) advance(id int64) {
	size := sw.numBucket + 1
	if !sw.started {
		sw.head = id
		sw.started = true
		return
	}
	if id <= sw.head {
		return
	}

	// Buckets are retained while they can still overlap a window ending at the
	// head, i.e. [head-numBucket, head]. Advancing sw.head to id expires
	// [sw.head-numBucket, id-numBucket-1]. For a short advance those buckets
	// are cleared directly; a jump of a full ring or more is disjoint from
	// every occupied slot, so the whole ring is cleared. Work is bounded by
	// min(jump, ring size), and a skipped slot is never mistaken for the
	// bucket that newly maps to it. id-size cannot overflow because IDs are
	// bounded by the nanosecond time range and the ring is at most 2^20 wide.
	if sw.head > id-size {
		firstExpired := sw.head - sw.numBucket
		lastExpired := id - sw.numBucket - 1
		for e := firstExpired; e <= lastExpired; e++ {
			idx := ringIndex(e, size)
			if sw.slots[idx] != nil {
				sw.slots[idx] = nil
			}
		}
	} else {
		for idx := range sw.slots {
			sw.slots[idx] = nil
		}
	}
	sw.head = id
}

// slot returns the bucket Sketch for id, creating it on first use. A reused
// slot always maps to an expired bucket that advance has already cleared, so a
// nil slot is unambiguously empty.
func (sw *SlidingWindow) slot(id int64) *Sketch {
	idx := ringIndex(id, sw.numBucket+1)
	if sw.slots[idx] == nil {
		sw.slots[idx] = newDense(sw.precision)
	}
	return sw.slots[idx]
}

// Insert hashes e with the package's MetroHash64 seed and records it in the
// bucket covering t.
//
// Late events whose bucket can no longer overlap a window ending at the most
// recent observed event time are dropped. Future events are accepted and move
// the window forward; buckets skipped by the jump are cleared.
func (sw *SlidingWindow) Insert(t time.Time, e []byte) {
	sw.InsertHash(t, hash(e))
}

// InsertHash records an already computed 64-bit hash in the bucket covering t,
// following the same late and future event rules as Insert.
func (sw *SlidingWindow) InsertHash(t time.Time, h uint64) {
	id := sw.bucketID(t)
	sw.advance(id)
	// Retain buckets that can still overlap a window ending at the head,
	// [head-numBucket, head]; older events are late and dropped.
	if id < sw.head-sw.numBucket {
		return // late event: its bucket has aged out
	}
	sw.slot(id).InsertHash(h)
}

// EstimateAt returns the distinct-count estimate over the window ending at t.
// It merges every live bucket whose interval overlaps [t-window, t], ignoring
// empty and aged-out buckets.
//
// A query at a later time advances the window and clears expired buckets; t is
// taken by value and the caller's clock or timestamp is never modified.
func (sw *SlidingWindow) EstimateAt(t time.Time) uint64 {
	id := sw.bucketID(t)
	sw.advance(id)

	// Retain buckets intersecting [t-window, t]: the oldest covered bucket is
	// floorDiv(t-window, width).
	oldest := floorDiv(t.UnixNano()-sw.window, sw.width)
	if oldest < sw.head-sw.numBucket {
		oldest = sw.head - sw.numBucket
	}

	sw.scratch.Reset()
	for b := oldest; b <= id; b++ {
		idx := ringIndex(b, sw.numBucket+1)
		if sk := sw.slots[idx]; sk != nil {
			_ = sw.scratch.Merge(sk)
		}
	}
	return sw.scratch.Estimate()
}

// Reset empties every bucket and forgets the observed time. The ring capacity
// is retained.
func (sw *SlidingWindow) Reset() {
	for i := range sw.slots {
		sw.slots[i] = nil
	}
	sw.head = 0
	sw.started = false
	sw.scratch.Reset()
}
