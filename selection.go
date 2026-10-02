// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

var (
	// ErrNoValidSources is returned when none of the provided samples are
	// valid or can vote.
	ErrNoValidSources = errors.New("no valid time sources")

	// ErrInsufficientSources is returned when fewer distinct valid sources
	// are available than the configured MinSources value.
	ErrInsufficientSources = errors.New("fewer distinct valid sources than MinSources")

	// ErrNoConsistentIntersection is returned when no set of at least the
	// required number of distinct sources has mutually intersecting
	// synchronization intervals. The suggested offset is deliberately not
	// derived by averaging all sources in this case.
	ErrNoConsistentIntersection = errors.New("no consistent majority intersection among time sources")

	// ErrInvalidMinSources is returned when SelectOptions.MinSources is
	// negative.
	ErrInvalidMinSources = errors.New("MinSources must be zero or positive")
)

// SelectionReason describes why a single source sample was included in or
// excluded from the consistent set.
type SelectionReason string

const (
	// ReasonTruechimer means the sample's synchronization interval
	// participates in the majority intersection, so the source votes.
	ReasonTruechimer SelectionReason = "truechimer"

	// ReasonInvalid means the sample failed the existing Response.Validate
	// sanity check, or its synchronization distance was negative.
	ReasonInvalid SelectionReason = "invalid"

	// ReasonDuplicate means an earlier, valid sample from the same source ID
	// already casts this source's single vote.
	ReasonDuplicate SelectionReason = "duplicate"

	// ReasonOverflow means the endpoints of the sample's synchronization
	// interval cannot be represented as a time.Duration.
	ReasonOverflow SelectionReason = "overflow"

	// ReasonFalseticker means the sample passed validation but its interval
	// does not intersect the majority window, so it disagrees with the
	// consistent set.
	ReasonFalseticker SelectionReason = "falseticker"
)

// A TimeSource pairs a Response obtained from a server with the unique ID of
// the source that produced it. Multiple samples may share a SourceID, but
// only the first valid sample for each ID is allowed to vote.
type TimeSource struct {
	// SourceID uniquely identifies the time source (for example a server
	// address). Repeated samples sharing an ID do not gain extra votes.
	SourceID string

	// Response is the response obtained from the source. Only its
	// ClockOffset and RootDistance fields are used, after Validate has been
	// called on it.
	Response *Response
}

// SelectOptions configures the multi-source time selection performed by
// SelectClockOffset.
type SelectOptions struct {
	// MinSources is the minimum number of distinct sources whose
	// synchronization intervals must mutually intersect for selection to
	// succeed. When zero, a simple majority of the distinct valid sources
	// (n/2 + 1) is required.
	MinSources int
}

// SourceStatus reports whether a single input sample was included in the
// consistent set and, if not, why it was excluded.
type SourceStatus struct {
	// SourceID is the ID of the sample's source.
	SourceID string

	// Index is the position of the sample in the input slice.
	Index int

	// Included reports whether the sample casts a vote for the returned
	// suggested offset.
	Included bool
	// Reason reports the inclusion or exclusion reason. It is set to
	// ReasonTruechimer when Included is true.
	Reason SelectionReason

	// Detail provides additional human-readable information, such as the
	// validation error that caused exclusion.
	Detail string

	// ClockOffset is the sample's reported clock offset.
	ClockOffset time.Duration

	// Distance is the sample's synchronization distance (half-width of its
	// synchronization interval), taken from Response.RootDistance.
	Distance time.Duration
}

// SelectionResult is the result of a SelectClockOffset call.
type SelectionResult struct {
	// Offset is the suggested clock offset. It is the median of the
	// ClockOffset values of the included truechimers. It is zero when
	// selection fails.
	Offset time.Duration

	// WindowLower and WindowUpper bound (inclusively) the lowest offset
	// window covered by at least the required number of sources. They are
	// zero when selection fails. A zero-width window (WindowLower equals
	// WindowUpper) is valid and means the intervals only touch at a boundary.
	WindowLower time.Duration
	WindowUpper time.Duration

	// Quorum is the number of distinct sources that had to agree.
	Quorum int

	// Sources reports the status of every input sample, in input order.
	Sources []SourceStatus
}

// IncludedSources returns the SourceStatus values of the truechimers, in
// input order.
func (r *SelectionResult) IncludedSources() []SourceStatus {
	var included []SourceStatus
	for _, s := range r.Sources {
		if s.Included {
			included = append(included, s)
		}
	}
	return included
}

// selectionCandidate describes a valid sample allowed to cast its source's
// single vote. Its synchronization interval is the closed range [lo, hi].
type selectionCandidate struct {
	index  int
	offset time.Duration
	lo     time.Duration
	hi     time.Duration
}

// selectionEvent is a lower or upper endpoint of a candidate interval, used
// by the intersection sweep.
type selectionEvent struct {
	value time.Duration
	isLow bool
	index int
}

// SelectClockOffset performs a pure-computation selection of a clock offset
// from multiple time sources. It does not query the network and does not
// modify the system clock.
// Each input sample is first checked with Response.Validate; samples that
// fail validation are excluded. Each distinct SourceID then casts at most
// one vote, using the first valid sample supplied for that ID.
//
// Every voting source defines the closed synchronization interval
// [ClockOffset-RootDistance, ClockOffset+RootDistance]. Using the
// intersection algorithm described by RFC 5905 section 12.1, the lowest
// offset window covered by at least MinSources (by default a simple
// majority) distinct intervals is selected. All intervals covering it are
// the truechimers and determine the suggested offset; the rest are reported
// as falsetickers.
//
// Deterministic tie rules are used throughout:
//   - Closed intervals: intervals that merely touch at an endpoint intersect.
//   - When more than one window has quorum coverage, the lowest window wins.
//   - When sorting offsets, ties are broken by the original input index.
//   - The median of an even number of offsets is their midpoint, with a
//     half-nanosecond rounded toward zero.
//   - A sample whose interval endpoints overflow time.Duration is excluded
//     with ReasonOverflow rather than silently clamped.
//
// If no quorum intersection exists, ErrNoConsistentIntersection is
// returned; the offset of all sources is never averaged as a fallback. The
// returned SelectionResult is still populated and may be inspected for the
// exclusion reasons.
func SelectClockOffset(sources []TimeSource, opts SelectOptions) (*SelectionResult, error) {
	if opts.MinSources < 0 {
		result := &SelectionResult{Sources: make([]SourceStatus, len(sources))}
		for i, src := range sources {
			result.Sources[i] = SourceStatus{SourceID: src.SourceID, Index: i, Reason: ReasonInvalid}
		}
		return result, ErrInvalidMinSources
	}

	result := &SelectionResult{Sources: make([]SourceStatus, len(sources))}
	candidates := make([]selectionCandidate, 0, len(sources))
	voted := make(map[string]bool)

	for i, src := range sources {
		status := SourceStatus{
			SourceID: src.SourceID,
			Index:    i,
			Reason:   ReasonInvalid,
		}

		if src.Response == nil {
			status.Detail = "response is nil"
			result.Sources[i] = status
			continue
		}
		status.ClockOffset = src.Response.ClockOffset
		status.Distance = src.Response.RootDistance

		if err := src.Response.Validate(); err != nil {
			status.Detail = err.Error()
			result.Sources[i] = status
			continue
		}

		if status.Distance < 0 {
			status.Detail = "negative synchronization distance"
			result.Sources[i] = status
			continue
		}

		lo, hi, err := intervalBounds(status.ClockOffset, status.Distance)
		if err != nil {
			status.Reason = ReasonOverflow
			status.Detail = err.Error()
			result.Sources[i] = status
			continue
		}

		if voted[src.SourceID] {
			status.Reason = ReasonDuplicate
			status.Detail = fmt.Sprintf("duplicate sample for source %q", src.SourceID)
			result.Sources[i] = status
			continue
		}

		voted[src.SourceID] = true
		result.Sources[i] = status
		candidates = append(candidates, selectionCandidate{
			index:  i,
			offset: status.ClockOffset,
			lo:     lo,
			hi:     hi,
		})
	}

	n := len(candidates)
	if n == 0 {
		return result, ErrNoValidSources
	}

	quorum := opts.MinSources
	if quorum == 0 {
		quorum = n/2 + 1
	}
	result.Quorum = quorum
	if quorum > n {
		markReasons(result, candidates, ReasonFalseticker)
		return result, ErrInsufficientSources
	}

	events := make([]selectionEvent, 0, 2*n)
	for _, c := range candidates {
		events = append(events,
			selectionEvent{value: c.lo, isLow: true, index: c.index},
			selectionEvent{value: c.hi, isLow: false, index: c.index},
		)
	}

	// Lows are ordered before highs at the same value, so closed intervals
	// touching only at a boundary intersect. Input index is the final
	// tie-break for fully deterministic output.
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].value != events[j].value {
			return events[i].value < events[j].value
		}
		if events[i].isLow != events[j].isLow {
			return events[i].isLow
		}
		return events[i].index < events[j].index
	})

	// RFC 5905 intersection sweep: find the lowest window covered by at
	// least quorum intervals.
	active := 0
	windowOpen := false
	var lower, upper time.Duration
	found := false
	for _, e := range events {
		if e.isLow {
			active++
			if !windowOpen && active >= quorum {
				windowOpen = true
				lower = e.value
			}
			continue
		}
		if windowOpen && active >= quorum {
			upper = e.value
			found = true
			break
		}
		active--
	}

	if !found {
		markReasons(result, candidates, ReasonFalseticker)
		return result, ErrNoConsistentIntersection
	}

	result.WindowLower = lower
	result.WindowUpper = upper

	included := make([]selectionCandidate, 0, n)
	for _, c := range candidates {
		// A truechimer's closed interval contains the entire majority
		// window [lower, upper].
		if c.lo <= lower && c.hi >= upper {
			included = append(included, c)
			s := &result.Sources[c.index]
			s.Included = true
			s.Reason = ReasonTruechimer
		} else {
			result.Sources[c.index].Reason = ReasonFalseticker
		}
	}

	sort.SliceStable(included, func(i, j int) bool {
		if included[i].offset != included[j].offset {
			return included[i].offset < included[j].offset
		}
		return included[i].index < included[j].index
	})
	result.Offset = medianOffset(included)

	return result, nil
}

// markReasons assigns the same reason to every candidate's status.
func markReasons(result *SelectionResult, candidates []selectionCandidate, reason SelectionReason) {
	for _, c := range candidates {
		result.Sources[c.index].Reason = reason
	}
}

// intervalBounds returns the closed synchronization interval
// [offset-distance, offset+distance]. An error is returned instead of
// wrapping a value if either endpoint cannot be represented.
func intervalBounds(offset, distance time.Duration) (lo, hi time.Duration, err error) {
	lo, loOK := addDuration(offset, -distance)
	hi, hiOK := addDuration(offset, distance)
	if !loOK || !hiOK {
		return 0, 0, fmt.Errorf("synchronization interval around %s with distance %s overflows time.Duration", offset, distance)
	}
	return lo, hi, nil
}

// addDuration returns a+b, reporting false if the result cannot be
// represented as a time.Duration.
func addDuration(a, b time.Duration) (time.Duration, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64, false
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64, false
	}
	return a + b, true
}

// medianOffset returns the median clock offset of the candidates, which must
// already be sorted by offset and input index.
func medianOffset(sorted []selectionCandidate) time.Duration {
	k := len(sorted)
	if k%2 == 1 {
		return sorted[k/2].offset
	}
	return midpoint(sorted[k/2-1].offset, sorted[k/2].offset)
}

// midpoint returns (a+b)/2 without overflowing. The result matches Go's
// truncating integer division, so a half-nanosecond remainder rounds toward
// zero.
func midpoint(a, b time.Duration) time.Duration {
	q := a/2 + b/2
	r := a%2 + b%2
	switch {
	case r >= 2:
		return q + 1
	case r <= -2:
		return q - 1
	case r == 1 && q < 0:
		return q + 1
	case r == -1 && q > 0:
		return q - 1
	default:
		return q
	}
}
