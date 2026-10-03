package backoff

import (
	"context"
	"errors"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	future := now.Add(30 * time.Second).Format(http.TimeFormat)
	past := now.Add(-time.Hour).Format(http.TimeFormat)

	tests := []struct {
		name   string
		values []string
		want   time.Duration
	}{
		{"zero", []string{"0"}, 0},
		{"seconds", []string{"120"}, 120 * time.Second},
		{"seconds with whitespace", []string{"  120\t"}, 120 * time.Second},
		{"future date", []string{future}, 30 * time.Second},
		{"past date is negative", []string{past}, -time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRetryAfter(tt.values, now, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("parseRetryAfter() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseRetryAfterErrors(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	hugeDigits := repeat9(19)

	tests := []struct {
		name     string
		values   []string
		sentinel error
	}{
		{"missing", nil, ErrRetryAfterInvalid},
		{"empty", []string{""}, ErrRetryAfterInvalid},
		{"only whitespace", []string{"   "}, ErrRetryAfterInvalid},
		{"negative integer", []string{"-5"}, ErrRetryAfterInvalid},
		{"decimal", []string{"1.5"}, ErrRetryAfterInvalid},
		{"garbage", []string{"soon"}, ErrRetryAfterInvalid},
		{"bad date", []string{"Wed, 31 Feb 2026 00:00:00 GMT"}, ErrRetryAfterInvalid},
		{"overflowing digits", []string{hugeDigits}, ErrRetryAfterInvalid},
		{"seconds past max duration", []string{"9223372037"}, ErrRetryAfterInvalid},
		{"far future date", []string{time.Unix(math.MaxInt64/2, 0).Format(http.TimeFormat)}, ErrRetryAfterInvalid},
		{"multiple values", []string{"5", "10"}, ErrRetryAfterMultiple},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRetryAfter(tt.values, now, nil)
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !errors.Is(err, tt.sentinel) {
				t.Errorf("error = %v, want errors.Is %v", err, tt.sentinel)
			}
		})
	}
}

func TestParseRetryAfterOverflowBoundary(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	// The largest representable whole-second Duration (9223372036s ≈ 292y)
	// parses; one second more is rejected rather than overflowing negative.
	maxSeconds := time.Duration(math.MaxInt64) / time.Second
	if d, err := parseRetryAfter([]string{"9223372036"}, now, nil); err != nil || d != maxSeconds*time.Second {
		t.Errorf("max seconds = (%v, %v), want %v, nil", d, err, maxSeconds*time.Second)
	}
	if _, err := parseRetryAfter([]string{"9223372037"}, now, nil); !errors.Is(err, ErrRetryAfterInvalid) {
		t.Errorf("max+1 seconds error = %v, want ErrRetryAfterInvalid", err)
	}

	// A real server cannot send a date before year 1000 (a valid HTTP-date
	// needs four digits), so the negative end of the Duration range (~year
	// 1700 BCE) is unreachable through well-formed input; ordinary past dates
	// still parse to a negative, clamped-to-zero delay.
	if d, err := parseRetryAfter([]string{"Thu, 01 Jan 1970 00:00:00 GMT"}, now, nil); err != nil || d >= 0 {
		t.Errorf("epoch date = (%v, %v), want a negative delay", d, err)
	}
}

func TestRetryAfterHeaderResult(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cause := errors.New("rate limited")

	err := RetryAfterHeader([]string{"30"}, now, cause)
	var ra *RetryAfterError
	if !errors.As(err, &ra) {
		t.Fatalf("result %v is not a *RetryAfterError", err)
	}
	if ra.Duration != 30*time.Second {
		t.Errorf("Duration = %v, want 30s", ra.Duration)
	}
	if !errors.Is(err, cause) {
		t.Errorf("result does not wrap cause: %v", err)
	}

	// HTTP-date gives the same delay as the equivalent delta-seconds value.
	future := now.Add(30 * time.Second).Format(http.TimeFormat)
	errDate := RetryAfterHeader([]string{future}, now, cause)
	var raDate *RetryAfterError
	if !errors.As(errDate, &raDate) {
		t.Fatalf("date result %v is not a *RetryAfterError", errDate)
	}
	if raDate.Duration != ra.Duration {
		t.Errorf("date delay %v != seconds delay %v", raDate.Duration, ra.Duration)
	}
}

func TestRetryAfterHeaderParseErrorPreservesCause(t *testing.T) {
	cause := errors.New("rate limited")

	err := RetryAfterHeader([]string{"soon"}, time.Now(), cause)
	if !errors.Is(err, ErrRetryAfterInvalid) {
		t.Errorf("error = %v, want ErrRetryAfterInvalid", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("parse error lost the cause: %v", err)
	}
	var ra *RetryAfterError
	if errors.As(err, &ra) {
		t.Errorf("parse error must not be a *RetryAfterError: %v", err)
	}

	// Multiple values are distinguishable and likewise preserve the cause.
	err = RetryAfterHeader([]string{"1", "2"}, time.Now(), cause)
	if !errors.Is(err, ErrRetryAfterMultiple) {
		t.Errorf("error = %v, want ErrRetryAfterMultiple", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("multiple-values error lost the cause: %v", err)
	}
}

func TestRetryAfterHeaderDateEquivalentToSeconds(t *testing.T) {
	// With a fixed clock, an HTTP-date and the equivalent delta-seconds value
	// drive the same Retry schedule.
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	future := now.Add(30 * time.Second).Format(http.TimeFormat)
	cause := errors.New("rate limited")

	schedule := func(values []string) []time.Duration {
		calls := 0
		tm := &spyTimer{}
		_, _ = Retry(context.Background(),
			func() (int, error) {
				calls++
				if calls > 1 {
					return 1, nil
				}
				return 0, RetryAfterHeader(values, now, cause)
			},
			WithBackOff(NewConstantBackOff(time.Minute)),
			WithMaxElapsedTime(0),
			withTimer(tm),
		)
		return tm.starts
	}

	fromDate := schedule([]string{future})
	fromSeconds := schedule([]string{"30"})
	if len(fromDate) != 1 || len(fromSeconds) != 1 || fromDate[0] != fromSeconds[0] {
		t.Errorf("schedules differ: date %v vs seconds %v", fromDate, fromSeconds)
	}
}

func TestRetryAfterHeaderRespectsMaxTries(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cause := errors.New("rate limited")

	// Even a tiny server-requested wait cannot exceed WithMaxTries(1).
	_, err := Retry(context.Background(),
		func() (int, error) { return 0, RetryAfterHeader([]string{"0"}, now, cause) },
		WithMaxTries(1),
		WithMaxElapsedTime(0),
		withTimer(&spyTimer{}),
	)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("error = %v, want ErrExhausted", err)
	}
	if re := AsRetryError(err); re == nil || re.LastErr != cause {
		t.Errorf("LastErr = %v, want %v", re, cause)
	}
}

func TestRetryAfterHeaderRespectsMaxElapsedTime(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cause := errors.New("rate limited")
	tm := &spyTimer{}

	// A server wait that overshoots the local budget stops with
	// ErrMaxElapsedTime instead of waiting.
	_, err := Retry(context.Background(),
		func() (int, error) { return 0, RetryAfterHeader([]string{"60"}, now, cause) },
		WithBackOff(&ZeroBackOff{}),
		WithMaxElapsedTime(time.Second),
		withTimer(tm),
	)
	if !errors.Is(err, ErrMaxElapsedTime) {
		t.Fatalf("error = %v, want ErrMaxElapsedTime", err)
	}
	if re := AsRetryError(err); re == nil || re.LastErr != cause {
		t.Errorf("LastErr = %v, want %v", re, cause)
	}
	if len(tm.starts) != 0 {
		t.Errorf("waits = %v, want no wait", tm.starts)
	}
}

func TestRetryAfterHeaderPastDateRetriesNow(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour).Format(http.TimeFormat)
	cause := errors.New("rate limited")
	tm := &spyTimer{}

	calls := 0
	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			if calls > 1 {
				return 1, nil
			}
			return 0, RetryAfterHeader([]string{past}, now, cause)
		},
		WithBackOff(NewConstantBackOff(time.Minute)),
		WithMaxElapsedTime(0),
		withTimer(tm),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tm.starts) != 1 || tm.starts[0] != 0 {
		t.Errorf("waits = %v, want a single wait of 0", tm.starts)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestRetryAfterHeaderRespectsCancellation(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cause := errors.New("rate limited")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancelErr := errors.New("caller stopped")
	cancel(cancelErr)

	// The long server wait is interrupted by the already-cancelled context.
	_, err := Retry(ctx,
		func() (int, error) { return 0, RetryAfterHeader([]string{"60"}, now, cause) },
		WithBackOff(&ZeroBackOff{}),
		WithMaxElapsedTime(0),
		withTimer(&spyTimer{}),
	)
	if !errors.Is(err, cancelErr) {
		t.Fatalf("error = %v, want %v", err, cancelErr)
	}
	if re := AsRetryError(err); re == nil || re.LastErr != cause {
		t.Errorf("LastErr = %v, want %v", re, cause)
	}
}

func repeat9(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '9'
	}
	return string(b)
}
