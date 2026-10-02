package gojq

import (
	"context"
	"errors"
	"io"
)

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
	closers   map[io.Closer]struct{}
	closed    map[io.Closer]struct{}
}

func newEnv(ctx context.Context) *env {
	return &env{
		stack:  newStack(),
		paths:  newStack(),
		scopes: newScopeStack(),
		ctx:    ctx,
	}
}

// Close closes the iterators tracked during the execution of the query.
// The iterators returned by the custom iterator functions (added by
// [WithIterFunction] or [WithIterFunctionWithContext]) are tracked when
// they implement [io.Closer], and closed exactly once; when the caller
// stops the iteration early, calling Close on the iterator returned by
// [Code.Run] or [Code.RunWithContext] releases the remaining resources.
func (env *env) Close() error {
	return env.closeClosers()
}

func (env *env) addCloser(closer io.Closer) {
	if _, ok := env.closed[closer]; ok {
		return
	}
	if env.closers == nil {
		env.closers = make(map[io.Closer]struct{})
	}
	env.closers[closer] = struct{}{}
}

func (env *env) closeCloser(closer io.Closer) error {
	if _, ok := env.closers[closer]; !ok {
		return nil
	}
	delete(env.closers, closer)
	return closer.Close()
}

func (env *env) closeClosers() error {
	var err error
	for closer := range env.closers {
		delete(env.closers, closer)
		if env.closed == nil {
			env.closed = make(map[io.Closer]struct{})
		}
		env.closed[closer] = struct{}{}
		if cerr := closer.Close(); cerr != nil {
			if err == nil {
				err = cerr
			} else {
				err = errors.Join(err, cerr)
			}
		}
	}
	return err
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
