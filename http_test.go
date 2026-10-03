package backoff

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

var retryAfterNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func TestHTTPRetryAfterSeconds(t *testing.T) {
	cause := errors.New("rate limited")
	ra, err := HTTPRetryAfter("120", retryAfterNow, cause)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if ra.Duration != 120*time.Second {
		t.Errorf("Duration = %v, want 120s", ra.Duration)
	}
	if !errors.Is(ra, cause) {
		t.Errorf("result does not wrap the operation error: %v", ra)
	}
}

func TestHTTPRetryAfterDate(t *testing.T) {
	ra, err := HTTPRetryAfter("Sat, 03 Oct 2026 12:02:30 GMT", retryAfterNow, nil)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if ra.Duration != 150*time.Second {
		t.Errorf("Duration = %v, want 150s", ra.Duration)
	}
}

func TestHTTPRetryAfterExpiredDate(t *testing.T) {
	// A date in the past means the operation may be retried immediately.
	ra, err := HTTPRetryAfter("Sat, 03 Oct 2026 11:59:00 GMT", retryAfterNow, nil)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if ra.Duration != 0 {
		t.Errorf("Duration = %v, want 0 for an expired date", ra.Duration)
	}
}

func TestHTTPRetryAfterHugeSeconds(t *testing.T) {
	// A seconds value beyond int64 nanoseconds must not overflow the
	// Duration; it is clamped to the maximum.
	for _, header := range []string{"9223372036854775807", "99999999999999999999999999"} {
		ra, err := HTTPRetryAfter(header, retryAfterNow, nil)
		if err != nil {
			t.Fatalf("HTTPRetryAfter(%q): unexpected parse error: %v", header, err)
		}
		if ra.Duration != time.Duration(math.MaxInt64) {
			t.Errorf("HTTPRetryAfter(%q) Duration = %v, want max Duration", header, ra.Duration)
		}
	}
}

func TestHTTPRetryAfterParseErrors(t *testing.T) {
	for _, header := range []string{
		"",
		"   ",
		"120, 180",                           // multiple values
		"Sat, 03 Oct 2026 12:00:00 GMT, 120", // multiple values
		"soon",                               // neither seconds nor a date
		"10s",                                // not a bare seconds count
		"Sat, 03 Oct 2026 12:00:00 PST",      // HTTP dates must be GMT
	} {
		ra, err := HTTPRetryAfter(header, retryAfterNow, nil)
		if !errors.Is(err, ErrRetryAfterParse) {
			t.Errorf("HTTPRetryAfter(%q) error = %v, want ErrRetryAfterParse", header, err)
		}
		if ra != nil {
			t.Errorf("HTTPRetryAfter(%q) returned a retry indication alongside the parse error", header)
		}
	}
}

// scheduleWith records the waits Retry schedules for an operation that fails
// once with the given error and then succeeds.
func scheduleWith(t *testing.T, opErr error) []time.Duration {
	t.Helper()
	tm := &spyTimer{}
	calls := 0
	_, err := Retry(
		context.Background(),
		func() (int, error) {
			calls++
			if calls == 1 {
				return 0, opErr
			}
			return 1, nil
		},
		WithBackOff(NewConstantBackOff(time.Hour)),
		withTimer(tm),
	)
	if err != nil {
		t.Fatalf("unexpected retry error: %v", err)
	}
	return tm.starts
}

func TestHTTPRetryAfterDateEqualsSeconds(t *testing.T) {
	// At a fixed point in time, an HTTP date and the equivalent delay in
	// seconds schedule the same wait.
	cause := errors.New("rate limited")
	fromDate, err := HTTPRetryAfter("Sat, 03 Oct 2026 12:02:30 GMT", retryAfterNow, cause)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	fromSeconds, err := HTTPRetryAfter("150", retryAfterNow, cause)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	dateWaits := scheduleWith(t, fromDate)
	secondsWaits := scheduleWith(t, fromSeconds)

	if len(dateWaits) != 1 || len(secondsWaits) != 1 {
		t.Fatalf("waits = %v (date), %v (seconds); want a single wait each", dateWaits, secondsWaits)
	}
	if dateWaits[0] != secondsWaits[0] {
		t.Errorf("date scheduled %v, seconds scheduled %v; want equal waits", dateWaits[0], secondsWaits[0])
	}
	if dateWaits[0] != 150*time.Second {
		t.Errorf("wait = %v, want 150s", dateWaits[0])
	}
}

func TestHTTPRetryAfterRespectsMaxTries(t *testing.T) {
	cause := errors.New("rate limited")
	calls := 0
	_, err := Retry(
		context.Background(),
		func() (int, error) {
			calls++
			ra, perr := HTTPRetryAfter("1", retryAfterNow, cause)
			if perr != nil {
				t.Fatalf("unexpected parse error: %v", perr)
			}
			return 0, ra
		},
		WithMaxTries(2),
		withTimer(&testTimer{}),
	)
	if calls != 2 {
		t.Errorf("operation ran %d times, want 2", calls)
	}
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("Cause = %v, want ErrExhausted", err)
	}
	if re := AsRetryError(err); re == nil || !errors.Is(re.LastErr, cause) {
		t.Errorf("LastErr = %v, want the operation error", err)
	}
}

func TestHTTPRetryAfterRespectsMaxElapsedTime(t *testing.T) {
	// A server-requested wait must not stretch the local time budget.
	cause := errors.New("rate limited")
	calls := 0
	_, err := Retry(
		context.Background(),
		func() (int, error) {
			calls++
			ra, perr := HTTPRetryAfter("120", retryAfterNow, cause)
			if perr != nil {
				t.Fatalf("unexpected parse error: %v", perr)
			}
			return 0, ra
		},
		WithMaxElapsedTime(time.Second),
		withTimer(&testTimer{}),
	)
	if calls != 1 {
		t.Errorf("operation ran %d times, want 1 (the 120s wait exceeds the budget)", calls)
	}
	if !errors.Is(err, ErrMaxElapsedTime) {
		t.Errorf("Cause = %v, want ErrMaxElapsedTime", err)
	}
	if re := AsRetryError(err); re == nil || !errors.Is(re.LastErr, cause) {
		t.Errorf("LastErr = %v, want the operation error", err)
	}
}

func TestHTTPRetryAfterRespectsContextCancel(t *testing.T) {
	cause := errors.New("rate limited")
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := Retry(
		ctx,
		func() (int, error) {
			calls++
			cancel()
			ra, perr := HTTPRetryAfter("120", retryAfterNow, cause)
			if perr != nil {
				t.Fatalf("unexpected parse error: %v", perr)
			}
			return 0, ra
		},
		withTimer(&testTimer{}),
	)
	if calls != 1 {
		t.Errorf("operation ran %d times, want 1 (context cancelled)", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Cause = %v, want context.Canceled", err)
	}
	if re := AsRetryError(err); re == nil || !errors.Is(re.LastErr, cause) {
		t.Errorf("LastErr = %v, want the operation error", err)
	}
}
