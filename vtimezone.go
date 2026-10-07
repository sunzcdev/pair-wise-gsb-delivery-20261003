package ics

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LocalTimeStatus describes how a floating local wall time maps onto the
// VTIMEZONE definition.
type LocalTimeStatus string

const (
	// LocalTimeOK means the local time falls within exactly one observance.
	LocalTimeOK LocalTimeStatus = "OK"
	// LocalTimeRepeated means the local time occurs twice because clocks are
	// set back. Alternatives holds the first (daylight saving) observation
	// and Time is the second (standard) observation.
	LocalTimeRepeated LocalTimeStatus = "REPEATED"
	// LocalTimeSkipped means the local time does not exist because clocks are
	// set forward. Time is the equivalent instant expressed in the offset
	// taking effect at the forward transition.
	LocalTimeSkipped LocalTimeStatus = "SKIPPED"
)

// ResolvedTime is the result of resolving a local wall time against a
// VTIMEZONE definition. All information comes from the calendar component
// itself; the host operating system timezone database is never consulted.
type ResolvedTime struct {
	// Time is the resulting absolute (UTC) instant.
	Time time.Time
	// Local is the wall-clock time in the chosen observance. It equals the
	// requested time for OK/REPEATED results, and the shifted wall time for a
	// SKIPPED result.
	Local time.Time
	// Offset is the observed UTC offset at Time, as a duration.
	Offset time.Duration
	// OffsetSeconds is Offset expressed in seconds east of UTC.
	OffsetSeconds int
	// TZName is the observance TZNAME at Time (may be empty).
	TZName string
	// Status reports whether the requested local time was unambiguous,
	// repeated or skipped.
	Status LocalTimeStatus
	// Alternatives, for a repeated time, holds the earlier observation.
	// It is nil for unambiguous and skipped times.
	Alternatives []ResolvedTime
	// Transition, for repeated or skipped times, describes the boundary that
	// caused the ambiguity. It is nil for unambiguous times.
	Transition *Transition
}

// EarlierObservation returns the first observation of a repeated local time.
// For an unambiguous time it returns r itself.
func (r ResolvedTime) EarlierObservation() ResolvedTime {
	if r.Status == LocalTimeRepeated && len(r.Alternatives) > 0 {
		return r.Alternatives[0]
	}
	return r
}

// Transition describes one onset at which an observance takes effect.
type Transition struct {
	// obsIndex identifies the observance that owns the onset.
	obsIndex int
	// At is the absolute UTC instant at which the transition happens.
	At time.Time
	// AtLocal is the wall-clock instant of the transition, expressed with
	// OffsetFrom (the offset in effect just before the change).
	AtLocal time.Time
	// OffsetFrom is the UTC offset in effect before the transition.
	OffsetFrom time.Duration
	// OffsetTo is the UTC offset in effect after the transition.
	OffsetTo time.Duration
	// TZName is the name of the observance that takes effect.
	TZName string
	// Kind is STANDARD or DAYLIGHT.
	Kind ComponentType
}

// Observance is one STANDARD or DAYLIGHT sub-component of a VTIMEZONE.
type Observance struct {
	// Kind is STANDARD or DAYLIGHT.
	Kind ComponentType
	// DTStart is the first onset, carried in a neutral time.UTC location.
	DTStart time.Time
	// DTStartDate is true when DTSTART was a DATE value (midnight implied).
	DTStartDate bool
	// OffsetFrom is TZOFFSETFROM, the offset in effect just before the onset.
	OffsetFrom time.Duration
	// OffsetTo is TZOFFSETTO, the offset in effect after the onset.
	OffsetTo time.Duration
	// TZName is the TZNAME of the observance.
	TZName string
	// RRule is the yearly recurrence rule, or nil.
	RRule *RecurrenceRule
	// RDates are explicit additional onsets, carried in time.UTC.
	RDates []time.Time
}

// TimezoneDefinition is a parsed VTIMEZONE component evaluated strictly from
// the rules embedded in the calendar. It is never combined with operating
// system timezone data.
type TimezoneDefinition struct {
	tzid        string
	observances []*Observance

	mu     sync.Mutex
	built  int
	states []genState
	table  []Transition
}

// ID returns the TZID of the definition.
func (d *TimezoneDefinition) ID() string {
	return d.tzid
}

// Observances returns the parsed STANDARD/DAYLIGHT rules.
func (d *TimezoneDefinition) Observances() []*Observance {
	out := make([]*Observance, len(d.observances))
	copy(out, d.observances)
	return out
}

// ParseTimezoneDefinition converts a parsed VTimezone component into an
// evaluable definition. Errors wrap the corresponding ErrVTimezone* sentinel
// so callers can distinguish malformed data.
func ParseTimezoneDefinition(tz *VTimezone) (*TimezoneDefinition, error) {
	tzidProp := tz.GetProperty(ComponentPropertyTzid)
	if tzidProp == nil || tzidProp.Value == "" {
		return nil, fmt.Errorf("%w", ErrVTimezoneMissingTZID)
	}
	def := &TimezoneDefinition{tzid: tzidProp.Value}
	for _, sub := range tz.SubComponents() {
		var kind ComponentType
		switch sub.(type) {
		case *Standard:
			kind = ComponentStandard
		case *Daylight:
			kind = ComponentDaylight
		default:
			continue
		}
		obs, err := parseObservance(sub, kind)
		if err != nil {
			return nil, err
		}
		def.observances = append(def.observances, obs)
	}
	if len(def.observances) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrVTimezoneMissingObservance, def.tzid)
	}
	return def, nil
}

func parseObservance(comp Component, kind ComponentType) (*Observance, error) {
	accessor, ok := comp.(interface {
		GetProperty(ComponentProperty) *IANAProperty
		GetProperties(ComponentProperty) []*IANAProperty
	})
	if !ok {
		return nil, fmt.Errorf("%w: cannot read %s observance", ErrVTimezoneMissingDTSTART, kind)
	}
	get := func(p ComponentProperty) string {
		if prop := accessor.GetProperty(p); prop != nil {
			return prop.Value
		}
		return ""
	}

	dtstartVal := get(ComponentPropertyDtStart)
	if dtstartVal == "" {
		return nil, fmt.Errorf("%w: %s missing DTSTART", ErrVTimezoneMissingDTSTART, kind)
	}
	dtstart, dtstartDate, err := parseTimezoneNaive(dtstartVal)
	if err != nil {
		return nil, fmt.Errorf("%w: %s DTSTART %q: %v", ErrVTimezoneMissingDTSTART, kind, dtstartVal, err)
	}

	offFromVal := get(ComponentProperty(PropertyTzoffsetfrom))
	if offFromVal == "" {
		return nil, fmt.Errorf("%w: %s TZOFFSETFROM", ErrVTimezoneMissingOffset, kind)
	}
	offFrom, err := parseUTCOffsetValue(offFromVal)
	if err != nil {
		return nil, fmt.Errorf("%w: %s TZOFFSETFROM %q: %v", ErrVTimezoneInvalidOffset, kind, offFromVal, err)
	}
	offToVal := get(ComponentProperty(PropertyTzoffsetto))
	if offToVal == "" {
		return nil, fmt.Errorf("%w: %s TZOFFSETTO", ErrVTimezoneMissingOffset, kind)
	}
	offTo, err := parseUTCOffsetValue(offToVal)
	if err != nil {
		return nil, fmt.Errorf("%w: %s TZOFFSETTO %q: %v", ErrVTimezoneInvalidOffset, kind, offToVal, err)
	}

	obs := &Observance{
		Kind:        kind,
		DTStart:     forceNaive(dtstart, dtstartDate),
		DTStartDate: dtstartDate,
		OffsetFrom:  offFrom,
		OffsetTo:    offTo,
		TZName:      get(ComponentProperty(PropertyTzname)),
	}

	if rruleVal := get(ComponentPropertyRrule); rruleVal != "" {
		r, err := ParseRecurrenceRule(rruleVal)
		if err != nil {
			return nil, fmt.Errorf("%w: %s RRULE: %v", ErrVTimezoneUnsupportedRRULE, kind, err)
		}
		if err := validateTimezoneRRULE(r); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrVTimezoneUnsupportedRRULE, kind, err)
		}
		obs.RRule = r
	}

	for _, prop := range accessor.GetProperties(ComponentPropertyRdate) {
		valueType := string(ValueDataTypeDateTime)
		if vals, ok := prop.ICalParameters[string(ParameterValue)]; ok && len(vals) > 0 {
			valueType = vals[0]
		}
		for _, raw := range strings.Split(prop.Value, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			switch ValueDataType(valueType) {
			case ValueDataTypeDateTime:
				t, isDate, err := parseTimezoneNaive(raw)
				if err != nil {
					return nil, fmt.Errorf("%w: %s RDATE %q: %v", ErrVTimezoneInvalidRDATE, kind, raw, err)
				}
				obs.RDates = append(obs.RDates, forceNaive(t, isDate))
			case ValueDataTypeDate:
				t, _, err := parseTimezoneNaive(raw)
				if err != nil {
					return nil, fmt.Errorf("%w: %s RDATE %q: %v", ErrVTimezoneInvalidRDATE, kind, raw, err)
				}
				obs.RDates = append(obs.RDates, forceNaive(t, true))
			default:
				return nil, fmt.Errorf("%w: %s RDATE VALUE=%s", ErrVTimezoneUnsupportedRDATE, kind, valueType)
			}
		}
	}

	return obs, nil
}

func forceNaive(t time.Time, isDate bool) time.Time {
	y, m, d := t.Date()
	if isDate {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	hh, mi, ss := t.Clock()
	return time.Date(y, m, d, hh, mi, ss, t.Nanosecond(), time.UTC)
}

func validateTimezoneRRULE(r *RecurrenceRule) error {
	if r.Freq != FrequencyYearly {
		return fmt.Errorf("only FREQ=YEARLY is supported in VTIMEZONE, got %s", r.Freq)
	}
	if len(r.ByYearDay) > 0 {
		return fmt.Errorf("BYYEARDAY is not supported in VTIMEZONE RRULEs")
	}
	if len(r.ByWeekNo) > 0 {
		return fmt.Errorf("BYWEEKNO is not supported in VTIMEZONE RRULEs")
	}
	if len(r.BySetPos) > 0 {
		return fmt.Errorf("BYSETPOS is not supported in VTIMEZONE RRULEs")
	}
	for _, d := range r.ByDay {
		if d.OrdWeek != 0 && (d.OrdWeek < -53 || d.OrdWeek > 53) {
			return fmt.Errorf("BYDAY ordinal %d out of range", d.OrdWeek)
		}
	}
	for _, md := range r.ByMonthDay {
		if md < -31 || md == 0 || md > 31 {
			return fmt.Errorf("BYMONTHDAY %d out of range", md)
		}
	}
	for _, mo := range r.ByMonth {
		if mo < 1 || mo > 12 {
			return fmt.Errorf("BYMONTH %d out of range", mo)
		}
	}
	if r.Interval <= 0 {
		return fmt.Errorf("INTERVAL must be positive")
	}
	return nil
}

// parseTimezoneNaive parses an iCalendar date or date-time into a time.Time
// carried in a neutral time.UTC location, deliberately ignoring the host
// timezone. The boolean reports whether the value was a DATE value.
func parseTimezoneNaive(s string) (time.Time, bool, error) {
	if t, err := time.Parse(icalTimestampFormatLocal, s); err == nil {
		return t, false, nil
	}
	if t, err := time.Parse(icalDateFormatLocal, s); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("unrecognized date/date-time %q", s)
}

// parseUTCOffsetValue parses an RFC 5545 UTC-OFFSET (e.g. "+0545", "-0800",
// "+010030") into a duration east of UTC.
func parseUTCOffsetValue(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty offset")
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	default:
		return 0, fmt.Errorf("missing sign")
	}
	var hh, mm, ss int
	switch len(s) {
	case 4:
		hh = atoiOr(s[0:2], -1)
		mm = atoiOr(s[2:4], -1)
	case 6:
		hh = atoiOr(s[0:2], -1)
		mm = atoiOr(s[2:4], -1)
		ss = atoiOr(s[4:6], -1)
	default:
		return 0, fmt.Errorf("expected HHMM or HHMMSS")
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 || ss < 0 || ss > 59 {
		return 0, fmt.Errorf("offset components out of range")
	}
	total := time.Duration(hh*3600+mm*60+ss) * time.Second
	if neg {
		total = -total
	}
	return total, nil
}

func atoiOr(s string, fallback int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

// weekdayIndex maps an iCalendar weekday token to a 0=Sunday..6=Saturday index.
func weekdayIndex(d Weekday) int {
	switch d {
	case WeekdaySunday:
		return 0
	case WeekdayMonday:
		return 1
	case WeekdayTuesday:
		return 2
	case WeekdayWednesday:
		return 3
	case WeekdayThursday:
		return 4
	case WeekdayFriday:
		return 5
	case WeekdaySaturday:
		return 6
	}
	return -1
}

// nthWeekdayInMonth returns the nth target weekday (n may be negative) in the
// given month. ok is false when no such day exists.
func nthWeekdayInMonth(year int, month time.Month, targetWeekday, n int) (day int, ok bool) {
	daysInMonth := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if n > 0 {
		first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
		offset := (targetWeekday - int(first.Weekday()) + 7) % 7
		day = 1 + offset + (n-1)*7
		if day > daysInMonth {
			return 0, false
		}
		return day, true
	}
	last := time.Date(year, month, daysInMonth, 0, 0, 0, 0, time.UTC)
	offset := (int(last.Weekday()) - targetWeekday + 7) % 7
	day = daysInMonth - offset + (n+1)*7
	if day < 1 {
		return 0, false
	}
	return day, true
}
