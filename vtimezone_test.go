package ics

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Fictional partner timezone with non-whole-hour offsets: standard time is
// UTC+05:45, daylight time is UTC+06:45. Clocks spring forward on the first
// Sunday of April at 02:30 and fall back on the last Sunday of October at 02:30.
const neoShibuyaVTimezone = "BEGIN:VTIMEZONE\r\n" +
	"TZID:ACME/Neo-Shibuya\r\n" +
	"BEGIN:DAYLIGHT\r\n" +
	"TZNAME:NDT\r\n" +
	"DTSTART:19700405T023000\r\n" +
	"TZOFFSETFROM:+0545\r\n" +
	"TZOFFSETTO:+0645\r\n" +
	"RRULE:FREQ=YEARLY;BYMONTH=4;BYDAY=1SU\r\n" +
	"END:DAYLIGHT\r\n" +
	"BEGIN:STANDARD\r\n" +
	"TZNAME:NST\r\n" +
	"DTSTART:19701025T023000\r\n" +
	"TZOFFSETFROM:+0645\r\n" +
	"TZOFFSETTO:+0545\r\n" +
	"RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\r\n" +
	"END:STANDARD\r\n" +
	"END:VTIMEZONE\r\n"

const rdateOnlyVTimezone = "BEGIN:VTIMEZONE\r\n" +
	"TZID:ACME/Rdate-Only\r\n" +
	"BEGIN:DAYLIGHT\r\n" +
	"DTSTART:20200405T023000\r\n" +
	"TZOFFSETFROM:+0545\r\n" +
	"TZOFFSETTO:+0645\r\n" +
	"RDATE:20210404T023000\r\n" +
	"END:DAYLIGHT\r\n" +
	"BEGIN:STANDARD\r\n" +
	"DTSTART:20201025T023000\r\n" +
	"TZOFFSETFROM:+0645\r\n" +
	"TZOFFSETTO:+0545\r\n" +
	"RDATE:20211024T023000\r\n" +
	"END:STANDARD\r\n" +
	"END:VTIMEZONE\r\n"

func wrapVCalendar(body string) string {
	return "BEGIN:VCALENDAR\r\n" + body + "END:VCALENDAR\r\n"
}

func mustVTimezoneResolver(t *testing.T, icsData, tzid string) *VTimezoneResolver {
	t.Helper()
	cal, err := ParseCalendar(strings.NewReader(icsData))
	if err != nil {
		t.Fatalf("ParseCalendar error: %v", err)
	}
	resolver, err := cal.VTimezoneResolver(tzid)
	if err != nil {
		t.Fatalf("VTimezoneResolver(%q) error: %v", tzid, err)
	}
	return resolver
}

func utcTime(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

func TestVTimezoneResolver_WinterAndSummer(t *testing.T) {
	resolver := mustVTimezoneResolver(t, wrapVCalendar(neoShibuyaVTimezone), "ACME/Neo-Shibuya")
	cases := []struct {
		name       string
		wall       time.Time
		wantUTC    time.Time
		wantOffset time.Duration
		wantZone   string
	}{
		{"winter", utcTime(2026, 1, 15, 12, 0), utcTime(2026, 1, 15, 6, 15), 5*time.Hour + 45*time.Minute, "NST"},
		{"summer", utcTime(2026, 7, 15, 12, 0), utcTime(2026, 7, 15, 5, 15), 6*time.Hour + 45*time.Minute, "NDT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := resolver.ResolveLocalTime(tc.wall)
			if err != nil {
				t.Fatalf("ResolveLocalTime error: %v", err)
			}
			if !resolved.Time.UTC().Equal(tc.wantUTC) {
				t.Errorf("expected UTC %s, got %s", tc.wantUTC, resolved.Time.UTC())
			}
			if resolved.Offset != tc.wantOffset {
				t.Errorf("expected offset %v, got %v", tc.wantOffset, resolved.Offset)
			}
			if resolved.Ambiguous || resolved.Nonexistent {
				t.Errorf("expected plain resolution, got Ambiguous=%v Nonexistent=%v", resolved.Ambiguous, resolved.Nonexistent)
			}
			if resolved.Time.Location().String() != tc.wantZone {
				t.Errorf("expected zone name %q, got %q", tc.wantZone, resolved.Time.Location().String())
			}
			if resolved.Time.Hour() != 12 {
				t.Errorf("expected wall clock 12:00 preserved, got %s", resolved.Time)
			}
		})
	}
}

func TestVTimezoneResolver_AmbiguousLocalTime(t *testing.T) {
	resolver := mustVTimezoneResolver(t, wrapVCalendar(neoShibuyaVTimezone), "ACME/Neo-Shibuya")
	// 2026-10-25 clocks fall back at 02:30 from +0645 to +0545, so the wall
	// clock interval [01:45, 02:30) happens twice.
	resolved, err := resolver.ResolveLocalTime(utcTime(2026, 10, 25, 2, 0))
	if err != nil {
		t.Fatalf("ResolveLocalTime error: %v", err)
	}
	if !resolved.Ambiguous {
		t.Error("expected Ambiguous to be reported")
	}
	if resolved.Nonexistent {
		t.Error("did not expect Nonexistent")
	}
	// RFC 5545 3.3.5: the first occurrence (daylight offset +0645) is used.
	if want := utcTime(2026, 10, 24, 19, 15); !resolved.Time.UTC().Equal(want) {
		t.Errorf("expected first occurrence %s, got %s", want, resolved.Time.UTC())
	}
	if resolved.Offset != 6*time.Hour+45*time.Minute {
		t.Errorf("expected daylight offset for first occurrence, got %v", resolved.Offset)
	}
}

func TestVTimezoneResolver_NonexistentLocalTime(t *testing.T) {
	resolver := mustVTimezoneResolver(t, wrapVCalendar(neoShibuyaVTimezone), "ACME/Neo-Shibuya")
	// 2026-04-05 clocks spring forward at 02:30 from +0545 to +0645, so the
	// wall clock interval [02:30, 03:30) never happens.
	resolved, err := resolver.ResolveLocalTime(utcTime(2026, 4, 5, 3, 0))
	if err != nil {
		t.Fatalf("ResolveLocalTime error: %v", err)
	}
	if !resolved.Nonexistent {
		t.Error("expected Nonexistent to be reported")
	}
	if resolved.Ambiguous {
		t.Error("did not expect Ambiguous")
	}
	// RFC 5545 3.3.5: interpreted with the offset before the gap (+0545).
	if want := utcTime(2026, 4, 4, 21, 15); !resolved.Time.UTC().Equal(want) {
		t.Errorf("expected instant %s, got %s", want, resolved.Time.UTC())
	}
	// The instant falls after the gap, so it displays as 04:00 in +0645.
	if resolved.Time.Hour() != 4 {
		t.Errorf("expected shifted wall clock 04:00, got %s", resolved.Time)
	}
}

func TestVTimezoneResolver_OutOfCoverage(t *testing.T) {
	resolver := mustVTimezoneResolver(t, wrapVCalendar(neoShibuyaVTimezone), "ACME/Neo-Shibuya")
	_, err := resolver.ResolveLocalTime(utcTime(1950, 6, 1, 12, 0))
	if !errors.Is(err, ErrVTimezoneOutOfCoverage) {
		t.Fatalf("expected ErrVTimezoneOutOfCoverage, got %v", err)
	}
}

func TestVTimezoneResolver_RDateOnly(t *testing.T) {
	resolver := mustVTimezoneResolver(t, wrapVCalendar(rdateOnlyVTimezone), "ACME/Rdate-Only")

	resolved, err := resolver.ResolveLocalTime(utcTime(2021, 6, 1, 12, 0))
	if err != nil {
		t.Fatalf("ResolveLocalTime error: %v", err)
	}
	if want := utcTime(2021, 6, 1, 5, 15); !resolved.Time.UTC().Equal(want) {
		t.Errorf("expected daylight instant %s, got %s", want, resolved.Time.UTC())
	}

	// After the final RDATE the last observance stays in effect.
	resolved, err = resolver.ResolveLocalTime(utcTime(2022, 6, 1, 12, 0))
	if err != nil {
		t.Fatalf("ResolveLocalTime error: %v", err)
	}
	if want := utcTime(2022, 6, 1, 6, 15); !resolved.Time.UTC().Equal(want) {
		t.Errorf("expected standard instant %s, got %s", want, resolved.Time.UTC())
	}

	// Before the first declared onset there is nothing to resolve against.
	if _, err = resolver.ResolveLocalTime(utcTime(2019, 6, 1, 12, 0)); !errors.Is(err, ErrVTimezoneOutOfCoverage) {
		t.Fatalf("expected ErrVTimezoneOutOfCoverage, got %v", err)
	}
}

func TestNewVTimezoneResolver_Contradictory(t *testing.T) {
	// STANDARD claims TZOFFSETFROM +0500 while DAYLIGHT ended at +0645.
	contradictory := "BEGIN:VTIMEZONE\r\n" +
		"TZID:ACME/Broken\r\n" +
		"BEGIN:DAYLIGHT\r\n" +
		"DTSTART:19700405T023000\r\n" +
		"TZOFFSETFROM:+0545\r\n" +
		"TZOFFSETTO:+0645\r\n" +
		"RRULE:FREQ=YEARLY;BYMONTH=4;BYDAY=1SU\r\n" +
		"END:DAYLIGHT\r\n" +
		"BEGIN:STANDARD\r\n" +
		"DTSTART:19701025T023000\r\n" +
		"TZOFFSETFROM:+0500\r\n" +
		"TZOFFSETTO:+0545\r\n" +
		"RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\r\n" +
		"END:STANDARD\r\n" +
		"END:VTIMEZONE\r\n"
	cal, err := ParseCalendar(strings.NewReader(wrapVCalendar(contradictory)))
	if err != nil {
		t.Fatalf("ParseCalendar error: %v", err)
	}
	_, err = NewVTimezoneResolver(cal.Timezones()[0])
	if !errors.Is(err, ErrVTimezoneContradictory) {
		t.Fatalf("expected ErrVTimezoneContradictory, got %v", err)
	}
}

func TestNewVTimezoneResolver_UnsupportedRule(t *testing.T) {
	cases := map[string]string{
		"non-yearly frequency": "RRULE:FREQ=MONTHLY;BYDAY=1SU",
		"unsupported BYxxx":    "RRULE:FREQ=YEARLY;BYMONTH=4;BYDAY=1SU;BYHOUR=2",
	}
	for name, rrule := range cases {
		t.Run(name, func(t *testing.T) {
			icsData := "BEGIN:VTIMEZONE\r\n" +
				"TZID:ACME/Unsupported\r\n" +
				"BEGIN:STANDARD\r\n" +
				"DTSTART:19701025T023000\r\n" +
				"TZOFFSETFROM:+0645\r\n" +
				"TZOFFSETTO:+0545\r\n" +
				rrule + "\r\n" +
				"END:STANDARD\r\n" +
				"END:VTIMEZONE\r\n"
			cal, err := ParseCalendar(strings.NewReader(wrapVCalendar(icsData)))
			if err != nil {
				t.Fatalf("ParseCalendar error: %v", err)
			}
			if _, err = NewVTimezoneResolver(cal.Timezones()[0]); !errors.Is(err, ErrVTimezoneUnsupportedRule) {
				t.Fatalf("expected ErrVTimezoneUnsupportedRule, got %v", err)
			}
		})
	}
}

func TestNewVTimezoneResolver_StructuralErrors(t *testing.T) {
	t.Run("no observances", func(t *testing.T) {
		cal, err := ParseCalendar(strings.NewReader(wrapVCalendar(
			"BEGIN:VTIMEZONE\r\nTZID:ACME/Empty\r\nEND:VTIMEZONE\r\n")))
		if err != nil {
			t.Fatalf("ParseCalendar error: %v", err)
		}
		if _, err = NewVTimezoneResolver(cal.Timezones()[0]); !errors.Is(err, ErrVTimezoneNoObservances) {
			t.Fatalf("expected ErrVTimezoneNoObservances, got %v", err)
		}
	})
	t.Run("missing TZID", func(t *testing.T) {
		cal, err := ParseCalendar(strings.NewReader(wrapVCalendar(
			"BEGIN:VTIMEZONE\r\nBEGIN:STANDARD\r\nDTSTART:19701025T023000\r\nTZOFFSETFROM:+0645\r\nTZOFFSETTO:+0545\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n")))
		if err != nil {
			t.Fatalf("ParseCalendar error: %v", err)
		}
		if _, err = NewVTimezoneResolver(cal.Timezones()[0]); !errors.Is(err, ErrVTimezoneMissingTZID) {
			t.Fatalf("expected ErrVTimezoneMissingTZID, got %v", err)
		}
	})
	t.Run("missing offset", func(t *testing.T) {
		cal, err := ParseCalendar(strings.NewReader(wrapVCalendar(
			"BEGIN:VTIMEZONE\r\nTZID:ACME/NoOffset\r\nBEGIN:STANDARD\r\nDTSTART:19701025T023000\r\nTZOFFSETTO:+0545\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n")))
		if err != nil {
			t.Fatalf("ParseCalendar error: %v", err)
		}
		if _, err = NewVTimezoneResolver(cal.Timezones()[0]); !errors.Is(err, ErrVTimezoneObservanceProperty) {
			t.Fatalf("expected ErrVTimezoneObservanceProperty, got %v", err)
		}
	})
}

func TestCalendar_VTimezoneResolver_EndToEnd(t *testing.T) {
	icsData := wrapVCalendar(neoShibuyaVTimezone +
		"BEGIN:VEVENT\r\n" +
		"UID:partner-sync-1\r\n" +
		"DTSTART;TZID=ACME/Neo-Shibuya:20260715T120000\r\n" +
		"DTEND;TZID=ACME/Neo-Shibuya:20260715T130000\r\n" +
		"SUMMARY:Partner sync\r\n" +
		"END:VEVENT\r\n")
	cal, err := ParseCalendar(strings.NewReader(icsData))
	if err != nil {
		t.Fatalf("ParseCalendar error: %v", err)
	}

	if _, err := cal.VTimezoneResolver("ACME/Does-Not-Exist"); !errors.Is(err, ErrVTimezoneNotFound) {
		t.Fatalf("expected ErrVTimezoneNotFound, got %v", err)
	}

	resolver, err := cal.VTimezoneResolver("ACME/Neo-Shibuya")
	if err != nil {
		t.Fatalf("VTimezoneResolver error: %v", err)
	}
	if resolver.TZID() != "ACME/Neo-Shibuya" {
		t.Errorf("expected TZID ACME/Neo-Shibuya, got %q", resolver.TZID())
	}

	events := cal.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	// The plain time.Location based lookup cannot explain the internal TZID.
	if _, err := events[0].GetStartAt(); err == nil {
		t.Fatal("expected GetStartAt to fail for the fictional TZID")
	}

	start, err := events[0].GetStartAtResolved(resolver)
	if err != nil {
		t.Fatalf("GetStartAtResolved error: %v", err)
	}
	if want := utcTime(2026, 7, 15, 5, 15); !start.Time.UTC().Equal(want) {
		t.Errorf("expected start %s, got %s", want, start.Time.UTC())
	}
	end, err := events[0].GetEndAtResolved(resolver)
	if err != nil {
		t.Fatalf("GetEndAtResolved error: %v", err)
	}
	if want := utcTime(2026, 7, 15, 6, 15); !end.Time.UTC().Equal(want) {
		t.Errorf("expected end %s, got %s", want, end.Time.UTC())
	}

	// A resolver for a different TZID must not be applied to this event.
	other := mustVTimezoneResolver(t, wrapVCalendar(rdateOnlyVTimezone), "ACME/Rdate-Only")
	if _, err := events[0].GetStartAtResolved(other); !errors.Is(err, ErrVTimezoneTZIDMismatch) {
		t.Fatalf("expected ErrVTimezoneTZIDMismatch, got %v", err)
	}
}

func TestVTimezoneResolver_ResolveDateTime(t *testing.T) {
	resolver := mustVTimezoneResolver(t, wrapVCalendar(neoShibuyaVTimezone), "ACME/Neo-Shibuya")

	resolved, err := resolver.ResolveDateTime("20260115")
	if err != nil {
		t.Fatalf("ResolveDateTime date-only error: %v", err)
	}
	if want := utcTime(2026, 1, 14, 18, 15); !resolved.Time.UTC().Equal(want) {
		t.Errorf("expected midnight in +0545 = %s, got %s", want, resolved.Time.UTC())
	}

	if _, err := resolver.ResolveDateTime("20260115T120000Z"); !errors.Is(err, ErrTimeValueNotMatched) {
		t.Errorf("expected UTC value to be rejected, got %v", err)
	}
	if _, err := resolver.ResolveDateTime("not-a-time"); !errors.Is(err, ErrTimeValueNotMatched) {
		t.Errorf("expected garbage value to be rejected, got %v", err)
	}
}

func TestVTimezoneResolver_IndependentOfLocalTimezone(t *testing.T) {
	original := time.Local
	time.Local = time.FixedZone("FAKE-TEST-ZONE", -8*3600)
	defer func() { time.Local = original }()

	resolver := mustVTimezoneResolver(t, wrapVCalendar(neoShibuyaVTimezone), "ACME/Neo-Shibuya")
	cases := []struct {
		wall    time.Time
		wantUTC time.Time
	}{
		{utcTime(2026, 1, 15, 12, 0), utcTime(2026, 1, 15, 6, 15)},
		{utcTime(2026, 7, 15, 12, 0), utcTime(2026, 7, 15, 5, 15)},
		{utcTime(2026, 10, 25, 2, 0), utcTime(2026, 10, 24, 19, 15)},
		{utcTime(2026, 4, 5, 3, 0), utcTime(2026, 4, 4, 21, 15)},
	}
	for _, tc := range cases {
		resolved, err := resolver.ResolveLocalTime(tc.wall)
		if err != nil {
			t.Fatalf("ResolveLocalTime(%s) error: %v", tc.wall, err)
		}
		if !resolved.Time.UTC().Equal(tc.wantUTC) {
			t.Errorf("wall %s: expected %s, got %s", tc.wall, tc.wantUTC, resolved.Time.UTC())
		}
	}
}

func TestParseUTCOffsetValue(t *testing.T) {
	cases := []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"+0545", 5*time.Hour + 45*time.Minute, false},
		{"-0330", -(3*time.Hour + 30*time.Minute), false},
		{"+054530", 5*time.Hour + 45*time.Minute + 30*time.Second, false},
		{"-0000", 0, false},
		{"+0000", 0, false},
		{"0545", 0, true},
		{"+05", 0, true},
		{"+0560", 0, true},
		{"+056099", 0, true},
		{"Z", 0, true},
	}
	for _, tc := range cases {
		got, err := parseUTCOffsetValue(tc.value)
		if tc.wantErr {
			if !errors.Is(err, ErrVTimezoneObservanceProperty) {
				t.Errorf("%q: expected ErrVTimezoneObservanceProperty, got %v", tc.value, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.value, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: expected %v, got %v", tc.value, tc.want, got)
		}
	}
}
