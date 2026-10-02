package backoff

import (
	"context"
	"errors"
	"time"
)

// DefaultMaxElapsedTime sets a default limit for the total retry duration.
const DefaultMaxElapsedTime = 15 * time.Minute

// Operation is the function Retry calls. It is invoked at least once and may be
// retried on error. Return a Permanent error to stop retrying immediately, or a
// RetryAfterError to control the delay before the next attempt.
type Operation[T any] func() (T, error)

// OperationContext is the function RetryContext calls. It behaves like
// Operation, but the ctx passed to each attempt is a fresh child context
// derived from the RetryContext context. See WithAttemptTimeout and
// RetryContext for how that context is bounded and released.
type OperationContext[T any] func(ctx context.Context) (T, error)

// Notify is called after a failed attempt that will be retried, with the
// operation error and the backoff duration before the next attempt. It is
// called once per retry, not for the final error that stops Retry (a permanent
// error, an exhausted limit, or a cancelled context).
type Notify func(error, time.Duration)

// retryOptions holds configuration settings for the retry mechanism.
type retryOptions struct {
	BackOff        BackOff       // Strategy for calculating backoff periods.
	Timer          timer         // Timer to manage retry delays.
	Notify         Notify        // Optional function called before each backoff wait.
	MaxTries       uint          // Maximum number of retry attempts.
	MaxElapsedTime time.Duration // Maximum total time for all retries.
	AttemptTimeout time.Duration // Per-attempt deadline used by RetryContext.
}

// RetryOption configures the behavior of Retry.
type RetryOption func(*retryOptions)

// WithBackOff configures the backoff policy used between attempts. The default
// is NewExponentialBackOff.
//
// Retry calls Reset on the policy before the first attempt, so a previously
// used policy may be passed. A BackOff is stateful and not safe for concurrent
// use: give each concurrent Retry call its own BackOff rather than sharing one.
func WithBackOff(b BackOff) RetryOption {
	return func(args *retryOptions) {
		args.BackOff = b
	}
}

// withTimer sets a custom timer for managing delays between retries.
func withTimer(t timer) RetryOption {
	return func(args *retryOptions) {
		args.Timer = t
	}
}

// WithNotify sets a function called after each failed attempt that will be
// retried. See Notify for exactly when it fires.
func WithNotify(n Notify) RetryOption {
	return func(args *retryOptions) {
		args.Notify = n
	}
}

// WithMaxTries limits the total number of attempts, not retries: WithMaxTries(1)
// runs the operation once and does not retry. When the limit is reached, Retry
// returns a *RetryError with Cause ErrExhausted. The default, 0, means no limit.
func WithMaxTries(n uint) RetryOption {
	return func(args *retryOptions) {
		args.MaxTries = n
	}
}

// WithMaxElapsedTime limits the total wall-clock time spent retrying, measured
// from when Retry is called. When the limit is reached, Retry returns a
// *RetryError with Cause ErrMaxElapsedTime.
//
// The limit is checked only between attempts: it gates whether another attempt
// is scheduled. It does not interrupt an operation that is already running, nor
// a backoff wait already in progress, and Retry stops early rather than
// starting a backoff that would overrun the limit.
//
// The elapsed time includes the runtime of the attempts themselves, not only
// the waits between them. An operation that runs longer than the limit before
// it fails (a long poll or a blocking receive, for example) is therefore not
// retried at all. Such operations need a bound of their own, such as a
// BackOff that returns Stop once failures have lasted too long.
//
// This differs from bounding Retry with a context deadline (e.g.
// context.WithTimeout): a context deadline is reactive — it interrupts the
// backoff wait and, if the operation observes the context, can abort an
// in-flight attempt — and Retry reports it with Cause context.DeadlineExceeded.
//
// The default is DefaultMaxElapsedTime (15 minutes), so both limits are active
// at once unless overridden. Pass 0 to disable the elapsed-time limit and rely
// on the context (or WithMaxTries) instead.
func WithMaxElapsedTime(d time.Duration) RetryOption {
	return func(args *retryOptions) {
		args.MaxElapsedTime = d
	}
}

// WithAttemptTimeout bounds each individual attempt in RetryContext. Every
// attempt receives a fresh child context whose deadline is this duration after
// the attempt starts; the child is derived from the RetryContext context, so
// cancellation of the overall context always wins and the attempt never
// outlives it.
//
// The timeout only cancels the context passed to the operation; it does not
// kill the goroutine running it. An attempt that ignores its context is
// allowed to finish, and what it returns is what RetryContext acts on, so a
// well-behaved operation must select on ctx.Done (or pass ctx to a context-
// aware callee) for the bound to take effect.
//
// An attempt whose context deadline fires is treated like any other failed
// attempt: the operation returns (typically with context.DeadlineExceeded)
// and RetryContext backs off and retries according to the policy and the
// other limits. Cancelling the overall context is different — it aborts the
// in-flight attempt and stops any further attempts, reported with the
// overall context's Cause.
//
// Each child context (and its timer) is released as soon as its attempt
// returns, whether the attempt succeeded or failed, never held until Retry
// finishes. The default, 0, gives each attempt the overall context with no
// per-attempt deadline. It applies only to RetryContext; Retry passes no
// context to its operation.
func WithAttemptTimeout(d time.Duration) RetryOption {
	return func(args *retryOptions) {
		args.AttemptTimeout = d
	}
}

// Retry attempts the operation until it succeeds, returns a Permanent error,
// or backoff completes. It ensures the operation is executed at least once.
//
// On success it returns the operation result and a nil error. On any failure
// it returns the last result and a *RetryError whose Cause reports why it
// stopped — ErrPermanent, ErrExhausted, ErrMaxElapsedTime, or the context
// cancellation cause — and whose LastErr holds the last operation error.
// See RetryError and AsRetryError.
//
// ctx bounds the retry loop: its cancellation or deadline stops further
// attempts and interrupts the wait between them. The operation receives no
// context, so capture ctx inside the operation if you want cancellation to
// abort an in-flight attempt. To bound only how long backoff keeps retrying,
// without affecting in-flight attempts, use WithMaxElapsedTime instead.
//
// Use RetryContext to pass a context into the operation and bound each
// attempt independently with WithAttemptTimeout.
func Retry[T any](ctx context.Context, operation Operation[T], opts ...RetryOption) (T, error) {
	return RetryContext(ctx, func(context.Context) (T, error) {
		return operation()
	}, opts...)
}

// RetryContext is Retry for an operation that takes a context. On every
// attempt the operation is called with a fresh child context derived from
// ctx, so the operation (or a context-aware callee) can react to
// cancellation. With WithAttemptTimeout the child also carries a deadline
// scoped to that single attempt.
//
// The child context is cancelled and its timer released as soon as the
// attempt returns, on success as well as failure; it is never kept alive
// until the whole retry ends. Because RetryContext runs the operation
// synchronously, a cancelled attempt cannot leave an old invocation running
// in the background: RetryContext waits for the operation to actually return
// before backing off or trying again.
//
// Cancellation of ctx aborts an in-flight attempt (through its child) and
// stops further attempts and the backoff wait, reported with ctx's Cause.
// A per-attempt timeout instead only fails that attempt; RetryContext
// continues according to the backoff policy, WithMaxTries, and
// WithMaxElapsedTime.
//
// Race between a deadline and a return: a successful attempt wins. If the
// operation returns a nil error, RetryContext returns its result even if the
// attempt's deadline, or ctx, was cancelled at essentially the same time.
// The return value of a completed, synchronous call is the verdict; context
// cancellation only decides what happens to an attempt that observes it and
// comes back with an error.
func RetryContext[T any](ctx context.Context, operation OperationContext[T], opts ...RetryOption) (T, error) {
	// Initialize default retry options.
	args := &retryOptions{
		BackOff:        NewExponentialBackOff(),
		Timer:          &defaultTimer{},
		MaxElapsedTime: DefaultMaxElapsedTime,
	}

	// Apply user-provided options to the default settings.
	for _, opt := range opts {
		opt(args)
	}

	defer args.Timer.Stop()

	startedAt := time.Now()
	args.BackOff.Reset()
	for numTries := uint(1); ; numTries++ {
		// Give every attempt a fresh child context derived from the overall
		// context, so per-attempt cancellation and (with WithAttemptTimeout)
		// the deadline timer never leak across attempts. The cancel runs as
		// soon as this iteration's operation returns — success or failure —
		// not when the whole retry ends.
		attemptCtx, cancelAttempt := newAttemptContext(ctx, args.AttemptTimeout)
		res, err := operation(attemptCtx)
		cancelAttempt()
		if err == nil {
			return res, nil
		}

		// Stop immediately on a permanent error; surface it as a RetryError.
		var perm *permanent
		if errors.As(err, &perm) {
			return res, &RetryError{LastErr: perm.err, Cause: ErrPermanent}
		}

		// A RetryAfterError carries the delay before the next attempt; if it also
		// carries an underlying error, that is the meaningful error to report as
		// LastErr should retrying stop (mirrors how a permanent error surfaces its
		// inner error). errors.As matches it whether returned directly or wrapped.
		// A typed-nil *RetryAfterError also matches; treat it as a plain error.
		lastErr := err
		var retryAfter *RetryAfterError
		if errors.As(err, &retryAfter) && retryAfter != nil && retryAfter.err != nil {
			lastErr = retryAfter.err
		}

		// Stop retrying if maximum tries exceeded.
		if args.MaxTries > 0 && numTries >= args.MaxTries {
			return res, &RetryError{LastErr: lastErr, Cause: ErrExhausted}
		}

		// Stop retrying if context is cancelled.
		if cerr := context.Cause(ctx); cerr != nil {
			return res, &RetryError{LastErr: lastErr, Cause: cerr}
		}

		// Calculate next backoff duration.
		next := args.BackOff.NextBackOff()
		if next == Stop {
			return res, &RetryError{LastErr: lastErr, Cause: ErrExhausted}
		}

		// Reset backoff if a RetryAfterError requested a specific delay.
		if retryAfter != nil {
			next = retryAfter.Duration
			args.BackOff.Reset()
		}

		// A negative delay (e.g. a Retry-After date already in the past) means retry now.
		// Clamp it so it cannot stretch the elapsed-time budget.
		if next < 0 {
			next = 0
		}

		// Stop retrying if maximum elapsed time exceeded.
		if args.MaxElapsedTime > 0 && next > args.MaxElapsedTime-time.Since(startedAt) {
			return res, &RetryError{LastErr: lastErr, Cause: ErrMaxElapsedTime}
		}

		// Notify on error if a notifier function is provided.
		if args.Notify != nil {
			args.Notify(err, next)
		}

		// Wait for the next backoff period or context cancellation.
		args.Timer.Start(next)
		select {
		case <-args.Timer.C():
		case <-ctx.Done():
			return res, &RetryError{LastErr: lastErr, Cause: context.Cause(ctx)}
		}
	}
}

// newAttemptContext derives the context for a single attempt from ctx. It is
// always cancellable so the caller can release it immediately after the
// attempt, and carries a per-attempt deadline when WithAttemptTimeout set a
// positive duration. The returned cancel must be called when the attempt
// returns; because ctx is the parent, cancelling ctx still aborts the
// attempt even if the per-attempt deadline has not fired.
func newAttemptContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}
