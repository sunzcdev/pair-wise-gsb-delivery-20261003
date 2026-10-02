// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"
)

var (
	// ErrNoConsistentTimeSources is returned by SelectTime when the
	// responses cannot produce an agreement interval supported by at least
	// the configured (or default majority) number of distinct sources. In
	// this case no suggested offset is returned and the average of all
	// sources is never substituted for a consensus.
	ErrNoConsistentTimeSources = errors.New("ntp: no consistent set of time sources meets the required minimum source count")

	// ErrInvalidMinSources is returned by SelectTime when the
	// SelectionOptions MinSources field is negative.
	ErrInvalidMinSources = errors.New("ntp: MinSources must be greater than or equal to 0")
)

// Reason strings reported in SourceDecision records. Callers should branch
// on the Included field and the returned error rather than matching the
// human-readable text.
const (
	reasonValidated          = "response validated; source participates in the selected consistent set"
	reasonValidatedSaturated = "response validated; source participates in the selected consistent set (interval endpoint saturated to the representable duration range)"
	reasonNilResponse        = "response is nil"
	reasonValidationFailed   = "response failed validation: %v"
	reasonNegativeDistance   = "synchronization distance must not be negative (got %s)"
	reasonDuplicateSource    = "duplicate sample from source ignored; sample %d with synchronization distance %s is no better than sample %d with distance %s (each source casts at most one vote)"
	reasonOutsideAgreement   = "source interval does not overlap the selected agreement interval [%s, %s]"
	reasonNoConsensus        = "excluded because no agreement interval reached the minimum source count (maximum agreement count %d < minimum %d)"
)

// A Sample pairs an already-obtained NTP Response with the identifier of
// its source. Multiple samples may share a SourceID, but only the best
// sample (smallest synchronization distance) from each distinct source is
// allowed to cast a vote.
type Sample struct {
	// SourceID uniquely identifies the time server that produced the
	// Response. It is typically the server's host name or address.
	SourceID string

	// Response is the time synchronization response obtained from the
	// source. It must have been populated by Query or constructed
	// synthetically; Response.Validate is run against it.
	Response *Response
}

// SelectionOptions contains configurable options used by the SelectTime
// function.
type SelectionOptions struct {
	// MinSources is the minimum number of distinct sources whose offset
	// intervals must agree for SelectTime to succeed. If 0, a strict
	// majority (n/2 + 1) of the distinct, validated sources is required.
	MinSources int
}

// A SourceDecision records whether a single submitted sample was included
// in the selected consistent set and why.
type SourceDecision struct {
	// SampleIndex is the zero-based index of the sample in the slice
	// submitted to SelectTime.
	SampleIndex int

	// SourceID is the identifier of the sample's source.
	SourceID string

	// ClockOffset is the clock offset reported by the sample. It is 0 for
	// samples that failed validation.
	ClockOffset time.Duration

	// SyncDistance is the synchronization distance (RootDistance) reported
	// by the sample. It is 0 for samples that failed validation.
	SyncDistance time.Duration

	// Included is true if the sample is the single representative of its
	// source and that source participates in the selected consistent set.
	Included bool

	// Reason is a human-readable explanation of the inclusion or exclusion
	// decision.
	Reason string
}

// A SelectionResult is the output of the SelectTime function.
type SelectionResult struct {
	// Offset is the suggested clock offset, computed as the midpoint of
	// the agreement interval of the selected consistent set. Add it to a
	// local clock reading to obtain the consensus time. It is 0 when the
	// returned error is non-nil.
	Offset time.Duration

	// AgreementLow and AgreementHigh bound the closed interval on which
	// every source in the selected consistent set agrees. Boundaries are
	// inclusive, so two intervals touching only at an endpoint are
	// considered to agree. Both are 0 when the returned error is non-nil.
	AgreementLow  time.Duration
	AgreementHigh time.Duration

	// MinSources is the minimum number of agreeing distinct sources that
	// was required.
	MinSources int

	// AgreementCount is the number of distinct sources whose intervals
	// cover the selected agreement interval. It is the maximum agreement
	// count found, even on failure.
	AgreementCount int

	// DistinctSourceCount is the number of distinct sources that produced
	// validated, usable samples.
	DistinctSourceCount int

	// SelectedSourceIDs lists the identifiers of the sources in the
	// selected consistent set, sorted lexicographically. It is nil on
	// failure.
	SelectedSourceIDs []string

	// Decisions contains one entry per submitted sample, in submission
	// order, explaining why the sample was included or excluded.
	Decisions []SourceDecision
}

// interval is the closed clock-offset interval [low, high] contributed by
// a single distinct source.
type interval struct {
	id        string
	offset    time.Duration
	distance  time.Duration
	low       int64
	high      int64
	saturated bool
}

// candidate is the representative sample selected for a distinct source.
type candidate struct {
	interval interval
	index    int
}

// event is a sweep-line endpoint. start=true is the lower bound of an
// interval; start=false is its upper bound.
type event struct {
	coord int64
	start bool
	id    string
}

// consensusRegion is a maximal closed region covered by the largest number
// of source intervals found during the sweep.
type consensusRegion struct {
	low     int64
	high    int64
	sources []string // sorted lexicographically
}

// less defines the deterministic ordering used to choose among equally tall
// candidate regions: narrowest width first, then smallest lower bound, then
// lexicographically smallest sorted source-ID set.
func (r consensusRegion) less(other consensusRegion) bool {
	width := new(big.Int).Sub(big.NewInt(r.high), big.NewInt(r.low))
	otherWidth := new(big.Int).Sub(big.NewInt(other.high), big.NewInt(other.low))
	if c := width.Cmp(otherWidth); c != 0 {
		return c < 0
	}
	if r.low != other.low {
		return r.low < other.low
	}
	return lexLess(r.sources, other.sources)
}

// SelectTime performs a purely computational selection of a clock offset
// from multiple NTP responses. It never queries the network and never reads
// or modifies the system clock.
//
// Each validated response contributes a closed interval centered on its
// ClockOffset with half-width equal to its RootDistance:
//
//	[ClockOffset - RootDistance, ClockOffset + RootDistance]
//
// Responses are first run through Response.Validate, so invalid responses
// never participate. Samples from the same SourceID cast at most one vote;
// only the sample with the smallest synchronization distance is kept, and
// on equal distance the earliest submitted sample wins.
//
// A sweep over the interval endpoints finds the points covered by the
// largest number of distinct sources. If this maximum count is below
// MinSources (or a strict majority when MinSources is 0), SelectTime fails
// with ErrNoConsistentTimeSources and returns a zero offset instead of
// averaging the sources.
//
// Deterministic rules resolve ambiguous geometry:
//  1. Intervals whose endpoints merely touch are considered intersecting
//     (closed intervals); start events are processed before end events at
//     the same coordinate.
//  2. Among equally supported candidate intervals, the narrowest agreement
//     interval wins.
//  3. If widths are equal, the interval with the smallest lower bound
//     wins.
//  4. If those are equal too, the lexicographically smallest sorted set of
//     source IDs wins.
//
// Endpoint duration arithmetic saturates at the representable
// time.Duration range rather than overflowing; the affected sample retains
// its vote with the saturated endpoint and the fact is recorded in its
// reason. The suggested Offset is the midpoint of the winning agreement
// interval, truncated toward zero to nanosecond precision.
func SelectTime(samples []Sample, opt SelectionOptions) (*SelectionResult, error) {
	if opt.MinSources < 0 {
		return nil, ErrInvalidMinSources
	}

	result := &SelectionResult{
		Decisions: make([]SourceDecision, len(samples)),
	}

	// Stage 1: validate every response and select one representative sample
	// per distinct source. Duplicate samples never increase a source's vote.
	bestBySource := make(map[string]candidate)
	chosenIndex := make(map[int]struct{})

	for i, sample := range samples {
		decision := SourceDecision{
			SampleIndex: i,
			SourceID:    sample.SourceID,
		}

		if sample.Response == nil {
			decision.Reason = reasonNilResponse
			result.Decisions[i] = decision
			continue
		}

		if err := sample.Response.Validate(); err != nil {
			decision.Reason = fmt.Sprintf(reasonValidationFailed, err)
			result.Decisions[i] = decision
			continue
		}

		distance := sample.Response.RootDistance
		decision.ClockOffset = sample.Response.ClockOffset
		decision.SyncDistance = distance

		if distance < 0 {
			decision.Reason = fmt.Sprintf(reasonNegativeDistance, distance)
			result.Decisions[i] = decision
			continue
		}

		low, high, saturated := intervalBounds(
			sample.Response.ClockOffset, distance)

		current, exists := bestBySource[sample.SourceID]
		if exists && !(distance < current.interval.distance) {
			// The new sample loses to the existing representative (or ties,
			// in which case the earlier sample is kept).
			decision.Reason = fmt.Sprintf(reasonDuplicateSource,
				i, distance, current.index, current.interval.distance)
			result.Decisions[i] = decision
			continue
		}
		if exists {
			// The new sample supersedes the earlier representative; mark the
			// earlier sample as an excluded duplicate.
			old := result.Decisions[current.index]
			old.Reason = fmt.Sprintf(reasonDuplicateSource,
				current.index, current.interval.distance, i, distance)
			result.Decisions[current.index] = old
			delete(chosenIndex, current.index)
		}

		result.Decisions[i] = decision
		bestBySource[sample.SourceID] = candidate{
			interval: interval{
				id:        sample.SourceID,
				offset:    sample.Response.ClockOffset,
				distance:  distance,
				low:       low,
				high:      high,
				saturated: saturated,
			},
			index: i,
		}
		chosenIndex[i] = struct{}{}
	}

	intervals := make([]interval, 0, len(bestBySource))
	indexSaturated := make(map[int]bool)
	for _, cand := range bestBySource {
		indexSaturated[cand.index] = cand.interval.saturated
		intervals = append(intervals, cand.interval)
	}
	result.DistinctSourceCount = len(intervals)

	// Default the required source count to a strict majority of the usable
	// sources.
	required := opt.MinSources
	if required == 0 {
		required = len(intervals)/2 + 1
	}
	result.MinSources = required

	// Stage 2: build the deterministic sweep-line representation. Start
	// events sort before end events at the same coordinate so that intervals
	// touching only at an endpoint are treated as agreeing.
	events := make([]event, 0, 2*len(intervals))
	for _, in := range intervals {
		events = append(events,
			event{coord: in.low, start: true, id: in.id},
			event{coord: in.high, start: false, id: in.id},
		)
	}
	sort.Slice(events, func(a, b int) bool {
		if events[a].coord != events[b].coord {
			return events[a].coord < events[b].coord
		}
		if events[a].start != events[b].start {
			return events[a].start
		}
		return events[a].id < events[b].id
	})

	// Stage 3: sweep and collect maximal regions whose active set has the
	// largest size. A region opens when the active set reaches a new maximum
	// (or re-enters an old one after leaving it), and closes at the end
	// event that drops the active set back below that maximum.
	var bestRegions []consensusRegion
	maxCount := 0
	active := make(map[string]struct{})
	var runLow int64
	runOpen := false
	var runSources []string

	for i := 0; i < len(events); {
		coord := events[i].coord

		j := i
		for j < len(events) && events[j].coord == coord && events[j].start {
			active[events[j].id] = struct{}{}
			j++
		}

		count := len(active)
		switch {
		case count > maxCount:
			bestRegions = nil
			maxCount = count
			runLow = coord
			runOpen = true
			runSources = keysSorted(active)
		case count == maxCount && !runOpen:
			runLow = coord
			runOpen = true
			runSources = keysSorted(active)
		}

		for j < len(events) && events[j].coord == coord && !events[j].start {
			if runOpen && len(active) == maxCount {
				sources := append([]string(nil), runSources...)
				sort.Strings(sources)
				bestRegions = append(bestRegions, consensusRegion{
					low:     runLow,
					high:    coord,
					sources: sources,
				})
				runOpen = false
			}
			delete(active, events[j].id)
			j++
		}

		i = j
	}

	result.AgreementCount = maxCount

	// Stage 4: require enough agreeing distinct sources. Never average.
	if maxCount < required {
		for i := range chosenIndex {
			decision := result.Decisions[i]
			decision.Included = false
			decision.Reason = fmt.Sprintf(reasonNoConsensus, maxCount, required)
			result.Decisions[i] = decision
		}
		return result, ErrNoConsistentTimeSources
	}

	// Stage 5: deterministically select among equally tall regions.
	sort.Slice(bestRegions, func(a, b int) bool {
		return bestRegions[a].less(bestRegions[b])
	})
	winner := bestRegions[0]

	result.AgreementLow = time.Duration(winner.low)
	result.AgreementHigh = time.Duration(winner.high)
	result.Offset = midpointTruncated(winner.low, winner.high)
	result.SelectedSourceIDs = winner.sources

	selected := make(map[string]struct{}, len(winner.sources))
	for _, id := range winner.sources {
		selected[id] = struct{}{}
	}

	for i := range chosenIndex {
		decision := result.Decisions[i]
		if _, in := selected[decision.SourceID]; in {
			decision.Included = true
			if indexSaturated[i] {
				decision.Reason = reasonValidatedSaturated
			} else {
				decision.Reason = reasonValidated
			}
		} else {
			decision.Included = false
			decision.Reason = fmt.Sprintf(reasonOutsideAgreement,
				time.Duration(winner.low), time.Duration(winner.high))
		}
		result.Decisions[i] = decision
	}

	return result, nil
}

// intervalBounds computes the saturated closed interval
// [offset-distance, offset+distance]. If an endpoint overflows the int64
// duration range, it is clamped to the minimum or maximum representable
// value and saturated is reported as true.
func intervalBounds(offset, distance time.Duration) (low, high int64, saturated bool) {
	const (
		minDuration = int64(-1 << 63)
		maxDuration = int64(1<<63 - 1)
	)
	o := int64(offset)
	d := int64(distance)

	low = o - d
	if (o < 0 && d > 0 && low > o) || (o > 0 && d < 0 && low < o) {
		low = minDuration
		saturated = true
	}

	high = o + d
	if (o > 0 && d > 0 && high < o) || (o < 0 && d < 0 && high > o) {
		high = maxDuration
		saturated = true
	}

	if low > high {
		low, high = high, low
	}
	return low, high, saturated
}

// midpointTruncated returns (a+b)/2 truncated toward zero, using arbitrary
// precision so the sum of two int64 values cannot overflow.
func midpointTruncated(a, b int64) time.Duration {
	sum := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
	sum.Quo(sum, big.NewInt(2))
	return time.Duration(sum.Int64())
}

// lexLess compares two sorted string slices lexicographically.
func lexLess(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// keysSorted returns the keys of m sorted lexicographically.
func keysSorted(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
