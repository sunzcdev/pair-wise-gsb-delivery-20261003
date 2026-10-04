package conc_test

import (
	"fmt"
	"sync/atomic"
	"testing"

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

	wg.Go(func() {
		panic("super bad thing")
	})
	wg.Go(func() {
		// does not panic, so it produces no record
	})

	recoveredPanics := wg.WaitAndRecoverAll()
	fmt.Println(len(recoveredPanics))
	fmt.Println(recoveredPanics[0].Value)
	fmt.Println(recoveredPanics[0].Seq)
	// Output:
	// 1
	// super bad thing
	// 0
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
			wg.Go(func() {
				panic("super badder thing")
			})

			all := wg.WaitAndRecoverAll()
			require.Len(t, all, 2)
			require.ElementsMatch(t, []any{"super bad thing", "super badder thing"},
				[]any{all[0].Value, all[1].Value})
			for seq, r := range all {
				require.Equal(t, seq, r.Seq, "Seq should match the reception order")
				require.NotEmpty(t, r.Stack, "every recovered panic should have a stacktrace")
			}
			require.Equal(t, int64(2), i.Load())
		})

		t.Run("panic nil is caught by waitandrecoverall", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() {
				panic(nil)
			})
			all := wg.WaitAndRecoverAll()
			require.Len(t, all, 1)
			require.Nil(t, all[0].Value)
		})

		t.Run("waitandrecoverall returns nil without panics", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			for i := 0; i < 10; i++ {
				wg.Go(func() {})
			}
			require.Nil(t, wg.WaitAndRecoverAll())
		})

		t.Run("multiple waits report the same round", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup
			wg.Go(func() {
				panic("super bad thing")
			})

			first := wg.WaitAndRecoverAll()
			require.Len(t, first, 1)
			require.Equal(t, first, wg.WaitAndRecoverAll())
			require.Equal(t, first[0].Value, wg.WaitAndRecover().Value)
			require.Panics(t, wg.Wait)
		})
	})

	t.Run("reuse", func(t *testing.T) {
		t.Parallel()

		t.Run("new round does not leak recovered panics", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup

			wg.Go(func() {
				panic("round one")
			})
			round1 := wg.WaitAndRecoverAll()
			require.Len(t, round1, 1)
			require.Equal(t, "round one", round1[0].Value)

			wg.Go(func() {
				panic("round two")
			})
			round2 := wg.WaitAndRecoverAll()
			require.Len(t, round2, 1)
			require.Equal(t, "round two", round2[0].Value)
			require.Equal(t, 0, round2[0].Seq)
		})

		t.Run("wait does not repanic a previous round", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup

			wg.Go(func() {
				panic("round one")
			})
			require.Panics(t, wg.Wait)

			wg.Go(func() {})
			require.NotPanics(t, wg.Wait)
			require.Nil(t, wg.WaitAndRecoverAll())
		})

		t.Run("waitandrecover does not recover a previous round", func(t *testing.T) {
			t.Parallel()
			var wg conc.WaitGroup

			wg.Go(func() {
				panic("round one")
			})
			require.NotNil(t, wg.WaitAndRecover())

			wg.Go(func() {})
			require.Nil(t, wg.WaitAndRecover())
		})
	})
}
