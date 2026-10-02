// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selectBaseTime is a fixed, valid NTPv4 transmit time used by synthetic
// responses. It never touches the system clock.
var selectBaseTime = timestamp(1 << 32).TimeV4()

// makeSelectResponse builds a synthetic NTPv4 Response that passes
// Validate and reports the requested clock offset and synchronization
// distance. RootDistance is a client-calculated field, so it is set
// directly; RootDelay/RootDispersion are set to keep Validate happy
// independently of RootDistance.
func makeSelectResponse(offset, rootDistance time.Duration) *Response {
	return &Response{
		ClockOffset:    offset,
		Time:           selectBaseTime,
		RTT:            10 * time.Millisecond,
		Precision:      time.Millisecond,
		Version:        4,
		Stratum:        2,
		ReferenceTime:  selectBaseTime,
		RootDelay:      1 * time.Millisecond,
		RootDispersion: 1 * time.Millisecond,
		RootDistance:   rootDistance,
		Leap:           LeapNoWarning,
		Poll:           64 * time.Second,
		Flags:          FlagSynchronized,
	}
}

// sampleByIndex indexes the returned decisions by sample index.
func sampleByIndex(r *SelectionResult) map[int]SourceDecision {
	m := make(map[int]SourceDecision, len(r.Decisions))
	for _, d := range r.Decisions {
		m[d.SampleIndex] = d
	}
	return m
}

func TestSelectTimeBasicConsensus(t *testing.T) {
	// Three sources agree on an offset near 100ms with distances small
	// enough for their intervals to overlap; a fourth is an outlier.
	samples := []Sample{
		{"a", makeSelectResponse(100*time.Millisecond, 10*time.Millisecond)},
		{"b", makeSelectResponse(104*time.Millisecond, 10*time.Millisecond)},
		{"c", makeSelectResponse(96*time.Millisecond, 10*time.Millisecond)},
		{"d", makeSelectResponse(5*time.Second, 10*time.Millisecond)},
	}

	result, err := SelectTime(samples, SelectionOptions{})
	require.NoError(t, err)

	// Default requirement is a strict majority: 4/2 + 1 = 3.
	assert.Equal(t, 3, result.MinSources)
	assert.Equal(t, 4, result.DistinctSourceCount)
	assert.Equal(t, 3, result.AgreementCount)
	assert.Equal(t, []string{"a", "b", "c"}, result.SelectedSourceIDs)

	// Agreement interval is the intersection of a, b and c:
	// max(90, 94, 86) = 94ms; min(110, 114, 106) = 106ms.
	assert.Equal(t, 94*time.Millisecond, result.AgreementLow)
	assert.Equal(t, 106*time.Millisecond, result.AgreementHigh)
	assert.Equal(t, 100*time.Millisecond, result.Offset)

	decisions := sampleByIndex(result)
	for _, idx := range []int{0, 1, 2} {
		assert.True(t, decisions[idx].Included)
		assert.Equal(t, reasonValidated, decisions[idx].Reason)
	}
	assert.False(t, decisions[3].Included)
	assert.Contains(t, decisions[3].Reason, "does not overlap")
}

func TestSelectTimeNoConsensus(t *testing.T) {
	// Three well-separated sources; at most one interval covers any point.
	samples := []Sample{
		{"a", makeSelectResponse(0, time.Millisecond)},
		{"b", makeSelectResponse(100*time.Millisecond, time.Millisecond)},
		{"c", makeSelectResponse(-100*time.Millisecond, time.Millisecond)},
	}

	result, err := SelectTime(samples, SelectionOptions{})
	require.ErrorIs(t, err, ErrNoConsistentTimeSources)
	require.NotNil(t, result)

	assert.Equal(t, 2, result.MinSources) // strict majority of 3
	assert.Equal(t, 1, result.AgreementCount)
	assert.Nil(t, result.SelectedSourceIDs)
	assert.Equal(t, time.Duration(0), result.Offset)

	decisions := sampleByIndex(result)
	for i := range samples {
		assert.False(t, decisions[i].Included)
		assert.Contains(t, decisions[i].Reason, "no agreement interval")
	}

	// Explicit minimum of 1 makes the best single interval sufficient; the
	// deterministic rules choose the narrowest, then smallest lower bound.
	result, err = SelectTime(samples, SelectionOptions{MinSources: 1})
	require.NoError(t, err)
	assert.Equal(t, []string{"c"}, result.SelectedSourceIDs)
	assert.Equal(t, -101*time.Millisecond, result.AgreementLow)
	assert.Equal(t, -99*time.Millisecond, result.AgreementHigh)
}

func TestSelectTimeDuplicateSourceOneVote(t *testing.T) {
	// Source "a" is sampled three times. Sample 0 has the largest distance,
	// sample 1 improves it, and sample 2 ties sample 1. Only sample 1 may
	// represent "a"; the duplicates cast no additional votes.
	samples := []Sample{
		{"a", makeSelectResponse(0, 20*time.Millisecond)},
		{"a", makeSelectResponse(0, 10*time.Millisecond)},
		{"a", makeSelectResponse(0, 10*time.Millisecond)},
		{"b", makeSelectResponse(0, 5*time.Millisecond)},
		{"c", makeSelectResponse(500*time.Millisecond, time.Millisecond)},
	}

	result, err := SelectTime(samples, SelectionOptions{})
	require.NoError(t, err)

	assert.Equal(t, 3, result.DistinctSourceCount)
	assert.Equal(t, 2, result.MinSources)
	assert.Equal(t, 2, result.AgreementCount)
	assert.Equal(t, []string{"a", "b"}, result.SelectedSourceIDs)

	// a's representative interval is [-10ms, 10ms]; intersecting with
	// b's [-5ms, 5ms] yields [-5ms, 5ms].
	assert.Equal(t, -5*time.Millisecond, result.AgreementLow)
	assert.Equal(t, 5*time.Millisecond, result.AgreementHigh)

	decisions := sampleByIndex(result)
	assert.True(t, decisions[1].Included)
	assert.True(t, decisions[3].Included)

	for _, idx := range []int{0, 2} {
		assert.False(t, decisions[idx].Included)
		assert.Contains(t, decisions[idx].Reason, "duplicate sample")
		// Both losing samples must report the winning sample (index 1).
		assert.True(t, strings.Contains(decisions[idx].Reason, "sample 1"),
			"expected duplicate reason to name winning sample 1, got %q",
			decisions[idx].Reason)
	}
	assert.False(t, decisions[4].Included)
	assert.Contains(t, decisions[4].Reason, "does not overlap")
}

func TestSelectTimeDuplicateSourceAloneNeedsOnlyOneVote(t *testing.T) {
	// Three identical samples from a single source must count as one source,
	// not three votes; the required majority is therefore 1.
	samples := []Sample{
		{"only", makeSelectResponse(42*time.Millisecond, 5*time.Millisecond)},
		{"only", makeSelectResponse(42*time.Millisecond, 5*time.Millisecond)},
		{"only", makeSelectResponse(42*time.Millisecond, 5*time.Millisecond)},
	}

	result, err := SelectTime(samples, SelectionOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.DistinctSourceCount)
	assert.Equal(t, 1, result.MinSources)
	assert.Equal(t, 1, result.AgreementCount)
	assert.Equal(t, []string{"only"}, result.SelectedSourceIDs)
	assert.Equal(t, 42*time.Millisecond, result.Offset)
}

func TestSelectTimeValidationRunsFirst(t *testing.T) {
	invalid := makeSelectResponse(0, time.Millisecond)
	invalid.Stratum = maxStratum // fails Validate with ErrInvalidStratum

	samples := []Sample{
		{"nil", nil},
		{"bad", invalid},
		{"g1", makeSelectResponse(10*time.Millisecond, 5*time.Millisecond)},
		{"g2", makeSelectResponse(12*time.Millisecond, 5*time.Millisecond)},
	}

	result, err := SelectTime(samples, SelectionOptions{})
	require.NoError(t, err) // the two valid sources meet the majority of 2

	assert.Equal(t, 2, result.DistinctSourceCount)
	assert.Equal(t, []string{"g1", "g2"}, result.SelectedSourceIDs)

	decisions := sampleByIndex(result)
	assert.False(t, decisions[0].Included)
	assert.Equal(t, reasonNilResponse, decisions[0].Reason)

	assert.False(t, decisions[1].Included)
	assert.Contains(t, decisions[1].Reason, "failed validation")
	assert.Contains(t, decisions[1].Reason, ErrInvalidStratum.Error())
	assert.Equal(t, time.Duration(0), decisions[1].ClockOffset)
	assert.Equal(t, time.Duration(0), decisions[1].SyncDistance)

	assert.True(t, decisions[2].Included)
	assert.True(t, decisions[3].Included)
}

func TestSelectTimeBoundaryTouching(t *testing.T) {
	// a = [0, 10ms], b = [10ms, 20ms]: closed intervals meet at 10ms.
	samples := []Sample{
		{"a", makeSelectResponse(5*time.Millisecond, 5*time.Millisecond)},
		{"b", makeSelectResponse(15*time.Millisecond, 5*time.Millisecond)},
	}

	result, err := SelectTime(samples, SelectionOptions{MinSources: 2})
	require.NoError(t, err)

	assert.Equal(t, 10*time.Millisecond, result.AgreementLow)
	assert.Equal(t, 10*time.Millisecond, result.AgreementHigh)
	assert.Equal(t, 10*time.Millisecond, result.Offset)
	assert.Equal(t, 2, result.AgreementCount)
}

func TestSelectTimeEqualCandidateTieBreak(t *testing.T) {
	// Two disjoint pairs agree with equal intersection width (4ms):
	//	a = [0, 10], b = [6, 14]   -> intersection [6, 10]
	//	c = [30, 38], d = [34, 42] -> intersection [34, 38]
	samples := []Sample{
		{"a", makeSelectResponse(5*time.Millisecond, 5*time.Millisecond)},
		{"b", makeSelectResponse(10*time.Millisecond, 4*time.Millisecond)},
		{"c", makeSelectResponse(34*time.Millisecond, 4*time.Millisecond)},
		{"d", makeSelectResponse(38*time.Millisecond, 4*time.Millisecond)},
	}

	result, err := SelectTime(samples, SelectionOptions{MinSources: 2})
	require.NoError(t, err)

	// Equal width: the candidate with the smallest lower bound wins.
	assert.Equal(t, 6*time.Millisecond, result.AgreementLow)
	assert.Equal(t, 10*time.Millisecond, result.AgreementHigh)
	assert.Equal(t, 8*time.Millisecond, result.Offset)
	assert.Equal(t, []string{"a", "b"}, result.SelectedSourceIDs)
}

func TestSelectTimeDurationSaturates(t *testing.T) {
	maxInt := time.Duration(1<<63 - 1)
	minInt := time.Duration(-1 << 63)

	// a = [MaxInt64-3h, MaxInt64] (high endpoint saturates)
	// b = [MaxInt64-1h, MaxInt64] (high endpoint saturates)
	samples := []Sample{
		{"a", makeSelectResponse(maxInt-time.Hour, 2*time.Hour)},
		{"b", makeSelectResponse(maxInt, time.Hour)},
	}

	result, err := SelectTime(samples, SelectionOptions{MinSources: 2})
	require.NoError(t, err)

	assert.Equal(t, maxInt-time.Hour, result.AgreementLow)
	assert.Equal(t, maxInt, result.AgreementHigh)
	assert.Equal(t, maxInt-30*time.Minute, result.Offset)

	decisions := sampleByIndex(result)
	assert.True(t, decisions[0].Included)
	assert.Equal(t, reasonValidatedSaturated, decisions[0].Reason)
	assert.True(t, decisions[1].Included)
	assert.Equal(t, reasonValidatedSaturated, decisions[1].Reason)

	// The negative extreme saturates its lower endpoint but still votes.
	samples = []Sample{
		{"c", makeSelectResponse(minInt+time.Hour, 2*time.Hour)},
		{"d", makeSelectResponse(minInt, time.Hour)},
	}
	result, err = SelectTime(samples, SelectionOptions{MinSources: 2})
	require.NoError(t, err)
	assert.Equal(t, minInt, result.AgreementLow)
	assert.Equal(t, minInt+time.Hour, result.AgreementHigh)
	assert.Equal(t, minInt+30*time.Minute, result.Offset)
}

func TestSelectTimeNegativeDistanceExcluded(t *testing.T) {
	bad := makeSelectResponse(0, -time.Millisecond)
	require.NoError(t, bad.Validate()) // Validate itself does not reject it

	samples := []Sample{
		{"neg", bad},
		{"g1", makeSelectResponse(10*time.Millisecond, 5*time.Millisecond)},
		{"g2", makeSelectResponse(12*time.Millisecond, 5*time.Millisecond)},
	}

	result, err := SelectTime(samples, SelectionOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.DistinctSourceCount)
	assert.Equal(t, []string{"g1", "g2"}, result.SelectedSourceIDs)

	decisions := sampleByIndex(result)
	assert.False(t, decisions[0].Included)
	assert.Contains(t, decisions[0].Reason, "must not be negative")
}

func TestSelectTimeMinSourcesConfig(t *testing.T) {
	// Only a and b agree; c and d are far away.
	samples := []Sample{
		{"a", makeSelectResponse(0, 2*time.Millisecond)},
		{"b", makeSelectResponse(time.Millisecond, 2*time.Millisecond)},
		{"c", makeSelectResponse(100*time.Millisecond, time.Millisecond)},
		{"d", makeSelectResponse(200*time.Millisecond, time.Millisecond)},
	}

	// Default strict majority of 4 is 3, but only 2 sources ever agree.
	result, err := SelectTime(samples, SelectionOptions{})
	require.ErrorIs(t, err, ErrNoConsistentTimeSources)
	assert.Equal(t, 3, result.MinSources)
	assert.Equal(t, 2, result.AgreementCount)

	// Requiring the default majority never falls back to averaging: the
	// reported offset must remain zero.
	assert.Equal(t, time.Duration(0), result.Offset)

	// An explicit minimum of 2 succeeds.
	result, err = SelectTime(samples, SelectionOptions{MinSources: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, result.SelectedSourceIDs)

	// An explicit minimum of 3 fails.
	_, err = SelectTime(samples, SelectionOptions{MinSources: 3})
	assert.ErrorIs(t, err, ErrNoConsistentTimeSources)
}

func TestSelectTimeMidpointTruncation(t *testing.T) {
	// a = [-2ns, 10ns], b = [-5ns, 1ns] intersect at [-2ns, 1ns], whose
	// mathematical midpoint is -0.5ns and truncates toward zero to 0.
	samples := []Sample{
		{"a", makeSelectResponse(4*time.Nanosecond, 6*time.Nanosecond)},
		{"b", makeSelectResponse(-2*time.Nanosecond, 3*time.Nanosecond)},
	}

	result, err := SelectTime(samples, SelectionOptions{MinSources: 2})
	require.NoError(t, err)
	assert.Equal(t, -2*time.Nanosecond, result.AgreementLow)
	assert.Equal(t, time.Nanosecond, result.AgreementHigh)
	assert.Equal(t, time.Duration(0), result.Offset)
}

func TestSelectTimeExplicitImpossibleMinSources(t *testing.T) {
	_, err := SelectTime(nil, SelectionOptions{MinSources: 1})
	assert.ErrorIs(t, err, ErrNoConsistentTimeSources)

	_, err = SelectTime(nil, SelectionOptions{MinSources: -1})
	assert.ErrorIs(t, err, ErrInvalidMinSources)

	// With no samples the default majority is 1, which cannot be met.
	_, err = SelectTime(nil, SelectionOptions{})
	assert.ErrorIs(t, err, ErrNoConsistentTimeSources)
}

func TestIntervalBounds(t *testing.T) {
	maxInt := time.Duration(1<<63 - 1)
	minInt := time.Duration(-1 << 63)

	tests := []struct {
		name             string
		offset, distance time.Duration
		low, high        time.Duration
		saturated        bool
	}{
		{"zero", 0, 0, 0, 0, false},
		{"simple", 10 * time.Millisecond, 3 * time.Millisecond,
			7 * time.Millisecond, 13 * time.Millisecond, false},
		{"high overflow", maxInt - time.Hour, 2 * time.Hour,
			maxInt - 3*time.Hour, maxInt, true},
		{"low overflow", minInt + time.Hour, 2 * time.Hour,
			minInt, minInt + 3*time.Hour, true},
		{"both extremes positive", maxInt, maxInt, 0, maxInt, true},
		{"both extremes negative", minInt, maxInt, minInt, -time.Nanosecond, true},
		{"low overflow by one", minInt, time.Nanosecond,
			minInt, minInt + time.Nanosecond, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			low, high, saturated := intervalBounds(tt.offset, tt.distance)
			assert.Equal(t, int64(tt.low), low)
			assert.Equal(t, int64(tt.high), high)
			assert.Equal(t, tt.saturated, saturated)
		})
	}
}

func TestMidpointTruncated(t *testing.T) {
	maxInt := int64(1<<63 - 1)
	minInt := int64(-1 << 63)

	tests := []struct {
		name     string
		a, b     int64
		expected time.Duration
	}{
		{"even", 2, 4, 3},
		{"odd positive truncates", 1, 2, 1},
		{"odd negative truncates toward zero", -2, 1, 0},
		{"negative odd truncates toward zero", -3, -2, -2},
		{"max extremes", maxInt, maxInt, time.Duration(maxInt)},
		{"min extremes", minInt, minInt, time.Duration(minInt)},
		{"min plus max", minInt, maxInt, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, midpointTruncated(tt.a, tt.b))
		})
	}
}
