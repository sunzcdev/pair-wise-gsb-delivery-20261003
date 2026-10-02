// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// synthResponse builds a valid NTPv4 Response with the specified clock offset
// and synchronization distance. It performs no network or system-clock access.
func synthResponse(offset, distance time.Duration) *Response {
	t := timestamp(1 << 32).TimeV4()
	return &Response{
		ClockOffset:    offset,
		RootDistance:   distance,
		Version:        4,
		Stratum:        1,
		Time:           t,
		ReferenceTime:  t,
		RootDelay:      1 * time.Millisecond,
		RootDispersion: 1 * time.Millisecond,
		Leap:           LeapNoWarning,
	}
}

func src(id string, offset, distance time.Duration) TimeSource {
	return TimeSource{SourceID: id, Response: synthResponse(offset, distance)}
}

func TestOfflineSelectClockOffsetBoundaryTouch(t *testing.T) {
	// Three consistent sources whose closed intervals only meet at the
	// single boundary offset +5ms, plus two distant outliers.
	sources := []TimeSource{
		src("a", -5*time.Millisecond, 5*time.Millisecond), // [-10ms, 0]
		src("b", 0, 0), // [0, 0]
		src("c", 5*time.Millisecond, 5*time.Millisecond), // [0, +10ms]
		src("d", 1000*time.Millisecond, 5*time.Millisecond),
		src("e", -1000*time.Millisecond, 5*time.Millisecond),
	}

	result, err := SelectClockOffset(sources, SelectOptions{})
	assert.NoError(t, err)
	assert.NotNil(t, result)

	// Boundary touching is a valid zero-width intersection.
	assert.Equal(t, time.Duration(0), result.WindowLower)
	assert.Equal(t, time.Duration(0), result.WindowUpper)
	assert.Equal(t, 3, result.Quorum)

	// Median of -5ms, 0 and +5ms is 0; outliers are not averaged in.
	assert.Equal(t, time.Duration(0), result.Offset)

	included := result.IncludedSources()
	assert.Len(t, included, 3)
	for i, wantID := range []string{"a", "b", "c"} {
		assert.Equal(t, wantID, included[i].SourceID)
		assert.True(t, included[i].Included)
		assert.Equal(t, ReasonTruechimer, included[i].Reason)
	}

	for _, idx := range []int{3, 4} {
		assert.False(t, result.Sources[idx].Included)
		assert.Equal(t, ReasonFalseticker, result.Sources[idx].Reason)
	}
}

func TestOfflineSelectNoMajorityIntersection(t *testing.T) {
	// Three mutually disjoint sources. No pair intersects, so no majority
	// exists and the average of all sources (100ms) must never be returned.
	sources := []TimeSource{
		src("a", 0, 1*time.Millisecond),
		src("b", 100*time.Millisecond, 1*time.Millisecond),
		src("c", 200*time.Millisecond, 1*time.Millisecond),
	}

	result, err := SelectClockOffset(sources, SelectOptions{})
	assert.ErrorIs(t, err, ErrNoConsistentIntersection)
	assert.NotNil(t, result)
	assert.Equal(t, time.Duration(0), result.Offset, "must not average all sources")
	assert.Len(t, result.IncludedSources(), 0)

	for _, status := range result.Sources {
		assert.False(t, status.Included)
		assert.Equal(t, ReasonFalseticker, status.Reason)
	}
}

func TestOfflineSelectDuplicateSourceVotesOnce(t *testing.T) {
	// Four samples from only three distinct sources. The duplicate is an
	// outlier and must neither cast a vote nor raise the quorum.
	sources := []TimeSource{
		src("a", 0, 5*time.Millisecond),
		src("b", 2*time.Millisecond, 5*time.Millisecond),
		src("a", 1000*time.Millisecond, 5*time.Millisecond), // duplicate
		src("c", 4*time.Millisecond, 7*time.Millisecond),    // [-3ms, +11ms]
	}

	result, err := SelectClockOffset(sources, SelectOptions{})
	assert.NoError(t, err)

	// Three distinct voters, so the default quorum is 2 rather than 3.
	assert.Equal(t, 2, result.Quorum)
	assert.Len(t, result.IncludedSources(), 3)

	dup := result.Sources[2]
	assert.False(t, dup.Included)
	assert.Equal(t, ReasonDuplicate, dup.Reason)
	assert.Contains(t, dup.Detail, "duplicate")

	assert.Equal(t, 2*time.Millisecond, result.Offset) // median of 0, 2ms, 4ms
	assert.Equal(t, -3*time.Millisecond, result.WindowLower)
	assert.Equal(t, 5*time.Millisecond, result.WindowUpper)
}

func TestOfflineSelectInvalidSampleDoesNotClaimVote(t *testing.T) {
	// Source "a" first appears with an invalid sample; its later valid
	// sample must still be allowed to vote.
	bad := synthResponse(0, 5*time.Millisecond)
	bad.Stratum = maxStratum // causes ErrInvalidStratum

	sources := []TimeSource{
		{SourceID: "a", Response: bad},
		src("a", 0, 5*time.Millisecond),
		src("b", 2*time.Millisecond, 5*time.Millisecond),
	}
	result, err := SelectClockOffset(sources, SelectOptions{})
	assert.NoError(t, err)
	assert.Equal(t, 2, result.Quorum)

	assert.False(t, result.Sources[0].Included)
	assert.Equal(t, ReasonInvalid, result.Sources[0].Reason)
	assert.NotEmpty(t, result.Sources[0].Detail)
	assert.True(t, result.Sources[1].Included)
	assert.True(t, result.Sources[2].Included)
}

func TestOfflineSelectExcludedSamples(t *testing.T) {
	negative := synthResponse(0, 5*time.Millisecond)
	negative.RootDistance = -1 * time.Nanosecond

	sources := []TimeSource{
		{SourceID: "nil", Response: nil},
		{SourceID: "neg", Response: negative},
		src("ok1", 0, 5*time.Millisecond),
		src("ok2", 1*time.Millisecond, 5*time.Millisecond),
	}
	result, err := SelectClockOffset(sources, SelectOptions{})
	assert.NoError(t, err)
	assert.Len(t, result.IncludedSources(), 2)

	assert.Equal(t, ReasonInvalid, result.Sources[0].Reason)
	assert.Equal(t, ReasonInvalid, result.Sources[1].Reason)

	// No usable samples at all.
	result, err = SelectClockOffset(nil, SelectOptions{})
	assert.ErrorIs(t, err, ErrNoValidSources)
	assert.NotNil(t, result)
}

func TestOfflineSelectMinSources(t *testing.T) {
	sources := []TimeSource{
		src("a", 0, 10*time.Millisecond),
		src("b", 1*time.Millisecond, 10*time.Millisecond),
		src("c", 2*time.Millisecond, 10*time.Millisecond),
	}

	result, err := SelectClockOffset(sources, SelectOptions{MinSources: 3})
	assert.NoError(t, err)
	assert.Len(t, result.IncludedSources(), 3)

	_, err = SelectClockOffset(sources, SelectOptions{MinSources: 4})
	assert.ErrorIs(t, err, ErrInsufficientSources)

	_, err = SelectClockOffset(sources, SelectOptions{MinSources: -1})
	assert.ErrorIs(t, err, ErrInvalidMinSources)

	// A single valid source succeeds with MinSources 1.
	result, err = SelectClockOffset(
		[]TimeSource{src("solo", 7*time.Millisecond, 1*time.Millisecond)},
		SelectOptions{MinSources: 1},
	)
	assert.NoError(t, err)
	assert.Equal(t, 7*time.Millisecond, result.Offset)
	assert.Equal(t, 6*time.Millisecond, result.WindowLower)
	assert.Equal(t, 8*time.Millisecond, result.WindowUpper)
}

func TestOfflineSelectLowestWindowWins(t *testing.T) {
	// With a sub-majority quorum, two disjoint windows each have quorum
	// coverage. The lowest window must be selected deterministically.
	sources := []TimeSource{
		src("a", 0, 0),
		src("b", 0, 0),
		src("c", 100*time.Millisecond, 0),
		src("d", 100*time.Millisecond, 0),
	}

	for range 10 {
		result, err := SelectClockOffset(sources, SelectOptions{MinSources: 2})
		assert.NoError(t, err)
		assert.Equal(t, time.Duration(0), result.WindowLower)
		assert.Equal(t, time.Duration(0), result.WindowUpper)
		assert.Equal(t, time.Duration(0), result.Offset)
		assert.Len(t, result.IncludedSources(), 2)
		assert.Equal(t, "a", result.IncludedSources()[0].SourceID)
		assert.Equal(t, "b", result.IncludedSources()[1].SourceID)
		assert.Equal(t, ReasonFalseticker, result.Sources[2].Reason)
		assert.Equal(t, ReasonFalseticker, result.Sources[3].Reason)
	}
}

func TestOfflineSelectTruechimerMustContainWindow(t *testing.T) {
	// With quorum 2 the lowest window is A∩B = [0,10]. Interval C contains
	// the window's upper endpoint (10) but not its lower endpoint (0), so it
	// merely overlaps the window and must not join the consistent set.
	sources := []TimeSource{
		src("a", 5*time.Millisecond, 5*time.Millisecond),     // [0, 10]
		src("b", 5*time.Millisecond, 5*time.Millisecond),     // [0, 10]
		src("late", 10*time.Millisecond, 5*time.Millisecond), // [5, 15]
	}

	result, err := SelectClockOffset(sources, SelectOptions{})
	assert.NoError(t, err)

	// The lowest quorum window is [0,10], defined by a and b only.
	assert.Equal(t, time.Duration(0), result.WindowLower)
	assert.Equal(t, 10*time.Millisecond, result.WindowUpper)
	assert.Len(t, result.IncludedSources(), 2)
	assert.Equal(t, ReasonFalseticker, result.Sources[2].Reason)
	assert.Equal(t, 5*time.Millisecond, result.Offset) // median of a and b
}

func TestOfflineSelectEvenMedianRoundsTowardZero(t *testing.T) {
	// Two truechimers with offsets 0 and 1ns; midpoint 0.5ns truncates to 0.
	sources := []TimeSource{
		src("a", 0, 10*time.Millisecond),
		src("b", 1*time.Nanosecond, 10*time.Millisecond),
	}
	result, err := SelectClockOffset(sources, SelectOptions{})
	assert.NoError(t, err)
	assert.Equal(t, time.Duration(0), result.Offset)
}

func TestOfflineSelectDurationOverflow(t *testing.T) {
	max := time.Duration(math.MaxInt64)
	min := time.Duration(math.MinInt64)

	sources := []TimeSource{
		src("pos-overflow", max, 1), // max + 1 overflows
		src("neg-overflow", min, 1), // min - 1 overflows
		src("valid", max, 0),        // zero-width interval is valid
	}
	result, err := SelectClockOffset(sources, SelectOptions{})
	assert.NoError(t, err)
	assert.Equal(t, max, result.Offset)
	assert.Equal(t, max, result.WindowLower)
	assert.Equal(t, max, result.WindowUpper)

	assert.False(t, result.Sources[0].Included)
	assert.Equal(t, ReasonOverflow, result.Sources[0].Reason)
	assert.False(t, result.Sources[1].Included)
	assert.Equal(t, ReasonOverflow, result.Sources[1].Reason)
	assert.True(t, result.Sources[2].Included)
}

func TestOfflineSelectIsDeterministic(t *testing.T) {
	sources := []TimeSource{
		src("a", 3*time.Millisecond, 2*time.Millisecond),
		src("b", 3*time.Millisecond, 2*time.Millisecond), // equal offset
		src("c", -1*time.Millisecond, 2*time.Millisecond),
		src("d", 100*time.Millisecond, 1*time.Millisecond),
		src("b", 4*time.Millisecond, 2*time.Millisecond), // duplicate
	}

	var first *SelectionResult
	for range 25 {
		result, err := SelectClockOffset(sources, SelectOptions{})
		assert.NoError(t, err)
		if first == nil {
			first = result
			continue
		}
		assert.Equal(t, first.Offset, result.Offset)
		assert.Equal(t, first.WindowLower, result.WindowLower)
		assert.Equal(t, first.WindowUpper, result.WindowUpper)
		assert.Equal(t, first.Quorum, result.Quorum)
		for i := range sources {
			assert.Equal(t, first.Sources[i], result.Sources[i])
		}
	}

	// Equal offsets are ordered by input index.
	included := first.IncludedSources()
	// IncludedSources reports samples in input order.
	assert.Equal(t, "a", included[0].SourceID) // 3ms, earlier index
	assert.Equal(t, "b", included[1].SourceID) // 3ms, later index
	assert.Equal(t, "c", included[2].SourceID) // -1ms
	assert.Equal(t, 3*time.Millisecond, first.Offset)
}

func TestOfflineSelectMidpoint(t *testing.T) {
	max := time.Duration(math.MaxInt64)
	min := time.Duration(math.MinInt64)
	cases := []struct {
		name string
		a, b time.Duration
		want time.Duration
	}{
		{"positive half rounds toward zero", 0, 1, 0},
		{"negative half rounds toward zero", -1, 0, 0},
		{"opposite signs", -1, 1, 0},
		{"exact", -4, 2, -1},
		{"max pair", max, max, max},
		{"min pair", min, min, min},
		{"extremes", max, min, 0},
		{"max and max-1", max, max - 1, max - 1},
		{"min and min+1", min, min + 1, min + 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, midpoint(c.a, c.b))
			assert.Equal(t, c.want, midpoint(c.b, c.a))
		})
	}
}

func TestOfflineSelectResultErrorsAreSentinels(t *testing.T) {
	assert.True(t, errors.Is(ErrNoConsistentIntersection, ErrNoConsistentIntersection))
	assert.True(t, errors.Is(ErrNoValidSources, ErrNoValidSources))
	assert.True(t, errors.Is(ErrInsufficientSources, ErrInsufficientSources))
	assert.True(t, errors.Is(ErrInvalidMinSources, ErrInvalidMinSources))
}
