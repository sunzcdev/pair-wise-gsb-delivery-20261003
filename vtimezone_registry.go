package ics

import (
	"sync"
)

// embeddedTimezoneLookup resolves a TZID against VTIMEZONE components that are
// present in the calendar itself, without consulting the host timezone
// database.
type embeddedTimezoneLookup interface {
	lookupEmbeddedTimezone(tzid string) (*TimezoneDefinition, error)
}

type embeddedTimezoneSetter interface {
	setEmbeddedTimezones(embeddedTimezoneLookup)
}

// embeddedTimezones lazily parses the VTIMEZONE components attached to a
// calendar into evaluable definitions.
type embeddedTimezones struct {
	once     sync.Once
	comps    []*VTimezone
	byID     map[string]*TimezoneDefinition
	errByID  map[string]error
	parseErr error
}

func newEmbeddedTimezones(comps []*VTimezone) *embeddedTimezones {
	return &embeddedTimezones{comps: comps}
}

func (e *embeddedTimezones) init() {
	e.once.Do(func() {
		e.byID = make(map[string]*TimezoneDefinition, len(e.comps))
		e.errByID = make(map[string]error)
		for _, comp := range e.comps {
			def, err := ParseTimezoneDefinition(comp)
			if err != nil {
				tzid := ""
				if prop := comp.GetProperty(ComponentPropertyTzid); prop != nil {
					tzid = prop.Value
				}
				if tzid == "" {
					e.parseErr = err
					continue
				}
				e.errByID[tzid] = err
				continue
			}
			if _, exists := e.byID[def.tzid]; !exists {
				e.byID[def.tzid] = def
			}
		}
	})
}

func (e *embeddedTimezones) lookupEmbeddedTimezone(tzid string) (*TimezoneDefinition, error) {
	if e == nil {
		return nil, nil
	}
	e.init()
	if e.parseErr != nil {
		return nil, e.parseErr
	}
	if err, ok := e.errByID[tzid]; ok {
		return nil, err
	}
	return e.byID[tzid], nil
}

// EmbeddedTimezones returns the parsed VTIMEZONE definitions carried in the
// calendar, keyed by TZID. It never consults operating system timezone data.
func (cal *Calendar) EmbeddedTimezones() (map[string]*TimezoneDefinition, error) {
	cal.embedOnce.Do(func() {
		cal.embedded = newEmbeddedTimezones(cal.Timezones())
	})
	if cal.embedded == nil {
		return map[string]*TimezoneDefinition{}, nil
	}
	cal.embedded.init()
	if cal.embedded.parseErr != nil {
		return nil, cal.embedded.parseErr
	}
	out := make(map[string]*TimezoneDefinition, len(cal.embedded.byID))
	for id, def := range cal.embedded.byID {
		out[id] = def
	}
	return out, nil
}

// attachEmbeddedTimezones shares the calendar's lazy registry with every
// parsed component so TZID references resolve via the embedded rules.
func (cal *Calendar) attachEmbeddedTimezones() {
	cal.embedOnce.Do(func() {
		cal.embedded = newEmbeddedTimezones(cal.Timezones())
	})
	if cal.embedded == nil {
		return
	}
	for _, comp := range cal.Components {
		if setter, ok := comp.(embeddedTimezoneSetter); ok {
			setter.setEmbeddedTimezones(cal.embedded)
		}
	}
}
