package gojq

import "context"

type env struct {
	pc        int
	stack     *stack
	paths     *stack
	scopes    *scopeStack
	values    []any
	codes     []*code
	codeinfos []codeinfo
	forks     []fork
	backtrack bool
	offset    int
	expdepth  int
	label     int
	args      [32]any // len(env.args) > maxarity
	ctx       context.Context
	instLimit int
	instLeft  int
}

func newEnv(ctx context.Context) *env {
	return &env{
		stack:  newStack(),
		paths:  newStack(),
		scopes: newScopeStack(),
		ctx:    ctx,
		// A negative value means that the instruction limit is disabled.
		instLimit: -1,
		instLeft:  -1,
	}
}

// chargeInstruction consumes one VM instruction from the budget.
// It returns false when the instruction limit has been exhausted.
func (env *env) chargeInstruction() bool {
	if env.instLimit < 0 {
		return true
	}
	env.instLeft--
	return env.instLeft >= 0
}

// Charge implements [InstructionMeter] for custom iterator functions.
// It deducts n VM instructions from the run budget and returns false once
// the limit has been exhausted. When the run has no instruction limit, it
// always returns true.
func (env *env) Charge(n int) bool {
	if env.instLimit < 0 {
		return true
	}
	if n < 0 {
		n = 0
	}
	env.instLeft -= n
	return env.instLeft >= 0
}

type scope struct {
	id         int
	offset     int
	pc         int
	saveindex  int
	outerindex int
}

type fork struct {
	pc         int
	stackindex int
	stacklimit int
	scopeindex int
	scopelimit int
	pathindex  int
	pathlimit  int
	offset     int
	expdepth   int
}
