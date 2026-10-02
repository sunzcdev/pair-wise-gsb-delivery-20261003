package gojq

import (
	"context"
	"sync"
)

type env struct {
	pc         int
	stack      *stack
	paths      *stack
	scopes     *scopeStack
	values     []any
	codes      []*code
	codeinfos  []codeinfo
	forks      []fork
	backtrack  bool
	offset     int
	expdepth   int
	label      int
	args       [32]any // len(env.args) > maxarity
	ctx        context.Context
	inputIter  Iter
	inputOwned bool
	closeOnce  sync.Once
	closeErr   error
}

func newEnv(ctx context.Context, inputIter Iter) *env {
	return &env{
		stack:     newStack(),
		paths:     newStack(),
		scopes:    newScopeStack(),
		ctx:       ctx,
		inputIter: inputIter,
	}
}

// Close releases the input source owned by this run. It closes the input
// iterator bound via [*Code.RunInput] (and [*Code.RunInputWithContext]) when
// it implements [io.Closer]. It is safe to call this method multiple times;
// the first call returns the result of closing the input iterator.
//
// The iterator is never closed automatically. Call Close (typically with
// defer) when stopping the iteration before the run terminates, such as after
// a [context.Context] cancellation, a halt error or consuming only a part of
// the results. The compile-time input iterator given to [WithInputIter] is
// shared across runs and never closed.
func (env *env) Close() error {
	env.closeOnce.Do(func() {
		if env.inputOwned {
			if c, ok := env.inputIter.(interface{ Close() error }); ok {
				env.closeErr = c.Close()
			}
		}
	})
	return env.closeErr
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
