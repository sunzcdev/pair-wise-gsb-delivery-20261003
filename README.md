# workerpool

[![GoDoc](https://pkg.go.dev/badge/github.com/gammazero/workerpool)](https://pkg.go.dev/github.com/gammazero/workerpool)
[![Build Status](https://github.com/gammazero/workerpool/actions/workflows/go.yml/badge.svg)](https://github.com/gammazero/workerpool/actions/workflows/go.yml)
[![codecov](https://codecov.io/gh/gammazero/workerpool/graph/badge.svg?token=9BLO50ZRY1)](https://codecov.io/gh/gammazero/workerpool)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](https://github.com/gammazero/workerpool/blob/master/LICENSE)

Concurrency limiting goroutine pool. Limits the concurrency of task execution, not the number of tasks queued. Never blocks submitting tasks, no matter how many tasks are queued.

If you need a worker pool that blocks when all workers are busy, see [workers](https://github.com/gammazero/workers).

This implementation builds on ideas from the following:

- http://marcio.io/2015/07/handling-1-million-requests-per-minute-with-golang
- http://nesv.github.io/golang/2014/02/25/worker-queues-in-go.html

## Installation

To install this package, you need to setup your Go workspace. The simplest way to install the library is to run:
```
$ go get github.com/gammazero/workerpool
```

## Example

```go
package main

import (
	"fmt"
	"github.com/gammazero/workerpool"
)

func main() {
	wp := workerpool.New(2)
	requests := []string{"alpha", "beta", "gamma", "delta", "epsilon"}

	for _, r := range requests {
		wp.Submit(func() {
			fmt.Println("Handling request:", r)
		})
	}

	wp.StopWait()
}
```

[Example wrapper function](https://go.dev/play/p/BWnRhJYarZ1) to show start and finish time of submitted function.

## Keyed tasks

`SubmitKey` (and `DoKey`) associates a task with a comparable key. Tasks with the same key run strictly serially in submission order; tasks with different keys, and tasks submitted with `Submit`, share the pool's workers and run concurrently up to the worker limit. A blocked hot key occupies only one worker, so a large backlog for one key never prevents independent keys from being scheduled.

```go
wp := workerpool.New(4)

// Updates for each user are serialized by user ID, but different users run
// concurrently.
for _, update := range updates {
    workerpool.SubmitKey(wp, update.UserID, func() {
        apply(update)
    })
}

wp.StopWait()
```

## Usage Note

There is no upper limit on the number of tasks queued, other than the limits of system resources. If the number of inbound tasks is too many to even queue for pending processing, then the solution is outside the scope of workerpool. It should be solved by distributing workload over multiple systems, and/or storing input for pending processing in intermediate storage such as a file system, distributed message queue, etc.
