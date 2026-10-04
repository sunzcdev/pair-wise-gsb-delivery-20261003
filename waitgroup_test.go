package conc_test

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/conc"

	"github.com/stretchr/testify/require"
)

func ExampleWaitGroup() {
	var count atomic.Int64

	var wg conc.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Go(func() {
			count.Add(1)
		})
	}
	wg.Wait()

	fmt.Println(count.Load())
	// Output:
	// 10
}

func ExampleWaitGroup_WaitAndRecover() {
	var wg conc.WaitGroup

	wg.Go(func() {
		panic("super bad thing")
	})

	recoveredPanic := wg.WaitAndRecover()
	fmt.Println(recoveredPanic.Value)
	// Output:
	// super bad thing
}

func ExampleWaitGroup_WaitAndRecoverAll() {
	var wg conc.WaitGroup

	// Goroutines finish (and are therefore recovered) in reverse order of
	// their panicking, so the returned slice stays stable in reception order
	// rather than in submission order.
	wg.Go(func() {
		time.Sleep(30 * time.Millisecond)
		panic("third")
	})
	wg.Go(func() {
		time.Sleep(20 * time.Millisecond)
		panic("second")
	})
	wg.Go(func() {
		time.Sleep(10 * time.Millisecond)
		panic("first")
	})

	recovered := wg.WaitAndRecoverAll()
	for i, p := range recovered {
		fmt.Println(i, p.Value)
	}
	// Output:
	// 0 first
	// 1 second
	// 2 third
}

func TestWaitGroup(t *testing.T) {
	t.Parallel()

	t.Run("ctor", func(t *testing.T) {
		t.Parallel()
		wg := conc.NewWaitGroup()
		require.IsType(t, &conc.WaitGroup{}, wg)
	})

	t.Run("all spawned run", func(t *testing.T) {
		t.Parallel()
		var count atomic.Int64
		var wg conc.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Go(func() {
				count.Add(1)
			})
		}
		wg.Wait()
		require.Equal(t, count.Load(), int64(100))
	})

	t.Run("panic", func(t *testing.T) {
		t.Parallel()

		t.Run("is propagated", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() {
				panic("super bad thing")
			})
			require.Panics(t, wg.Wait)
		})

		t.Run("one is propagated", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() {
				panic("super bad thing")
			})
			wg.Go(func() {
				panic("super badder thing")
			})
			require.Panics(t, wg.Wait)
		})

		t.Run("non-panics do not overwrite panic", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() {
				panic("super bad thing")
			})
			for i := 0; i < 10; i++ {
				wg.Go(func() {})
			}
			require.Panics(t, wg.Wait)
		})

		t.Run("non-panics run successfully", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			var i atomic.Int64
			wg.Go(func() {
				i.Add(1)
			})
			wg.Go(func() {
				panic("super bad thing")
			})
			wg.Go(func() {
				i.Add(1)
			})
			require.Panics(t, wg.Wait)
			require.Equal(t, int64(2), i.Load())
		})

		t.Run("is caught by waitandrecover", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() {
				panic("super bad thing")
			})
			p := wg.WaitAndRecover()
			require.Equal(t, p.Value, "super bad thing")
		})

		t.Run("one is caught by waitandrecover", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() {
				panic("super bad thing")
			})
			wg.Go(func() {
				panic("super badder thing")
			})
			p := wg.WaitAndRecover()
			require.NotNil(t, p)
		})

		t.Run("nonpanics run successfully with waitandrecover", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			var i atomic.Int64
			wg.Go(func() {
				i.Add(1)
			})
			wg.Go(func() {
				panic("super bad thing")
			})
			wg.Go(func() {
				i.Add(1)
			})
			p := wg.WaitAndRecover()
			require.Equal(t, p.Value, "super bad thing")
			require.Equal(t, int64(2), i.Load())
		})

		t.Run("all are caught by waitandrecoverall", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() { panic("a") })
			wg.Go(func() { panic("b") })
			wg.Go(func() { panic("c") })

			all := wg.WaitAndRecoverAll()
			require.Len(t, all, 3)
			values := []any{all[0].Value, all[1].Value, all[2].Value}
			require.ElementsMatch(t, []any{"a", "b", "c"}, values)

			// Wait/WaitAndRecover expose the first received panic.
			require.Equal(t, all[0], wg.WaitAndRecover())
			require.PanicsWithValue(t, all[0], wg.Wait)
		})

		t.Run("waitandrecoverall contains stack info", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() { panic("boom") })

			all := wg.WaitAndRecoverAll()
			require.Len(t, all, 1)
			require.NotEmpty(t, all[0].Callers)
			require.NotEmpty(t, all[0].Stack)
			require.Contains(t, all[0].String(), "boom")
		})

		t.Run("normal tasks produce no entries", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			var ran atomic.Int64
			for i := 0; i < 10; i++ {
				wg.Go(func() { ran.Add(1) })
			}
			wg.Go(func() { panic("only one") })
			for i := 0; i < 10; i++ {
				wg.Go(func() { ran.Add(1) })
			}

			all := wg.WaitAndRecoverAll()
			require.Len(t, all, 1)
			require.Equal(t, "only one", all[0].Value)
			require.Equal(t, int64(20), ran.Load())
		})

		t.Run("nil panic is recovered", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() { panic(nil) })

			all := wg.WaitAndRecoverAll()
			require.Len(t, all, 1)
			require.Nil(t, all[0].Value)
			require.NotEmpty(t, all[0].Stack)
			require.Contains(t, all[0].String(), "panic called with nil argument")

			// The first panic is also exposed through the legacy entry points.
			require.NotNil(t, wg.WaitAndRecover())
			require.Panics(t, wg.Wait)
		})

		t.Run("nil and non-nil panics are both collected", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() { panic("real") })
			wg.Go(func() { panic(nil) })

			all := wg.WaitAndRecoverAll()
			require.Len(t, all, 2)
			values := []any{all[0].Value, all[1].Value}
			require.ElementsMatch(t, []any{"real", nil}, values)
		})

		t.Run("runtime.Goexit does not produce an entry", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			var ran atomic.Bool
			wg.Go(func() { runtime.Goexit() })
			wg.Go(func() { ran.Store(true) })

			require.Nil(t, wg.WaitAndRecoverAll())
			require.True(t, ran.Load())
			require.NotPanics(t, wg.Wait)
		})

		t.Run("order follows reception order", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup

			// Submitted slow-first, but panics are received fast-first.
			wg.Go(func() { time.Sleep(30 * time.Millisecond); panic("slow") })
			wg.Go(func() { time.Sleep(20 * time.Millisecond); panic("medium") })
			wg.Go(func() { time.Sleep(10 * time.Millisecond); panic("fast") })

			all := wg.WaitAndRecoverAll()
			require.Equal(t, []any{"fast", "medium", "slow"}, []any{
				all[0].Value, all[1].Value, all[2].Value,
			})
		})

		t.Run("no panics returns nil", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() {})
			require.Nil(t, wg.WaitAndRecoverAll())

			var empty conc.WaitGroup
			require.Nil(t, empty.WaitAndRecoverAll())
		})

		t.Run("multiple waits observe the same round", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() { panic("same round") })

			first := wg.WaitAndRecoverAll()
			second := wg.WaitAndRecoverAll()
			require.Equal(t, first, second)
			require.Equal(t, "same round", wg.WaitAndRecover().Value)
			require.Panics(t, wg.Wait)

			// Mutating the returned slice does not corrupt the WaitGroup state.
			first[0] = nil
			require.NotNil(t, wg.WaitAndRecover())
		})

		t.Run("reuse clears recovered panics from the previous round", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup

			wg.Go(func() { panic("round one") })
			require.Len(t, wg.WaitAndRecoverAll(), 1)

			// A normal second round must not leak the previous round's panic.
			var ran atomic.Bool
			wg.Go(func() { ran.Store(true) })
			require.NotPanics(t, wg.Wait)
			require.Nil(t, wg.WaitAndRecover())
			require.Nil(t, wg.WaitAndRecoverAll())
			require.True(t, ran.Load())

			// A panicking third round only contains its own panics.
			wg.Go(func() { panic("round three") })
			all := wg.WaitAndRecoverAll()
			require.Len(t, all, 1)
			require.Equal(t, "round three", all[0].Value)
		})
	})
}
