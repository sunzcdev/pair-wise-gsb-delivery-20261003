package ics

import (
	"testing"
	"time"
)

func TestResolveRuleShapes(t *testing.T) {
	// Two observances switching on fixed BYMONTHDAY dates.
	icsData := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:ZZ/MD\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20200115T000000\r\nRRULE:FREQ=YEARLY;BYMONTH=1;BYMONTHDAY=15\r\nTZOFFSETFROM:+0400\r\nTZOFFSETTO:+0500\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20200715T000000\r\nRRULE:FREQ=YEARLY;BYMONTH=7;BYMONTHDAY=15\r\nTZOFFSETFROM:+0500\r\nTZOFFSETTO:+0400\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\nEND:VCALENDAR"
	def := parseOneTimezone(t, icsData)
	r, err := def.Resolve(time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || r.OffsetSeconds != 5*3600 {
		t.Fatalf("apr off=%d err=%v", r.OffsetSeconds, err)
	}
	r, err = def.Resolve(time.Date(2023, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || r.OffsetSeconds != 4*3600 {
		t.Fatalf("oct off=%d err=%v", r.OffsetSeconds, err)
	}
}

func TestResolveLastWeekday(t *testing.T) {
	// -1FR in February: last Friday. 2023 -> Feb 24.
	icsData := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:ZZ/LW\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20200201T000000\r\nRRULE:FREQ=YEARLY;BYMONTH=2;BYDAY=-1FR\r\nTZOFFSETFROM:+0400\r\nTZOFFSETTO:+0500\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20200801T000000\r\nRRULE:FREQ=YEARLY;BYMONTH=8;BYDAY=1SA\r\nTZOFFSETFROM:+0500\r\nTZOFFSETTO:+0400\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\nEND:VCALENDAR"
	def := parseOneTimezone(t, icsData)
	// Feb 23 still +0400, Feb 24 onward (until Aug) +0500.
	r, err := def.Resolve(time.Date(2023, 2, 23, 12, 0, 0, 0, time.UTC))
	if err != nil || r.OffsetSeconds != 4*3600 {
		t.Fatalf("feb23 off=%d err=%v", r.OffsetSeconds, err)
	}
	r, err = def.Resolve(time.Date(2023, 2, 24, 12, 0, 0, 0, time.UTC))
	if err != nil || r.OffsetSeconds != 5*3600 {
		t.Fatalf("feb24 off=%d err=%v", r.OffsetSeconds, err)
	}
}

func TestResolveFixedObservance(t *testing.T) {
	// Single observance, no RRULE: offset applies forever after DTSTART.
	icsData := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:ZZ/Fixed\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20200101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0545\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\nEND:VCALENDAR"
	def := parseOneTimezone(t, icsData)
	r, err := def.Resolve(time.Date(2099, 6, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("fixed tail: %v", err)
	}
	if r.OffsetSeconds != 5*3600+45*60 {
		t.Fatalf("offset %d", r.OffsetSeconds)
	}
}

func TestResolveCount(t *testing.T) {
	// COUNT=2: only 2020 and 2021 spring switches occur, then offset persists.
	icsData := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:ZZ/Count\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20200301T000000\r\nRRULE:FREQ=YEARLY;COUNT=2;BYMONTH=3;BYMONTHDAY=1\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0100\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\nEND:VCALENDAR"
	def := parseOneTimezone(t, icsData)
	if r, err := def.Resolve(time.Date(2019, 12, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatalf("before coverage should fail, got off=%d", r.OffsetSeconds)
	}
	if r, err := def.Resolve(time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)); err != nil || r.OffsetSeconds != 3600 {
		t.Fatalf("2020 off=%d err=%v", r.OffsetSeconds, err)
	}
	if r, err := def.Resolve(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)); err != nil || r.OffsetSeconds != 3600 {
		t.Fatalf("persisted off=%d err=%v", r.OffsetSeconds, err)
	}
}
