package backoff_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
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
			seconds, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64)
			if err == nil {
				return "", backoff.RetryAfter(time.Duration(seconds)*time.Second, fmt.Errorf("rate limited: %s", resp.Status))
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

// Failures of different kinds can be routed to independent backoff policies.
// The classifier is supplied by the caller and decides both whether to retry
// and which class's BackOff paces the wait.
func ExampleWithClassifiedBackOff() {
	var (
		errConn = errors.New("connection refused")
		errRate = errors.New("too many requests")
		errAuth = errors.New("bad credentials")
	)

	const (
		classConn = "connection"
		classRate = "rate-limit"
	)

	classify := func(err error) (backoff.ErrorClass, bool) {
		switch {
		case errors.Is(err, errConn):
			return classConn, true
		case errors.Is(err, errRate):
			return classRate, true
		default:
			// Any other error is non-retriable and stops like Permanent.
			return "", false
		}
	}

	// The attempt sequence below is: connection error, rate-limit error,
	// connection error, success. Each class owns its own BackOff.
	attempt := 0
	operation := func() (string, error) {
		attempt++
		switch attempt {
		case 1, 3:
			return "", errConn
		case 2:
			return "", errRate
		case 4:
			return "hello", nil
		default:
			return "", errAuth
		}
	}

	result, err := backoff.Retry(context.TODO(), operation,
		backoff.WithMaxElapsedTime(0),
		backoff.WithClassifiedBackOff(classify, func(class backoff.ErrorClass) backoff.BackOff {
			if class == classRate {
				return backoff.NewConstantBackOff(50 * time.Millisecond)
			}
			return backoff.NewConstantBackOff(10 * time.Millisecond)
		}),
	)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	fmt.Println(result)
	// Output: hello
}
