package ics

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ResolvedTime is the result of interpreting a local wall-clock time against a
// VTIMEZONE definition carried by the calendar itself.
type ResolvedTime struct {
	// Time is the resolved instant. Its location is a fixed zone whose offset is
	// the UTC offset in effect at that instant according to the VTIMEZONE.
	Time time.Time
	// Offset is the UTC offset in effect at Time.
	Offset time.Duration
	// Ambiguous reports that the local time occurred twice (a repeated wall-clock
	// time when clocks fall back). Per RFC 5545 3.3.5 the first occurrence is
	// used.
	Ambiguous bool
	// Nonexistent reports that the local time never occurred (it falls inside the
	// gap when clocks spring forward). Per RFC 5545 3.3.5 the value is
	// interpreted using the UTC offset in effect before the gap.
	Nonexistent bool
}

// VTimezoneResolver resolves local wall-clock times using a VTIMEZONE component
// embedded in the calendar. It never consults the host IANA/tz database, so the
// results depend only on the calendar's own definition and are identical on any
// machine.
type VTimezoneResolver struct {
	tzid        string
	observances []vtimezoneObservance
	offsets     []time.Duration // distinct TZOFFSETTO values, sorted
}

// TZID returns the TZID of the VTIMEZONE this resolver was built from.
func (r *VTimezoneResolver) TZID() string {
	return r.tzid
}

// NewVTimezoneResolver compiles the STANDARD/DAYLIGHT observances of a VTIMEZONE
// component into a resolver. Offsets are computed exclusively from the
// component's own DTSTART, TZOFFSETFROM, TZOFFSETTO, RDATE and (yearly) RRULE
// properties. It returns a specific error when required properties are missing
// or invalid (ErrVTimezoneObservanceProperty), when an RRULE uses constructs
// outside the supported yearly transition rules (ErrVTimezoneUnsupportedRule),
// or when the observances contradict each other (ErrVTimezoneContradictory).
func NewVTimezoneResolver(tz *VTimezone) (*VTimezoneResolver, error) {
	if tz == nil {
		return nil, errors.New("nil VTIMEZONE component")
	}
	tzidProp := tz.GetProperty(ComponentPropertyTzid)
	if tzidProp == nil || tzidProp.Value == "" {
		return nil, ErrVTimezoneMissingTZID
	}
	r := &VTimezoneResolver{tzid: tzidProp.Value}
	for _, sub := range tz.SubComponents() {
		switch obs := sub.(type) {
		case *Standard:
			parsed, err := parseVTimezoneObservance(&obs.ComponentBase, false)
			if err != nil {
				return nil, err
			}
			r.observances = append(r.observances, parsed)
		case *Daylight:
			parsed, err := parseVTimezoneObservance(&obs.ComponentBase, true)
			if err != nil {
				return nil, err
			}
			r.observances = append(r.observances, parsed)
		}
	}
	if len(r.observances) == 0 {
		return nil, ErrVTimezoneNoObservances
	}
	seen := map[time.Duration]bool{}
	for _, obs := range r.observances {
		if !seen[obs.offsetTo] {
			seen[obs.offsetTo] = true
			r.offsets = append(r.offsets, obs.offsetTo)
		}
	}
	sort.Slice(r.offsets, func(i, j int) bool { return r.offsets[i] < r.offsets[j] })
	// Validate the transitions around the explicitly declared onsets so that
	// contradictory observances are reported eagerly. Resolution validates the
	// transitions it expands as well.
	maxYear := 0
	for _, obs := range r.observances {
		if y := obs.dtstart.Year(); y > maxYear {
			maxYear = y
		}
		for _, rd := range obs.rdates {
			if y := rd.Year(); y > maxYear {
				maxYear = y
			}
		}
	}
	if _, err := r.transitionsThrough(maxYear + 2); err != nil {
		return nil, err
	}
	return r, nil
}

// VTimezoneResolver returns a resolver for the VTIMEZONE component in this
// calendar whose TZID matches tzid. It returns ErrVTimezoneNotFound when the
// calendar carries no such component.
func (cal *Calendar) VTimezoneResolver(tzid string) (*VTimezoneResolver, error) {
	for _, tz := range cal.Timezones() {
		prop := tz.GetProperty(ComponentPropertyTzid)
		if prop != nil && prop.Value == tzid {
			return NewVTimezoneResolver(tz)
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrVTimezoneNotFound, tzid)
}

// ResolveLocalTime interprets the wall-clock fields of t (year through
// nanosecond; the location of t is ignored) as a local time in the resolved
// VTIMEZONE and returns the corresponding instant.
//
// Repeated and nonexistent local times are handled as mandated by RFC 5545
// 3.3.5 and flagged on the returned ResolvedTime. A local time earlier than the
// first transition described by the VTIMEZONE is outside the rule coverage and
// yields ErrVTimezoneOutOfCoverage.
func (r *VTimezoneResolver) ResolveLocalTime(t time.Time) (ResolvedTime, error) {
	wall := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	transitions, err := r.transitionsThrough(wall.Year() + 1)
	if err != nil {
		return ResolvedTime{}, err
	}
	if len(transitions) == 0 {
		return ResolvedTime{}, ErrVTimezoneNoObservances
	}
	// A wall-clock time is valid for a UTC offset o exactly when interpreting it
	// with o lands on an instant where o is the offset in effect.
	var candidates []time.Time
	for _, offset := range r.offsets {
		u := wall.Add(-offset)
		inEffect, err := offsetInEffectAt(transitions, u)
		if err != nil {
			if errors.Is(err, ErrVTimezoneOutOfCoverage) {
				continue
			}
			return ResolvedTime{}, err
		}
		if inEffect.offsetTo == offset {
			candidates = append(candidates, u)
		}
	}
	switch {
	case len(candidates) == 1:
		return r.resolvedAt(transitions, candidates[0], false, false)
	case len(candidates) > 1:
		// RFC 5545 3.3.5: a repeated local time refers to its first occurrence.
		earliest := candidates[0]
		for _, u := range candidates[1:] {
			if u.Before(earliest) {
				earliest = u
			}
		}
		return r.resolvedAt(transitions, earliest, true, false)
	}
	// No offset maps the wall-clock time onto itself: it either falls into the
	// gap of a forward transition or precedes the described period.
	for _, tr := range transitions {
		gap := tr.offsetTo - tr.offsetFrom
		if gap > 0 && !wall.Before(tr.localOnset) && wall.Before(tr.localOnset.Add(gap)) {
			// RFC 5545 3.3.5: a nonexistent local time is interpreted using the
			// UTC offset in effect before the gap.
			return r.resolvedAt(transitions, wall.Add(-tr.offsetFrom), false, true)
		}
	}
	return ResolvedTime{}, fmt.Errorf("%w: local time %s precedes the first transition of TZID %q at %s",
		ErrVTimezoneOutOfCoverage, wall.Format(icalTimestampFormatLocal), r.tzid,
		transitions[0].localOnset.Format(icalTimestampFormatLocal))
}

// ResolveDateTime parses an iCalendar local DATE-TIME value (e.g.
// "20060102T150405") or DATE value (e.g. "20060102") and resolves it with
// ResolveLocalTime. UTC ("...Z") and otherwise non-local values are rejected.
func (r *VTimezoneResolver) ResolveDateTime(value string) (ResolvedTime, error) {
	if t, err := time.Parse(icalTimestampFormatLocal, value); err == nil {
		return r.ResolveLocalTime(t)
	}
	if t, err := time.Parse(icalDateFormatLocal, value); err == nil {
		return r.ResolveLocalTime(t)
	}
	return ResolvedTime{}, fmt.Errorf("%w, got '%s'", ErrTimeValueNotMatched, value)
}

// resolvedAt builds the ResolvedTime for an instant known to be covered.
func (r *VTimezoneResolver) resolvedAt(transitions []vtimezoneTransition, u time.Time, ambiguous, nonexistent bool) (ResolvedTime, error) {
	inEffect, err := offsetInEffectAt(transitions, u)
	if err != nil {
		return ResolvedTime{}, err
	}
	name := inEffect.name
	if name == "" {
		name = r.tzid
	}
	loc := time.FixedZone(name, int(inEffect.offsetTo/time.Second))
	return ResolvedTime{
		Time:        u.In(loc),
		Offset:      inEffect.offsetTo,
		Ambiguous:   ambiguous,
		Nonexistent: nonexistent,
	}, nil
}

// GetStartAtResolved resolves the DTSTART property against the given VTIMEZONE
// resolver. When the property carries a TZID parameter it must match the
// resolver's TZID.
func (cb *ComponentBase) GetStartAtResolved(r *VTimezoneResolver) (ResolvedTime, error) {
	return cb.getTimePropResolved(ComponentPropertyDtStart, r)
}

// GetEndAtResolved resolves the DTEND property against the given VTIMEZONE
// resolver. When the property carries a TZID parameter it must match the
// resolver's TZID.
func (cb *ComponentBase) GetEndAtResolved(r *VTimezoneResolver) (ResolvedTime, error) {
	return cb.getTimePropResolved(ComponentPropertyDtEnd, r)
}

func (cb *ComponentBase) getTimePropResolved(componentProperty ComponentProperty, r *VTimezoneResolver) (ResolvedTime, error) {
	if r == nil {
		return ResolvedTime{}, errors.New("nil VTimezoneResolver")
	}
	timeProp := cb.GetProperty(componentProperty)
	if timeProp == nil {
		return ResolvedTime{}, fmt.Errorf("%w: %s", ErrPropertyNotFound, componentProperty)
	}
	if tzids, ok := timeProp.ICalParameters[string(ParameterTzid)]; ok {
		if len(tzids) != 1 {
			return ResolvedTime{}, ErrExpectedOneTZID
		}
		if tzids[0] != r.TZID() {
			return ResolvedTime{}, fmt.Errorf("%w: property TZID %q, resolver TZID %q", ErrVTimezoneTZIDMismatch, tzids[0], r.TZID())
		}
	}
	return r.ResolveDateTime(timeProp.BaseProperty.Value)
}

// vtimezoneObservance is a compiled STANDARD or DAYLIGHT sub-component.
type vtimezoneObservance struct {
	daylight   bool
	dtstart    time.Time // local wall clock, kept in a UTC location for comparability
	offsetFrom time.Duration
	offsetTo   time.Duration
	rdates     []time.Time
	rrule      *RecurrenceRule
	name       string
}

// vtimezoneTransition is a single onset of an observance.
type vtimezoneTransition struct {
	utcOnset   time.Time
	localOnset time.Time
	offsetFrom time.Duration
	offsetTo   time.Duration
	daylight   bool
	name       string
}

func parseVTimezoneObservance(cb *ComponentBase, daylight bool) (vtimezoneObservance, error) {
	obs := vtimezoneObservance{daylight: daylight}
	kind := ComponentStandard
	if daylight {
		kind = ComponentDaylight
	}

	dtstartProp := cb.GetProperty(ComponentPropertyDtStart)
	if dtstartProp == nil {
		return obs, fmt.Errorf("%w: %s missing DTSTART", ErrVTimezoneObservanceProperty, kind)
	}
	if _, ok := dtstartProp.ICalParameters[string(ParameterTzid)]; ok {
		return obs, fmt.Errorf("%w: %s DTSTART must be a local time without TZID", ErrVTimezoneObservanceProperty, kind)
	}
	dtstart, err := parseVTimezoneLocalDateTime(dtstartProp.Value)
	if err != nil {
		return obs, fmt.Errorf("%s DTSTART: %w", kind, err)
	}
	obs.dtstart = dtstart

	offsetFromProp := cb.GetProperty(ComponentProperty(PropertyTzoffsetfrom))
	if offsetFromProp == nil {
		return obs, fmt.Errorf("%w: %s missing TZOFFSETFROM", ErrVTimezoneObservanceProperty, kind)
	}
	obs.offsetFrom, err = parseUTCOffsetValue(offsetFromProp.Value)
	if err != nil {
		return obs, fmt.Errorf("%s TZOFFSETFROM: %w", kind, err)
	}

	offsetToProp := cb.GetProperty(ComponentProperty(PropertyTzoffsetto))
	if offsetToProp == nil {
		return obs, fmt.Errorf("%w: %s missing TZOFFSETTO", ErrVTimezoneObservanceProperty, kind)
	}
	obs.offsetTo, err = parseUTCOffsetValue(offsetToProp.Value)
	if err != nil {
		return obs, fmt.Errorf("%s TZOFFSETTO: %w", kind, err)
	}

	for _, rdateProp := range cb.GetProperties(ComponentPropertyRdate) {
		for _, value := range strings.Split(rdateProp.Value, ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			rd, err := parseVTimezoneLocalDateTime(value)
			if err != nil {
				return obs, fmt.Errorf("%s RDATE: %w", kind, err)
			}
			obs.rdates = append(obs.rdates, rd)
		}
	}

	if rruleProp := cb.GetProperty(ComponentPropertyRrule); rruleProp != nil {
		rule, err := ParseRecurrenceRule(rruleProp.Value)
		if err != nil {
			return obs, fmt.Errorf("%s RRULE: %w", kind, err)
		}
		if err := validateVTimezoneRule(rule); err != nil {
			return obs, fmt.Errorf("%s RRULE: %w", kind, err)
		}
		obs.rrule = rule
	}

	if nameProp := cb.GetProperty(ComponentProperty(PropertyTzname)); nameProp != nil {
		obs.name = nameProp.Value
	}
	return obs, nil
}

// validateVTimezoneRule restricts observance rules to the yearly transition
// rules used by VTIMEZONE definitions.
func validateVTimezoneRule(rule *RecurrenceRule) error {
	if rule.Freq != FrequencyYearly {
		return fmt.Errorf("%w: FREQ=%s, only YEARLY transition rules are supported", ErrVTimezoneUnsupportedRule, rule.Freq)
	}
	if len(rule.ByMonthDay) > 0 && len(rule.ByDay) > 0 {
		return fmt.Errorf("%w: BYMONTHDAY combined with BYDAY", ErrVTimezoneUnsupportedRule)
	}
	switch {
	case len(rule.BySecond) > 0:
		return fmt.Errorf("%w: BYSECOND", ErrVTimezoneUnsupportedRule)
	case len(rule.ByMinute) > 0:
		return fmt.Errorf("%w: BYMINUTE", ErrVTimezoneUnsupportedRule)
	case len(rule.ByHour) > 0:
		return fmt.Errorf("%w: BYHOUR", ErrVTimezoneUnsupportedRule)
	case len(rule.ByWeekNo) > 0:
		return fmt.Errorf("%w: BYWEEKNO", ErrVTimezoneUnsupportedRule)
	case len(rule.ByYearDay) > 0:
		return fmt.Errorf("%w: BYYEARDAY", ErrVTimezoneUnsupportedRule)
	}
	return nil
}

func parseVTimezoneLocalDateTime(value string) (time.Time, error) {
	if t, err := time.Parse(icalTimestampFormatLocal, value); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%w: %q is not a local DATE-TIME value", ErrVTimezoneObservanceProperty, value)
}

var utcOffsetValuePattern = regexp.MustCompile(`^([+-])([0-9]{2})([0-9]{2})([0-9]{2})?$`)

// parseUTCOffsetValue parses an iCalendar UTC-OFFSET value such as "+0545" or
// "-033015" into a duration. Non-whole-hour and seconds precision are kept.
func parseUTCOffsetValue(value string) (time.Duration, error) {
	m := utcOffsetValuePattern.FindStringSubmatch(value)
	if m == nil {
		return 0, fmt.Errorf("%w: invalid UTC offset %q", ErrVTimezoneObservanceProperty, value)
	}
	hours, _ := strconv.Atoi(m[2])
	minutes, _ := strconv.Atoi(m[3])
	seconds := 0
	if m[4] != "" {
		seconds, _ = strconv.Atoi(m[4])
	}
	if minutes > 59 || seconds > 59 {
		return 0, fmt.Errorf("%w: invalid UTC offset %q", ErrVTimezoneObservanceProperty, value)
	}
	d := time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second
	if m[1] == "-" {
		d = -d
	}
	return d, nil
}

func formatUTCOffsetValue(d time.Duration) string {
	sign := "+"
	if d < 0 {
		sign = "-"
		d = -d
	}
	total := int(d / time.Second)
	if seconds := total % 60; seconds != 0 {
		return fmt.Sprintf("%s%02d%02d%02d", sign, total/3600, (total%3600)/60, seconds)
	}
	return fmt.Sprintf("%s%02d%02d", sign, total/3600, (total%3600)/60)
}

// onsetsThrough returns the sorted, de-duplicated local onset times of the
// observance up to and including throughYear.
func (obs *vtimezoneObservance) onsetsThrough(throughYear int) ([]time.Time, error) {
	set := map[time.Time]struct{}{obs.dtstart: {}}
	for _, rd := range obs.rdates {
		set[rd] = struct{}{}
	}
	if obs.rrule != nil {
		rule := obs.rrule
		interval := rule.Interval
		if interval <= 0 {
			interval = 1
		}
		count := 1 // DTSTART is always the first occurrence of the recurrence set.
		done := false
		for year := obs.dtstart.Year(); year <= throughYear && !done; year += interval {
			candidates, err := yearlyRuleCandidates(rule, year, obs.dtstart)
			if err != nil {
				return nil, err
			}
			for _, candidate := range candidates {
				if !candidate.After(obs.dtstart) {
					continue
				}
				if !rule.Until.IsZero() {
					if rule.UntilDateOnly {
						if candidate.Format(icalDateFormatLocal) > rule.Until.Format(icalDateFormatLocal) {
							done = true
							break
						}
					} else if candidate.Add(-obs.offsetFrom).After(rule.Until) {
						done = true
						break
					}
				}
				count++
				if rule.Count > 0 && count > rule.Count {
					done = true
					break
				}
				set[candidate] = struct{}{}
			}
		}
	}
	onsets := make([]time.Time, 0, len(set))
	for onset := range set {
		onsets = append(onsets, onset)
	}
	sort.Slice(onsets, func(i, j int) bool { return onsets[i].Before(onsets[j]) })
	return onsets, nil
}

var icalWeekdayToTimeWeekday = map[Weekday]time.Weekday{
	WeekdaySunday:    time.Sunday,
	WeekdayMonday:    time.Monday,
	WeekdayTuesday:   time.Tuesday,
	WeekdayWednesday: time.Wednesday,
	WeekdayThursday:  time.Thursday,
	WeekdayFriday:    time.Friday,
	WeekdaySaturday:  time.Saturday,
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// yearlyRuleCandidates computes the occurrence dates of a yearly observance
// rule within a single year, carrying the time of day over from dtstart.
func yearlyRuleCandidates(rule *RecurrenceRule, year int, dtstart time.Time) ([]time.Time, error) {
	months := rule.ByMonth
	if len(months) == 0 {
		months = []int{int(dtstart.Month())}
	}
	hour, minute, second := dtstart.Clock()
	var dates []time.Time
	for _, m := range months {
		if m < 1 || m > 12 {
			return nil, fmt.Errorf("%w: BYMONTH=%d", ErrVTimezoneUnsupportedRule, m)
		}
		month := time.Month(m)
		switch {
		case len(rule.ByMonthDay) > 0:
			for _, md := range rule.ByMonthDay {
				day, ok, err := monthDayDate(year, month, md)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue // e.g. February 30: the instance simply does not occur.
				}
				dates = append(dates, time.Date(year, month, day, hour, minute, second, 0, time.UTC))
			}
		case len(rule.ByDay) > 0:
			for _, wd := range rule.ByDay {
				days, err := weekdayDatesInMonth(year, month, wd)
				if err != nil {
					return nil, err
				}
				for _, day := range days {
					dates = append(dates, time.Date(year, month, day, hour, minute, second, 0, time.UTC))
				}
			}
		default:
			dates = append(dates, time.Date(year, month, dtstart.Day(), hour, minute, second, 0, time.UTC))
		}
	}
	seen := map[time.Time]bool{}
	unique := dates[:0]
	for _, d := range dates {
		if !seen[d] {
			seen[d] = true
			unique = append(unique, d)
		}
	}
	dates = unique
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	if len(rule.BySetPos) > 0 {
		var selected []time.Time
		for _, pos := range rule.BySetPos {
			idx := pos - 1
			if pos < 0 {
				idx = len(dates) + pos
			}
			if pos == 0 || idx < 0 || idx >= len(dates) {
				return nil, fmt.Errorf("%w: BYSETPOS=%d selects outside %d candidate(s)", ErrVTimezoneUnsupportedRule, pos, len(dates))
			}
			selected = append(selected, dates[idx])
		}
		dates = selected
		sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	}
	return dates, nil
}

// monthDayDate resolves a BYMONTHDAY value (negative counts from the end of the
// month) to a day of the month. ok is false when the date does not exist.
func monthDayDate(year int, month time.Month, md int) (day int, ok bool, err error) {
	dim := daysInMonth(year, month)
	switch {
	case md > 0:
		if md > dim {
			return 0, false, nil
		}
		return md, true, nil
	case md < 0:
		d := dim + md + 1
		if d < 1 {
			return 0, false, nil
		}
		return d, true, nil
	}
	return 0, false, fmt.Errorf("%w: BYMONTHDAY=0", ErrVTimezoneUnsupportedRule)
}

// weekdayDatesInMonth resolves a BYDAY entry to days of the month. An ordinal
// selects the nth matching weekday (negative counts from the end); an ordinal
// that does not occur yields no dates.
func weekdayDatesInMonth(year int, month time.Month, wdn WeekdayNum) ([]int, error) {
	weekday, ok := icalWeekdayToTimeWeekday[wdn.Day]
	if !ok {
		return nil, fmt.Errorf("%w: BYDAY weekday %q", ErrVTimezoneUnsupportedRule, wdn.Day)
	}
	var matches []int
	for day := 1; day <= daysInMonth(year, month); day++ {
		if time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Weekday() == weekday {
			matches = append(matches, day)
		}
	}
	switch {
	case wdn.OrdWeek == 0:
		return matches, nil
	case wdn.OrdWeek > 0:
		if wdn.OrdWeek > len(matches) {
			return nil, nil
		}
		return []int{matches[wdn.OrdWeek-1]}, nil
	default:
		idx := len(matches) + wdn.OrdWeek
		if idx < 0 {
			return nil, nil
		}
		return []int{matches[idx]}, nil
	}
}

// transitionsThrough builds the sorted, validated transition table covering all
// onsets up to and including throughYear.
func (r *VTimezoneResolver) transitionsThrough(throughYear int) ([]vtimezoneTransition, error) {
	var transitions []vtimezoneTransition
	for i := range r.observances {
		obs := &r.observances[i]
		onsets, err := obs.onsetsThrough(throughYear)
		if err != nil {
			return nil, err
		}
		for _, onset := range onsets {
			transitions = append(transitions, vtimezoneTransition{
				utcOnset:   onset.Add(-obs.offsetFrom),
				localOnset: onset,
				offsetFrom: obs.offsetFrom,
				offsetTo:   obs.offsetTo,
				daylight:   obs.daylight,
				name:       obs.name,
			})
		}
	}
	sort.SliceStable(transitions, func(i, j int) bool { return transitions[i].utcOnset.Before(transitions[j].utcOnset) })
	if err := validateVTimezoneTransitions(transitions); err != nil {
		return nil, err
	}
	return transitions, nil
}

// validateVTimezoneTransitions checks that the observances tell one coherent
// story: no two transitions may take effect at the same instant, and every
// transition must start from the offset the previous transition ended at.
func validateVTimezoneTransitions(transitions []vtimezoneTransition) error {
	for i := 1; i < len(transitions); i++ {
		prev, cur := transitions[i-1], transitions[i]
		if cur.utcOnset.Equal(prev.utcOnset) {
			return fmt.Errorf("%w: two transitions take effect at %s UTC",
				ErrVTimezoneContradictory, cur.utcOnset.Format(icalTimestampFormatUtc))
		}
		if cur.offsetFrom != prev.offsetTo {
			return fmt.Errorf("%w: transition at local %s declares TZOFFSETFROM %s but the preceding transition ended at %s",
				ErrVTimezoneContradictory,
				cur.localOnset.Format(icalTimestampFormatLocal),
				formatUTCOffsetValue(cur.offsetFrom),
				formatUTCOffsetValue(prev.offsetTo))
		}
	}
	return nil
}

// offsetInEffectAt returns the transition whose offset applies at UTC instant u.
func offsetInEffectAt(transitions []vtimezoneTransition, u time.Time) (vtimezoneTransition, error) {
	idx := sort.Search(len(transitions), func(i int) bool { return transitions[i].utcOnset.After(u) }) - 1
	if idx < 0 {
		return vtimezoneTransition{}, fmt.Errorf("%w: %s is before the first transition at %s UTC",
			ErrVTimezoneOutOfCoverage,
			u.Format(icalTimestampFormatUtc),
			transitions[0].utcOnset.Format(icalTimestampFormatUtc))
	}
	return transitions[idx], nil
}
