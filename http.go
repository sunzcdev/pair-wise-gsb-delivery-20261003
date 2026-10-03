package backoff

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrRetryAfterParse is the cause of the parse errors returned by
// HTTPRetryAfter: the header carried multiple values, or its content is
// neither a delay in seconds nor an HTTP date. Match it with errors.Is.
var ErrRetryAfterParse = errors.New("backoff: cannot parse Retry-After header")

// HTTPRetryAfter converts an HTTP Retry-After header value (RFC 9110,
// section 10.2.3) into the library's retry indication: a *RetryAfterError
// that Retry honours by waiting the requested delay before the next attempt
// and resetting the backoff policy, exactly as with RetryAfter.
//
// The header may carry a delay in seconds ("120") or an HTTP date
// ("Wed, 21 Oct 2015 07:28:00 GMT"). now is the current time, supplied by the
// caller, and turns a date into a duration; a date in the past means the
// operation may be retried immediately. A seconds value too large for a
// time.Duration is clamped to the maximum Duration rather than overflowing.
//
// cause is the operation error that triggered the wait; it is preserved as
// RetryError.LastErr if retrying later stops.
//
// If the header holds multiple values or cannot be parsed, HTTPRetryAfter
// returns a nil *RetryAfterError and an error wrapping ErrRetryAfterParse;
// the caller then decides how to fall back, for example by returning cause
// and letting the plain backoff policy schedule the next attempt.
//
// The requested delay never overrides local limits: combined with Retry, the
// backoff policy's Stop, WithMaxTries, WithMaxElapsedTime, and context
// cancellation still apply.
func HTTPRetryAfter(header string, now time.Time, cause error) (*RetryAfterError, error) {
	d, err := parseRetryAfter(header, now)
	if err != nil {
		return nil, err
	}
	return &RetryAfterError{Duration: d, err: cause}, nil
}

// parseRetryAfter parses a single Retry-After header value into a wait
// duration relative to now.
func parseRetryAfter(header string, now time.Time) (time.Duration, error) {
	v := strings.TrimSpace(header)
	if v == "" {
		return 0, fmt.Errorf("%w: empty value", ErrRetryAfterParse)
	}
	if isDigits(v) {
		return retryAfterSeconds(v), nil
	}
	// A single HTTP date contains a comma itself ("Wed, 21 Oct 2015 ..."),
	// so only a value that does not parse as one date is reported as
	// carrying multiple values.
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, nil
		}
		return 0, nil
	}
	if strings.Contains(v, ",") {
		return 0, fmt.Errorf("%w: multiple values %q", ErrRetryAfterParse, header)
	}
	return 0, fmt.Errorf("%w: %q is neither seconds nor an HTTP date", ErrRetryAfterParse, header)
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// retryAfterSeconds parses a decimal seconds string into a Duration, clamping
// values that would overflow to the maximum Duration.
func retryAfterSeconds(s string) time.Duration {
	const maxSeconds = int64(math.MaxInt64) / int64(time.Second)
	secs, err := strconv.ParseInt(s, 10, 64)
	if err != nil || secs > maxSeconds {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(secs) * time.Second
}
