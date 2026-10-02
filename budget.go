package sasl

import (
	"context"
	"errors"
)

// Common errors returned by budget-limited clients and servers. They are
// stable sentinel values and never contain exchanged data such as passwords
// or bearer tokens.
var (
	// ErrBudgetSteps is returned when the maximum number of exchange steps
	// has been reached.
	ErrBudgetSteps = errors.New("sasl: exchange step budget exhausted")
	// ErrBudgetMessage is returned when a single incoming message exceeds
	// the per-message size budget.
	ErrBudgetMessage = errors.New("sasl: incoming message exceeds per-message size budget")
	// ErrBudgetInput is returned when the cumulative size of incoming
	// messages exceeds the input byte budget.
	ErrBudgetInput = errors.New("sasl: cumulative input byte budget exhausted")
	// ErrBudgetOutput is returned when the cumulative size of outgoing
	// messages exceeds the output byte budget. The offending message is
	// truncated to fit the remaining budget before being delivered.
	ErrBudgetOutput = errors.New("sasl: cumulative output byte budget exhausted, output truncated")
)

// Budget describes resource limits enforced on a SASL exchange. A zero or
// negative value disables the corresponding limit.
type Budget struct {
	// MaxSteps limits the total number of exchange steps. Each Start and
	// Next call counts as one step.
	MaxSteps int
	// MaxMessageBytes limits the size of a single incoming message (a
	// challenge for clients, a response for servers).
	MaxMessageBytes int
	// MaxInputBytes limits the cumulative size of all incoming messages.
	MaxInputBytes int
	// MaxOutputBytes limits the cumulative size of all outgoing messages.
	// An outgoing message that would exceed the remaining budget is
	// truncated and ErrBudgetOutput is returned.
	MaxOutputBytes int
}

// budgetState tracks consumption of a Budget. Once a limit is hit or the
// context is canceled, err latches and all subsequent calls fail with the
// same error without touching the underlying mechanism.
type budgetState struct {
	budget Budget
	ctx    context.Context

	steps  int
	input  int
	output int
	err    error
}

// begin accounts for one exchange step with an incoming message of n bytes.
func (s *budgetState) begin(n int) error {
	if s.err != nil {
		return s.err
	}
	if s.ctx != nil {
		if err := s.ctx.Err(); err != nil {
			s.err = err
			return s.err
		}
	}
	if s.budget.MaxSteps > 0 && s.steps >= s.budget.MaxSteps {
		s.err = ErrBudgetSteps
		return s.err
	}
	s.steps++
	if s.budget.MaxMessageBytes > 0 && n > s.budget.MaxMessageBytes {
		s.err = ErrBudgetMessage
		return s.err
	}
	if s.budget.MaxInputBytes > 0 && s.input+n > s.budget.MaxInputBytes {
		s.err = ErrBudgetInput
		return s.err
	}
	s.input += n
	return nil
}

// end accounts for an outgoing message produced by the underlying
// mechanism, truncating it to the remaining output budget if needed. A nil
// message stays nil and a non-nil message stays non-nil, even when
// truncated to zero bytes.
func (s *budgetState) end(out []byte) ([]byte, error) {
	if s.budget.MaxOutputBytes > 0 && s.output+len(out) > s.budget.MaxOutputBytes {
		remaining := s.budget.MaxOutputBytes - s.output
		if remaining < 0 {
			remaining = 0
		}
		s.output = s.budget.MaxOutputBytes
		s.err = ErrBudgetOutput
		if out == nil {
			return nil, s.err
		}
		return out[:remaining], s.err
	}
	s.output += len(out)
	return out, nil
}

type budgetClient struct {
	Client
	state *budgetState
}

func (c *budgetClient) Start() (mech string, ir []byte, err error) {
	if err := c.state.begin(0); err != nil {
		return "", nil, err
	}
	mech, ir, err = c.Client.Start()
	if err != nil {
		return "", nil, err
	}
	ir, err = c.state.end(ir)
	return mech, ir, err
}

func (c *budgetClient) Next(challenge []byte) (response []byte, err error) {
	if err := c.state.begin(len(challenge)); err != nil {
		return nil, err
	}
	response, err = c.Client.Next(challenge)
	if err != nil {
		return nil, err
	}
	return c.state.end(response)
}

// NewBudgetClient wraps c so that the SASL exchange cannot exceed the given
// budget. If ctx is canceled, the exchange aborts. Once the budget is
// exhausted or the context is canceled, all subsequent calls return the
// same terminal error without invoking the underlying client.
func NewBudgetClient(c Client, budget Budget, ctx context.Context) Client {
	return &budgetClient{Client: c, state: &budgetState{budget: budget, ctx: ctx}}
}

type budgetServer struct {
	Server
	state *budgetState
}

func (s *budgetServer) Next(response []byte) (challenge []byte, done bool, err error) {
	if err := s.state.begin(len(response)); err != nil {
		return nil, false, err
	}
	challenge, done, err = s.Server.Next(response)
	if err != nil {
		return nil, done, err
	}
	challenge, err = s.state.end(challenge)
	return challenge, done, err
}

// NewBudgetServer wraps s so that the SASL exchange cannot exceed the given
// budget. If ctx is canceled, the exchange aborts. Once the budget is
// exhausted or the context is canceled, all subsequent calls return the
// same terminal error without invoking the underlying server.
func NewBudgetServer(s Server, budget Budget, ctx context.Context) Server {
	return &budgetServer{Server: s, state: &budgetState{budget: budget, ctx: ctx}}
}
