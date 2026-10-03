package hyperloglog

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// SlidingWindow is a fixed-capacity ring of HyperLogLog sketches partitioned
// into fixed-width event-time buckets. Bucket boundaries are aligned to the
// Unix epoch.
//
// Events newer than the window watermark advance the watermark. Events whose
// bucket is entirely older than watermark-window are late and are dropped.
// Future events are accepted and advance the watermark; buckets skipped by the
// jump remain empty. Expired buckets are reset and reused; the window never
// subtracts counts from an aggregate sketch.
type SlidingWindow struct {
	window      time.Duration
	bucketWidth time.Duration
	precision   uint8
	sparse      bool

	buckets      []*Sketch
	bucketStarts []int64
	bucketInUse  []bool
	watermark    int64
	hasWatermark bool
}

// NewSlidingWindow returns a sliding window divided into fixed-width buckets.
// The ring holds every bucket that can intersect a window, including two
// partial boundary buckets. Every bucket uses an independent HyperLogLog
// sketch with the given precision.
func NewSlidingWindow(windowSize, bucketWidth time.Duration, precision uint8, sparse bool) (*SlidingWindow, error) {
	if windowSize <= 0 {
		return nil, fmt.Errorf("hyperloglog: window must be positive, got %s", windowSize)
	}
	if bucketWidth <= 0 {
		return nil, fmt.Errorf("hyperloglog: bucket width must be positive, got %s", bucketWidth)
	}
	if windowSize < bucketWidth {
		return nil, fmt.Errorf("hyperloglog: window %s is shorter than bucket width %s", windowSize, bucketWidth)
	}
	if err := checkPrecision(precision); err != nil {
		return nil, err
	}

	bucketCount := int64(windowSize/bucketWidth) + 1
	if windowSize%bucketWidth != 0 {
		bucketCount++
	}
	if int64(int(bucketCount)) != bucketCount {
		return nil, errors.New("hyperloglog: window contains too many buckets")
	}

	return &SlidingWindow{
		window:       windowSize,
		bucketWidth:  bucketWidth,
		precision:    precision,
		sparse:       sparse,
		buckets:      make([]*Sketch, bucketCount),
		bucketStarts: make([]int64, bucketCount),
		bucketInUse:  make([]bool, bucketCount),
	}, nil
}

// Insert hashes event with the package's MetroHash64 seed and adds it to the
// bucket containing eventTime. It reports whether the event was accepted.
func (w *SlidingWindow) Insert(event []byte, eventTime time.Time) bool {
	return w.InsertHash(hash(event), eventTime)
}

// InsertHash adds hash to the bucket containing eventTime. It reports whether
// the event was accepted.
func (w *SlidingWindow) InsertHash(hashValue uint64, eventTime time.Time) bool {
	eventNanos := eventTime.UnixNano()
	bucketStart, slot := w.bucketFor(eventNanos)

	if w.hasWatermark && bucketStart < w.cutoffStart(w.watermark) {
		return false
	}
	if !w.hasWatermark || eventNanos > w.watermark {
		w.watermark = eventNanos
		w.hasWatermark = true
	}
	w.advanceTo(w.watermark)

	if !w.bucketInUse[slot] {
		if w.buckets[slot] == nil {
			w.buckets[slot] = newSketchNoError(w.precision, w.sparse)
		}
		w.bucketStarts[slot] = bucketStart
		w.bucketInUse[slot] = true
	}
	w.buckets[slot].InsertHash(hashValue)
	return true
}

// Estimate returns the cardinality estimate for [now-window, now). Queries use
// the timestamp supplied by the caller and never read or mutate a caller clock.
// A query after now advances the window's internal retention watermark;
// backdated queries do not revive expired or future buckets.
func (w *SlidingWindow) Estimate(now time.Time) uint64 {
	nowNanos := now.UnixNano()
	if !w.hasWatermark {
		return 0
	}
	if nowNanos > w.watermark {
		w.watermark = nowNanos
	}

	w.advanceTo(w.watermark)
	activeStarts := w.activeBucketStarts(nowNanos)
	if len(activeStarts.startTimes) == 0 {
		return 0
	}

	merged := newSketchNoError(w.precision, w.sparse)
	for _, slot := range activeStarts.slotIndexes {
		if err := merged.Merge(w.buckets[slot]); err != nil {
			panic(err)
		}
	}
	return merged.Estimate()
}

type activeWindowBuckets struct {
	startTimes  []int64
	slotIndexes []int
}

func (w *SlidingWindow) activeBucketStarts(nowNanos int64) activeWindowBuckets {
	cutoff := w.cutoffStart(w.watermark)
	active := activeWindowBuckets{}
	for slot, inUse := range w.bucketInUse {
		start := w.bucketStarts[slot]
		if inUse && start >= cutoff && start < nowNanos {
			active.startTimes = append(active.startTimes, start)
			active.slotIndexes = append(active.slotIndexes, slot)
		}
	}
	return active
}

func (w *SlidingWindow) advanceTo(nowNanos int64) {
	if nowNanos > w.watermark {
		w.watermark = nowNanos
	}
	w.expireBefore(w.cutoffStart(w.watermark))
}

func (w *SlidingWindow) bucketFor(eventNanos int64) (int64, int) {
	width := int64(w.bucketWidth)
	quotient := eventNanos / width
	if remainder := eventNanos % width; remainder < 0 {
		quotient--
	}

	slot := quotient % int64(len(w.buckets))
	if slot < 0 {
		slot += int64(len(w.buckets))
	}
	return quotient * width, int(slot)
}

func (w *SlidingWindow) cutoffStart(referenceNanos int64) int64 {
	start, _ := w.bucketFor(referenceNanos - int64(w.window))
	return start
}

func (w *SlidingWindow) expireBefore(cutoff int64) {
	for slot, inUse := range w.bucketInUse {
		if inUse && w.bucketStarts[slot] < cutoff {
			w.buckets[slot].Reset()
			w.bucketInUse[slot] = false
			w.bucketStarts[slot] = math.MinInt64
		}
	}
}
