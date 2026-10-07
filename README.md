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

### Reconfiguring a live limiter

`Reconfigure` atomically switches a limiter to a new rate and period
without discarding its state:

1. the allowance accrued under the old configuration is settled up to
   the moment of the switch;
2. the remaining balance is migrated to the new capacity according to a
   strategy:
   - `PreserveAllowance` keeps the absolute balance, clamped to the new
     maximum;
   - `PreserveRatio` keeps the fill ratio of the bucket.

```go
rl := ratelimit.New(10, time.Second)

// switch to 100/minute, keeping the fill ratio
if !rl.Reconfigure(100, time.Minute, ratelimit.PreserveRatio) {
  log.Println("invalid configuration, limiter left unchanged")
}
```

Invalid arguments (rate < 1, period < 1ns, overflowing capacity, unknown
strategy) leave the limiter untouched and return `false`. Reconfigure is
fully serialized with `Limit`/`Undo`, so concurrent callers observe the
configuration either entirely before or entirely after the switch.

The legacy `UpdateRate` only adjusts the rate and maximum; it neither
settles nor migrates the balance and is kept for backward compatibility.
Prefer `Reconfigure` for precise semantics.
