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

### Bulk consumption

An export request may consume hundreds of units. Use `TakeN` to deduct
them atomically in a single call instead of looping over `Limit`. It
returns whether the request was accepted and the earliest time at which
the required units can become available:

```go
if allowed, earliest := rl.TakeN(500); !allowed {
  log.Printf("rate limited; retry no earlier than %s", earliest)
}
```

`TakeN` is all-or-nothing: either all N units are deducted or none are.
Use the read-only `EarliestAvailable` to inspect availability without
consuming or pre-borrowing allowance.

Semantics:

- A cost of zero (`n <= 0`) always succeeds and deducts nothing.
- A cost above the bucket capacity (more than `rate` units) can never be
  accepted; the returned time is the zero value.
- Costs that would overflow the internal representation are treated as
  over capacity. Long idle periods refill the bucket to full at most.

### Injecting a clock

For fast, deterministic tests, inject a controllable time source with
`NewWithClock` instead of sleeping:

```go
clk := &fakeClock{}
rl := ratelimit.NewWithClock(10, time.Second, clk)

if allowed, earliest := rl.TakeN(50); !allowed {
  clk.Advance(earliest.Sub(clk.Now()))
}
```

### Documentation

Full documentation is available on [pkg.go.dev](https://pkg.go.dev/github.com/bsm/ratelimit).
