# pacer

[![GoDoc](https://godoc.org/github.com/gammazero/workerpool/pacer?status.svg)](https://godoc.org/github.com/gammazero/workerpool/pacer)

A utility to limit the rate at which concurrent goroutines begin execution. This addresses situations where running the concurrent goroutines is OK, as long as their execution does not start at the same time.

The pacer package is independent of the workerpool package. Paced functions can be submitted to a workerpool or can be run as goroutines, and execution will be paced in both cases.

## Cancellation and shutdown

`NextContext(ctx)` waits for a release like `Next()`, but also honors context cancellation and reports the outcome: it returns `true` when a release was acquired and `false` when the wait ended due to cancellation or `Stop()`. `Next()` is equivalent to `NextContext` with a context that is never canceled; the two share the same release sequence and may be mixed freely.

`Pause()` retains waiters, and `Resume()` continues releasing them at the configured rate. A waiter whose context is canceled while queued does not consume a release opportunity. If a release and a cancellation are both ready, the release wins.

`Stop()` immediately unblocks every current and future waiter with a consistent stop result (`NextContext` returns `false`, `Next` returns without waiting), and is safe to call multiple times.
