package hyperloglog

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testSlidingWindowPrecision = 14

func TestNewSlidingWindowValidation(t *testing.T) {
	tests := []struct {
		name        string
		window      time.Duration
		bucketWidth time.Duration
		precision   uint8
	}{
		{name: "zero window", window: 0, bucketWidth: time.Second, precision: 14},
		{name: "negative window", window: -time.Second, bucketWidth: time.Second, precision: 14},
		{name: "zero bucket width", window: time.Second, bucketWidth: 0, precision: 14},
		{name: "negative bucket width", window: time.Second, bucketWidth: -time.Second, precision: 14},
		{name: "window shorter than bucket", window: time.Second, bucketWidth: time.Minute, precision: 14},
		{name: "precision too low", window: time.Minute, bucketWidth: time.Second, precision: 3},
		{name: "precision too high", window: time.Minute, bucketWidth: time.Second, precision: 19},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSlidingWindow(tt.window, tt.bucketWidth, tt.precision, true)
			require.Error(t, err)
		})
	}
}

func TestSlidingWindowBucketsEstimateAndLateEvents(t *testing.T) {
	window := 30 * time.Minute
	width := 10 * time.Minute
	sw, err := NewSlidingWindow(window, width, testSlidingWindowPrecision, true)
	require.NoError(t, err)

	at := func(minute int) time.Time {
		return time.Unix(0, 0).Add(time.Duration(minute) * time.Minute)
	}
	hashForBucket := func(id uint64) uint64 { return id << 50 }

	require.True(t, sw.InsertHash(hashForBucket(1), at(1)))
	require.True(t, sw.InsertHash(hashForBucket(2), at(11)))
	require.True(t, sw.InsertHash(hashForBucket(3), at(21)))

	require.ElementsMatch(t, []int64{0, 10, 20}, activeBucketStartsMinute(t, sw, at(30)))
	require.Equal(t, uint64(3), sw.Estimate(at(30)))

	require.True(t, sw.InsertHash(hashForBucket(4), at(40)))
	require.False(t, sw.InsertHash(hashForBucket(99), at(9)), "event in an expired bucket is late")
	require.ElementsMatch(t, []int64{10, 20, 40}, activeBucketStartsMinute(t, sw, at(41)))
	require.Equal(t, uint64(3), sw.Estimate(at(41)))
}

func TestSlidingWindowNonDivisibleWindowBucketCapacity(t *testing.T) {
	sw, err := NewSlidingWindow(1500*time.Millisecond, time.Second, testSlidingWindowPrecision, true)
	require.NoError(t, err)
	require.Len(t, sw.buckets, 3)
}

func TestSlidingWindowLargeForwardJumpReusesStaleSlot(t *testing.T) {
	window := 30 * time.Minute
	width := 10 * time.Minute
	sw, err := NewSlidingWindow(window, width, testSlidingWindowPrecision, true)
	require.NoError(t, err)

	at := func(minute int) time.Time {
		return time.Unix(0, 0).Add(time.Duration(minute) * time.Minute)
	}
	hashForBucket := func(id uint64) uint64 { return id << 50 }

	require.True(t, sw.InsertHash(hashForBucket(1), at(1)))
	require.True(t, sw.InsertHash(hashForBucket(2), at(11)))
	require.True(t, sw.InsertHash(hashForBucket(3), at(21)))

	jumpTo := 130
	targetStart, targetSlot := sw.bucketFor(at(jumpTo).UnixNano())
	require.Equal(t, int64(130)*int64(time.Minute), targetStart)
	require.Equal(t, 1, targetSlot, "the jump lands on a slot previously used by bucket 10")

	require.True(t, sw.InsertHash(hashForBucket(4), at(40)))
	require.True(t, sw.InsertHash(hashForBucket(5), at(jumpTo)), "future events advance the watermark")
	require.False(t, sw.InsertHash(hashForBucket(6), at(20)), "old events are late after the jump")

	require.Equal(t, []int64{130}, activeBucketStartsMinute(t, sw, at(131)))
	require.Equal(t, uint64(1), sw.Estimate(at(131)))

	require.Len(t, sw.buckets, 4)
	require.Equal(t, 4, cap(sw.buckets))
	for minute := int64(140); minute <= 10_000; minute += 37 {
		require.True(t, sw.InsertHash(hashForBucket(uint64(minute)), at(int(minute))))
	}
	require.Equal(t, 4, cap(sw.buckets))
	require.Len(t, sw.buckets, 4)
}

func activeBucketStartsMinute(t *testing.T, sw *SlidingWindow, now time.Time) []int64 {
	t.Helper()

	sw.advanceTo(now.UnixNano())
	active := sw.activeBucketStarts(now.UnixNano())
	starts := make([]int64, 0, len(active.startTimes))
	for _, start := range active.startTimes {
		starts = append(starts, start/int64(time.Minute))
	}
	return starts
}
