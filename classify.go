package backoff

// ErrorClass is the comparable key a classifier uses to name a failure kind.
// It may be a string, an int, a typed enum, or any other comparable type; the
// same value must identify the same kind of failure across calls.
type ErrorClass = any

// Classifier maps an operation error to a failure class together with whether
// the failure may be retried. It is supplied by the caller of WithClassifiedBackOff.
//
// The classifier decides only two things: whether to retry, and which class's
// BackOff paces the retry. It never rewraps or replaces the error: the original
// error chain is returned untouched as RetryError.LastErr.
//
// Return retry=false to stop immediately, exactly like a Permanent error. A
// Permanent error is detected before the classifier runs and always wins, so it
// need not be classified.
//
// The classifier is not called for a nil error, and it is called only for a
// failure that would otherwise be retried — after the MaxTries, elapsed-time,
// and context limits have already had their say.
//
// The returned class is used as a map key, so it must be comparable.
type Classifier func(err error) (class ErrorClass, retry bool)

// ClassBackOffFunc builds a fresh BackOff for one failure class. It is invoked
// the first time that class is seen during a single Retry call; the returned
// BackOff is then reused for every subsequent failure of that class within that
// call.
//
// Because the function runs once per class per Retry call, two concurrent Retry
// calls never share a BackOff or any mutable pacing state.
//
// It must return a non-nil BackOff; to forbid retries for a class, either have
// the Classifier return retry=false for it or return a BackOff whose
// NextBackOff returns Stop.
type ClassBackOffFunc func(class ErrorClass) BackOff

// RouteOption configures how a Classifier and a per-class BackOff source behave
// together under WithClassifiedBackOff.
type RouteOption func(*classifiedRoute)

// WithResetOnSwitch makes a class's backoff sequence restart from its first
// interval whenever that class becomes active after a different class failed.
//
// Without it (the default), each class keeps its own sequence across switches:
// the BackOff a class resumes with is the one it would have produced next had
// no other class interrupted it.
//
// Each Retry call starts with fresh BackOffs, so every class's sequence begins
// at its initial interval on the first failure of that class.
func WithResetOnSwitch() RouteOption {
	return func(r *classifiedRoute) { r.resetOnSwitch = true }
}

// WithClassifiedBackOff routes each failure to an independent BackOff chosen by
// the error's class. classify is the caller's classifier: it names the failure
// class and decides whether the failure is retriable. newBackOff supplies a
// distinct BackOff for each class, created lazily on first use.
//
// Classification affects only whether Retry retries and the pace at which it
// does so; it never changes the original error, which is preserved as
// RetryError.LastErr. A class the classifier marks as non-retriable stops Retry
// with Cause ErrPermanent, and a class BackOff that returns Stop stops it with
// Cause ErrExhausted.
//
// Each class owns its BackOff and its place in the backoff sequence. When the
// active class changes between two failures the new class resumes where its own
// sequence left off; pass WithResetOnSwitch to instead reset the newly active
// class to its initial interval. On a retriable error a RetryAfterError
// overrides the wait for that single retry and resets the active class's
// BackOff.
//
// When set, this option takes precedence over WithBackOff.
//
// Every Retry call invokes newBackOff for itself, so two concurrent calls that
// share these options do not share any BackOff or other mutable state.
//
// Precedence, highest first:
//
//   - success ends Retry;
//   - a Permanent error stops Retry at once, before the classifier runs;
//   - the classifier decides retry or stop and selects the pacing class. A
//     retry=false verdict stops Retry even if the error wraps a
//     RetryAfterError: RetryAfter requests a delay, it does not grant a retry;
//   - the selected class's BackOff paces the retry, and Stop from it stops
//     Retry with ErrExhausted;
//   - a RetryAfterError on a retriable error overrides that single wait and
//     resets the active class's BackOff;
//   - WithMaxTries, WithMaxElapsedTime, and ctx apply unchanged.
func WithClassifiedBackOff(classify Classifier, newBackOff ClassBackOffFunc, opts ...RouteOption) RetryOption {
	return func(args *retryOptions) {
		r := &classifiedRoute{classify: classify, newBackOff: newBackOff}
		for _, opt := range opts {
			opt(r)
		}
		args.route = r
	}
}

// classifiedRoute holds the configuration and per-Retry state for
// WithClassifiedBackOff. A new value is built for every Retry call from the
// option's closures, so concurrent calls never share it.
type classifiedRoute struct {
	classify      Classifier
	newBackOff    ClassBackOffFunc
	resetOnSwitch bool

	active    ErrorClass
	hasActive bool
	policies  map[ErrorClass]BackOff
}

// backOffFor returns the BackOff that paces a retry of err, or nil if the
// classifier says the error is not retriable. It creates the class's BackOff on
// first use and applies the retain/reset-on-switch rule.
func (r *classifiedRoute) backOffFor(err error) BackOff {
	class, retry := r.classify(err)
	if !retry {
		return nil
	}

	if r.policies == nil {
		r.policies = make(map[ErrorClass]BackOff)
	}
	b, ok := r.policies[class]
	if !ok {
		b = r.newBackOff(class)
		r.policies[class] = b
	}

	if r.hasActive && class != r.active && r.resetOnSwitch {
		b.Reset()
	}
	r.active = class
	r.hasActive = true

	return b
}

// resetActive resets only the currently active class's BackOff, used when a
// RetryAfterError requests a specific delay.
func (r *classifiedRoute) resetActive() {
	if r.hasActive {
		r.policies[r.active].Reset()
	}
}
