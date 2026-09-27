package cron

import "time"

// SpecSchedule specifies a duty cycle (to the second granularity), based on a
// traditional crontab specification. It is computed initially and stored as bit sets.
type SpecSchedule struct {
	Second, Minute, Hour, Dom, Month, Dow uint64

	// Override location for this schedule.
	Location *time.Location

	// RepeatedPolicy determines how wall-clock times that occur twice
	// (daylight-saving fall-back) are handled. The zero value preserves
	// the historical behavior.
	RepeatedPolicy RepeatedPolicy

	// SkippedPolicy determines how wall-clock times that do not exist
	// (daylight-saving spring-forward) are handled. The zero value
	// preserves the historical behavior.
	SkippedPolicy SkippedPolicy
}

// bounds provides a range of acceptable values (plus a map of name to value).
type bounds struct {
	min, max uint
	names    map[string]uint
}

// The bounds for each field.
var (
	seconds = bounds{0, 59, nil}
	minutes = bounds{0, 59, nil}
	hours   = bounds{0, 23, nil}
	dom     = bounds{1, 31, nil}
	months  = bounds{1, 12, map[string]uint{
		"jan": 1,
		"feb": 2,
		"mar": 3,
		"apr": 4,
		"may": 5,
		"jun": 6,
		"jul": 7,
		"aug": 8,
		"sep": 9,
		"oct": 10,
		"nov": 11,
		"dec": 12,
	}}
	dow = bounds{0, 6, map[string]uint{
		"sun": 0,
		"mon": 1,
		"tue": 2,
		"wed": 3,
		"thu": 4,
		"fri": 5,
		"sat": 6,
	}}
)

const (
	// Set the top bit if a star was included in the expression.
	starBit = 1 << 63
)

// Next returns the next time this schedule is activated, greater than the given
// time.  If no time can be found to satisfy the schedule, return the zero time.
func (s *SpecSchedule) Next(t time.Time) time.Time {
	// Schedules with an explicit DST policy use a wall-clock based search
	// that resolves ambiguous and nonexistent local times deterministically.
	if s.RepeatedPolicy != RepeatedDefault || s.SkippedPolicy != SkippedDefault {
		return s.nextPolicy(t)
	}

	// General approach
	//
	// For Month, Day, Hour, Minute, Second:
	// Check if the time value matches.  If yes, continue to the next field.
	// If the field doesn't match the schedule, then increment the field until it matches.
	// While incrementing the field, a wrap-around brings it back to the beginning
	// of the field list (since it is necessary to re-verify previous field
	// values)

	// Convert the given time into the schedule's timezone, if one is specified.
	// Save the original timezone so we can convert back after we find a time.
	// Note that schedules without a time zone specified (time.Local) are treated
	// as local to the time provided.
	origLocation := t.Location()
	loc := s.Location
	if loc == time.Local {
		loc = t.Location()
	}
	if s.Location != time.Local {
		t = t.In(s.Location)
	}

	// Start at the earliest possible time (the upcoming second).
	t = t.Add(1*time.Second - time.Duration(t.Nanosecond())*time.Nanosecond)

	// This flag indicates whether a field has been incremented.
	added := false

	// If no time is found within five years, return zero.
	yearLimit := t.Year() + 5

WRAP:
	if t.Year() > yearLimit {
		return time.Time{}
	}

	// Find the first applicable month.
	// If it's this month, then do nothing.
	for 1<<uint(t.Month())&s.Month == 0 {
		// If we have to add a month, reset the other parts to 0.
		if !added {
			added = true
			// Otherwise, set the date at the beginning (since the current time is irrelevant).
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
		}
		t = t.AddDate(0, 1, 0)

		// Wrapped around.
		if t.Month() == time.January {
			goto WRAP
		}
	}

	// Now get a day in that month.
	//
	// NOTE: This causes issues for daylight savings regimes where midnight does
	// not exist.  For example: Sao Paulo has DST that transforms midnight on
	// 11/3 into 1am. Handle that by noticing when the Hour ends up != 0.
	for !dayMatches(s, t) {
		if !added {
			added = true
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		}
		t = t.AddDate(0, 0, 1)
		// Notice if the hour is no longer midnight due to DST.
		// Add an hour if it's 23, subtract an hour if it's 1.
		if t.Hour() != 0 {
			if t.Hour() > 12 {
				t = t.Add(time.Duration(24-t.Hour()) * time.Hour)
			} else {
				t = t.Add(time.Duration(-t.Hour()) * time.Hour)
			}
		}

		if t.Day() == 1 {
			goto WRAP
		}
	}

	for 1<<uint(t.Hour())&s.Hour == 0 {
		if !added {
			added = true
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
		}
		t = t.Add(1 * time.Hour)

		if t.Hour() == 0 {
			goto WRAP
		}
	}

	for 1<<uint(t.Minute())&s.Minute == 0 {
		if !added {
			added = true
			t = t.Truncate(time.Minute)
		}
		t = t.Add(1 * time.Minute)

		if t.Minute() == 0 {
			goto WRAP
		}
	}

	for 1<<uint(t.Second())&s.Second == 0 {
		if !added {
			added = true
			t = t.Truncate(time.Second)
		}
		t = t.Add(1 * time.Second)

		if t.Second() == 0 {
			goto WRAP
		}
	}

	return t.In(origLocation)
}

// nextPolicy implements Next for schedules carrying an explicit DST policy.
// It searches in pure wall-clock space (using UTC as a stand-in so that
// wall-clock arithmetic is never disturbed by transitions) and resolves each
// matching wall-clock time into absolute instant(s) according to the
// configured policies. The result is always strictly after the input.
func (s *SpecSchedule) nextPolicy(t time.Time) time.Time {
	origLocation := t.Location()
	loc := s.Location
	if loc == time.Local {
		loc = t.Location()
	}
	input := t
	lt := t.In(loc)

	// Wall-clock cursor, expressed as if its fields were UTC.
	w := time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), lt.Minute(), lt.Second(), 0, time.UTC)

	// If the input falls inside a fall-back fold, wall-clock times shortly
	// before the cursor still have their second occurrence ahead of the
	// input. Rewind the cursor to the start of the repeated interval so
	// those occurrences are considered; occurrences not later than the
	// input are filtered out when resolving.
	for _, tr := range transitionsNear(input, loc) {
		if tr.before > tr.after {
			fold := time.Duration(tr.before-tr.after) * time.Second
			if input.Before(tr.at.Add(fold)) {
				repeatStart := tr.at.Add(time.Duration(tr.after) * time.Second)
				if repeatStart.Before(w) {
					w = repeatStart
				}
			}
		}
	}

	// If no time is found within five years, return zero.
	yearLimit := w.Year() + 5

	for {
		if w.Year() > yearLimit {
			return time.Time{}
		}
		if 1<<uint(w.Month())&s.Month == 0 {
			w = time.Date(w.Year(), w.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
			continue
		}
		if !dayMatches(s, w) {
			w = time.Date(w.Year(), w.Month(), w.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if 1<<uint(w.Hour())&s.Hour == 0 {
			w = time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
			continue
		}
		if 1<<uint(w.Minute())&s.Minute == 0 {
			w = w.Truncate(time.Minute).Add(time.Minute)
			continue
		}
		if 1<<uint(w.Second())&s.Second == 0 {
			w = w.Add(time.Second)
			continue
		}
		if next, ok := s.resolve(w, loc, input); ok {
			return next.In(origLocation)
		}
		// The matching wall-clock time has no usable occurrence (it falls
		// into a skipped gap, or all of its occurrences are not later than
		// the input). Advance one second and keep searching.
		w = w.Add(time.Second)
	}
}

// resolve maps a matching wall-clock time to the next absolute activation
// instant strictly after input, honoring the schedule's DST policies.
func (s *SpecSchedule) resolve(wall time.Time, loc *time.Location, input time.Time) (time.Time, bool) {
	occ := occurrences(wall, loc)
	if len(occ) == 0 {
		// The wall-clock time does not exist (spring-forward gap).
		if s.SkippedPolicy == SkippedShift {
			if end, ok := gapEnd(wall, loc); ok && end.After(input) {
				return end, true
			}
		}
		return time.Time{}, false
	}
	switch s.RepeatedPolicy {
	case RepeatedFirst:
		occ = occ[:1]
	case RepeatedSecond:
		if len(occ) > 1 {
			occ = occ[1:]
		}
	}
	for _, o := range occ {
		if o.After(input) {
			return o, true
		}
	}
	return time.Time{}, false
}

// dayMatches returns true if the schedule's day-of-week and day-of-month
// restrictions are satisfied by the given time.
func dayMatches(s *SpecSchedule, t time.Time) bool {
	var (
		domMatch bool = 1<<uint(t.Day())&s.Dom > 0
		dowMatch bool = 1<<uint(t.Weekday())&s.Dow > 0
	)
	if s.Dom&starBit > 0 || s.Dow&starBit > 0 {
		return domMatch && dowMatch
	}
	return domMatch || dowMatch
}
