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

### Reconfiguration

Both the rate and the period (burst capacity) of a live limiter can be
switched atomically, without discarding the instance:

```go
ok := rl.Reconfigure(20, time.Second, ratelimit.PreserveFillRatio)
```

The switch first settles the allowance under the old configuration up to
the exact moment of the switch, then migrates the remaining balance to
the new capacity using one of two strategies:

* `PreserveAllowance` keeps the absolute remaining allowance, clamped to
  the new maximum capacity.
* `PreserveFillRatio` keeps the relative fill level: a bucket that was
  60% full stays 60% full of the new capacity. Scaling always rounds
  down, so repeated reconfiguration can never create extra allowance.

If any argument is invalid (`rate < 1`, `per < 1ns`, an overflowing
capacity, or an unknown strategy), the call is a no-op and returns
`false`. `Reconfigure` is fully serialized with `Limit`/`Undo` through
the same mutex: every concurrent call happens either entirely before or
entirely after the switch.

The legacy `UpdateRate` remains available for backwards compatibility;
it only replaces the rate and leaves the period and the unsettled
allowance untouched.

### Documentation

Full documentation is available on [pkg.go.dev](https://pkg.go.dev/github.com/bsm/ratelimit).
