package cron

import (
	"testing"
	"time"
)

// mustParseTime parses an RFC3339 string or fails the test.
func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("failed to parse time %q: %v", value, err)
	}
	return parsed
}

// dstSchedule parses a standard spec and attaches DST policies to it.
func dstSchedule(t *testing.T, spec string, ambiguous AmbiguousTimePolicy, nonexistent NonexistentTimePolicy) Schedule {
	t.Helper()
	sched, err := ParseStandard(spec)
	if err != nil {
		t.Fatalf("failed to parse spec %q: %v", spec, err)
	}
	return WithDSTPolicy(sched, ambiguous, nonexistent)
}

// nextRuns returns the next len(expected) activation times of the schedule,
// feeding each result back into Next, and asserts they are strictly
// increasing and equal to the expected absolute instants.
func assertNextRuns(t *testing.T, sched Schedule, from string, expected []string) {
	t.Helper()
	prev := mustParseTime(t, from)
	for i, want := range expected {
		got := sched.Next(prev)
		if !got.After(prev) {
			t.Errorf("run %d: Next(%v) = %v, not strictly later than its input", i, prev, got)
		}
		if wantTime := mustParseTime(t, want); !got.Equal(wantTime) {
			t.Errorf("run %d: Next(%v) = %v, want %v", i, prev, got.UTC().Format(time.RFC3339), want)
		}
		prev = got
	}
}

// TestDSTFallBack covers the repeated hour when clocks turn back in
// America/New_York on 2026-11-01: wall times 01:00-01:59 occur twice.
func TestDSTFallBack(t *testing.T) {
	// 2026-11-01 02:00 EDT -> 01:00 EST, i.e. 06:00 UTC.
	const spec = "CRON_TZ=America/New_York 30 1 * * *"
	from := "2026-11-01T00:00:00-04:00"

	// The default behavior (first occurrence) is preserved.
	assertNextRuns(t, dstSchedule(t, spec, AmbiguousFirst, NonexistentSkip), from, []string{
		"2026-11-01T05:30:00Z", // 01:30 EDT
		"2026-11-02T06:30:00Z", // 01:30 EST next day
	})

	// Only the second occurrence runs.
	assertNextRuns(t, dstSchedule(t, spec, AmbiguousSecond, NonexistentSkip), from, []string{
		"2026-11-01T06:30:00Z", // 01:30 EST
		"2026-11-02T06:30:00Z",
	})

	// Both occurrences run; the second is found even right after the first
	// fired, and the schedule does not get stuck in the repeated period.
	assertNextRuns(t, dstSchedule(t, spec, AmbiguousBoth, NonexistentSkip), from, []string{
		"2026-11-01T05:30:00Z", // 01:30 EDT
		"2026-11-01T06:30:00Z", // 01:30 EST
		"2026-11-02T06:30:00Z",
		"2026-11-03T06:30:00Z",
	})
}

// TestDSTSpringForward covers the missing hour when clocks jump forward in
// America/New_York on 2026-03-08: wall times 02:00-02:59 do not exist.
func TestDSTSpringForward(t *testing.T) {
	// 2026-03-08 02:00 EST -> 03:00 EDT, i.e. 07:00 UTC.
	const spec = "CRON_TZ=America/New_York 30 2 * * *"
	from := "2026-03-08T00:00:00-05:00"

	// The default behavior (skip the nonexistent time) is preserved.
	assertNextRuns(t, dstSchedule(t, spec, AmbiguousFirst, NonexistentSkip), from, []string{
		"2026-03-09T06:30:00Z", // 02:30 EDT next day
	})

	// The job is pushed to the first valid moment after the jump: 03:00 EDT.
	assertNextRuns(t, dstSchedule(t, spec, AmbiguousFirst, NonexistentPush), from, []string{
		"2026-03-08T07:00:00Z", // 03:00 EDT, the moment the gap ends
		"2026-03-09T06:30:00Z",
	})
}

// TestDSTCrossMidnight covers transitions that happen at midnight in
// America/Santiago, so the repeated and missing wall times straddle dates.
func TestDSTCrossMidnight(t *testing.T) {
	// Fall-back: 2026-04-05 00:00 -0300 -> 2026-04-04 23:00 -0400.
	// Wall times 23:00-23:59 on 2026-04-04 occur twice.
	assertNextRuns(t, dstSchedule(t, "CRON_TZ=America/Santiago 30 23 * * *", AmbiguousBoth, NonexistentSkip),
		"2026-04-04T12:00:00-03:00", []string{
			"2026-04-05T02:30:00Z", // 23:30 -0300, first occurrence
			"2026-04-05T03:30:00Z", // 23:30 -0400, second occurrence
			"2026-04-06T03:30:00Z", // 23:30 -0400 next day
		})

	// Spring-forward: 2026-09-06 00:00 -0400 -> 01:00 -0300.
	// Wall times 00:00-00:59 on 2026-09-06 do not exist; midnight is pushed
	// to 01:00, the first valid moment of that day.
	assertNextRuns(t, dstSchedule(t, "CRON_TZ=America/Santiago 0 0 * * *", AmbiguousFirst, NonexistentPush),
		"2026-09-05T12:00:00-04:00", []string{
			"2026-09-06T04:00:00Z", // 01:00 -0300, the moment the gap ends
			"2026-09-07T03:00:00Z", // 00:00 -0300 next day
		})
	assertNextRuns(t, dstSchedule(t, "CRON_TZ=America/Santiago 0 0 * * *", AmbiguousFirst, NonexistentSkip),
		"2026-09-05T12:00:00-04:00", []string{
			"2026-09-07T03:00:00Z", // skipped to the next day
		})
}

// TestDSTNonHourTransition covers Australia/Lord_Howe, whose DST shifts are
// 30 minutes rather than a whole hour.
func TestDSTNonHourTransition(t *testing.T) {
	// Fall-back: 2026-04-05 02:00 +1100 -> 01:30 +1030.
	// Wall times 01:30-01:59 occur twice.
	assertNextRuns(t, dstSchedule(t, "CRON_TZ=Australia/Lord_Howe 45 1 * * *", AmbiguousBoth, NonexistentSkip),
		"2026-04-04T12:00:00+11:00", []string{
			"2026-04-04T14:45:00Z", // 01:45 +1100, first occurrence
			"2026-04-04T15:15:00Z", // 01:45 +1030, second occurrence
			"2026-04-05T15:15:00Z", // 01:45 +1030 next day
		})

	// Spring-forward: 2026-10-04 02:00 +1030 -> 02:30 +1100.
	// Wall times 02:00-02:29 do not exist.
	assertNextRuns(t, dstSchedule(t, "CRON_TZ=Australia/Lord_Howe 15 2 * * *", AmbiguousFirst, NonexistentPush),
		"2026-10-03T12:00:00+10:30", []string{
			"2026-10-03T15:30:00Z", // 02:30 +1100, the moment the gap ends
			"2026-10-04T15:15:00Z", // 02:15 +1100 next day
		})
	assertNextRuns(t, dstSchedule(t, "CRON_TZ=Australia/Lord_Howe 15 2 * * *", AmbiguousFirst, NonexistentSkip),
		"2026-10-03T12:00:00+10:30", []string{
			"2026-10-04T15:15:00Z", // skipped to the next day
		})
}

// TestDSTStrictlyIncreasing walks a schedule with AmbiguousBoth across the
// fall-back transition many times, verifying Next never stalls or goes back.
func TestDSTStrictlyIncreasing(t *testing.T) {
	sched := dstSchedule(t, "CRON_TZ=America/New_York 30 1 * * *", AmbiguousBoth, NonexistentSkip)
	prev := mustParseTime(t, "2026-10-31T00:00:00-04:00")
	for i := 0; i < 100; i++ {
		next := sched.Next(prev)
		if next.IsZero() {
			t.Fatalf("iteration %d: Next(%v) returned zero time", i, prev)
		}
		if !next.After(prev) {
			t.Fatalf("iteration %d: Next(%v) = %v, not strictly later", i, prev, next)
		}
		prev = next
	}
}

// TestDSTDefaultBehaviorUnchanged verifies that a schedule without explicit
// policies behaves exactly as before on DST transition days.
func TestDSTDefaultBehaviorUnchanged(t *testing.T) {
	sched, err := ParseStandard("CRON_TZ=America/New_York 30 1 * * *")
	if err != nil {
		t.Fatal(err)
	}
	assertNextRuns(t, sched, "2026-11-01T00:00:00-04:00", []string{
		"2026-11-01T05:30:00Z", // first occurrence only
	})

	sched, err = ParseStandard("CRON_TZ=America/New_York 30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	assertNextRuns(t, sched, "2026-03-08T00:00:00-05:00", []string{
		"2026-03-09T06:30:00Z", // nonexistent time skipped
	})
}

// TestDSTEveryUnaffected verifies that fixed-interval "@every" schedules keep
// their absolute-interval semantics across DST transitions.
func TestDSTEveryUnaffected(t *testing.T) {
	sched := Every(24 * time.Hour)
	if got := WithDSTPolicy(sched, AmbiguousBoth, NonexistentPush); got != sched {
		t.Errorf("WithDSTPolicy should return non-SpecSchedule schedules unchanged")
	}
	from := mustParseTime(t, "2026-10-31T12:00:00Z")
	for i := 0; i < 5; i++ {
		next := sched.Next(from)
		if diff := next.Sub(from); diff != 24*time.Hour {
			t.Errorf("interval across DST = %v, want exactly 24h", diff)
		}
		from = next
	}
}
