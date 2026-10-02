gobreaker
=========

[![GoDoc](https://godoc.org/github.com/sony/gobreaker/v2?status.svg)](https://godoc.org/github.com/sony/gobreaker/v2)

[gobreaker][repo-url] implements the [Circuit Breaker pattern](https://msdn.microsoft.com/en-us/library/dn589784.aspx) in Go.

Installation
------------

```
go get github.com/sony/gobreaker/v2
```

Usage
-----

The struct `CircuitBreaker` is a state machine to prevent sending requests that are likely to fail.
The function `NewCircuitBreaker` creates a new `CircuitBreaker`.
The type parameter `T` specifies the return type of requests.

```go
func NewCircuitBreaker[T any](st Settings) *CircuitBreaker[T]
```

You can configure `CircuitBreaker` using the struct `Settings`:

```go
type Settings struct {
	Name          string
	MaxRequests   uint32
	Interval      time.Duration
	BucketPeriod  time.Duration
	Timeout       time.Duration
	ReadyToTrip   func(counts Counts) bool
	OnStateChange func(name string, from State, to State)
	IsSuccessful  func(err error) bool
	IsExcluded    func(err error) bool
	SlowRequestThreshold             time.Duration
	SlowRequestRatio                 float64
	MinSlowRequests                  uint32
	TreatSlowCallsAsFailuresWhenHalfOpen bool
}
```

- `Name` is the name of the `CircuitBreaker`.

- `MaxRequests` is the maximum number of requests allowed to pass through
  when the `CircuitBreaker` is half-open.
  If `MaxRequests` is 0, `CircuitBreaker` allows only 1 request.

- `Interval` is the cyclic period of the closed state
  for `CircuitBreaker` to clear the internal `Counts`, described later in this section.
  If `Interval` is 0, `CircuitBreaker` does not clear the internal `Counts` during the closed state.

- `BucketPeriod` defines the time duration for each bucket in the rolling window strategy.
  The internal `Counts` will be updated and reset gradually for each bucket.
  `Interval` will be automatically adjusted to be a multiple of `BucketPeriod`.
  If `BucketPeriod` is less than or equal to 0, `CircuitBreaker` will use a fixed window strategy instead.

- `Timeout` is the period of the open state,
  after which the state of `CircuitBreaker` becomes half-open.
  If `Timeout` is 0, the timeout value of `CircuitBreaker` is set to 60 seconds.

- `ReadyToTrip` is called with a copy of `Counts` whenever a request fails in the closed state.
  If `ReadyToTrip` returns true, `CircuitBreaker` will be placed into the open state.
  If `ReadyToTrip` is `nil`, the default `ReadyToTrip` is used.
  The default `ReadyToTrip` returns true when the number of consecutive failures is more than 5.

- `OnStateChange` is called whenever the state of `CircuitBreaker` changes.

- `IsSuccessful` is called with the error returned from a request.
  If `IsSuccessful` returns true, the error is counted as a success.
  Otherwise, the error is counted as a failure.
  If `IsSuccessful` is nil, the default `IsSuccessful` is used, which returns false for all non-nil errors.

- `IsExcluded` determines whether a request error should be ignored
  for the purposes of updating the circuit breaker metrics.
  If `IsExcluded` returns true for a given error,
  the request is neither counted as a success nor as a failure.
  This can be used, for example, to ignore context cancellations or
  other errors that should not affect the circuit breaker state.
  If `IsExcluded` is nil, no requests are excluded.
  Excluded requests are excluded from every decision denominator
  (both the error ratio in `ReadyToTrip` and the slow-call ratio below).

- `SlowRequestThreshold`, when greater than 0, enables slow-call detection in
  addition to the error-based `ReadyToTrip` decision. A successful,
  non-excluded request whose elapsed time is greater than or equal to
  `SlowRequestThreshold` is recorded as a slow request. Failed requests are not
  counted as slow. Latency is measured with the monotonic clock
  (`time.Since` the admission instant), so wall clock adjustments do not affect
  it. Requests are attributed to the rolling bucket in which they *start*, not
  the one in which they complete; a request that finishes in a later bucket is
  still recorded in its start bucket while that bucket is inside the window.

- `SlowRequestRatio` is the minimum fraction of slow requests over the valid
  (non-excluded) requests that trips `CircuitBreaker` in the closed state, once
  `MinSlowRequests` valid requests have been observed. Set it in the range
  (0, 1]; slow-call tripping is disabled when it is 0.

- `MinSlowRequests` is the minimum number of valid requests required before the
  slow-call ratio is evaluated, so a handful of requests (fast or slow) cannot
  trigger a trip. If `MinSlowRequests` is 0, it defaults to 5.

- `TreatSlowCallsAsFailuresWhenHalfOpen` controls slow successes in the
  half-open state. When false (the default), a slow success counts as an
  ordinary success and still helps close the breaker. When true, a slow success
  is treated as a failure and immediately reopens the breaker.

The struct `Counts` holds the numbers of requests and their successes/failures/exclusions:

```go
type Counts struct {
	Requests             uint32
	TotalSuccesses       uint32
	TotalFailures        uint32
	TotalExclusions      uint32
	TotalSlowRequests    uint32
	ConsecutiveSuccesses uint32
	ConsecutiveFailures  uint32
}
```

`CircuitBreaker` clears the internal `Counts` either
on the change of the state or at the closed-state intervals.
`Counts` ignores the results of the requests sent before clearing.

`CircuitBreaker` can wrap any function to send a request:

```go
func (cb *CircuitBreaker[T]) Execute(req func() (T, error)) (T, error)
```

The method `Execute` runs the given request if `CircuitBreaker` accepts it.
`Execute` returns an error instantly if `CircuitBreaker` rejects the request.
Otherwise, `Execute` returns the result of the request.
If a panic occurs in the request, `CircuitBreaker` handles it as an error
and causes the same panic again.

Example
-------

```go
var cb *gobreaker.CircuitBreaker[[]byte]

func Get(url string) ([]byte, error) {
	body, err := cb.Execute(func() ([]byte, error) {
		resp, err := http.Get(url)
		if err != nil {
			return nil, err
		}

		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	})
	if err != nil {
		return nil, err
	}

	return body, nil
}
```

See [example](https://github.com/sony/gobreaker/blob/master/v2/example) for details.

License
-------

The MIT License (MIT)

See [LICENSE](https://github.com/sony/gobreaker/blob/master/LICENSE) for details.


[repo-url]: https://github.com/sony/gobreaker
