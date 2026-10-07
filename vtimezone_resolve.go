package ics

import (
	"fmt"
	"sort"
	"time"
)

// genState accumulates the generated onset instants of one observance across
// incremental year builds.
type genState struct {
	onsets   []time.Time
	lastYear int
	primed   bool
	done     bool
}

// extendThrough lazily appends onsets up to and including the given year.
func (o *Observance) extendThrough(year int, state *genState) {
	if !state.primed {
		state.primed = true
		state.lastYear = o.DTStart.Year()
		state.onsets = appendUniqueNaive(state.onsets, o.DTStart)
		if o.RRule == nil {
			for _, r := range o.RDates {
				state.onsets = appendUniqueNaive(state.onsets, r)
			}
			sortNaive(state.onsets)
			state.done = true
			return
		}
		for _, r := range o.rdatesInYear(o.DTStart.Year()) {
			if !r.Before(o.DTStart) && o.untilAllows(r) {
				state.onsets = appendUniqueNaive(state.onsets, r)
			}
		}
		if o.countReached(state) {
			state.done = true
		}
	}

	if o.RRule == nil || state.done {
		return
	}
	interval := o.RRule.Interval
	if interval <= 0 {
		interval = 1
	}
	startYear := o.DTStart.Year()
	for y := state.lastYear + 1; y <= year && !state.done; y++ {
		state.lastYear = y
		if (y-startYear)%interval != 0 {
			continue
		}
		if !o.untilAllowsYear(y) {
			state.done = true
			break
		}
		for _, cand := range o.ruleCandidatesForYear(y) {
			if cand.Before(o.DTStart) {
				continue
			}
			if !o.untilAllows(cand) {
				state.done = true
				break
			}
			state.onsets = appendUniqueNaive(state.onsets, cand)
			if o.countReached(state) {
				state.done = true
				break
			}
		}
		for _, r := range o.rdatesInYear(y) {
			if r.Before(o.DTStart) || !o.untilAllows(r) {
				continue
			}
			state.onsets = appendUniqueNaive(state.onsets, r)
		}
	}
	sortNaive(state.onsets)
}

func (o *Observance) countReached(state *genState) bool {
	return o.RRule != nil && o.RRule.Count > 0 && len(state.onsets) >= o.RRule.Count
}

func (o *Observance) untilAllowsYear(year int) bool {
	if o.RRule == nil || o.RRule.Until.IsZero() {
		return true
	}
	return year <= o.RRule.Until.Year()
}

func (o *Observance) untilAllows(t time.Time) bool {
	if o.RRule == nil || o.RRule.Until.IsZero() {
		return true
	}
	return !t.After(o.RRule.Until)
}

func (o *Observance) rdatesInYear(year int) []time.Time {
	var out []time.Time
	for _, r := range o.RDates {
		if r.Year() == year {
			out = append(out, r)
		}
	}
	return out
}

// ruleCandidatesForYear expands the yearly RRULE candidate dates for the year
// using the time-of-day (and default day/month) inherited from DTSTART.
func (o *Observance) ruleCandidatesForYear(year int) []time.Time {
	hh, mi, ss := o.DTStart.Clock()
	nsec := o.DTStart.Nanosecond()
	at := func(month time.Month, day int) time.Time {
		return time.Date(year, month, day, hh, mi, ss, nsec, time.UTC)
	}

	var months []time.Month
	if len(o.RRule.ByMonth) > 0 {
		for _, m := range o.RRule.ByMonth {
			months = append(months, time.Month(m))
		}
	} else {
		months = []time.Month{o.DTStart.Month()}
	}

	var out []time.Time
	for _, month := range months {
		daysInMonth := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
		if len(o.RRule.ByDay) > 0 {
			for _, wd := range o.RRule.ByDay {
				target := weekdayIndex(wd.Day)
				if target < 0 {
					continue
				}
				if wd.OrdWeek == 0 {
					for day := 1; day <= daysInMonth; day++ {
						t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
						if int(t.Weekday()) == target {
							out = append(out, at(month, day))
						}
					}
				} else {
					if day, ok := nthWeekdayInMonth(year, month, target, wd.OrdWeek); ok {
						out = append(out, at(month, day))
					}
				}
			}
			continue
		}
		if len(o.RRule.ByMonthDay) > 0 {
			for _, md := range o.RRule.ByMonthDay {
				day := md
				if day < 0 {
					day = daysInMonth + day + 1
				}
				if day >= 1 && day <= daysInMonth {
					out = append(out, at(month, day))
				}
			}
			continue
		}
		day := o.DTStart.Day()
		if day > daysInMonth {
			day = daysInMonth
		}
		out = append(out, at(month, day))
	}
	return out
}

func sortNaive(ts []time.Time) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
}

func appendUniqueNaive(ts []time.Time, t time.Time) []time.Time {
	for _, existing := range ts {
		if existing.Equal(t) {
			return ts
		}
	}
	return append(ts, t)
}

func (d *TimezoneDefinition) buildThrough(year int) ([]Transition, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.states == nil {
		d.states = make([]genState, len(d.observances))
	}
	for d.built < year {
		d.built++
		for i, obs := range d.observances {
			obs.extendThrough(d.built, &d.states[i])
			for _, wall := range d.states[i].onsets {
				if wall.Year() != d.built {
					continue
				}
				d.table = append(d.table, Transition{
					obsIndex:   i,
					At:         wall.Add(-obs.OffsetFrom).UTC(),
					AtLocal:    wall,
					OffsetFrom: obs.OffsetFrom,
					OffsetTo:   obs.OffsetTo,
					TZName:     obs.TZName,
					Kind:       obs.Kind,
				})
			}
		}
		sort.SliceStable(d.table, func(i, j int) bool {
			if d.table[i].At.Equal(d.table[j].At) {
				return d.table[i].AtLocal.Before(d.table[j].AtLocal)
			}
			return d.table[i].At.Before(d.table[j].At)
		})
		d.table = dedupeTransitions(d.table)
		if err := validateTransitions(d.tzid, d.table); err != nil {
			return nil, err
		}
	}
	return d.table, nil
}

func dedupeTransitions(t []Transition) []Transition {
	if len(t) < 2 {
		return t
	}
	out := t[:1]
	for i := 1; i < len(t); i++ {
		prev := t[i-1]
		cur := t[i]
		if cur.At.Equal(prev.At) && cur.AtLocal.Equal(prev.AtLocal) &&
			cur.OffsetFrom == prev.OffsetFrom && cur.OffsetTo == prev.OffsetTo {
			continue
		}
		out = append(out, cur)
	}
	return out
}

// validateTransitions checks the offsets where observance coverage joins.
// Every onset must begin in the offset left behind by the previous onset;
// otherwise the VTIMEZONE is self-contradictory.
func validateTransitions(tzid string, t []Transition) error {
	for i := 1; i < len(t); i++ {
		prev, cur := t[i-1], t[i]
		if cur.At.Equal(prev.At) {
			if cur.obsIndex == prev.obsIndex {
				continue
			}
			return fmt.Errorf("%w: %s has two onsets at %s (%s -> %s and %s -> %s)",
				ErrVTimezoneContradiction, tzid, prev.At.UTC().Format(time.RFC3339),
				prev.OffsetFrom, prev.OffsetTo, cur.OffsetFrom, cur.OffsetTo)
		}
		// Repeated onsets of one observance (e.g. an RDATE) always restate
		// that observance's own constant TZOFFSETFROM/TZOFFSETTO; they do not
		// need to follow the offset left behind by a different observance.
		if cur.obsIndex == prev.obsIndex {
			continue
		}
		if cur.OffsetFrom != prev.OffsetTo {
			return fmt.Errorf("%w: %s onset %s expects TZOFFSETFROM %s but the previous observance leaves %s",
				ErrVTimezoneContradiction, tzid, cur.At.UTC().Format(time.RFC3339),
				cur.OffsetFrom, prev.OffsetTo)
		}
	}
	return nil
}

// Resolve converts a local wall time in this timezone to an absolute instant
// using only the embedded VTIMEZONE rules.
//
// Ambiguity handling at observance boundaries:
//   - When clocks are set forward, the wall times strictly between the
//     transition instant (old offset) and the same instant rendered in the
//     new offset do not exist. Such a time is reported as LocalTimeSkipped;
//     Time is computed with the offset that takes effect after the jump,
//     matching the POSIX/TZif convention.
//   - When clocks are set back, the overlapping wall times occur twice. They
//     are reported as LocalTimeRepeated: Time is the second (later)
//     observation and Alternatives holds the first (earlier) one. The exact
//     transition wall time belongs to the post-transition observance.
func (d *TimezoneDefinition) Resolve(local time.Time) (ResolvedTime, error) {
	wall := forceNaive(local, false)
	table, err := d.buildThrough(wall.Year() + 1)
	if err != nil {
		return ResolvedTime{}, err
	}
	firstStart := table[0].At.Add(table[0].OffsetTo).UTC()
	if len(table) == 0 || wall.Before(firstStart) {
		return ResolvedTime{}, fmt.Errorf("%w: %s has no rule for %s (earliest onset %s)",
			ErrVTimezoneBeforeCoverage, d.tzid,
			wall.Format(icalTimestampFormatLocal),
			firstStart.Format(icalTimestampFormatLocal))
	}

	type match struct {
		idx  int
		inst time.Time
	}
	var matches []match
	for i := range table {
		// The observance that takes effect at transition i owns wall times
		// from At+OffsetTo up to the next transition expressed in its own
		// offset. Intervals can overlap (fall back) or leave a gap
		// (spring forward).
		lo := table[i].At.Add(table[i].OffsetTo).UTC()
		offset := table[i].OffsetTo
		var hi time.Time
		if i+1 < len(table) {
			hi = table[i+1].At.Add(offset).UTC()
		}
		if (wall.After(lo) || wall.Equal(lo)) && (hi.IsZero() || wall.Before(hi)) {
			matches = append(matches, match{idx: i, inst: wall.Add(-offset).UTC()})
		}
	}

	switch len(matches) {
	case 0:
		for i := 0; i+1 < len(table); i++ {
			tr := table[i+1]
			// Wall-clock span that does not exist: the transition instant
			// rendered in the old offset up to the same instant in the new.
			gapLo := tr.At.Add(tr.OffsetFrom).UTC()
			gapHi := tr.At.Add(tr.OffsetTo).UTC()
			if wall.Before(gapLo) || !wall.Before(gapHi) {
				continue
			}
			return ResolvedTime{
				Time:          wall.Add(-tr.OffsetTo).UTC(),
				Local:         wall.Add(tr.OffsetTo - tr.OffsetFrom),
				Offset:        tr.OffsetTo,
				OffsetSeconds: int(tr.OffsetTo / time.Second),
				TZName:        tr.TZName,
				Status:        LocalTimeSkipped,
				Transition:    &tr,
			}, nil
		}
		return ResolvedTime{}, fmt.Errorf("%w: %s cannot place %s on any observance",
			ErrVTimezoneContradiction, d.tzid, wall.Format(icalTimestampFormatLocal))
	case 1:
		m := matches[0]
		tr := table[m.idx]
		return ResolvedTime{
			Time:          m.inst,
			Local:         wall,
			Offset:        tr.OffsetTo,
			OffsetSeconds: int(tr.OffsetTo / time.Second),
			TZName:        tr.TZName,
			Status:        LocalTimeOK,
		}, nil
	default:
		first := matches[0]
		last := matches[len(matches)-1]
		firstTr := table[first.idx]
		lastTr := table[last.idx]
		earlier := ResolvedTime{
			Time:          first.inst,
			Local:         wall,
			Offset:        firstTr.OffsetTo,
			OffsetSeconds: int(firstTr.OffsetTo / time.Second),
			TZName:        firstTr.TZName,
			Status:        LocalTimeOK,
		}
		return ResolvedTime{
			Time:          last.inst,
			Local:         wall,
			Offset:        lastTr.OffsetTo,
			OffsetSeconds: int(lastTr.OffsetTo / time.Second),
			TZName:        lastTr.TZName,
			Status:        LocalTimeRepeated,
			Alternatives:  []ResolvedTime{earlier},
			Transition:    &lastTr,
		}, nil
	}
}
