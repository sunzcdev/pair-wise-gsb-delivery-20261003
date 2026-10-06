# RateLimit

[![GoDoc](https://pkg.go.dev/badge/github.com/bsm/ratelimit)](https://pkg.go.dev/github.com/bsm/ratelimit)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

Simple, thread-safe Go rate-limiter.
Inspired by Antti Huima's algorithm on http://stackoverflow.com/a/668327

### Example

```go
package main

import (
  "github.com/bsm/ratelimit/v3"
  "log"
)

func main() {
  // Create a new rate-limiter, allowing up-to 10 calls
  // per second
  rl := ratelimit.New(10, time.Second)

  for i:=0; i<20; i++ {
    if rl.Limit() {
      fmt.Println("Doh! Over limit!")
    } else {
      fmt.Println("OK")
    }
  }
}
```

### Reservations

A batch service that wants to claim a future slot instead of failing can
reserve one ahead of time.

```go
// Reserve one request, willing to wait at most a minute.
r, err := rl.Reserve(time.Minute)
if err != nil {
  // ErrInvalidCost:        bad arguments
  // ErrMaxBurstExceeded:   cost is larger than the burst size
  // ErrWouldExceedWait:    the slot would be later than the max wait
  return err
}

// Wait until the slot is due, then mark it consumed.
time.Sleep(r.Delay())
if !r.Consume() {
  return errors.New("reservation was cancelled")
}

// If the work is no longer needed, cancel it to return unused allowance.
r.Cancel()
```

Reservation semantics:

- A reservation carries a **cost** (`ReserveN`), an **earliest-execution
  time** (`TimeToAct`/`Delay`) and a **cancel method**.
- Consume and Cancel are mutually exclusive and idempotent: whichever runs
  first wins, the other is a no-op, so the balance can never change twice.
- Cancellation only returns allowance that has not accrued yet; it never
  reclaims capacity for work that is already due.
- Cancelling a reservation **does not move later reservations earlier**:
  reservations keep the time they were promised. The freed capacity may be
  handed to a brand-new reservation, but it is queued at its own (earliest)
  time rather than pulled forward. This FIFO guarantee is maintained under
  concurrency by a single shared queue anchor guarded by the limiter mutex.
- `UpdateRate` only affects new reservations. Outstanding ones keep their
  promised times; cancelling one restores at its original rate, capped by the
  new burst size.
- Reservations that cannot be served within the requested wait return
  `ErrWouldExceedWait`; costs above the burst return
  `ErrMaxBurstExceeded` — there is never an infinite wait.

`ReserveAt`, `ReserveNAt` and `Reservation.CancelAt` take an explicit time,
which makes the behaviour deterministically testable with a fake clock.

### Documentation

Full documentation is available on [pkg.go.dev](https://pkg.go.dev/github.com/bsm/ratelimit).
