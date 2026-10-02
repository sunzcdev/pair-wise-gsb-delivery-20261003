package sasl

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Stable terminal errors reported by a budgeted exchange. They never contain
// exchanged data, in particular no passwords or bearer tokens.
var (
	// ErrBudgetExceeded is returned when an exchange step or byte budget is
	// exceeded.
	ErrBudgetExceeded = errors.New("sasl: exchange budget exceeded")
	// ErrExchangeCanceled is returned when an exchange is canceled via its
	// context.
	ErrExchangeCanceled = errors.New("sasl: exchange canceled")
)

// ExchangeBudget restricts the amount of work a single SASL exchange may
// perform. A field with a value <= 0 means that the corresponding limit is
// not enforced.
type ExchangeBudget struct {
	// MaxSteps is the maximum total number of exchange steps. For a wrapped
	// client Start counts as one step and each subsequent Next call counts as
	// another; for a wrapped server each Next call counts as a step.
	MaxSteps int

	// MaxMessageBytes is the maximum size, in bytes, of a single message
	// received from the peer: a server challenge for a wrapped client, or a
	// client response for a wrapped server.
	MaxMessageBytes int

	// MaxInputBytes is the maximum cumulative number of bytes received from
	// the peer. Zero-length messages contribute zero bytes.
	MaxInputBytes int

	// MaxOutputBytes is the maximum cumulative number of bytes sent to the
	// peer. Output produced by the underlying mechanism that would exceed the
	// budget is truncated before delivery.
	MaxOutputBytes int
}

// BudgetLimit identifies the budget limit that caused an exchange to abort.
type BudgetLimit int

const (
	// LimitCanceled indicates that the exchange was canceled via its context.
	LimitCanceled BudgetLimit = iota
	// LimitSteps indicates that MaxSteps was exceeded.
	LimitSteps
	// LimitMessage indicates that a single message exceeded MaxMessageBytes.
	LimitMessage
	// LimitInput indicates that MaxInputBytes was exceeded.
	LimitInput
	// LimitOutput indicates that output was truncated at MaxOutputBytes.
	LimitOutput
)

// BudgetError is returned by a wrapped Client or Server when the exchange is
// aborted because its budget is exhausted or its context is canceled.
//
// The error only carries numeric accounting information: exchanged bytes
// themselves, including credentials such as passwords or bearer tokens, are
// never included in its message.
type BudgetError struct {
	// Kind is either ErrBudgetExceeded or ErrExchangeCanceled.
	Kind error
	// Limit identifies the limit that was exceeded, or LimitCanceled.
	Limit BudgetLimit
	// Step is the 1-based step at which the exchange aborted.
	Step int
	// Size is the observed size at the violation: the step number for
	// LimitSteps, the message size for LimitMessage, and the cumulative byte
	// count for LimitInput and LimitOutput. It is zero for LimitCanceled.
	Size int
	// LimitValue is the configured value of the exceeded limit. It is zero
	// for LimitCanceled.
	LimitValue int
	// Delivered is the number of bytes actually delivered from an oversized
	// output message. It is non-zero only for LimitOutput, where the beginning
	// of the message is delivered before the exchange becomes terminal.
	Delivered int

	// Err holds the context error when Limit is LimitCanceled.
	Err error
}

func (e *BudgetError) Error() string {
	switch e.Limit {
	case LimitSteps:
		return fmt.Sprintf("sasl: exchange budget exceeded: step %d exceeds limit of %d steps", e.Size, e.LimitValue)
	case LimitMessage:
		return fmt.Sprintf("sasl: exchange budget exceeded: message of %d bytes exceeds limit of %d bytes", e.Size, e.LimitValue)
	case LimitInput:
		return fmt.Sprintf("sasl: exchange budget exceeded: %d input bytes exceed limit of %d bytes", e.Size, e.LimitValue)
	case LimitOutput:
		if e.Delivered > 0 {
			return fmt.Sprintf("sasl: exchange budget exceeded: %d output bytes exceed limit of %d bytes (%d bytes delivered)", e.Size, e.LimitValue, e.Delivered)
		}
		return fmt.Sprintf("sasl: exchange budget exceeded: %d output bytes exceed limit of %d bytes", e.Size, e.LimitValue)
	default:
		if e.Err != nil {
			return "sasl: exchange canceled: " + e.Err.Error()
		}
		return "sasl: exchange canceled"
	}
}

// Is supports errors.Is for ErrBudgetExceeded and ErrExchangeCanceled.
func (e *BudgetError) Is(target error) bool {
	return target == e.Kind
}

// Unwrap returns the context error for a canceled exchange.
func (e *BudgetError) Unwrap() error {
	return e.Err
}

type budgetState struct {
	mu     sync.Mutex
	ctx    context.Context
	budget ExchangeBudget

	steps  int
	input  int
	output int

	// terminal is the first error that aborted the exchange. All later calls
	// return it without entering the underlying mechanism.
	terminal *BudgetError
}

func newBudgetState(ctx context.Context, budget ExchangeBudget) *budgetState {
	if ctx == nil {
		ctx = context.Background()
	}
	return &budgetState{
		ctx:    ctx,
		budget: budget,
	}
}

// begin accounts for one exchange step and one inbound message. It returns
// the 1-based step number. If the exchange must not proceed, it records (if
// needed) and returns the stable terminal error.
func (s *budgetState) begin(msgSize int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.terminal != nil {
		return s.steps, s.terminal
	}

	step := s.steps + 1

	if err := s.ctx.Err(); err != nil {
		s.terminal = &BudgetError{
			Kind:  ErrExchangeCanceled,
			Limit: LimitCanceled,
			Step:  step,
			Err:   err,
		}
		return step, s.terminal
	}

	s.steps = step

	if s.budget.MaxSteps > 0 && step > s.budget.MaxSteps {
		s.terminal = &BudgetError{
			Kind:       ErrBudgetExceeded,
			Limit:      LimitSteps,
			Step:       step,
			Size:       step,
			LimitValue: s.budget.MaxSteps,
		}
		return step, s.terminal
	}

	if s.budget.MaxMessageBytes > 0 && msgSize > s.budget.MaxMessageBytes {
		s.terminal = &BudgetError{
			Kind:       ErrBudgetExceeded,
			Limit:      LimitMessage,
			Step:       step,
			Size:       msgSize,
			LimitValue: s.budget.MaxMessageBytes,
		}
		return step, s.terminal
	}

	s.input += msgSize
	if s.budget.MaxInputBytes > 0 && s.input > s.budget.MaxInputBytes {
		s.terminal = &BudgetError{
			Kind:       ErrBudgetExceeded,
			Limit:      LimitInput,
			Step:       step,
			Size:       s.input,
			LimitValue: s.budget.MaxInputBytes,
		}
		return step, s.terminal
	}

	return step, nil
}

// accountOutput records bytes produced by the underlying mechanism. If the
// output budget is exceeded, the beginning of out is delivered (possibly
// zero bytes) and the exchange becomes terminal. A nil slice is returned
// unchanged so that nil and zero-length outputs remain distinguishable.
func (s *budgetState) accountOutput(step int, out []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.terminal != nil {
		return nil, s.terminal
	}
	if out == nil {
		return nil, nil
	}

	total := s.output + len(out)
	if s.budget.MaxOutputBytes <= 0 || total <= s.budget.MaxOutputBytes {
		s.output = total
		return out, nil
	}

	delivered := s.budget.MaxOutputBytes - s.output
	if delivered < 0 {
		delivered = 0
	}
	truncated := out[:delivered]
	s.output += delivered

	s.terminal = &BudgetError{
		Kind:       ErrBudgetExceeded,
		Limit:      LimitOutput,
		Step:       step,
		Size:       total,
		LimitValue: s.budget.MaxOutputBytes,
		Delivered:  delivered,
	}
	return truncated, s.terminal
}

type budgetClient struct {
	inner Client
	state *budgetState
}

// NewBudgetClient wraps a Client so that its exchange is restricted by budget
// and can be canceled through ctx. A nil ctx is treated as context.Background.
//
// The underlying client is not entered once the budget is exhausted or the
// context is canceled: further calls keep returning the same stable terminal
// error. Oversized initial responses or challenge responses produced by the
// underlying client are truncated to the remaining output budget and reported
// in the error. A nil initial response is kept distinct from a zero-length
// initial response. Cancellation is observed at the start of each step; a
// call already executing inside the underlying client is not interrupted.
func NewBudgetClient(ctx context.Context, budget ExchangeBudget, inner Client) Client {
	return &budgetClient{
		inner: inner,
		state: newBudgetState(ctx, budget),
	}
}

func (c *budgetClient) Start() (mech string, ir []byte, err error) {
	step, berr := c.state.begin(0)
	if berr != nil {
		return "", nil, berr
	}

	mech, ir, err = c.inner.Start()
	if err != nil {
		return mech, nil, err
	}

	out, berr := c.state.accountOutput(step, ir)
	if berr != nil {
		return mech, out, berr
	}
	return mech, out, nil
}

func (c *budgetClient) Next(challenge []byte) (response []byte, err error) {
	step, berr := c.state.begin(len(challenge))
	if berr != nil {
		return nil, berr
	}

	response, err = c.inner.Next(challenge)
	if err != nil {
		return nil, err
	}

	return c.state.accountOutput(step, response)
}

type budgetServer struct {
	inner Server
	state *budgetState
}

// NewBudgetServer wraps a Server so that its exchange is restricted by budget
// and can be canceled through ctx. A nil ctx is treated as context.Background.
//
// The underlying server is not entered once the budget is exhausted or the
// context is canceled: further calls keep returning the same stable terminal
// error. Oversized challenges produced by the underlying server are truncated
// to the remaining output budget and reported in the error. Nil responses and
// zero-length responses remain distinguishable. Cancellation is observed at
// the start of each step; a call already executing inside the underlying
// server is not interrupted.
func NewBudgetServer(ctx context.Context, budget ExchangeBudget, inner Server) Server {
	return &budgetServer{
		inner: inner,
		state: newBudgetState(ctx, budget),
	}
}

func (s *budgetServer) Next(response []byte) (challenge []byte, done bool, err error) {
	step, berr := s.state.begin(len(response))
	if berr != nil {
		return nil, false, berr
	}

	challenge, done, err = s.inner.Next(response)
	if err != nil {
		return nil, done, err
	}

	out, berr := s.state.accountOutput(step, challenge)
	if berr != nil {
		return out, done, berr
	}
	return out, done, nil
}
