package gojq

import "context"

// InstructionLimitError is an error emitted when the optional VM instruction
// limit of a run is exhausted. It is a host limit, not a query error, so it is
// never caught by try-catch; the query cannot continue running after it.
//
// The iterator emits this error once and then terminates: every further call
// of [Iter.Next] returns false. Emitted values produced before the limit was
// exhausted are already available to the caller.
type InstructionLimitError struct {
	// Limit is the configured number of VM instructions per run.
	Limit int
}

func (*InstructionLimitError) Error() string {
	return "instruction limit exceeded"
}

// InstructionMeter is the interface passed to custom functions registered with
// [WithMeteredFunction] and [WithMeteredIterFunction].
//
// Built-in and custom Go functions do not consume VM instructions automatically
// for the work they do inside Go; use Charge to deduct instructions for the
// estimated or actual cost of the work. Charge returns false when the run
// instruction limit has been exhausted, in which case the query is terminated
// with an [*InstructionLimitError] that try-catch cannot suppress.
//
// When the run has no instruction limit, Charge always returns true.
type InstructionMeter interface {
	Charge(n int) bool
}

// RunOption is an option for [*Code.RunWithOptions].
type RunOption func(*runConfig)

type runConfig struct {
	ctx       context.Context
	instLimit int
}

// WithInstructionLimit is a [RunOption] that limits the number of VM
// instructions executed during a single run. The budget is shared across the
// whole iteration; it is not reset after each emitted value. It covers branch
// backtracking, recursion and multiple [Iter.Next] calls on the same
// iterator.
//
// The work done inside built-in functions and custom Go functions is not
// counted automatically; such functions can deduct instructions with
// [InstructionMeter] by registering them with [WithMeteredFunction] or
// [WithMeteredIterFunction].
//
// When the budget is exhausted, the iterator emits an [*InstructionLimitError],
// which try-catch cannot catch, and then terminates. A negative value disables
// the limit; zero allows no instructions.
func WithInstructionLimit(limit int) RunOption {
	return func(c *runConfig) {
		c.instLimit = limit
	}
}

// WithRunContext is a [RunOption] that sets the context used by
// [*Code.RunWithOptions], just like [*Code.RunWithContext].
func WithRunContext(ctx context.Context) RunOption {
	return func(c *runConfig) {
		c.ctx = ctx
	}
}

// RunWithOptions runs the code with run options. The values are the variable
// values, in the same order as the variables given to [WithVariables]. Use
// [WithInstructionLimit] to limit the number of VM instructions.
//
// It is safe to call this method in goroutines, to reuse a compiled [*Code].
func (c *Code) RunWithOptions(v any, values []any, options ...RunOption) Iter {
	cfg := &runConfig{
		ctx:       context.Background(),
		instLimit: -1,
	}
	for _, opt := range options {
		opt(cfg)
	}
	if cfg.ctx == nil {
		cfg.ctx = context.Background()
	}
	if len(values) > len(c.variables) {
		return NewIter(&tooManyVariableValuesError{})
	} else if len(values) < len(c.variables) {
		return NewIter(&expectedVariableError{c.variables[len(values)]})
	}
	env := newEnv(cfg.ctx)
	env.instLimit = cfg.instLimit
	env.instLeft = cfg.instLimit
	return env.execute(c, v, values...)
}
