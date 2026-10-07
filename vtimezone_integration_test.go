package ics

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// withFakeLocal temporarily swaps time.Local so any accidental reliance on the
// host timezone would surface.
func withFakeLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	orig := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = orig })
}

func TestEmbeddedParseFictional(t *testing.T) {
	withFakeLocal(t, time.FixedZone("Fake/Host", 7*3600+23*60))
	icsData := "BEGIN:VCALENDAR\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:ZZ-Partners/Custom_City_545\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20201101T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nTZNAME:CST545\r\nTZOFFSETFROM:+0430\r\nTZOFFSETTO:+0545\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20200308T023000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nTZNAME:CDT430\r\nTZOFFSETFROM:+0545\r\nTZOFFSETTO:+0430\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:e1\r\nDTSTART;TZID=ZZ-Partners/Custom_City_545:20230115T120000\r\nSUMMARY:x\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:e2\r\nDTSTART;TZID=ZZ-Partners/Custom_City_545:20230701T120000\r\nSUMMARY:x\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR"
	cal, err := ParseCalendar(strings.NewReader(icsData))
	if err != nil {
		t.Fatal(err)
	}
	byUID := map[string]*VEvent{}
	for _, e := range cal.Events() {
		byUID[e.Id()] = e
	}

	info, err := byUID["e1"].GetStartAtInfo()
	if err != nil {
		t.Fatalf("e1: %v", err)
	}
	if info.Kind != TimeKindEmbedded || info.LocalTimeStatus != LocalTimeOK {
		t.Fatalf("e1 kind=%s status=%s", info.Kind, info.LocalTimeStatus)
	}
	if info.Embedded.OffsetSeconds != 5*3600+45*60 || info.Embedded.TZName != "CST545" {
		t.Fatalf("e1 offset=%d name=%q", info.Embedded.OffsetSeconds, info.Embedded.TZName)
	}
	if want := time.Date(2023, 1, 15, 6, 15, 0, 0, time.UTC); !info.Time.Equal(want) {
		t.Fatalf("e1 got %v want %v", info.Time, want)
	}

	info2, err := byUID["e2"].GetStartAtInfo()
	if err != nil {
		t.Fatalf("e2: %v", err)
	}
	if want := time.Date(2023, 7, 1, 7, 30, 0, 0, time.UTC); !info2.Time.Equal(want) {
		t.Fatalf("e2 got %v want %v", info2.Time, want)
	}
	if info2.Embedded.OffsetSeconds != 4*3600+30*60 {
		t.Fatalf("e2 offset=%d", info2.Embedded.OffsetSeconds)
	}
}

func TestEmbeddedHostIndependenceGapOverlap(t *testing.T) {
	withFakeLocal(t, time.UTC)
	// New-York-like but fictional TZID, exercised under time.Local = UTC and
	// (in the second run) a bizarre fixed zone.
	run := func() {
		icsData := "BEGIN:VCALENDAR\r\n" +
			"BEGIN:VTIMEZONE\r\nTZID:ZZ/PartnerNY\r\n" +
			"BEGIN:STANDARD\r\nDTSTART:19701101T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nTZOFFSETFROM:-0400\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\n" +
			"BEGIN:DAYLIGHT\r\nDTSTART:19700308T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0400\r\nEND:DAYLIGHT\r\n" +
			"END:VTIMEZONE\r\n" +
			"BEGIN:VEVENT\r\nUID:fall\r\nDTSTART;TZID=ZZ/PartnerNY:20231105T013000\r\nEND:VEVENT\r\n" +
			"BEGIN:VEVENT\r\nUID:spring\r\nDTSTART;TZID=ZZ/PartnerNY:20230312T023000\r\nEND:VEVENT\r\n" +
			"END:VCALENDAR"
		cal, err := ParseCalendar(strings.NewReader(icsData))
		if err != nil {
			t.Fatal(err)
		}
		byUID := map[string]*VEvent{}
		for _, e := range cal.Events() {
			byUID[e.Id()] = e
		}
		fallInfo, err := byUID["fall"].GetStartAtInfo()
		if err != nil {
			t.Fatal(err)
		}
		if !fallInfo.IsRepeated() {
			t.Fatalf("fall status=%s", fallInfo.LocalTimeStatus)
		}
		if want := time.Date(2023, 11, 5, 6, 30, 0, 0, time.UTC); !fallInfo.Time.Equal(want) {
			t.Fatalf("fall later=%v want %v", fallInfo.Time, want)
		}
		if want := time.Date(2023, 11, 5, 5, 30, 0, 0, time.UTC); !fallInfo.Embedded.EarlierObservation().Time.Equal(want) {
			t.Fatalf("fall earlier=%v want %v", fallInfo.Embedded.EarlierObservation().Time, want)
		}
		springInfo, err := byUID["spring"].GetStartAtInfo()
		if err != nil {
			t.Fatal(err)
		}
		if !springInfo.IsSkipped() {
			t.Fatalf("spring status=%s", springInfo.LocalTimeStatus)
		}
		if want := time.Date(2023, 3, 12, 6, 30, 0, 0, time.UTC); !springInfo.Time.Equal(want) {
			t.Fatalf("spring=%v want %v", springInfo.Time, want)
		}
		if want := time.Date(2023, 3, 12, 3, 30, 0, 0, time.UTC); !springInfo.Embedded.Local.Equal(want) {
			t.Fatalf("spring local=%v want %v", springInfo.Embedded.Local, want)
		}
	}
	run()
	time.Local = time.FixedZone("Fake/Host2", -3*3600+7*60)
	run()
}

func TestEmbeddedCoverageAndUnknown(t *testing.T) {
	icsData := "BEGIN:VCALENDAR\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:ZZ/New\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20200101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:old\r\nDTSTART;TZID=ZZ/New:20190101T000000\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:unknown\r\nDTSTART;TZID=ZZ/Missing:20200101T000000\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR"
	cal, err := ParseCalendar(strings.NewReader(icsData))
	if err != nil {
		t.Fatal(err)
	}
	byUID := map[string]*VEvent{}
	for _, e := range cal.Events() {
		byUID[e.Id()] = e
	}
	if _, err := byUID["old"].GetStartAt(); !errors.Is(err, ErrVTimezoneBeforeCoverage) {
		t.Fatalf("old: %v", err)
	}
	if _, err := byUID["unknown"].GetStartAt(); err == nil {
		t.Fatal("unknown TZID should fail (no IANA mapping for fictional id)")
	}
}
