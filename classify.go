package backoff

import "time"

// Classifier maps an operation error to a retry category. Categories are
// caller-defined strings, e.g. "conn" for connection errors and "rate" for
// rate-limit errors. It is consulted once per failed attempt, only after the
// error is known to be non-permanent.
//
// Classification only decides whether to retry and how long to wait. It never
// alters the error itself: Notify and RetryError.LastErr always see the
// original operation error, and errors.Is/errors.As behave as without
// classification.
type Classifier func(err error) string

// WithClassifiedBackOff routes each failed attempt to an independent BackOff
// chosen by the classifier, so different failure types keep separate retry
// rhythms — a connection error and a rate-limit error advance their own
// sequences instead of sharing one. It takes precedence over WithBackOff.
//
// factory creates the BackOff for a category the first time that category is
// seen. It must return a fresh instance per call: every Retry call builds its
// own set of policies through the factory, so two concurrent Retry calls
// never share mutable backoff state. A nil policy means the category is not
// retried: Retry stops with a *RetryError whose Cause is ErrExhausted.
//
// Decision order for a failed attempt, highest priority first:
//
//  1. A Permanent error stops Retry immediately (ErrPermanent); the
//     classifier is never consulted for it.
//  2. WithMaxTries and context cancellation stop Retry before any policy
//     is consulted.
//  3. The classifier routes the error to its category policy; that policy
//     decides whether to retry at all (Stop or a nil policy ends Retry
//     with ErrExhausted) and how long to wait.
//  4. A RetryAfterError overrides the computed delay with its own Duration
//     and resets the current category's sequence; other categories are
//     untouched.
//  5. WithMaxElapsedTime gates the resulting wait as usual.
//
// By default each category's sequence is kept when the category changes:
// with the error sequence A, A, B, A, the last A continues A's sequence
// (its third wait). WithResetOnSwitch(true) instead resets a category's
// policy when switching away from it, so the last A restarts A's sequence
// (its first wait).
func WithClassifiedBackOff(c Classifier, factory func(category string) BackOff, opts ...ClassifiedOption) RetryOption {
	return func(args *retryOptions) {
		args.Classifier = c
		args.BackOffFactory = factory
		for _, opt := range opts {
			opt(args)
		}
	}
}

// ClassifiedOption configures classified routing. Pass them to
// WithClassifiedBackOff.
type ClassifiedOption func(*retryOptions)

// WithResetOnSwitch controls what happens to a category's backoff sequence
// when a failure switches to a different category. The default, false, keeps
// every category's sequence across interleaved failures. When true, the
// previous category's policy is reset on each switch, so returning to a
// category restarts its sequence from the beginning.
func WithResetOnSwitch(reset bool) ClassifiedOption {
	return func(args *retryOptions) {
		args.ResetOnSwitch = reset
	}
}

// categoryRouter holds the per-Retry-call routing state: the policy of each
// category seen so far and the category of the latest failure. A new router
// is created for every Retry call, so concurrent calls share nothing.
type categoryRouter struct {
	classifier    Classifier
	factory       func(category string) BackOff
	resetOnSwitch bool
	policies      map[string]BackOff
	current       string
	hasCurrent    bool
}

func newCategoryRouter(c Classifier, factory func(category string) BackOff, resetOnSwitch bool) *categoryRouter {
	return &categoryRouter{
		classifier:    c,
		factory:       factory,
		resetOnSwitch: resetOnSwitch,
		policies:      make(map[string]BackOff),
	}
}

// NextBackOff classifies err and returns the next delay of its category's
// policy, or Stop if the category has no policy or its policy is done.
func (r *categoryRouter) NextBackOff(err error) time.Duration {
	category := r.classifier(err)
	if r.hasCurrent && category != r.current && r.resetOnSwitch {
		r.policies[r.current].Reset()
	}
	r.current, r.hasCurrent = category, true

	policy, ok := r.policies[category]
	if !ok {
		policy = r.factory(category)
		if policy == nil {
			return Stop
		}
		policy.Reset()
		r.policies[category] = policy
	}
	return policy.NextBackOff()
}

// ResetCurrent resets the policy of the latest failure's category, used when
// a RetryAfterError takes over the delay. Other categories keep their state.
func (r *categoryRouter) ResetCurrent() {
	if r.hasCurrent {
		if policy, ok := r.policies[r.current]; ok {
			policy.Reset()
		}
	}
}
