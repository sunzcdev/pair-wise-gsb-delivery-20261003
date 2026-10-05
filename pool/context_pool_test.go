package pool_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/conc/pool"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ExampleContextPool_WithCancelOnError() {
	p := pool.New().
		WithMaxGoroutines(4).
		WithContext(context.Background()).
		WithCancelOnError()
	for i := 0; i < 3; i++ {
		i := i
		p.Go(func(ctx context.Context) error {
			if i == 2 {
				return errors.New("I will cancel all other tasks!")
			}
			<-ctx.Done()
			return nil
		})
	}
	err := p.Wait()
	fmt.Println(err)
	// Output:
	// I will cancel all other tasks!
}

func TestContextPool(t *testing.T) {
	t.Parallel()

	err1 := errors.New("err1")
	err2 := errors.New("err2")
	bgctx := context.Background()

	t.Run("panics on configuration after init", func(t *testing.T) {
		t.Run("before wait", func(t *testing.T) {
			t.Parallel()
			g := pool.New().WithContext(context.Background())
			g.Go(func(context.Context) error { return nil })
			require.Panics(t, func() { g.WithMaxGoroutines(10) })
		})

		t.Run("after wait", func(t *testing.T) {
			t.Parallel()
			g := pool.New().WithContext(context.Background())
			g.Go(func(context.Context) error { return nil })
			_ = g.Wait()
			require.Panics(t, func() { g.WithMaxGoroutines(10) })
		})
	})

	t.Run("behaves the same as ErrorGroup", func(t *testing.T) {
		t.Parallel()

		t.Run("wait returns no error if no errors", func(t *testing.T) {
			t.Parallel()
			p := pool.New().WithContext(bgctx)
			p.Go(func(context.Context) error { return nil })
			require.NoError(t, p.Wait())
		})

		t.Run("wait errors if func returns error", func(t *testing.T) {
			t.Parallel()
			p := pool.New().WithContext(bgctx)
			p.Go(func(context.Context) error { return err1 })
			require.ErrorIs(t, p.Wait(), err1)
		})

		t.Run("wait error is all returned errors", func(t *testing.T) {
			t.Parallel()
			p := pool.New().WithErrors().WithContext(bgctx)
			p.Go(func(context.Context) error { return err1 })
			p.Go(func(context.Context) error { return nil })
			p.Go(func(context.Context) error { return err2 })
			err := p.Wait()
			require.ErrorIs(t, err, err1)
			require.ErrorIs(t, err, err2)
		})
	})

	t.Run("context error propagates", func(t *testing.T) {
		t.Parallel()

		t.Run("canceled", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(bgctx)
			p := pool.New().WithContext(ctx)
			p.Go(func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			})
			cancel()
			require.ErrorIs(t, p.Wait(), context.Canceled)
		})

		t.Run("timed out", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(bgctx, time.Millisecond)
			defer cancel()
			p := pool.New().WithContext(ctx)
			p.Go(func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			})
			require.ErrorIs(t, p.Wait(), context.DeadlineExceeded)
		})

		t.Run("return before timed out", func(t *testing.T) {
			t.Parallel()
			p := pool.New().WithContext(context.Background())
			p.Go(func(ctx context.Context) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(1 * time.Millisecond):
					return nil
				}
			})
			require.NoError(t, p.Wait())
		})
	})

	t.Run("WithCancelOnError", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithContext(bgctx).WithCancelOnError()
		p.Go(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		p.Go(func(ctx context.Context) error {
			return err1
		})
		err := p.Wait()
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, err1)
	})

	t.Run("no WithCancelOnError", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithContext(bgctx)
		p.Go(func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
				return nil
			}
		})
		p.Go(func(ctx context.Context) error {
			return err1
		})
		err := p.Wait()
		require.ErrorIs(t, err, err1)
		require.NotErrorIs(t, err, context.Canceled)
	})

	t.Run("WithFirstError", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithContext(bgctx).WithFirstError()
		sync := make(chan struct{})
		p.Go(func(ctx context.Context) error {
			defer close(sync)
			return err1
		})
		p.Go(func(ctx context.Context) error {
			// This test has a race condition. After the first goroutine
			// completes, this goroutine is woken up because sync is closed.
			// However, this goroutine might be woken up before the error from
			// the first goroutine is registered. To prevent that, we sleep for
			// another 10 milliseconds, giving the other goroutine time to return
			// and register its error before this goroutine returns its error.
			<-sync
			time.Sleep(10 * time.Millisecond)
			return err2
		})
		err := p.Wait()
		require.ErrorIs(t, err, err1)
		require.NotErrorIs(t, err, err2)
	})

	t.Run("WithFailFast", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithContext(bgctx).WithFailFast()
		p.Go(func(ctx context.Context) error {
			return err1
		})
		p.Go(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		err := p.Wait()
		require.ErrorIs(t, err, err1)
		require.NotErrorIs(t, err, context.Canceled)
	})

	t.Run("WithCancelOnError and panic", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithContext(bgctx).WithCancelOnError()
		var cancelledTasks atomic.Int64
		p.Go(func(ctx context.Context) error {
			<-ctx.Done()
			cancelledTasks.Add(1)
			return ctx.Err()
		})
		p.Go(func(ctx context.Context) error {
			<-ctx.Done()
			cancelledTasks.Add(1)
			return ctx.Err()
		})
		p.Go(func(ctx context.Context) error {
			panic("abort!")
		})
		assert.Panics(t, func() { _ = p.Wait() })
		assert.EqualValues(t, 2, cancelledTasks.Load())
	})

	t.Run("limit", func(t *testing.T) {
		t.Parallel()
		for _, maxConcurrent := range []int{1, 10, 100} {
			t.Run(strconv.Itoa(maxConcurrent), func(t *testing.T) {
				maxConcurrent := maxConcurrent // copy

				t.Parallel()
				p := pool.New().WithContext(bgctx).WithMaxGoroutines(maxConcurrent)

				var currentConcurrent atomic.Int64
				for i := 0; i < 100; i++ {
					p.Go(func(context.Context) error {
						cur := currentConcurrent.Add(1)
						if cur > int64(maxConcurrent) {
							return fmt.Errorf("expected no more than %d concurrent goroutine", maxConcurrent)
						}
						time.Sleep(time.Millisecond)
						currentConcurrent.Add(-1)
						return nil
					})
				}
				require.NoError(t, p.Wait())
				require.Equal(t, int64(0), currentConcurrent.Load())
			})
		}
	})
}

func TestContextPoolCancelCause(t *testing.T) {
	t.Parallel()

	err1 := errors.New("err1")
	err2 := errors.New("err2")
	parentErr := errors.New("parent canceled")

	t.Run("task error becomes the cancel cause", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithContext(context.Background()).WithCancelOnError()
		var poolCtx context.Context
		p.Go(func(ctx context.Context) error {
			poolCtx = ctx
			<-ctx.Done()
			return ctx.Err()
		})
		p.Go(func(context.Context) error {
			return err1
		})
		err := p.Wait()
		require.ErrorIs(t, err, err1)
		require.ErrorIs(t, err, context.Canceled)
		require.Same(t, err1, context.Cause(poolCtx))
	})

	t.Run("already canceled parent keeps its cause", func(t *testing.T) {
		t.Parallel()
		parent, cancel := context.WithCancelCause(context.Background())
		p := pool.New().WithContext(parent).WithCancelOnError()
		var poolCtx context.Context
		p.Go(func(ctx context.Context) error {
			poolCtx = ctx
			<-ctx.Done()
			return ctx.Err()
		})
		cancel(parentErr)
		p.Go(func(context.Context) error {
			return err1
		})
		err := p.Wait()
		require.ErrorIs(t, err, err1)
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, parentErr)
		require.Same(t, parentErr, context.Cause(poolCtx))
	})

	t.Run("parent cancel races task errors", func(t *testing.T) {
		t.Parallel()
		parent, cancel := context.WithCancelCause(context.Background())
		p := pool.New().WithContext(parent).WithCancelOnError()
		var poolCtx context.Context
		start := make(chan struct{})
		p.Go(func(ctx context.Context) error {
			poolCtx = ctx
			<-ctx.Done()
			<-start
			return ctx.Err()
		})
		p.Go(func(ctx context.Context) error {
			<-start
			return err1
		})
		p.Go(func(ctx context.Context) error {
			<-start
			return err2
		})
		cancel(parentErr)
		close(start)
		err := p.Wait()
		// Aggregation keeps every real failure; the cause is never a substitute.
		require.ErrorIs(t, err, err1)
		require.ErrorIs(t, err, err2)
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, parentErr)
		require.Same(t, parentErr, context.Cause(poolCtx))
	})

	t.Run("task returning only context.Canceled is the cause", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithContext(context.Background()).WithCancelOnError()
		var poolCtx context.Context
		p.Go(func(ctx context.Context) error {
			poolCtx = ctx
			<-ctx.Done()
			return ctx.Err()
		})
		p.Go(func(context.Context) error {
			return context.Canceled
		})
		err := p.Wait()
		require.ErrorIs(t, err, context.Canceled)
		require.Same(t, context.Canceled, context.Cause(poolCtx))
	})

	t.Run("cleanup cancel does not overwrite the business cause", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithContext(context.Background()).WithCancelOnError()
		var poolCtx context.Context
		p.Go(func(ctx context.Context) error {
			poolCtx = ctx
			<-ctx.Done()
			return ctx.Err()
		})
		p.Go(func(context.Context) error {
			return err1
		})
		_ = p.Wait()
		require.Same(t, err1, context.Cause(poolCtx))
	})
}
