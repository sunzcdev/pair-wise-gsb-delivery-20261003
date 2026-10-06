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

### Multi-unit costs

For requests that cost more than one unit, `Take` deducts `n` units in a
single all-or-nothing operation and reports the earliest time the request
can be accepted at the current rate. `Estimate` performs the same check
without consuming any allowance:

```go
rl := ratelimit.New(100, time.Second)

if ok, availableAt := rl.Take(250); !ok {
  if availableAt.IsZero() {
    log.Println("cost exceeds bucket capacity, will never be accepted")
  } else {
    log.Println("over limit, retry at", availableAt)
  }
}
```

A zero cost always succeeds; a cost exceeding the bucket capacity is
rejected with a zero `time.Time`. Use `ratelimit.WithClock` to inject a
custom time source for tests.

### Documentation

Full documentation is available on [pkg.go.dev](https://pkg.go.dev/github.com/bsm/ratelimit).
