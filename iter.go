package gojq

import "io"

// Iter is an interface for an iterator.
type Iter interface {
	Next() (any, bool)
}

// NewIter creates a new [Iter] from values.
func NewIter[T any](values ...T) Iter {
	switch len(values) {
	case 0:
		return emptyIter{}
	case 1:
		return &unitIter{value: values[0]}
	default:
		iter := sliceIter[T](values)
		return &iter
	}
}

type emptyIter struct{}

func (emptyIter) Next() (any, bool) {
	return nil, false
}

type unitIter struct {
	value any
	done  bool
}

func (iter *unitIter) Next() (any, bool) {
	if iter.done {
		return nil, false
	}
	iter.done = true
	return iter.value, true
}

type sliceIter[T any] []T

func (iter *sliceIter[T]) Next() (any, bool) {
	if len(*iter) == 0 {
		return nil, false
	}
	value := (*iter)[0]
	*iter = (*iter)[1:]
	return value, true
}

// closeIter wraps an [Iter] which implements [io.Closer], and ensures that
// the Close method is called exactly once; when the iterator is exhausted,
// yields an error, the run is canceled, or the iterator is abandoned by
// the query. Each run tracks its own iterators, so concurrent runs of the
// same [*Code] close their iterators independently.
type closeIter struct {
	iter   Iter
	closer io.Closer
	env    *env
	closed bool
}

func (env *env) newCloseIter(iter Iter, closer io.Closer) Iter {
	it := &closeIter{iter: iter, closer: closer, env: env}
	if env.closers == nil {
		env.closers = make(map[*closeIter]struct{})
	}
	env.closers[it] = struct{}{}
	return it
}

func (iter *closeIter) Next() (any, bool) {
	v, ok := iter.iter.Next()
	if !ok {
		iter.close()
	} else if _, ok := v.(error); ok {
		iter.close()
	}
	return v, ok
}

func (iter *closeIter) close() {
	if iter.closed {
		return
	}
	iter.closed = true
	delete(iter.env.closers, iter)
	iter.closer.Close()
}

func (env *env) closeIters() {
	for iter := range env.closers {
		iter.close()
	}
}
