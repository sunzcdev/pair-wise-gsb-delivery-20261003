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

// OperationCtx is the function RetryCtx calls. The context passed in is a fresh
// child of the context given to RetryCtx, created separately for each attempt.
// It is cancelled as soon as the attempt ends — never reuse it after the
// operation returns — and, with WithAttemptTimeout, when the attempt's own
// deadline is reached.
//
// The retry loop is synchronous: the next attempt does not start until the
// previous operation call has returned. When the attempt context ends mid-call
// the operation should observe ctx.Done (or the context error from the call it
// is making) and return promptly; a synchronous function that ignores its
// context cannot be interrupted, so an attempt timeout cannot fire until it
// returns.
type OperationCtx[T any] func(ctx context.Context) (T, error)

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
	AttemptTimeout time.Duration // Per-attempt timeout; only used by RetryCtx.
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

// WithAttemptTimeout bounds the duration of a single attempt in RetryCtx.
// Each attempt receives a fresh child context derived from the context passed
// to RetryCtx with this deadline; the child is cancelled and its timer
// released as soon as the attempt ends, including on success, so timing
// resources are not held for the whole RetryCtx call.
//
// A single attempt reaching this timeout is a normal failure: the attempt's
// context cause is ErrAttemptTimeout, and retrying proceeds according to the
// backoff policy and the other limits (WithMaxTries, WithMaxElapsedTime),
// exactly as for any other error. The timeout does not cancel the parent
// context, so later attempts still get the full timeout.
//
// If the parent context ends while an attempt is running, it wins over the
// attempt timeout: the child reports the parent's cancellation cause, RetryCtx
// stops immediately, and no further attempt is scheduled.
//
// WithAttemptTimeout only affects RetryCtx: Retry's operation takes no context
// and cannot observe it. The default, 0, gives each attempt a cancellable
// child of the parent context without a deadline of its own.
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
func Retry[T any](ctx context.Context, operation Operation[T], opts ...RetryOption) (T, error) {
	return retryLoop(ctx, func(context.Context) (T, error) {
		return operation()
	}, opts...)
}

// RetryCtx is Retry for an operation that accepts a context. A fresh child
// context is derived from ctx for every attempt and handed to operation; it is
// cancelled and its timing resources are released as soon as that attempt
// returns, before the backoff wait and before any further attempt.
//
// With WithAttemptTimeout, each attempt's child context also carries an
// independent timeout measured from the start of that attempt; the retry loop
// itself is otherwise identical to Retry's. A timed-out attempt is retried
// per the policy, while cancellation of ctx ends the whole retry: the wait is
// interrupted and no further attempt runs.
//
// Adjudication when a deadline and an attempt ending nearly coincide:
//   - If the operation returns a nil error, the result is accepted and
//     RetryCtx returns it, even if the attempt context has already fired. A
//     value produced by the operation is never discarded as timed out.
//   - If the operation returns an error, the parent ctx is checked first. If
//     it has ended, RetryCtx stops with its cause (context.Cause(ctx)) — a
//     parent cancellation outranks both the attempt timeout and the operation
//     error. Otherwise an attempt whose own timeout fired is a regular
//     retriable failure whose context cause is ErrAttemptTimeout.
func RetryCtx[T any](ctx context.Context, operation OperationCtx[T], opts ...RetryOption) (T, error) {
	return retryLoop(ctx, operation, opts...)
}

// attemptContext derives the context for one attempt. With an attempt timeout
// it carries an independent deadline whose cause is ErrAttemptTimeout;
// otherwise it is a plain cancellable child. The returned cancel must be
// invoked as soon as the attempt ends, so a timeout timer never lives past the
// attempt that owns it.
func attemptContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeoutCause(parent, timeout, ErrAttemptTimeout)
	}
	return context.WithCancel(parent)
}

// retryLoop drives the attempts. The operation is invoked synchronously, one
// attempt at a time, with a freshly derived context that is cancelled before
// the loop waits or moves on.
func retryLoop[T any](ctx context.Context, operation OperationCtx[T], opts ...RetryOption) (T, error) {
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
		// Execute the operation under a fresh per-attempt context. The closure
		// scopes cancel (and, with a timeout, its timer) to this attempt: defer
		// runs as soon as the attempt returns, not when the retry finishes, so
		// the timing resource is released on every path, including success.
		res, err := func() (T, error) {
			attemptCtx, cancel := attemptContext(ctx, args.AttemptTimeout)
			defer cancel()
			return operation(attemptCtx)
		}()
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

		// Parent cancellation outranks the per-attempt timeout and any other
		// failure: when ctx has ended the whole retry stops here. Checking it
		// before treating a fired attempt timeout (ErrAttemptTimeout) as a
		// retriable error ensures an overall cancellation never schedules
		// another attempt.
		if cerr := context.Cause(ctx); cerr != nil {
			return res, &RetryError{LastErr: lastErr, Cause: cerr}
		}

		// Stop retrying if maximum tries exceeded.
		if args.MaxTries > 0 && numTries >= args.MaxTries {
			return res, &RetryError{LastErr: lastErr, Cause: ErrExhausted}
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
