package cron

import (
	"sort"
	"time"
)

// AmbiguousTimePolicy determines how a schedule behaves when a wall-clock
// time occurs twice on the day clocks are turned back (the "fall-back" DST
// transition).
type AmbiguousTimePolicy int

const (
	// AmbiguousDefault keeps the historical behavior: the schedule advances
	// in absolute time, which selects the first occurrence of an ambiguous
	// wall-clock time when it is approached from before the repeated
	// period. This is the zero value.
	AmbiguousDefault AmbiguousTimePolicy = iota

	// AmbiguousFirst runs the job only at the first occurrence of an
	// ambiguous wall-clock time.
	AmbiguousFirst

	// AmbiguousSecond runs the job only at the second occurrence of an
	// ambiguous wall-clock time.
	AmbiguousSecond

	// AmbiguousBoth runs the job at both occurrences of an ambiguous
	// wall-clock time.
	AmbiguousBoth
)

// NonexistentTimePolicy determines how a schedule behaves when a wall-clock
// time does not exist on the day clocks jump forward (the "spring-forward"
// DST transition).
type NonexistentTimePolicy int

const (
	// NonexistentDefault keeps the historical behavior: a wall-clock time
	// that does not exist is skipped. This is the zero value.
	NonexistentDefault NonexistentTimePolicy = iota

	// NonexistentSkip skips the job when its wall-clock time does not exist.
	NonexistentSkip

	// NonexistentPush runs the job at the first valid moment after the
	// forward jump ends, when its wall-clock time does not exist.
	NonexistentPush
)

// WithDSTPolicy returns a Schedule that behaves like the given schedule, but
// resolves daylight-saving-time edge cases using the given policies.
//
// The policies only apply to *SpecSchedule values (i.e. schedules parsed from
// crontab specs, including those using the CRON_TZ=/TZ= prefix, which follow
// the same rules). Any other schedule, such as the fixed-interval schedule
// produced by "@every", is returned unchanged: fixed-interval semantics are
// not affected by DST.
func WithDSTPolicy(sched Schedule, ambiguous AmbiguousTimePolicy, nonexistent NonexistentTimePolicy) Schedule {
	spec, ok := sched.(*SpecSchedule)
	if !ok {
		return sched
	}
	clone := *spec
	clone.AmbiguousPolicy = ambiguous
	clone.NonexistentPolicy = nonexistent
	return &clone
}

const (
	// dstLookback is how far back the wall-clock candidate search starts
	// before the requested time, so that the second occurrence of an
	// ambiguous wall time is still found right after its first occurrence
	// fired. It exceeds any DST setback in the IANA database.
	dstLookback = 4 * time.Hour

	// dstOffsetWindow bounds how far a wall-clock time may drift from its
	// absolute instant, covering every zone offset in the IANA database.
	dstOffsetWindow = 16 * time.Hour
)

// nextWithDSTPolicy implements Next for schedules carrying an explicit
// AmbiguousPolicy or NonexistentPolicy.
//
// It enumerates matching wall-clock times in the schedule's location (which
// is DST-free by construction), maps each candidate to the absolute instant
// or instants at which that wall time actually occurs, applying the
// configured policies, and returns the earliest resulting instant strictly
// later than t.
func (s *SpecSchedule) nextWithDSTPolicy(t time.Time) time.Time {
	origLocation := t.Location()
	loc := s.Location
	if loc == time.Local {
		loc = t.Location()
	}
	t = t.In(loc)

	// The smallest still-valid activation is the upcoming second.
	after := t.Add(1*time.Second - time.Duration(t.Nanosecond())*time.Nanosecond)

	// Wall-clock cursor, represented as a UTC time holding the local
	// wall-clock components. It starts slightly in the past so that the
	// second occurrence of an ambiguous wall time is still considered right
	// after its first occurrence fired; stale candidates are filtered out
	// by the comparison with "after" below.
	cursor := wallTime(after).Add(-dstLookback)

	yearLimit := after.Year() + 5
	var best time.Time
	for {
		wall := s.nextWall(cursor)
		if wall.IsZero() || wall.Year() > yearLimit {
			break
		}
		for _, instant := range s.resolveWall(wall, loc) {
			if !instant.Before(after) && (best.IsZero() || instant.Before(best)) {
				best = instant
			}
		}
		// Once the candidate wall time is far enough ahead of the best
		// instant found, no later candidate can map to an earlier instant
		// (offsets and DST gaps are bounded well within a day).
		if !best.IsZero() && wall.After(wallTime(best).Add(27*time.Hour)) {
			break
		}
		cursor = wall
	}
	if best.IsZero() {
		return time.Time{}
	}
	return best.In(origLocation)
}

// nextWall returns the next wall-clock time matching this schedule, strictly
// later than the given naive wall time. Both the argument and the result are
// UTC times whose components are interpreted as local wall-clock components.
// It reuses the standard field-matching algorithm with a UTC location, which
// is free of DST transitions and therefore advances in pure wall-clock terms.
func (s *SpecSchedule) nextWall(naive time.Time) time.Time {
	shadow := &SpecSchedule{
		Second:   s.Second,
		Minute:   s.Minute,
		Hour:     s.Hour,
		Dom:      s.Dom,
		Month:    s.Month,
		Dow:      s.Dow,
		Location: time.UTC,
	}
	return shadow.Next(naive)
}

// resolveWall maps a naive wall-clock candidate to the absolute instants at
// which it should run in the given location, applying the schedule's DST
// policies. It returns no instant when the wall time does not exist and the
// policy is NonexistentSkip.
func (s *SpecSchedule) resolveWall(wall time.Time, loc *time.Location) []time.Time {
	occurrences := wallOccurrences(wall, loc)
	switch len(occurrences) {
	case 0:
		if s.NonexistentPolicy == NonexistentPush {
			return []time.Time{gapEnd(wall, loc)}
		}
		return nil
	case 1:
		return occurrences
	}
	switch s.AmbiguousPolicy {
	case AmbiguousSecond:
		return occurrences[1:]
	case AmbiguousBoth:
		return occurrences
	default:
		// AmbiguousFirst, or AmbiguousDefault combined with an explicit
		// NonexistentPolicy.
		return occurrences[:1]
	}
}

// wallTime returns t's local wall-clock components as a UTC time, so that
// wall-clock values can be compared and ordered independently of any zone.
func wallTime(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

// wallOccurrences returns the absolute instants at which the given naive
// wall-clock time occurs in loc, in chronological order. The result is empty
// when the wall time does not exist (a spring-forward gap), has one element
// for ordinary times, and two elements when the wall time is ambiguous (a
// fall-back repeat).
func wallOccurrences(wall time.Time, loc *time.Location) []time.Time {
	// A wall time occurs once per distinct zone offset in effect around it.
	// Sample the offsets in a generous window; IANA transitions are at least
	// minutes apart, so 15-minute sampling cannot miss an offset.
	seen := make(map[int]bool)
	var occurrences []time.Time
	for x := wall.Add(-dstOffsetWindow); !x.After(wall.Add(dstOffsetWindow)); x = x.Add(15 * time.Minute) {
		_, offset := x.In(loc).Zone()
		if seen[offset] {
			continue
		}
		seen[offset] = true
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		if wallTime(candidate.In(loc)).Equal(wall) {
			occurrences = append(occurrences, candidate)
		}
	}
	sort.Slice(occurrences, func(i, j int) bool {
		return occurrences[i].Before(occurrences[j])
	})
	return occurrences
}

// gapEnd returns the first valid instant at or after a nonexistent wall-clock
// time: the moment the forward jump ends and valid wall times resume.
func gapEnd(wall time.Time, loc *time.Location) time.Time {
	// Binary search for the first instant whose wall clock is at or past the
	// nonexistent wall time. The window is guaranteed to straddle the jump.
	lo := wall.Add(-dstOffsetWindow)
	hi := wall.Add(dstOffsetWindow)
	for hi.Sub(lo) > time.Nanosecond {
		mid := lo.Add(hi.Sub(lo) / 2)
		if wallTime(mid.In(loc)).Before(wall) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}
