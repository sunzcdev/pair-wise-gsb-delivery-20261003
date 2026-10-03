package hyperloglog

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// occupiedSlots reports the bucket IDs currently held by the ring, paired with
// whether their Sketch is non-empty.
func occupiedSlots(sw *SlidingWindow) map[int64]bool {
	got := make(map[int64]bool)
	size := sw.numBucket + 1
	for idx, sk := range sw.slots {
		if sk == nil {
			continue
		}
		headIdx := ringIndex(sw.head, size)
		b := sw.head - int64((headIdx-idx+int(size))%int(size))
		got[b] = sk.Estimate() > 0
	}
	return got
}

// queryBucketIDs mirrors EstimateAt's bucket selection so tests can assert
// exactly which buckets a query merges.
func queryBucketIDs(sw *SlidingWindow, t time.Time) []int64 {
	id := sw.bucketID(t)
	oldest := floorDiv(t.UnixNano()-sw.window, sw.width)
	if oldest < sw.head-sw.numBucket {
		oldest = sw.head - sw.numBucket
	}
	var ids []int64
	for b := oldest; b <= id; b++ {
		if sk := sw.slots[ringIndex(b, sw.numBucket+1)]; sk != nil {
			ids = append(ids, b)
		}
	}
	return ids
}

func TestSlidingWindow_NewErrors(t *testing.T) {
	cases := []struct {
		name      string
		window    time.Duration
		width     time.Duration
		precision uint8
		wrap      error
	}{
		{"precision too small", time.Minute, time.Second, 3, ErrorInvalidPrecision},
		{"precision too large", time.Minute, time.Second, 19, ErrorInvalidPrecision},
		{"zero width", time.Minute, 0, 14, ErrorInvalidWindow},
		{"negative width", time.Minute, -time.Second, 14, ErrorInvalidWindow},
		{"window shorter than width", time.Second, time.Minute, 14, ErrorInvalidWindow},
		{"too many buckets", (1<<20 + 1) * time.Second, time.Second, 14, ErrorInvalidWindow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSlidingWindow(tc.window, tc.width, tc.precision)
			require.Error(t, err)
			require.ErrorIs(t, err, tc.wrap)
		})
	}

	sw, err := NewSlidingWindow((1<<20)*time.Second, time.Second, 14)
	require.NoError(t, err)
	require.Equal(t, int64(1<<20), sw.numBucket)

	_, err = NewSlidingWindow(time.Second, time.Second, 14)
	require.NoError(t, err)
}

func TestFloorDivAndRingIndex(t *testing.T) {
	require.Equal(t, int64(2), floorDiv(20, 7))
	require.Equal(t, int64(-3), floorDiv(-20, 7))
	require.Equal(t, int64(-2), floorDiv(-14, 7))
	require.Equal(t, 0, ringIndex(0, 4))
	require.Equal(t, 3, ringIndex(-1, 4))
	require.Equal(t, 1, ringIndex(-3, 4))

	sw, err := NewSlidingWindow(3*time.Second, time.Second, 14)
	require.NoError(t, err)
	// Half a second before the epoch still belongs to bucket -1.
	require.Equal(t, int64(-1), sw.bucketID(time.Unix(-1, 500_000_000)))
	require.Equal(t, int64(0), sw.bucketID(time.Unix(0, 0)))
}

func TestSlidingWindow_BucketMergeAndExpiry(t *testing.T) {
	const width = time.Second
	sw, err := NewSlidingWindow(3*time.Second, width, 14)
	require.NoError(t, err)
	require.Equal(t, int64(3), sw.numBucket)
	require.Len(t, sw.slots, 4)

	base := time.Unix(1000, 0)
	b := sw.bucketID(base)

	insert := func(offset time.Duration, name string) {
		sw.Insert(base.Add(offset), []byte(name))
	}

	insert(0, "A")
	insert(time.Second, "B")
	insert(2*time.Second, "C")
	insert(3*time.Second, "D")

	// Every live bucket is occupied.
	require.Equal(t, map[int64]bool{b: true, b + 1: true, b + 2: true, b + 3: true}, occupiedSlots(sw))

	// A query at the boundary merges all four overlapping buckets.
	require.Equal(t, []int64{b, b + 1, b + 2, b + 3}, queryBucketIDs(sw, base.Add(3*time.Second)))
	require.EqualValues(t, 4, sw.EstimateAt(base.Add(3*time.Second)))

	// A query one second earlier merges only the first three buckets.
	require.Equal(t, []int64{b, b + 1, b + 2}, queryBucketIDs(sw, base.Add(2*time.Second)))
	require.EqualValues(t, 3, sw.EstimateAt(base.Add(2*time.Second)))

	// Sub-second movement stays in the same bucket set.
	require.Equal(t, []int64{b, b + 1, b + 2, b + 3}, queryBucketIDs(sw, base.Add(3999*time.Millisecond)))

	// Advancing one second expires bucket b (A) directly; it is cleared, never
	// subtracted from any aggregate.
	require.EqualValues(t, 3, sw.EstimateAt(base.Add(4*time.Second)))
	require.Equal(t, []int64{b + 1, b + 2, b + 3}, queryBucketIDs(sw, base.Add(4*time.Second)))

	insert(4*time.Second, "E")
	require.EqualValues(t, 4, sw.EstimateAt(base.Add(4*time.Second)))

	// A late event for the expired bucket is dropped.
	insert(0, "late-A")
	require.EqualValues(t, 4, sw.EstimateAt(base.Add(4*time.Second)))
}

func TestSlidingWindow_FutureJumpClearsAndBoundsMemory(t *testing.T) {
	sw, err := NewSlidingWindow(3*time.Second, time.Second, 14)
	require.NoError(t, err)

	base := time.Unix(1000, 0)
	sw.Insert(base, []byte("old"))

	// Advance time by far more than one ring width.
	future := base.Add(1_000_000 * time.Second)
	sw.Insert(future, []byte("new"))

	nonNil := 0
	for _, sk := range sw.slots {
		if sk != nil {
			nonNil++
		}
	}
	require.Equal(t, 1, nonNil, "jump must clear all skipped and expired slots")
	require.Len(t, sw.slots, 4, "ring allocation must not grow")

	// The old element cannot reappear through a reused slot.
	require.EqualValues(t, 1, sw.EstimateAt(future))

	// Walking forward one bucket at a time keeps at most one Sketch per slot.
	for i := int64(1); i <= 50; i++ {
		sw.InsertHash(future.Add(time.Duration(i)*time.Second), uint64(1000+i))
		require.LessOrEqual(t, nonNilCount(sw), 4)
	}
	require.Len(t, sw.slots, 4)

	// Another huge jump from a populated ring still bounds memory and work.
	far := future.Add(10_000_000 * time.Second)
	require.EqualValues(t, 0, sw.EstimateAt(far))
	require.Equal(t, 0, nonNilCount(sw))
	require.Len(t, sw.slots, 4)
}

func nonNilCount(sw *SlidingWindow) int {
	n := 0
	for _, sk := range sw.slots {
		if sk != nil {
			n++
		}
	}
	return n
}

func TestSlidingWindow_SlotReuseAfterFullRing(t *testing.T) {
	sw, err := NewSlidingWindow(3*time.Second, time.Second, 14)
	require.NoError(t, err)
	base := time.Unix(1000, 0)

	sw.Insert(base, []byte("wraps"))
	idx := ringIndex(sw.bucketID(base), sw.numBucket+1)

	// Advancing by exactly the ring size lands the new bucket on the same
	// slot; it must be empty rather than the previous occupant.
	t4 := base.Add(4 * time.Second)
	require.Equal(t, idx, ringIndex(sw.bucketID(t4), sw.numBucket+1))
	sw.Insert(t4, []byte("fresh"))
	require.EqualValues(t, 1, sw.EstimateAt(t4))
}

func TestSlidingWindow_QueryDoesNotMutateCallerClock(t *testing.T) {
	sw, err := NewSlidingWindow(3*time.Second, time.Second, 14)
	require.NoError(t, err)
	base := time.Unix(1000, 0)
	sw.Insert(base, []byte("A"))
	sw.Insert(base.Add(time.Second), []byte("B"))

	query := base.Add(5 * time.Second)
	snapshot := query
	require.EqualValues(t, 0, sw.EstimateAt(query))
	require.Equal(t, snapshot, query, "query must not alter the caller's time")

	// The query advanced the window: re-querying at the old time does not
	// resurrect expired buckets.
	require.EqualValues(t, 0, sw.EstimateAt(base.Add(time.Second)))

	// Repeated identical queries are stable.
	require.Equal(t, sw.EstimateAt(query), sw.EstimateAt(query))
}

func TestSlidingWindow_EstimateAccuracy(t *testing.T) {
	sw, err := NewSlidingWindow(time.Minute, time.Minute, 14)
	require.NoError(t, err)
	base := time.Unix(1000, 0)

	const n = 1000
	for i := 0; i < n; i++ {
		sw.Insert(base, []byte(fmt.Sprintf("distinct-user-%d", i)))
	}
	got := sw.EstimateAt(base.Add(30 * time.Second))
	ratio := 100.0 * float64(abs64(int64(got)-n)) / n
	require.LessOrEqual(t, ratio, 5.0, "got %d for exact %d", got, n)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestSlidingWindow_Reset(t *testing.T) {
	sw, err := NewSlidingWindow(3*time.Second, time.Second, 14)
	require.NoError(t, err)
	base := time.Unix(1000, 0)
	sw.Insert(base, []byte("A"))
	sw.Reset()

	require.False(t, sw.started)
	require.Equal(t, 0, nonNilCount(sw))
	require.Len(t, sw.slots, 4)
	require.EqualValues(t, 0, sw.EstimateAt(base))

	sw.Insert(base, []byte("A"))
	require.EqualValues(t, 1, sw.EstimateAt(base))
}
