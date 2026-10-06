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

### Atomic multi-bucket limits

Use `LimitMany` to require capacity in several buckets at once, such as global,
tenant, and user budgets. All costs are deducted only when every bucket can
admit the request. The second result is the first parameter index that failed,
or `-1` on success:

```go
costs := []uint64{1, 3, 1}
limiters := []*ratelimit.RateLimiter{global, tenant, user}

if limited, failed := ratelimit.LimitMany(costs, limiters...); limited {
  log.Printf("rejected by bucket %d", failed)
} else {
  log.Println("admitted")
}
```

The same limiter can appear more than once. Its costs are merged before the
check, and an overflowing merged cost reports that bucket as failed without
changing any limiter state.
