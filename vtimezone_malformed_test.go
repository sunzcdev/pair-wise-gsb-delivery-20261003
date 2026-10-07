package ics

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedMalformedIsolation(t *testing.T) {
	icsData := "BEGIN:VCALENDAR\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:ZZ/Good\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0530\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:ZZ/Bad\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:garbage\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:g\r\nDTSTART;TZID=ZZ/Good:20230101T120000\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:b\r\nDTSTART;TZID=ZZ/Bad:20230101T120000\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR"
	cal, err := ParseCalendar(strings.NewReader(icsData))
	if err != nil {
		t.Fatal(err)
	}
	defs, err := cal.EmbeddedTimezones()
	if err != nil {
		t.Fatalf("valid sibling definitions should still parse: %v", err)
	}
	if _, ok := defs["ZZ/Good"]; !ok {
		t.Fatalf("good definition missing: %v", defs)
	}
	for _, e := range cal.Events() {
		switch e.Id() {
		case "g":
			info, gerr := e.GetStartAtInfo()
			if gerr != nil {
				t.Fatalf("good: %v", gerr)
			}
			if info.Embedded.OffsetSeconds != 5*3600+30*60 {
				t.Fatalf("good off=%d", info.Embedded.OffsetSeconds)
			}
		case "b":
			_, gerr := e.GetStartAtInfo()
			if !errors.Is(gerr, ErrVTimezoneInvalidOffset) {
				t.Fatalf("bad: want invalid offset, got %v", gerr)
			}
		}
	}
}

func TestEmbeddedExdatesUseDefinition(t *testing.T) {
	icsData := "BEGIN:VCALENDAR\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:ZZ/Ex\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0545\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:e\r\nDTSTART;TZID=ZZ/Ex:20230101T120000\r\nEXDATE;TZID=ZZ/Ex:20230108T120000\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR"
	cal, err := ParseCalendar(strings.NewReader(icsData))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := cal.Events()[0].GetExDates()
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 {
		t.Fatalf("exdates=%d", len(ex))
	}
	if want := time.Date(2023, 1, 8, 6, 15, 0, 0, time.UTC); !ex[0].Equal(want) {
		t.Fatalf("exdate %v want %v", ex[0], want)
	}
}
