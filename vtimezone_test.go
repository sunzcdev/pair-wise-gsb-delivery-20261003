package ics

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func parseOneTimezone(t *testing.T, icsData string) *TimezoneDefinition {
	t.Helper()
	cal, err := ParseCalendar(strings.NewReader(icsData))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tzs := cal.Timezones()
	if len(tzs) != 1 {
		t.Fatalf("want 1 tz, got %d", len(tzs))
	}
	def, err := ParseTimezoneDefinition(tzs[0])
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	return def
}

const nyLike = "BEGIN:VCALENDAR\r\n" +
	"BEGIN:VTIMEZONE\r\nTZID:ZZ/ScratchNY\r\n" +
	"BEGIN:STANDARD\r\nDTSTART:19701101T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nTZOFFSETFROM:-0400\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\n" +
	"BEGIN:DAYLIGHT\r\nDTSTART:19700308T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0400\r\nEND:DAYLIGHT\r\n" +
	"END:VTIMEZONE\r\nEND:VCALENDAR"

func TestResolveYearlyRRULE(t *testing.T) {
	def := parseOneTimezone(t, nyLike)

	cases := []struct {
		name string
		wall time.Time
		want time.Time
		off  int
	}{
		{"summer", time.Date(2023, 7, 1, 12, 0, 0, 0, time.UTC), time.Date(2023, 7, 1, 16, 0, 0, 0, time.UTC), -4 * 3600},
		{"winter", time.Date(2023, 1, 15, 12, 0, 0, 0, time.UTC), time.Date(2023, 1, 15, 17, 0, 0, 0, time.UTC), -5 * 3600},
		{"winter just before overlap", time.Date(2023, 11, 5, 0, 59, 0, 0, time.UTC), time.Date(2023, 11, 5, 4, 59, 0, 0, time.UTC), -4 * 3600},
		{"winter after overlap", time.Date(2023, 11, 5, 2, 0, 0, 0, time.UTC), time.Date(2023, 11, 5, 7, 0, 0, 0, time.UTC), -5 * 3600},
		{"spring exact onset new offset", time.Date(2023, 3, 12, 3, 0, 0, 0, time.UTC), time.Date(2023, 3, 12, 7, 0, 0, 0, time.UTC), -4 * 3600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := def.Resolve(tc.wall)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != LocalTimeOK || r.OffsetSeconds != tc.off || !r.Time.Equal(tc.want) {
				t.Fatalf("got %v status=%s off=%d, want %v off=%d", r.Time, r.Status, r.OffsetSeconds, tc.want, tc.off)
			}
		})
	}
}

func TestResolveFallBackRepeated(t *testing.T) {
	def := parseOneTimezone(t, nyLike)
	r, err := def.Resolve(time.Date(2023, 11, 5, 1, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != LocalTimeRepeated {
		t.Fatalf("status=%s", r.Status)
	}
	if r.OffsetSeconds != -5*3600 {
		t.Fatalf("later offset %d", r.OffsetSeconds)
	}
	earlier := r.EarlierObservation()
	if earlier.OffsetSeconds != -4*3600 {
		t.Fatalf("earlier offset %d", earlier.OffsetSeconds)
	}
	if want := time.Date(2023, 11, 5, 5, 30, 0, 0, time.UTC); !earlier.Time.Equal(want) {
		t.Fatalf("earlier %v want %v", earlier.Time, want)
	}
	if want := time.Date(2023, 11, 5, 6, 30, 0, 0, time.UTC); !r.Time.Equal(want) {
		t.Fatalf("later %v want %v", r.Time, want)
	}
	if r.Transition == nil || r.Transition.Kind != ComponentStandard {
		t.Fatalf("transition %+v", r.Transition)
	}
}

func TestResolveSpringForwardSkipped(t *testing.T) {
	def := parseOneTimezone(t, nyLike)
	r, err := def.Resolve(time.Date(2023, 3, 12, 2, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != LocalTimeSkipped {
		t.Fatalf("status=%s", r.Status)
	}
	if want := time.Date(2023, 3, 12, 6, 30, 0, 0, time.UTC); !r.Time.Equal(want) {
		t.Fatalf("instant %v want %v", r.Time, want)
	}
	if want := time.Date(2023, 3, 12, 3, 30, 0, 0, time.UTC); !r.Local.Equal(want) {
		t.Fatalf("shifted local %v want %v", r.Local, want)
	}
	if r.Transition == nil || r.Transition.Kind != ComponentDaylight {
		t.Fatalf("transition %+v", r.Transition)
	}
}

func TestResolveBeforeCoverage(t *testing.T) {
	def := parseOneTimezone(t, nyLike)
	_, err := def.Resolve(time.Date(1969, 6, 1, 12, 0, 0, 0, time.UTC))
	if !errors.Is(err, ErrVTimezoneBeforeCoverage) {
		t.Fatalf("got %v", err)
	}
}

func TestResolveNonIntegerOffsets(t *testing.T) {
	icsData := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:ZZ-Partners/Custom_City_545\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20201101T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nTZNAME:CST545\r\nTZOFFSETFROM:+0430\r\nTZOFFSETTO:+0545\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20200308T023000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nTZNAME:CDT430\r\nTZOFFSETFROM:+0545\r\nTZOFFSETTO:+0430\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\nEND:VCALENDAR"
	def := parseOneTimezone(t, icsData)
	r, err := def.Resolve(time.Date(2023, 1, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if r.OffsetSeconds != 5*3600+45*60 {
		t.Fatalf("offset %d", r.OffsetSeconds)
	}
	if want := time.Date(2023, 1, 15, 6, 15, 0, 0, time.UTC); !r.Time.Equal(want) {
		t.Fatalf("got %v want %v", r.Time, want)
	}
}

func TestParseUTCOffsetValue(t *testing.T) {
	cases := map[string]time.Duration{
		"+0545":   (5*3600 + 45*60) * time.Second,
		"+054430": (5*3600 + 44*60 + 30) * time.Second,
		"-0800":   -8 * time.Hour,
		"-0030":   -30 * time.Minute,
	}
	for in, want := range cases {
		got, err := parseUTCOffsetValue(in)
		if err != nil || got != want {
			t.Fatalf("%s got %v err=%v want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0500", "+500", "+2500", "+0599", "+2400"} {
		if _, err := parseUTCOffsetValue(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestResolveRDATE(t *testing.T) {
	icsData := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:ZZ/Rdate\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20200101T000000\r\nRDATE:20210101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20210601T000000\r\nTZOFFSETFROM:+0100\r\nTZOFFSETTO:+0200\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\nEND:VCALENDAR"
	def := parseOneTimezone(t, icsData)
	if _, err := def.Resolve(time.Date(2019, 6, 1, 12, 0, 0, 0, time.UTC)); !errors.Is(err, ErrVTimezoneBeforeCoverage) {
		t.Fatalf("before: %v", err)
	}
	r, err := def.Resolve(time.Date(2021, 3, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("rdate window: %v", err)
	}
	if r.OffsetSeconds != 3600 {
		t.Fatalf("rdate window offset %d", r.OffsetSeconds)
	}
	r, err = def.Resolve(time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if r.OffsetSeconds != 2*3600 {
		t.Fatalf("tail offset %d", r.OffsetSeconds)
	}
}

func TestResolveContradiction(t *testing.T) {
	icsData := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:ZZ/Bad\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20201101T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20200308T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0200\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\nEND:VCALENDAR"
	def := parseOneTimezone(t, icsData)
	_, err := def.Resolve(time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC))
	if !errors.Is(err, ErrVTimezoneContradiction) {
		t.Fatalf("got %v", err)
	}
}

func TestParseDefinitionErrors(t *testing.T) {
	wrap := func(body string) string {
		return "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\n" + body + "\r\nEND:VTIMEZONE\r\nEND:VCALENDAR"
	}
	check := func(t *testing.T, data string, target error) {
		t.Helper()
		cal, err := ParseCalendar(strings.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseTimezoneDefinition(cal.Timezones()[0]); !errors.Is(err, target) {
			t.Fatalf("want %v got %v", target, err)
		}
	}
	t.Run("missing tzid", func(t *testing.T) {
		check(t, wrap("BEGIN:STANDARD\r\nDTSTART:20200101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0100\r\nEND:STANDARD"), ErrVTimezoneMissingTZID)
	})
	t.Run("no observance", func(t *testing.T) {
		check(t, wrap("TZID:ZZ/N"), ErrVTimezoneMissingObservance)
	})
	t.Run("monthly rrule", func(t *testing.T) {
		check(t, wrap("TZID:ZZ/X\r\nBEGIN:STANDARD\r\nDTSTART:20200101T000000\r\nRRULE:FREQ=MONTHLY\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0100\r\nEND:STANDARD"), ErrVTimezoneUnsupportedRRULE)
	})
	t.Run("bysetpos", func(t *testing.T) {
		check(t, wrap("TZID:ZZ/X2\r\nBEGIN:STANDARD\r\nDTSTART:20200101T000000\r\nRRULE:FREQ=YEARLY;BYMONTH=1;BYDAY=MO;BYSETPOS=1\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0100\r\nEND:STANDARD"), ErrVTimezoneUnsupportedRRULE)
	})
	t.Run("missing offset", func(t *testing.T) {
		check(t, wrap("TZID:ZZ/Y\r\nBEGIN:STANDARD\r\nDTSTART:20200101T000000\r\nTZOFFSETFROM:+0000\r\nEND:STANDARD"), ErrVTimezoneMissingOffset)
	})
	t.Run("bad offset", func(t *testing.T) {
		check(t, wrap("TZID:ZZ/Z\r\nBEGIN:STANDARD\r\nDTSTART:20200101T000000\r\nTZOFFSETFROM:garbage\r\nTZOFFSETTO:+0100\r\nEND:STANDARD"), ErrVTimezoneInvalidOffset)
	})
}
