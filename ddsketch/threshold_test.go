// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).

package ddsketch

import (
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const thresholdCountSlack = 1e-9

type thresholdQuery struct {
	name    string
	orEqual bool
	run     func(s *DDSketch, threshold float64) (ThresholdCount, error)
}

var thresholdQueries = []thresholdQuery{
	{
		name:    "strictly greater",
		orEqual: false,
		run:     (*DDSketch).CountAboveThreshold,
	},
	{
		name:    "greater than or equal",
		orEqual: true,
		run:     (*DDSketch).CountAtOrAboveThreshold,
	},
}

func trueCountAbove(values []float64, threshold float64, orEqual bool) float64 {
	var count float64
	for _, v := range values {
		if v > threshold || (orEqual && v == threshold) {
			count++
		}
	}
	return count
}

// assertThresholdCounts verifies, for every query mode and threshold, that the
// hand-computed count of the raw samples always lies within the guaranteed and
// possible weights, and that the two weights are monotonic (non-increasing) as
// the threshold grows.
func assertThresholdCounts(t *testing.T, s *DDSketch, values, thresholds []float64) {
	t.Helper()
	total := float64(len(values))
	sort.Float64s(thresholds)

	for _, q := range thresholdQueries {
		var prevDefinitely, prevPossibly float64
		first := true
		for _, threshold := range thresholds {
			c, err := q.run(s, threshold)
			require.NoError(t, err, q.name)

			truth := trueCountAbove(values, threshold, q.orEqual)
			definitely := c.DefinitelyAbove()
			possibly := c.PossiblyAbove()

			assert.InDelta(t, total, c.Total(), thresholdCountSlack, q.name)
			assert.True(t, definitely <= truth+thresholdCountSlack,
				"%s: threshold %v, definitely %v above true %v", q.name, threshold, definitely, truth)
			assert.True(t, truth <= possibly+thresholdCountSlack,
				"%s: threshold %v, true %v above possible %v", q.name, threshold, truth, possibly)
			assert.True(t, definitely <= possibly+thresholdCountSlack,
				"%s: threshold %v, definitely %v above possible %v", q.name, threshold, definitely, possibly)
			assert.True(t, possibly <= total+thresholdCountSlack,
				"%s: threshold %v, possible %v above total %v", q.name, threshold, possibly, total)

			if !first {
				assert.True(t, prevDefinitely >= definitely-thresholdCountSlack,
					"%s: threshold %v, guaranteed weight increased from %v", q.name, threshold, definitely, prevDefinitely)
				assert.True(t, prevPossibly >= possibly-thresholdCountSlack,
					"%s: threshold %v, possible weight increased from %v", q.name, threshold, possibly, prevPossibly)
			}
			prevDefinitely, prevPossibly, first = definitely, possibly, false
		}
	}
}

func TestThresholdCountHandConstructedSamples(t *testing.T) {
	s, err := LogUnboundedDenseDDSketch(0.1)
	require.NoError(t, err)

	// Negative, zero and positive values, including values small enough to fall
	// into the zero bin.
	values := []float64{
		-1000, -100, -10, -5, -2, -1,
		-1e-320, 0, 1e-320,
		1, 2, 5, 10, 100, 1000,
	}
	for _, v := range values {
		require.NoError(t, s.Add(v))
	}

	thresholds := []float64{
		math.Inf(-1), -1000, -500, -100, -50, -10, -5, -2, -1, -0.5,
		-1e-200, 0, 1e-200, 0.5, 1, 2, 5, 10, 100, 500, 1000, math.Inf(1),
	}
	assertThresholdCounts(t, s, values, thresholds)

	// Far below every value: all weight is guaranteed above, for both modes.
	for _, q := range thresholdQueries {
		c, err := q.run(s, math.Inf(-1))
		require.NoError(t, err)
		assert.InDelta(t, 15, c.DefinitelyAbove(), thresholdCountSlack, q.name)
		assert.InDelta(t, 15, c.PossiblyAbove(), thresholdCountSlack, q.name)
	}

	// Far above every value: no weight can be above, for both modes.
	for _, q := range thresholdQueries {
		c, err := q.run(s, math.Inf(1))
		require.NoError(t, err)
		assert.InDelta(t, 0, c.DefinitelyAbove(), thresholdCountSlack, q.name)
		assert.InDelta(t, 0, c.PossiblyAbove(), thresholdCountSlack, q.name)
	}

	// At threshold 0, the zero bin straddles the threshold and must stay
	// uncertain; the ordering of negative, zero and positive values is
	// preserved.
	strict, err := s.CountAboveThreshold(0)
	require.NoError(t, err)
	assert.InDelta(t, 6, strict.DefinitelyAbove(), thresholdCountSlack)
	assert.InDelta(t, 9, strict.PossiblyAbove(), thresholdCountSlack)

	atOrAbove, err := s.CountAtOrAboveThreshold(0)
	require.NoError(t, err)
	assert.InDelta(t, 6, atOrAbove.DefinitelyAbove(), thresholdCountSlack)
	assert.InDelta(t, 9, atOrAbove.PossiblyAbove(), thresholdCountSlack)
}

func TestThresholdCountStraddlingBin(t *testing.T) {
	// Two positive values that share a single bin, with a threshold in between.
	s, err := LogUnboundedDenseDDSketch(0.1)
	require.NoError(t, err)
	require.NoError(t, s.Add(1.0))
	require.NoError(t, s.Add(1.10))

	c, err := s.CountAboveThreshold(1.05)
	require.NoError(t, err)
	assert.InDelta(t, 0, c.DefinitelyAbove(), thresholdCountSlack)
	assert.InDelta(t, 2, c.PossiblyAbove(), thresholdCountSlack)
	assert.InDelta(t, 2, c.Total(), thresholdCountSlack)

	// Same situation for negative values.
	ns, err := LogUnboundedDenseDDSketch(0.1)
	require.NoError(t, err)
	require.NoError(t, ns.Add(-1.0))
	require.NoError(t, ns.Add(-1.10))

	nc, err := ns.CountAboveThreshold(-1.05)
	require.NoError(t, err)
	assert.InDelta(t, 0, nc.DefinitelyAbove(), thresholdCountSlack)
	assert.InDelta(t, 2, nc.PossiblyAbove(), thresholdCountSlack)

	// Threshold inside a bin is distinct from the "greater than or equal" query,
	// which stays uncertain the same way here.
	ge, err := s.CountAtOrAboveThreshold(1.05)
	require.NoError(t, err)
	assert.InDelta(t, 0, ge.DefinitelyAbove(), thresholdCountSlack)
	assert.InDelta(t, 2, ge.PossiblyAbove(), thresholdCountSlack)
}

func TestThresholdCountZeroBin(t *testing.T) {
	s, err := LogUnboundedDenseDDSketch(0.1)
	require.NoError(t, err)
	values := []float64{-1, 0, 0, 0, 1}
	for _, v := range values {
		require.NoError(t, s.Add(v))
	}

	// Strictly above 0: the three zero values cannot be guaranteed above.
	strict, err := s.CountAboveThreshold(0)
	require.NoError(t, err)
	assert.InDelta(t, 1, strict.DefinitelyAbove(), thresholdCountSlack)
	assert.InDelta(t, 4, strict.PossiblyAbove(), thresholdCountSlack)

	// Greater than or equal 0: the zero values satisfy the condition, but the
	// zero bin also contains -1, so only the positive value is guaranteed.
	atOrAbove, err := s.CountAtOrAboveThreshold(0)
	require.NoError(t, err)
	assert.InDelta(t, 1, atOrAbove.DefinitelyAbove(), thresholdCountSlack)
	assert.InDelta(t, 4, atOrAbove.PossiblyAbove(), thresholdCountSlack)
}

func TestThresholdCountCollapsedLowestBins(t *testing.T) {
	s, err := LogCollapsingLowestDenseDDSketch(0.01, 8)
	require.NoError(t, err)
	values := []float64{
		-1e25, -1e20, -1e15, -1e10, -1e5, -1,
		0,
		1, 1e5, 1e10, 1e15, 1e20, 1e25,
	}
	for _, v := range values {
		require.NoError(t, s.Add(v))
	}

	collapsed := s.GetPositiveValueStore().(interface{ IsCollapsed() bool })
	assert.True(t, collapsed.IsCollapsed())

	thresholds := []float64{
		-1e27, -1e23, -1e18, -1e12, -1e7, -1, -1e-10, 0,
		1e-10, 1, 1e7, 1e12, 1e18, 1e23, 1e27,
	}
	assertThresholdCounts(t, s, values, thresholds)
}

func TestThresholdCountCollapsedHighestBins(t *testing.T) {
	s, err := LogCollapsingHighestDenseDDSketch(0.01, 8)
	require.NoError(t, err)
	values := []float64{
		-1e25, -1e20, -1e15, -1e10, -1e5, -1,
		0,
		1, 1e5, 1e10, 1e15, 1e20, 1e25,
	}
	for _, v := range values {
		require.NoError(t, s.Add(v))
	}

	collapsed := s.GetPositiveValueStore().(interface{ IsCollapsed() bool })
	assert.True(t, collapsed.IsCollapsed())

	thresholds := []float64{
		-1e27, -1e23, -1e18, -1e12, -1e7, -1, -1e-10, 0,
		1e-10, 1, 1e7, 1e12, 1e18, 1e23, 1e27,
	}
	assertThresholdCounts(t, s, values, thresholds)
}

func TestThresholdCountEmptySketch(t *testing.T) {
	s, err := LogUnboundedDenseDDSketch(0.1)
	require.NoError(t, err)

	_, err = s.CountAboveThreshold(0)
	assert.Error(t, err)
	_, err = s.CountAtOrAboveThreshold(0)
	assert.Error(t, err)
}

func TestThresholdCountOnExactSummarySketch(t *testing.T) {
	s, err := NewDefaultDDSketchWithExactSummaryStatistics(0.1)
	require.NoError(t, err)
	require.NoError(t, s.Add(1))
	require.NoError(t, s.Add(1.10))

	c, err := s.CountAboveThreshold(1.05)
	require.NoError(t, err)
	assert.InDelta(t, 2, c.Total(), thresholdCountSlack)
	assert.InDelta(t, 0, c.DefinitelyAbove(), thresholdCountSlack)
	assert.InDelta(t, 2, c.PossiblyAbove(), thresholdCountSlack)

	// The promoted methods share the DDSketch implementation and also report
	// exact weights when the threshold lies between bins.
	require.NoError(t, s.Add(2))
	separated, err := s.CountAboveThreshold(1.5)
	require.NoError(t, err)
	assert.InDelta(t, 3, separated.Total(), thresholdCountSlack)
	assert.InDelta(t, 1, separated.DefinitelyAbove(), thresholdCountSlack)
	assert.InDelta(t, 1, separated.PossiblyAbove(), thresholdCountSlack)
}

func TestThresholdCountNaNThreshold(t *testing.T) {
	s, err := LogUnboundedDenseDDSketch(0.1)
	require.NoError(t, err)
	require.NoError(t, s.Add(1))

	// No comparison with NaN holds, so nothing can be above it.
	c, err := s.CountAboveThreshold(math.NaN())
	require.NoError(t, err)
	assert.InDelta(t, 1, c.Total(), thresholdCountSlack)
	assert.InDelta(t, 0, c.PossiblyAbove(), thresholdCountSlack)
}

func TestThresholdCountRandomized(t *testing.T) {
	constructors := map[string]func() (*DDSketch, error){
		"unbounded": func() (*DDSketch, error) { return LogUnboundedDenseDDSketch(0.05) },
		"collapsing-lowest": func() (*DDSketch, error) {
			return LogCollapsingLowestDenseDDSketch(0.05, 16)
		},
		"collapsing-highest": func() (*DDSketch, error) {
			return LogCollapsingHighestDenseDDSketch(0.05, 16)
		},
	}

	rng := rand.New(rand.NewSource(20261007))
	for name, newSketch := range constructors {
		t.Run(name, func(t *testing.T) {
			for iteration := 0; iteration < 50; iteration++ {
				s, err := newSketch()
				require.NoError(t, err)

				values := make([]float64, 0, 200)
				for i := 0; i < 200; i++ {
					v := randomThresholdTestValue(rng)
					if err := s.Add(v); err != nil {
						t.Fatalf("add %v: %v", v, err)
					}
					values = append(values, v)
				}

				thresholds := make([]float64, 0, 32)
				for i := 0; i < 30; i++ {
					thresholds = append(thresholds, randomThresholdTestValue(rng))
				}
				thresholds = append(thresholds,
					math.Inf(-1), math.Inf(1), 0,
					s.MinIndexableValue(), -s.MinIndexableValue(),
				)
				assertThresholdCounts(t, s, values, thresholds)
			}
		})
	}
}

// randomThresholdTestValue draws values spanning negative, zero-bin and
// positive ranges, including subnormals and ordinary magnitudes.
func randomThresholdTestValue(rng *rand.Rand) float64 {
	switch rng.Intn(5) {
	case 0:
		return 0
	case 1:
		return rng.NormFloat64() * 1e-315 // often lands in the zero bin
	case 2:
		// Ordinary positive magnitudes.
		return math.Exp(rng.NormFloat64() * 5)
	case 3:
		// Ordinary negative magnitudes.
		return -math.Exp(rng.NormFloat64() * 5)
	default:
		return rng.NormFloat64() * 100
	}
}
