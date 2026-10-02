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
	Name                  string
	MaxRequests           uint32
	Interval              time.Duration
	BucketPeriod          time.Duration
	Timeout               time.Duration
	ReadyToTrip           func(counts Counts) bool
	OnStateChange         func(name string, from State, to State)
	IsSuccessful          func(err error) bool
	IsExcluded            func(err error) bool
	Clock                 gobreaker.Clock
	SlowCallThreshold     time.Duration
	SlowCallRatio         float64
	MinSampleRequests     uint32
	SlowSuccessInHalfOpen bool
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
  Excluded requests are also excluded from the denominator of every ratio
  decision (the failure ratio in `ReadyToTrip` and the slow-call ratio).
  If `IsExcluded` is nil, no requests are excluded.

- `Clock` is the source of time used for bucket boundaries, the open-state
  timeout and request latency. If `Clock` is nil, the system clock
  (`time.Now`) is used. Request latency is the elapsed time between the
  `Clock.Now` reading at request start and the one at request completion;
  with the system clock these readings carry Go's monotonic clock, so
  latency is unaffected by wall-clock adjustments. Each request is
  attributed to the bucket that was current when it started, so requests
  that complete in a later bucket (including across bucket boundaries)
  remain consistent with the request slot reserved at start time.

- `SlowCallThreshold` marks a request as slow when its latency is strictly
  greater than the threshold. If `SlowCallThreshold` is less than or equal
  to 0, slow-call detection is disabled.

- `SlowCallRatio` and `MinSampleRequests` add an independent trip condition
  in the closed state: after every non-excluded request completes, the
  breaker opens when the number of admitted non-excluded requests
  (`Requests - TotalExclusions`, the same denominator as the failure ratio)
  is at least `MinSampleRequests` and the proportion of slow requests
  (`TotalSlow / non-excluded requests`) is at least `SlowCallRatio`, a
  value in the range (0, 1]. This is evaluated in addition to
  `ReadyToTrip`; the existing error/success-rate decision is unchanged, and
  either condition can open the breaker.

- `SlowSuccessInHalfOpen` controls half-open behavior. By default a slow
  success still counts toward the consecutive successes needed to close the
  breaker. When true, a slow success in the half-open state re-opens the
  breaker immediately, like a failure.

The struct `Counts` holds the numbers of requests and their successes/failures/exclusions:

```go
type Counts struct {
	Requests             uint32
	TotalSuccesses       uint32
	TotalFailures        uint32
	TotalExclusions      uint32
	TotalSlow            uint32
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
