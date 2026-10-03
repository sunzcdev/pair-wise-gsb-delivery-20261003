package milter

import (
	"errors"
	"fmt"
)

// Default budgets applied by NewResultCollector when CollectorOptions fields
// are left at zero.
const (
	// DefaultMaxCollectedActions is the default limit on the number of
	// buffered modify actions.
	DefaultMaxCollectedActions = 1024
	// DefaultMaxCollectedBytes is the default limit on the total size of
	// buffered modify action payloads.
	DefaultMaxCollectedBytes = 32 << 20 // 32 MiB
)

var (
	// ErrTooManyActions is recorded in CollectedResult.Err when the number of
	// modify actions sent by the milter exceeds the collector budget.
	ErrTooManyActions = errors.New("milter: collector: too many modify actions")
	// ErrTooManyBytes is recorded in CollectedResult.Err when the total size
	// of modify action payloads exceeds the collector budget.
	ErrTooManyBytes = errors.New("milter: collector: modify actions byte budget exceeded")
)

// CollectorOptions configures budgets of a ResultCollector.
//
// The collector is a purely in-memory structure; it is not backed by a
// database or disk, so budgets are the only protection against misbehaving
// or malicious milters.
type CollectorOptions struct {
	// MaxActions limits the number of buffered modify actions.
	// Zero means DefaultMaxCollectedActions, negative means no limit.
	MaxActions int
	// MaxBytes limits the total size of buffered action payloads (recipient
	// addresses, header names and values, replacement body chunks, envelope
	// sender and quarantine reason strings).
	// Zero means DefaultMaxCollectedBytes, negative means no limit.
	MaxBytes int
}

// ResultCollector buffers modify actions received in response to the EOB
// message and delivers them to the caller atomically: buffered actions become
// observable through Result only if the milter transaction ends with a
// successful terminal action (ActAccept or ActContinue).
//
// Side-effect boundary: while collecting, no action is exposed to the caller,
// so no caller-visible side effect happens before the final verdict is known.
// If the message is rejected, a budget is exceeded, an illegal action is
// received or the connection breaks mid-stream, the result is uncommitted and
// no buffered action is delivered - the caller does not need to roll anything
// back.
//
// A ResultCollector must not be reused across messages; create a new one for
// every EndWithOptions call.
type ResultCollector struct {
	maxActions int
	maxBytes   int

	actions []ModifyAction
	bytes   int
	err     error
	final   *Action
}

// NewResultCollector creates a ResultCollector with the given budgets.
func NewResultCollector(opts CollectorOptions) *ResultCollector {
	if opts.MaxActions == 0 {
		opts.MaxActions = DefaultMaxCollectedActions
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = DefaultMaxCollectedBytes
	}
	return &ResultCollector{
		maxActions: opts.MaxActions,
		maxBytes:   opts.MaxBytes,
	}
}

// CollectedResult is the outcome of a collected EOB transaction.
type CollectedResult struct {
	// Committed is true only if the milter finished the transaction with a
	// successful terminal action (ActAccept or ActContinue) and no budget
	// was exceeded, no illegal action was received and the connection held.
	Committed bool

	// Actions contains buffered modify actions in the exact order they were
	// received, but only when Committed is true. It is nil otherwise:
	// an uncommitted result never delivers actions.
	//
	// Actions are preserved verbatim. In particular, HeaderIndex of
	// ActChangeHeader and ActInsertHeader actions keeps its protocol
	// meaning: a 1-based, per-header-name index into the header fields sent
	// with HeaderField.
	Actions []ModifyAction

	// Final is the terminal action sent by the milter. It is nil if the
	// connection broke before any terminal action was received.
	Final *Action

	// Err explains why the result is not committed: budget overflow
	// (ErrTooManyActions or ErrTooManyBytes), a malformed action or a
	// connection error. It is nil for a clean rejection, where Final alone
	// carries the verdict.
	Err error
}

// Result returns the collected outcome. It is only meaningful after the
// EndWithOptions call the collector was passed to has returned.
func (c *ResultCollector) Result() CollectedResult {
	res := CollectedResult{
		Final: c.final,
		Err:   c.err,
	}
	if c.final != nil && c.err == nil &&
		(c.final.Code == ActAccept || c.final.Code == ActContinue) {
		res.Committed = true
		res.Actions = c.actions
	}
	return res
}

// modifyActionSize approximates the in-memory footprint of an action payload
// for budgeting purposes.
func modifyActionSize(act *ModifyAction) int {
	size := len(act.Rcpt) + len(act.From) + len(act.Body) +
		len(act.HeaderName) + len(act.HeaderValue) + len(act.Reason)
	for _, arg := range act.FromArgs {
		size += len(arg)
	}
	return size
}

// add buffers one action. Budget violations are recorded but not returned:
// the read loop keeps draining the connection so the session stays in sync,
// and the overflow surfaces later as an uncommitted result.
func (c *ResultCollector) add(act ModifyAction) {
	if c.err != nil {
		return
	}
	if c.maxActions >= 0 && len(c.actions) >= c.maxActions {
		c.err = ErrTooManyActions
		return
	}
	size := modifyActionSize(&act)
	if c.maxBytes >= 0 && c.bytes+size > c.maxBytes {
		c.err = fmt.Errorf("%w (%d + %d > %d)", ErrTooManyBytes, c.bytes, size, c.maxBytes)
		return
	}
	c.actions = append(c.actions, act)
	c.bytes += size
}

// commit records the terminal action of the transaction.
func (c *ResultCollector) commit(final *Action) {
	c.final = final
}

// fail records a terminal failure (malformed action, connection loss).
func (c *ResultCollector) fail(err error) {
	if c.err == nil {
		c.err = err
	}
}
