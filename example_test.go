package backoff_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/cenkalti/backoff/v7"
)

func ExampleRetry() {
	// A stand-in for the remote service. In real code this is the endpoint you
	// are calling; here it is an in-process test server so the example is
	// self-contained and does not depend on the network.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Define an operation function that returns a value and an error.
	// The value can be any type.
	// We'll pass this operation to Retry function.
	operation := func() (string, error) {
		// An example request that may fail.
		resp, err := http.Get(server.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()

		// If we are being rate limited, return a RetryAfter to specify how long to wait.
		// This will also reset the backoff policy.
		if resp.StatusCode == http.StatusTooManyRequests {
			opErr := fmt.Errorf("rate limited: %s", resp.Status)
			err := backoff.RetryAfterHeader(resp.Header.Values("Retry-After"), time.Now(), opErr)
			if !errors.Is(err, backoff.ErrRetryAfterMultiple) &&
				!errors.Is(err, backoff.ErrRetryAfterInvalid) {
				return "", err
			}
		}

		// In case of non-retriable error, return Permanent error to stop retrying.
		// For this HTTP example, client errors are non-retriable.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return "", backoff.Permanent(errors.New("bad request"))
		}

		// Return successful response.
		return "hello", nil
	}

	result, err := backoff.Retry(context.TODO(), operation, backoff.WithBackOff(backoff.NewExponentialBackOff()))
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// Operation is successful after retries.

	fmt.Println(result)
	// Output: hello
}

func ExampleRetryAfterHeader() {
	// A fixed "now" keeps the example deterministic: the same instant is used
	// for both representations of the wait, so an HTTP-date and its equivalent
	// delta-seconds value parse to the identical duration and drive the same
	// Retry schedule.
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cause := errors.New("rate limited: 429")

	date := now.Add(30 * time.Second).Format(http.TimeFormat)

	show := func(values []string) {
		err := backoff.RetryAfterHeader(values, now, cause)
		var ra *backoff.RetryAfterError
		switch {
		case errors.As(err, &ra):
			fmt.Println("wait", ra.Duration)
		case errors.Is(err, backoff.ErrRetryAfterMultiple):
			fmt.Println("multiple header values")
		case errors.Is(err, backoff.ErrRetryAfterInvalid):
			fmt.Println("invalid header")
		}
	}

	show([]string{"30"})                                        // delta-seconds
	show([]string{date})                                        // equivalent HTTP-date
	show([]string{now.Add(-time.Hour).Format(http.TimeFormat)}) // negative: Retry waits 0 and retries now
	show([]string{"1", "2"})                                    // more than one value
	show([]string{"soon"})                                      // neither a number nor a date
	// Output:
	// wait 30s
	// wait 30s
	// wait -1h0m0s
	// multiple header values
	// invalid header
}

func ExampleRetry_outcomes() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	operation := func() (string, error) {
		resp, err := http.Get(server.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		return "ok", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := backoff.Retry(ctx, operation,
		backoff.WithMaxElapsedTime(10*time.Second),
		backoff.WithMaxTries(5),
	)

	switch {
	case err == nil:
		// Operation succeeded.
		fmt.Println(result)

	case errors.Is(err, backoff.ErrPermanent):
		// The operation returned a Permanent (non-retriable) error.
		fmt.Println("permanent:", err)

	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller's context was cancelled or its deadline expired.
		fmt.Println("context done:", err)

	case errors.Is(err, backoff.ErrMaxElapsedTime):
		// The WithMaxElapsedTime budget was exhausted.
		fmt.Println("timed out:", err)

	case errors.Is(err, backoff.ErrExhausted):
		// WithMaxTries was reached or the backoff policy stopped.
		fmt.Println("retries exhausted:", err)
	}

	// The last operation error is always available, whatever the cause:
	if re := backoff.AsRetryError(err); re != nil {
		fmt.Println("last error:", re.LastErr)
	}
	// Output: ok
}

func ExampleTicker() {
	// An operation that may fail.
	operation := func() (string, error) {
		return "hello", nil
	}

	ticker := backoff.NewTicker(backoff.NewExponentialBackOff())
	defer ticker.Stop()

	var result string
	var err error

	// Ticks will continue to arrive when the previous operation is still running,
	// so operations that take a while to fail could run in quick succession.
	for range ticker.C {
		if result, err = operation(); err != nil {
			log.Println(err, "will retry...")
			continue
		}

		break
	}

	if err != nil {
		// Operation has failed.
		fmt.Println("Error:", err)
		return
	}

	// Operation is successful after retries.

	fmt.Println(result)
	// Output: hello
}
