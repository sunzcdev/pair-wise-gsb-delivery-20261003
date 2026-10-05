package gojq

import "strconv"

// RunOption is an option for running a query.
// It is accepted by [*Query.Run], [*Query.RunWithContext],
// [*Code.RunWithOptions], and [*Code.RunWithContextOptions].
type RunOption func(*runOptions)

type runOptions struct {
	budget *InstructionBudget
}

// WithInstructionLimit is a [RunOption] to limit the number of VM
// instructions executed in a single run.
//
// A new [InstructionBudget] is created for every run, so the limit applies
// to the entire execution of that run; it is not reset each time the result
// iterator emits a value. The limit accounts for backtracking, recursion and
// repeated calls of the result iterator.
//
// When the number of executed instructions reaches the limit, the iterator
// emits an [*InstructionLimitError], discards the pending backtracking
// forks and terminates; the error is not caught by try-catch.
//
// Executing internals of built-in functions and functions added by
// [WithFunction] or [WithIterFunction] does not consume instructions.
// Such functions can consume the budget explicitly using
// [InstructionBudget.Charge] when the same budget is provided via
// [WithInstructionBudget].
//
// The limit must not be negative; zero aborts the query on its first
// instruction.
func WithInstructionLimit(limit uint64) RunOption {
	return func(o *runOptions) {
		o.budget = NewInstructionBudget(limit)
	}
}

// WithInstructionBudget is a [RunOption] to set an [InstructionBudget] for a
// run. Unlike [WithInstructionLimit], the same budget can be shared across
// runs and with custom functions, which can consume it using
// [InstructionBudget.Charge].
func WithInstructionBudget(budget *InstructionBudget) RunOption {
	return func(o *runOptions) {
		o.budget = budget
	}
}

// InstructionBudget counts VM instructions executed during runs.
//
// It is consumed one instruction at a time by the VM; the internal work of
// built-in functions and custom Go functions is not counted automatically.
// Custom functions can consume the budget explicitly with [Charge].
//
// A budget is not safe for concurrent use; create one budget per run, or
// synchronize access when sharing it between goroutines.
type InstructionBudget struct {
	limit uint64
	used  uint64
}

// NewInstructionBudget creates an [InstructionBudget] with the given
// instruction limit. The limit must not be negative.
func NewInstructionBudget(limit uint64) *InstructionBudget {
	return &InstructionBudget{limit: limit}
}

// Charge consumes n instructions from the budget.
//
// The argument n must not be negative; Charge panics on a negative value.
// If the charge makes the used count reach the limit, Charge returns an
// [*InstructionLimitError]. Returning this error from a custom function
// (or emitting it from an iterator function) aborts the whole run: the
// virtual machine discards pending forks and terminates the iterator
// without being caught by try-catch.
//
// When the budget is nil, Charge does nothing and returns nil; custom
// functions do not need to care whether the host set a budget or not.
func (b *InstructionBudget) Charge(n uint64) error {
	if b == nil {
		return nil
	}
	if b.used >= b.limit {
		return &InstructionLimitError{limit: b.limit, used: b.used}
	}
	b.used += n
	if b.used > b.limit {
		b.used = b.limit
	}
	if b.used == b.limit {
		return &InstructionLimitError{limit: b.limit, used: b.used}
	}
	return nil
}

// Used returns the number of instructions consumed so far.
func (b *InstructionBudget) Used() uint64 {
	if b == nil {
		return 0
	}
	return b.used
}

// Limit returns the instruction limit.
func (b *InstructionBudget) Limit() uint64 {
	if b == nil {
		return 0
	}
	return b.limit
}

// Remaining returns the number of instructions left before the limit.
func (b *InstructionBudget) Remaining() uint64 {
	if b == nil {
		return 0
	}
	return b.limit - b.used
}

// tick consumes a single VM instruction and reports whether the limit is
// reached.
func (b *InstructionBudget) tick() bool {
	if b == nil || b.used < b.limit {
		if b != nil {
			b.used++
		}
		return false
	}
	return true
}

// InstructionLimitError is emitted by the iterator when the number of
// executed VM instructions reaches the limit set by [WithInstructionLimit]
// or [WithInstructionBudget].
//
// This is a host error; it propagates through the pending backtracking
// forks and is not caught by try-catch (try EXP catch ... and //
// alternative). After the error, the iterator terminates. Ordinary errors
// raised by the query or custom functions are still caught by try-catch.
type InstructionLimitError struct {
	limit uint64
	used  uint64
}

// Error implements the error interface.
func (err *InstructionLimitError) Error() string {
	return "instruction limit exceeded: " +
		strconv.FormatUint(err.used, 10) + "/" + strconv.FormatUint(err.limit, 10)
}

// Limit returns the configured instruction limit.
func (err *InstructionLimitError) Limit() uint64 {
	return err.limit
}

// Used returns the number of instructions consumed when the limit was
// reached; it equals [InstructionLimitError.Limit].
func (err *InstructionLimitError) Used() uint64 {
	return err.used
}
