package panics_test

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/sourcegraph/conc/panics"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ExampleCatcher() {
	var pc panics.Catcher
	i := 0
	pc.Try(func() { i += 1 })
	pc.Try(func() { panic("abort!") })
	pc.Try(func() { i += 1 })

	rc := pc.Recovered()

	fmt.Println(i)
	fmt.Println(rc.Value.(string))
	// Output:
	// 2
	// abort!
}

func ExampleCatcher_callers() {
	var pc panics.Catcher
	pc.Try(func() { panic("mayday!") })

	recovered := pc.Recovered()

	// For debugging, the pre-formatted recovered.Stack is easier to use than
	// rc.Callers. This is not used in the example because its output is
	// machine-specific.

	frames := runtime.CallersFrames(recovered.Callers)
	for {
		frame, more := frames.Next()

		fmt.Println(frame.Function)

		if !more {
			break
		}
	}
	// Output:
	// github.com/sourcegraph/conc/panics.(*Catcher).tryRecover
	// runtime.gopanic
	// github.com/sourcegraph/conc/panics_test.ExampleCatcher_callers.func1
	// github.com/sourcegraph/conc/panics.(*Catcher).Try
	// github.com/sourcegraph/conc/panics_test.ExampleCatcher_callers
	// testing.runExample
	// testing.runExamples
	// testing.(*M).Run
	// main.main
	// runtime.main
	// runtime.goexit
}

func ExampleCatcher_error() {
	helper := func() error {
		var pc panics.Catcher
		pc.Try(func() { panic(errors.New("error")) })
		return pc.Recovered().AsError()
	}

	if err := helper(); err != nil {
		// In normal use cases, you can use err.Error() output directly to
		// dump the panic's stack. This is not used in the example because
		// its output is machine-specific - instead, we demonstrate getting
		// the underlying error that was used for the panic.
		if cause := errors.Unwrap(err); cause != nil {
			fmt.Printf("helper panicked with an error: %s", cause)
		}
	}
	// Output:
	// helper panicked with an error: error
}

func TestCatcher(t *testing.T) {
	t.Parallel()

	err1 := errors.New("SOS")

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		var pc panics.Catcher
		pc.Try(func() { panic(err1) })
		recovered := pc.Recovered()
		require.ErrorIs(t, recovered.AsError(), err1)
		require.ErrorAs(t, recovered.AsError(), &err1)
		// The exact contents aren't tested because the stacktrace contains local file paths
		// and even the structure of the stacktrace is bound to be unstable over time. Just
		// test a couple of basics.
		require.Contains(t, recovered.String(), "SOS", "formatted panic should contain the panic message")
		require.Contains(t, recovered.String(), "panics.(*Catcher).Try", recovered.String(), "formatted panic should contain the stack trace")
	})

	t.Run("not error", func(t *testing.T) {
		var pc panics.Catcher
		pc.Try(func() { panic("definitely not an error") })
		recovered := pc.Recovered()
		require.NotErrorIs(t, recovered.AsError(), err1)
		require.Nil(t, errors.Unwrap(recovered.AsError()))
	})

	t.Run("repanic panics", func(t *testing.T) {
		var pc panics.Catcher
		pc.Try(func() { panic(err1) })
		require.Panics(t, pc.Repanic)
	})

	t.Run("repanic does not panic without child panic", func(t *testing.T) {
		t.Parallel()
		var pc panics.Catcher
		pc.Try(func() { _ = 1 })
		require.NotPanics(t, pc.Repanic)
	})

	t.Run("is goroutine safe", func(t *testing.T) {
		t.Parallel()
		var wg sync.WaitGroup
		var pc panics.Catcher
		for i := 0; i < 100; i++ {
			i := i
			wg.Add(1)
			func() {
				defer wg.Done()
				pc.Try(func() {
					if i == 50 {
						panic("50")
					}

				})
			}()
		}
		wg.Wait()
		require.Equal(t, "50", pc.Recovered().Value)
	})

	t.Run("recovered all collects every panic in order", func(t *testing.T) {
		t.Parallel()
		var pc panics.Catcher
		pc.Try(func() { panic("first") })
		pc.Try(func() { _ = 1 })
		pc.Try(func() { panic("second") })
		pc.Try(func() { panic("third") })

		all := pc.RecoveredAll()
		require.Len(t, all, 3)
		require.Equal(t, "first", all[0].Value)
		require.Equal(t, "second", all[1].Value)
		require.Equal(t, "third", all[2].Value)
		for i, r := range all {
			require.Equal(t, i, r.Seq, "Seq should match the reception order")
			require.NotEmpty(t, r.Stack, "every recovered panic should have a stacktrace")
			require.NotEmpty(t, r.Callers, "every recovered panic should have callers")
		}

		// Recovered still returns the first panic.
		require.Equal(t, "first", pc.Recovered().Value)
	})

	t.Run("recovered all is nil without panics", func(t *testing.T) {
		t.Parallel()
		var pc panics.Catcher
		pc.Try(func() { _ = 1 })
		require.Nil(t, pc.RecoveredAll())
	})

	t.Run("panic nil is recovered", func(t *testing.T) {
		t.Parallel()
		var pc panics.Catcher
		pc.Try(func() { panic(nil) })

		all := pc.RecoveredAll()
		require.Len(t, all, 1)
		require.Nil(t, all[0].Value)
		require.Equal(t, 0, all[0].Seq)

		recovered := pc.Recovered()
		require.NotNil(t, recovered)
		require.Nil(t, recovered.Value)
	})

	t.Run("reset clears recovered panics", func(t *testing.T) {
		t.Parallel()
		var pc panics.Catcher
		pc.Try(func() { panic("abort!") })
		require.NotNil(t, pc.Recovered())

		pc.Reset()
		require.Nil(t, pc.Recovered())
		require.Nil(t, pc.RecoveredAll())

		// The catcher is reusable after a reset, and sequence numbers
		// start over.
		pc.Try(func() { panic("again!") })
		all := pc.RecoveredAll()
		require.Len(t, all, 1)
		require.Equal(t, "again!", all[0].Value)
		require.Equal(t, 0, all[0].Seq)
	})

	t.Run("recovered all is goroutine safe", func(t *testing.T) {
		t.Parallel()
		var wg sync.WaitGroup
		var pc panics.Catcher
		for i := 0; i < 100; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				pc.Try(func() { panic(i) })
			}()
		}
		wg.Wait()

		all := pc.RecoveredAll()
		require.Len(t, all, 100)
		seen := make(map[int]bool, 100)
		for i, r := range all {
			require.Equal(t, i, r.Seq, "Seq should match the reception order")
			seen[r.Value.(int)] = true
		}
		for i := 0; i < 100; i++ {
			require.True(t, seen[i], "panic %d should have been recovered", i)
		}
	})
}

func TestRecoveredAsError(t *testing.T) {
	t.Parallel()
	t.Run("as error is nil", func(t *testing.T) {
		t.Parallel()
		fn := func() error {
			var c panics.Catcher
			c.Try(func() {})
			return c.Recovered().AsError()
		}
		err := fn()
		assert.Nil(t, err)
	})

	t.Run("as error is not nil", func(t *testing.T) {
		t.Parallel()
		fn := func() error {
			var c panics.Catcher
			c.Try(func() { panic("oh dear!") })
			return c.Recovered().AsError()
		}
		err := fn()
		assert.NotNil(t, err)
	})
}
