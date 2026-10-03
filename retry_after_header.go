package backoff

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Parse errors returned by RetryAfterHeader. Match them with errors.Is.
var (
	// ErrRetryAfterMultiple is returned when several Retry-After header
	// values are present; the header is defined to carry at most one value,
	// so a choice between them cannot be made.
	ErrRetryAfterMultiple = errors.New("backoff: multiple Retry-After header values")

	// ErrRetryAfterInvalid is returned when the Retry-After header is missing,
	// empty, neither a delta-seconds value nor an HTTP-date, or names a wait
	// too long to represent as a time.Duration. The parse error wraps cause,
	// if one was passed, so the operation error is not lost.
	ErrRetryAfterInvalid = errors.New("backoff: invalid Retry-After header")
)

// retryAfterParseError is the parse error returned by RetryAfterHeader. It
// wraps both a sentinel (so errors.Is distinguishes multiple values from
// invalid content) and the operation error cause (so it is preserved).
type retryAfterParseError struct {
	sentinel error
	value    string
	cause    error
}

func (e *retryAfterParseError) Error() string {
	if e.sentinel == ErrRetryAfterMultiple {
		return e.sentinel.Error()
	}
	return fmt.Sprintf("%s: %q", e.sentinel, e.value)
}

func (e *retryAfterParseError) Unwrap() []error {
	if e.cause != nil {
		return []error{e.sentinel, e.cause}
	}
	return []error{e.sentinel}
}

// maxRetryAfterSeconds is the largest whole-second Duration
// (math.MaxInt64 nanoseconds expressed in seconds). A delta-seconds value at
// or below it converts without overflow; t.Sub(now) for an HTTP-date can
// differ by up to a second, so date parsing allows one extra second before
// deciding the range.
const maxRetryAfterSeconds = math.MaxInt64 / int64(time.Second)

// RetryAfterHeader converts an HTTP Retry-After header into a
// *RetryAfterError suitable for returning from a Retry operation. It accepts
// both forms defined by RFC 9110:
//
//   - delta-seconds: a non-negative integer number of seconds ("120")
//   - HTTP-date: an absolute timestamp ("Wed, 21 Oct 2015 07:28:00 GMT")
//
// values are the header values exactly as carried on the response
// (resp.Header.Values("Retry-After")); now is the current time supplied by
// the caller, used only to turn an HTTP-date into a relative duration.
//
// A delta-seconds value yields a delay of that many seconds. An HTTP-date in
// the future yields the time left until it; an HTTP-date at or in the past
// yields a negative duration, which Retry clamps to zero so the operation is
// retried immediately rather than waiting.
//
// cause is the error that triggered the retry (e.g. the 429 response); it is
// wrapped by the returned *RetryAfterError and preserved as
// RetryError.LastErr if Retry later stops on a limit or cancellation.
//
// More than one header value returns a parse error wrapping
// ErrRetryAfterMultiple. Missing, empty, malformed, or overflowing content
// returns a parse error wrapping ErrRetryAfterInvalid. Both kinds also wrap
// cause, so the operation error survives. Distinguish them with errors.Is:
//
//	err := RetryAfterHeader(resp.Header.Values("Retry-After"), time.Now(), opErr)
//	switch {
//	case errors.Is(err, ErrRetryAfterMultiple):
//	case errors.Is(err, ErrRetryAfterInvalid):
//	default:
//		return err // a *RetryAfterError driving the next attempt
//	}
//
// A *RetryAfterError only requests a delay: it never bypasses the local
// budget. WithMaxTries, WithMaxElapsedTime, and ctx cancellation apply as
// usual, and a wait longer than the remaining elapsed-time budget makes
// Retry stop with ErrMaxElapsedTime instead of waiting.
func RetryAfterHeader(values []string, now time.Time, cause error) error {
	d, err := parseRetryAfter(values, now, cause)
	if err != nil {
		return err
	}
	return RetryAfter(d, cause)
}

// parseRetryAfter contains the parsing rules for RetryAfterHeader, split out
// so they can be tested without constructing a *RetryAfterError.
func parseRetryAfter(values []string, now time.Time, cause error) (time.Duration, error) {
	if len(values) == 0 {
		return 0, &retryAfterParseError{sentinel: ErrRetryAfterInvalid, cause: cause}
	}
	if len(values) > 1 {
		return 0, &retryAfterParseError{sentinel: ErrRetryAfterMultiple, cause: cause}
	}

	v := strings.TrimSpace(values[0])
	if v == "" {
		return 0, &retryAfterParseError{sentinel: ErrRetryAfterInvalid, value: v, cause: cause}
	}

	if isDigits(v) {
		return parseRetryAfterSeconds(v, cause)
	}
	return parseRetryAfterDate(v, now, cause)
}

// parseRetryAfterSeconds converts a delta-seconds value without letting the
// seconds*time.Second multiplication overflow the Duration.
func parseRetryAfterSeconds(v string, cause error) (time.Duration, error) {
	var seconds int64
	for i := 0; i < len(v); i++ {
		digit := int64(v[i] - '0')
		if seconds > (math.MaxInt64-digit)/10 {
			return 0, &retryAfterParseError{sentinel: ErrRetryAfterInvalid, value: v, cause: cause}
		}
		seconds = seconds*10 + digit
	}
	if seconds > maxRetryAfterSeconds {
		return 0, &retryAfterParseError{sentinel: ErrRetryAfterInvalid, value: v, cause: cause}
	}
	return time.Duration(seconds) * time.Second, nil
}

// parseRetryAfterDate converts an HTTP-date. Dates beyond the representable
// Duration range are rejected; past dates produce a negative duration.
func parseRetryAfterDate(v string, now time.Time, cause error) (time.Duration, error) {
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, &retryAfterParseError{sentinel: ErrRetryAfterInvalid, value: v, cause: cause}
	}

	// Bound by whole seconds first; the full-precision difference is within a
	// second of this, so the guard below also bounds the final Duration and
	// avoids overflow in t.Sub(now) for dates near the Duration range ends.
	secs := t.Unix() - now.Unix()
	if secs > maxRetryAfterSeconds+1 {
		return 0, &retryAfterParseError{sentinel: ErrRetryAfterInvalid, value: v, cause: cause}
	}
	if secs < -maxRetryAfterSeconds-1 {
		// The wait is beyond the negative end of the Duration range. Retry
		// treats a negative delay as "retry now", so clamp the magnitude.
		return time.Duration(math.MinInt64), nil
	}

	// Keep the full-precision difference so a date a fraction of a second in
	// the future is not rounded down to zero (retry now).
	return t.Sub(now), nil
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
