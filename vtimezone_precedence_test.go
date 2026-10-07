package ics

import (
	"embed"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, icsData string, opts ...any) *Calendar {
	t.Helper()
	cal, err := ParseCalendarWithOptions(strings.NewReader(icsData), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return cal
}

//go:embed testdata/timeparsing.ics
var tpFS embed.FS

func TestEmbeddedCopenhagenTestdata(t *testing.T) {
	f, err := tpFS.Open("testdata/timeparsing.ics")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cal, err := ParseCalendar(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cal.Events() {
		if e.Id() != "269cf715-4e14-4a10-8753-f2feeb9d060e" {
			continue
		}
		info, err := e.GetStartAtInfo()
		if err != nil {
			t.Fatal(err)
		}
		if info.Kind != TimeKindEmbedded {
			t.Fatalf("kind=%s want EMBEDDED", info.Kind)
		}
		if info.Embedded.OffsetSeconds != 3600 || info.Embedded.TZName != "CET" {
			t.Fatalf("off=%d name=%q", info.Embedded.OffsetSeconds, info.Embedded.TZName)
		}
		if want := time.Date(2021, 12, 7, 13, 0, 0, 0, time.UTC); !info.Time.Equal(want) {
			t.Fatalf("got %v want %v", info.Time, want)
		}
		if info.Time.Location() != time.UTC {
			t.Fatalf("expected UTC instant, got %v", info.Time.Location())
		}
	}
}

func TestEmbeddedPrecedenceOverIANA(t *testing.T) {
	// TZID uses an IANA name but the embedded VTIMEZONE deliberately declares
	// a bogus fixed offset; the calendar's own definition must win.
	icsData := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:Europe/Copenhagen\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:+0700\r\nTZOFFSETTO:+0700\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:x\r\nDTSTART;TZID=Europe/Copenhagen:20230601T120000\r\nEND:VEVENT\r\nEND:VCALENDAR"
	cal := mustParse(t, icsData)
	info, err := cal.Events()[0].GetStartAtInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != TimeKindEmbedded || info.Embedded.OffsetSeconds != 7*3600 {
		t.Fatalf("kind=%s off=%d (embedded must win)", info.Kind, info.Embedded.OffsetSeconds)
	}
	if want := time.Date(2023, 6, 1, 5, 0, 0, 0, time.UTC); !info.Time.Equal(want) {
		t.Fatalf("got %v want %v", info.Time, want)
	}
}

func TestExplicitMapperWinsOverEmbedded(t *testing.T) {
	icsData := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:ZZ/Map\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:+0700\r\nTZOFFSETTO:+0700\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:x\r\nDTSTART;TZID=ZZ/Map:20230601T120000\r\nEND:VEVENT\r\nEND:VCALENDAR"
	cal := mustParse(t, icsData, WithTimezoneMapper(func(string) *time.Location {
		return time.UTC
	}))
	info, err := cal.Events()[0].GetStartAtInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != TimeKindLocation || !info.Time.Equal(time.Date(2023, 6, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("kind=%s time=%v (mapper must win)", info.Kind, info.Time)
	}
}
