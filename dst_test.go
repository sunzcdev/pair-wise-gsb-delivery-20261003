package cron

import (
	"testing"
	"time"
)

func mustParseSpec(t *testing.T, spec string) *SpecSchedule {
	t.Helper()
	sched, err := ParseStandard(spec)
	if err != nil {
		t.Fatalf("failed to parse %q: %v", spec, err)
	}
	specSched, ok := sched.(*SpecSchedule)
	if !ok {
		t.Fatalf("expected *SpecSchedule for %q, got %T", spec, sched)
	}
	return specSched
}

func utcTime(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t
}

func assertNext(t *testing.T, sched *SpecSchedule, from, want time.Time) {
	t.Helper()
	got := sched.Next(from)
	if !got.Equal(want) {
		t.Errorf("Next(%v) = %v (%v), want %v (UTC)", from, got, got.UTC(), want)
	}
	if !got.After(from) {
		t.Errorf("Next(%v) = %v, not strictly after input", from, got)
	}
}

// Fall-back: on 2021-11-07 in America/New_York, 01:30 occurs twice:
// 05:30 UTC (EDT, UTC-4) and 06:30 UTC (EST, UTC-5).
func TestDSTRepeatedPolicies(t *testing.T) {
	const spec = "CRON_TZ=America/New_York 30 1 * * *"
	from := utcTime("2021-11-07T00:00:00")
	first := utcTime("2021-11-07T05:30:00")
	second := utcTime("2021-11-07T06:30:00")
	nextDay := utcTime("2021-11-08T06:30:00")

	sched := mustParseSpec(t, spec)
	sched.RepeatedPolicy = RepeatedFirst
	assertNext(t, sched, from, first)
	assertNext(t, sched, first, nextDay)

	sched = mustParseSpec(t, spec)
	sched.RepeatedPolicy = RepeatedSecond
	assertNext(t, sched, from, second)
	assertNext(t, sched, second, nextDay)

	sched = mustParseSpec(t, spec)
	sched.RepeatedPolicy = RepeatedBoth
	assertNext(t, sched, from, first)
	assertNext(t, sched, first, second)
	assertNext(t, sched, second, nextDay)

	// An input inside the fold (01:45 EDT) must still find the second
	// occurrence of 01:30 instead of spinning or skipping a day.
	assertNext(t, sched, utcTime("2021-11-07T05:45:00"), second)
	// An input at the second occurrence itself must move on.
	assertNext(t, sched, utcTime("2021-11-07T06:30:00"), nextDay)
}

// Repeated schedules must advance monotonically and never return the same
// instant twice, even while iterating through a fall-back fold.
func TestDSTRepeatedStrictlyIncreasing(t *testing.T) {
	sched := mustParseSpec(t, "CRON_TZ=America/New_York 30 1 * * *")
	sched.RepeatedPolicy = RepeatedBoth

	want := []time.Time{
		utcTime("2021-11-06T05:30:00"), // Nov 6, 01:30 EDT
		utcTime("2021-11-07T05:30:00"), // Nov 7, 01:30 EDT (first)
		utcTime("2021-11-07T06:30:00"), // Nov 7, 01:30 EST (second)
		utcTime("2021-11-08T06:30:00"), // Nov 8, 01:30 EST
		utcTime("2021-11-09T06:30:00"),
	}
	now := utcTime("2021-11-06T00:00:00")
	for i, w := range want {
		next := sched.Next(now)
		if !next.After(now) {
			t.Fatalf("iteration %d: Next(%v) = %v, not strictly increasing", i, now, next)
		}
		if !next.Equal(w) {
			t.Fatalf("iteration %d: Next(%v) = %v, want %v", i, now, next, w)
		}
		now = next
	}
}

// Spring-forward: on 2021-03-14 in America/New_York, 02:30 does not exist
// (02:00 EST jumps to 03:00 EDT at 07:00 UTC).
func TestDSTSkippedPolicies(t *testing.T) {
	const spec = "CRON_TZ=America/New_York 30 2 * * *"
	from := utcTime("2021-03-14T00:00:00")
	gapEnd := utcTime("2021-03-14T07:00:00") // 03:00 EDT, first valid moment
	nextDay := utcTime("2021-03-15T06:30:00")

	sched := mustParseSpec(t, spec)
	sched.SkippedPolicy = SkippedSkip
	assertNext(t, sched, from, nextDay)

	sched = mustParseSpec(t, spec)
	sched.SkippedPolicy = SkippedShift
	assertNext(t, sched, from, gapEnd)
	assertNext(t, sched, gapEnd, nextDay)
}

// Cross-midnight fall-back: on 2021-04-04 00:00 America/Santiago falls back
// to 2021-04-03 23:00, so 23:30 on Apr 3 occurs twice: 02:30 UTC (CLST,
// UTC-3) and 03:30 UTC (CLT, UTC-4).
func TestDSTRepeatedAcrossMidnight(t *testing.T) {
	sched := mustParseSpec(t, "CRON_TZ=America/Santiago 30 23 * * *")
	sched.RepeatedPolicy = RepeatedBoth

	from := utcTime("2021-04-03T03:00:00") // Apr 3 00:00 CLT
	first := utcTime("2021-04-04T02:30:00")
	second := utcTime("2021-04-04T03:30:00")
	nextDay := utcTime("2021-04-05T03:30:00")

	assertNext(t, sched, from, first)
	assertNext(t, sched, first, second)
	assertNext(t, sched, second, nextDay)

	sched = mustParseSpec(t, "CRON_TZ=America/Santiago 30 23 * * *")
	sched.RepeatedPolicy = RepeatedFirst
	assertNext(t, sched, from, first)
	assertNext(t, sched, first, nextDay)
}

// Cross-midnight spring-forward: on 2021-09-05 00:00 America/Santiago jumps
// to 01:00, so midnight does not exist on that date.
func TestDSTSkippedAcrossMidnight(t *testing.T) {
	from := utcTime("2021-09-04T04:00:00")   // Sep 4 00:00 CLT
	gapEnd := utcTime("2021-09-05T04:00:00") // Sep 5 01:00 CLST, first valid moment
	nextMidnight := utcTime("2021-09-06T03:00:00")

	sched := mustParseSpec(t, "CRON_TZ=America/Santiago 0 0 * * *")
	sched.SkippedPolicy = SkippedSkip
	assertNext(t, sched, from, nextMidnight)

	sched = mustParseSpec(t, "CRON_TZ=America/Santiago 0 0 * * *")
	sched.SkippedPolicy = SkippedShift
	assertNext(t, sched, from, gapEnd)
	assertNext(t, sched, gapEnd, nextMidnight)
}

// Non-integral-hour transitions: Australia/Lord_Howe shifts clocks by 30
// minutes. On 2021-04-04, 02:00 AEDT (UTC+11) falls back to 01:30 AEST
// (UTC+10:30), so 01:45 occurs at 14:45 UTC and 15:15 UTC.
func TestDSTRepeatedNonIntegralHour(t *testing.T) {
	sched := mustParseSpec(t, "CRON_TZ=Australia/Lord_Howe 45 1 * * *")
	sched.RepeatedPolicy = RepeatedBoth

	from := utcTime("2021-04-03T00:00:00")
	first := utcTime("2021-04-03T14:45:00")
	second := utcTime("2021-04-03T15:15:00")
	nextDay := utcTime("2021-04-04T15:15:00")

	assertNext(t, sched, from, first)
	assertNext(t, sched, first, second)
	assertNext(t, sched, second, nextDay)

	sched = mustParseSpec(t, "CRON_TZ=Australia/Lord_Howe 45 1 * * *")
	sched.RepeatedPolicy = RepeatedSecond
	assertNext(t, sched, from, second)
	assertNext(t, sched, second, nextDay)
}

// On 2021-10-03, 02:00 AEST springs forward to 02:30 AEDT, so 02:00-02:29
// do not exist. The first valid moment is 02:30 AEDT = 15:30 UTC.
func TestDSTSkippedNonIntegralHour(t *testing.T) {
	from := utcTime("2021-10-02T00:00:00")
	gapEnd := utcTime("2021-10-02T15:30:00")
	nextDay := utcTime("2021-10-03T15:15:00")

	sched := mustParseSpec(t, "CRON_TZ=Australia/Lord_Howe 15 2 * * *")
	sched.SkippedPolicy = SkippedSkip
	assertNext(t, sched, from, nextDay)

	sched = mustParseSpec(t, "CRON_TZ=Australia/Lord_Howe 15 2 * * *")
	sched.SkippedPolicy = SkippedShift
	assertNext(t, sched, from, gapEnd)
	assertNext(t, sched, gapEnd, nextDay)
}

// WithDSTPolicy applies to schedules parsed with CRON_TZ prefixes, while
// "@every" schedules keep their fixed-interval semantics.
func TestWithDSTPolicy(t *testing.T) {
	c := New(WithDSTPolicy(RepeatedFirst, SkippedShift))

	if _, err := c.AddFunc("CRON_TZ=America/New_York 30 1 * * *", func() {}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddFunc("@every 1h", func() {}); err != nil {
		t.Fatal(err)
	}

	entries := c.Entries()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// The spec schedule honors the cron-wide policy.
	specSched, ok := entries[0].Schedule.(*SpecSchedule)
	if !ok {
		t.Fatalf("expected *SpecSchedule, got %T", entries[0].Schedule)
	}
	assertNext(t, specSched, utcTime("2021-11-07T00:00:00"), utcTime("2021-11-07T05:30:00"))
	assertNext(t, specSched, utcTime("2021-11-07T05:30:00"), utcTime("2021-11-08T06:30:00"))

	// @every keeps fixed-interval semantics: exactly one hour later,
	// straight through the fall-back transition.
	every, ok := entries[1].Schedule.(ConstantDelaySchedule)
	if !ok {
		t.Fatalf("expected ConstantDelaySchedule, got %T", entries[1].Schedule)
	}
	from := utcTime("2021-11-07T05:30:00")
	if got, want := every.Next(from), from.Add(time.Hour); !got.Equal(want) {
		t.Errorf("@every Next(%v) = %v, want %v", from, got, want)
	}
}

// Schedules without an explicit policy keep the historical behavior.
func TestDSTDefaultBehaviorUnchanged(t *testing.T) {
	sched := mustParseSpec(t, "CRON_TZ=America/New_York 30 1 * * *")
	if sched.RepeatedPolicy != RepeatedDefault || sched.SkippedPolicy != SkippedDefault {
		t.Fatal("parsed schedule should carry default policies")
	}
	// Historical fall-back behavior: both occurrences of 01:30 activate.
	assertNext(t, sched, utcTime("2021-11-07T00:00:00"), utcTime("2021-11-07T05:30:00"))
	assertNext(t, sched, utcTime("2021-11-07T05:30:00"), utcTime("2021-11-07T06:30:00"))
	// Historical spring-forward behavior: 02:30 is skipped.
	skipped := mustParseSpec(t, "CRON_TZ=America/New_York 30 2 * * *")
	assertNext(t, skipped, utcTime("2021-03-14T00:00:00"), utcTime("2021-03-15T06:30:00"))
}
