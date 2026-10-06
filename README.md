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

### Documentation

Full documentation is available on [pkg.go.dev](https://pkg.go.dev/github.com/bsm/ratelimit).

### Reservations

When a caller can wait before sending a request, it can reserve a future
slot instead of being rejected:

```go
r, err := rl.Reserve()          // or rl.ReserveN(n)
if err != nil {
    // ErrCostExceedsBurst: the request can never be served in finite time
}
defer func() {
    if canceled {
        _ = r.Cancel()          // release only what nobody else needs
    }
}()

time.Sleep(r.Delay())           // wait until TimeToAct
if err := r.Consume(); err != nil {
    // reservation was already consumed or canceled
}
```

Each `Reservation` exposes its `Cost`, its earliest execution time
(`TimeToAct` / `Delay`) and a one-shot `Cancel` (plus `CancelAt(t)`).

Semantics:

- **Cancellation does not move later reservations.** When an earlier
  reservation is canceled, reservations made afterward keep the exact time
  they were promised; they are never rescheduled earlier. A newly made
  reservation may of course reuse whatever capacity the cancellation freed.
  This keeps scheduling decisions immutable and makes the outcome identical
  under concurrent reserve/cancel operations.
- **Cancel cannot reclaim executed work.** At or after the reservation's
  earliest execution time, `Cancel`/`CancelAt` returns nothing; the work may
  already have happened.
- **Only uncommitted allowance is returned.** Cancel is ledger-aware, not an
  `Undo` stack: tokens already committed to later reservations stay with them.
- **One-shot.** Once consumed or canceled, a reservation never changes the
  limiter balance again; repeat calls return `ErrReservationFinal`.
- **Rate changes across reservations** apply from the change onward. Existing
  reservations keep their times; the new rate governs subsequent refill and
  new reservations.
- **Unsatisfiable requests fail fast.** `ReserveN(n)` with `n` greater than
  the burst returns `ErrCostExceedsBurst` instead of scheduling at an
  infinite time.

For deterministic tests, build the limiter with a custom clock:

```go
rl := ratelimit.NewWithClock(10, time.Second, myClock)
```
