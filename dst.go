package cron

import (
	"sort"
	"time"
)

// RepeatedPolicy determines how a schedule handles wall-clock times that
// occur twice because of a daylight-saving fall-back transition.
type RepeatedPolicy int

const (
	// RepeatedDefault preserves the historical behavior: the schedule is
	// evaluated in wall-clock time and a repeated local time may activate
	// the job once or twice, depending on how the clock unfolds.
	RepeatedDefault RepeatedPolicy = iota

	// RepeatedFirst activates the job only at the first occurrence of a
	// repeated wall-clock time.
	RepeatedFirst

	// RepeatedSecond activates the job only at the second occurrence of a
	// repeated wall-clock time. Wall-clock times that occur only once are
	// unaffected.
	RepeatedSecond

	// RepeatedBoth activates the job at both occurrences of a repeated
	// wall-clock time.
	RepeatedBoth
)

// SkippedPolicy determines how a schedule handles wall-clock times that do
// not exist because of a daylight-saving spring-forward transition.
type SkippedPolicy int

const (
	// SkippedDefault preserves the historical behavior: a nonexistent
	// wall-clock time never activates the job.
	SkippedDefault SkippedPolicy = iota

	// SkippedSkip skips activations whose wall-clock time does not exist.
	SkippedSkip

	// SkippedShift shifts an activation whose wall-clock time does not exist
	// to the first valid moment after the clock jump ends.
	SkippedShift
)

// dstTransition describes a single UTC offset change in a time.Location.
type dstTransition struct {
	at            time.Time // the absolute instant of the transition
	before, after int       // zone offsets in seconds before and after
}

// transitionsNear returns every offset transition of loc within a window
// surrounding ref. The window is wide enough that any wall-clock time whose
// fields are read from an instant near ref is covered.
func transitionsNear(ref time.Time, loc *time.Location) []dstTransition {
	const (
		window = 72 * time.Hour
		step   = time.Hour
	)
	var transitions []dstTransition
	start := ref.Add(-window)
	end := ref.Add(window)
	_, prevOff := start.In(loc).Zone()
	prev := start
	for t := start.Add(step); !t.After(end); t = t.Add(step) {
		_, off := t.In(loc).Zone()
		if off != prevOff {
			transitions = append(transitions, dstTransition{
				at:     findTransition(prev, t, loc, prevOff),
				before: prevOff,
				after:  off,
			})
			prevOff = off
		}
		prev = t
	}
	return transitions
}

// findTransition returns the exact instant within (lo, hi] at which loc's
// zone offset changes away from off. lo must have offset off, hi must not.
func findTransition(lo, hi time.Time, loc *time.Location, off int) time.Time {
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2)
		if _, o := mid.In(loc).Zone(); o == off {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}

// occurrences returns the absolute instants, in chronological order, at which
// the wall-clock time wall occurs in loc. wall is expressed as if its fields
// were UTC. A wall time inside a fall-back fold yields two instants, a wall
// time inside a spring-forward gap yields none.
func occurrences(wall time.Time, loc *time.Location) []time.Time {
	seen := make(map[int]bool)
	var offsets []int
	add := func(off int) {
		if !seen[off] {
			seen[off] = true
			offsets = append(offsets, off)
		}
	}
	_, off := wall.Add(-72 * time.Hour).In(loc).Zone()
	add(off)
	for _, tr := range transitionsNear(wall, loc) {
		add(tr.after)
	}

	var result []time.Time
	for _, off := range offsets {
		cand := wall.Add(-time.Duration(off) * time.Second)
		c := cand.In(loc)
		if c.Year() == wall.Year() && c.Month() == wall.Month() && c.Day() == wall.Day() &&
			c.Hour() == wall.Hour() && c.Minute() == wall.Minute() && c.Second() == wall.Second() {
			result = append(result, cand)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Before(result[j]) })
	return result
}

// gapEnd returns the first valid instant at or after the nonexistent
// wall-clock time wall, i.e. the instant at which the spring-forward gap
// containing wall ends. It reports false if wall is not inside a gap.
func gapEnd(wall time.Time, loc *time.Location) (time.Time, bool) {
	for _, tr := range transitionsNear(wall, loc) {
		if tr.after > tr.before {
			// The nonexistent wall-clock interval, expressed in the same
			// wall-as-UTC terms as wall.
			gapStart := tr.at.Add(time.Duration(tr.before) * time.Second)
			gapStop := tr.at.Add(time.Duration(tr.after) * time.Second)
			if !wall.Before(gapStart) && wall.Before(gapStop) {
				return tr.at, true
			}
		}
	}
	return time.Time{}, false
}
