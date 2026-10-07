package ics

import (
	"fmt"
	"time"
)

// TimeKind classifies how a DATE-TIME value is anchored.
type TimeKind string

const (
	// TimeKindUTC is a value suffixed with Z (absolute UTC time).
	TimeKindUTC TimeKind = "UTC"
	// TimeKindLocal is a floating date-time with no TZID; it is interpreted
	// in the host local timezone by default.
	TimeKindLocal TimeKind = "LOCAL"
	// TimeKindDate is a DATE-only value.
	TimeKindDate TimeKind = "DATE"
	// TimeKindEmbedded is a TZID value resolved from a VTIMEZONE component
	// carried in the calendar itself.
	TimeKindEmbedded TimeKind = "EMBEDDED"
	// TimeKindLocation is a TZID value resolved via a mapper or the host
	// timezone database.
	TimeKindLocation TimeKind = "LOCATION"
)

// TimePropertyInfo describes the result of parsing one date/date-time value,
// including how a TZID was resolved and, for embedded timezones, whether the
// local time was unambiguous, repeated or skipped.
type TimePropertyInfo struct {
	// Time is the parsed absolute instant (UTC for Z and TZID values).
	Time time.Time
	// Kind identifies the anchoring of the value.
	Kind TimeKind
	// TZID is the TZID parameter, if present.
	TZID string
	// LocalTimeStatus, for embedded resolutions, reports gap/overlap status.
	// It is empty for non-embedded values.
	LocalTimeStatus LocalTimeStatus
	// Embedded, for embedded resolutions, is the full resolution detail.
	Embedded *ResolvedTime
	// Location, for mapper/database resolutions, is the location used.
	Location *time.Location
}

// IsRepeated reports whether the value names a local time that occurs twice.
func (i TimePropertyInfo) IsRepeated() bool {
	return i.LocalTimeStatus == LocalTimeRepeated
}

// IsSkipped reports whether the value names a local time that does not exist.
func (i TimePropertyInfo) IsSkipped() bool {
	return i.LocalTimeStatus == LocalTimeSkipped
}

func (cb *ComponentBase) getTimePropInfo(componentProperty ComponentProperty, expectAllDay bool) (TimePropertyInfo, error) {
	timeProp := cb.GetProperty(componentProperty)
	if timeProp == nil {
		return TimePropertyInfo{}, fmt.Errorf("%w: %s", ErrPropertyNotFound, componentProperty)
	}
	return parseTimeValueInfo(timeProp.BaseProperty.Value, timeProp.ICalParameters, expectAllDay, cb.timeParseOps()...)
}

func (cb *ComponentBase) timeParseOps() []any {
	ops := make([]any, 0, 2)
	if cb.timezoneMapper != nil {
		ops = append(ops, cb.timezoneMapper)
	}
	if cb.embeddedTimezones != nil {
		ops = append(ops, cb.embeddedTimezones)
	}
	return ops
}

// GetStartAtInfo is like GetStartAt but also reports how the value and TZID
// were resolved, including repeated/skipped local time details.
func (cb *ComponentBase) GetStartAtInfo() (TimePropertyInfo, error) {
	return cb.getTimePropInfo(ComponentPropertyDtStart, false)
}

// GetEndAtInfo is the information-rich counterpart of GetEndAt.
func (cb *ComponentBase) GetEndAtInfo() (TimePropertyInfo, error) {
	return cb.getTimePropInfo(ComponentPropertyDtEnd, false)
}

// GetAllDayStartAtInfo is the information-rich counterpart of
// GetAllDayStartAt.
func (cb *ComponentBase) GetAllDayStartAtInfo() (TimePropertyInfo, error) {
	return cb.getTimePropInfo(ComponentPropertyDtStart, true)
}

// GetAllDayEndAtInfo is the information-rich counterpart of GetAllDayEndAt.
func (cb *ComponentBase) GetAllDayEndAtInfo() (TimePropertyInfo, error) {
	return cb.getTimePropInfo(ComponentPropertyDtEnd, true)
}

// GetRecurrenceIDInfo is the information-rich counterpart of GetRecurrenceID.
func (cb *ComponentBase) GetRecurrenceIDInfo() (TimePropertyInfo, error) {
	return cb.getTimePropInfo(ComponentPropertyRecurrenceId, false)
}
