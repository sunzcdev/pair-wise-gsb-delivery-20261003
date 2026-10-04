package panics_test

import (
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/sourcegraph/conc/panics"

	"github.com/stretchr/testify/require"
)

func TestCatch(t *testing.T) {
	t.Parallel()

	t.Run("no panic", func(t *testing.T) {
		t.Parallel()
		ran := false
		r := panics.Catch(func() { ran = true })
		require.True(t, ran)
		require.Nil(t, r)
	})

	t.Run("non-nil panic", func(t *testing.T) {
		t.Parallel()
		err := errors.New("SOS")
		r := panics.Catch(func() { panic(err) })
		require.NotNil(t, r)
		require.Equal(t, err, r.Value)
		require.ErrorIs(t, r.AsError(), err)
		require.NotEmpty(t, r.Callers)
		require.NotEmpty(t, r.Stack)
		require.Contains(t, r.String(), "SOS")
	})

	t.Run("nil panic", func(t *testing.T) {
		t.Parallel()
		r := panics.Catch(func() { panic(nil) })
		require.NotNil(t, r)
		require.Nil(t, r.Value)
		require.NotEmpty(t, r.Stack)
		require.Contains(t, r.String(), "panic called with nil argument")
	})

	t.Run("nil panic is not mistaken for a normal return", func(t *testing.T) {
		t.Parallel()
		require.NotPanics(t, func() {
			r := panics.Catch(func() { panic(nil) })
			require.NotNil(t, r)
		})
	})

	t.Run("runtime.Goexit produces nothing", func(t *testing.T) {
		t.Parallel()

		// Catch must not resume after runtime.Goexit, so it has to run in a
		// goroutine we join explicitly.
		var wg sync.WaitGroup
		var result *panics.Recovered
		var returned bool
		wg.Add(1)
		go func() {
			defer wg.Done()
			result = panics.Catch(func() { runtime.Goexit() })
			returned = true
		}()
		wg.Wait()

		require.False(t, returned)
		require.Nil(t, result)
	})

	t.Run("is goroutine safe", func(t *testing.T) {
		t.Parallel()
		var wg sync.WaitGroup
		results := make([]*panics.Recovered, 100)
		for i := 0; i < 100; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					results[i] = panics.Catch(func() { panic(i) })
				} else {
					results[i] = panics.Catch(func() {})
				}
			}()
		}
		wg.Wait()

		for i := 0; i < 100; i++ {
			if i%2 == 0 {
				require.NotNil(t, results[i])
				require.Equal(t, i, results[i].Value)
			} else {
				require.Nil(t, results[i])
			}
		}
	})
}
