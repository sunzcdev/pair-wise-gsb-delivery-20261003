package stream_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/conc/panics"
	"github.com/sourcegraph/conc/stream"

	"github.com/stretchr/testify/require"
)

func ExampleStream() {
	times := []int{20, 52, 16, 45, 4, 80}

	s := stream.New()
	for _, millis := range times {
		dur := time.Duration(millis) * time.Millisecond
		s.Go(func() stream.Callback {
			time.Sleep(dur)
			// This will print in the order the tasks were submitted
			return func() { fmt.Println(dur) }
		})
	}
	s.Wait()

	// Output:
	// 20ms
	// 52ms
	// 16ms
	// 45ms
	// 4ms
	// 80ms
}

func TestStream(t *testing.T) {
	t.Parallel()

	t.Run("simple", func(t *testing.T) {
		t.Parallel()
		s := stream.New()
		var res []int
		for i := 0; i < 5; i++ {
			i := i
			s.Go(func() stream.Callback {
				i *= 2
				return func() {
					res = append(res, i)
				}
			})
		}
		s.Wait()
		require.Equal(t, []int{0, 2, 4, 6, 8}, res)
	})

	t.Run("nil callback", func(t *testing.T) {
		t.Parallel()
		s := stream.New()
		var totalCount atomic.Int64
		for i := 0; i < 5; i++ {
			s.Go(func() stream.Callback {
				totalCount.Add(1)
				return nil
			})
		}
		s.Wait()
		require.Equal(t, int64(5), totalCount.Load())
	})

	t.Run("max goroutines", func(t *testing.T) {
		t.Parallel()
		s := stream.New().WithMaxGoroutines(5)
		var currentTaskCount atomic.Int64
		var currentCallbackCount atomic.Int64
		for i := 0; i < 50; i++ {
			s.Go(func() stream.Callback {
				curr := currentTaskCount.Add(1)
				if curr > 5 {
					t.Fatal("too many concurrent tasks being executed")
				}
				defer currentTaskCount.Add(-1)

				time.Sleep(time.Millisecond)

				return func() {
					curr := currentCallbackCount.Add(1)
					if curr > 1 {
						t.Fatal("too many concurrent callbacks being executed")
					}

					time.Sleep(time.Millisecond)

					defer currentCallbackCount.Add(-1)
				}
			})
		}
		s.Wait()
	})

	t.Run("panic in task is propagated", func(t *testing.T) {
		t.Parallel()
		s := stream.New().WithMaxGoroutines(5)
		s.Go(func() stream.Callback {
			panic("something really bad happened in the task")
		})
		require.Panics(t, s.Wait)
	})

	t.Run("panic in callback is propagated", func(t *testing.T) {
		t.Parallel()
		s := stream.New().WithMaxGoroutines(5)
		s.Go(func() stream.Callback {
			return func() {
				panic("something really bad happened in the callback")
			}
		})
		require.Panics(t, s.Wait)
	})

	t.Run("panic in callback does not block producers", func(t *testing.T) {
		t.Parallel()
		s := stream.New().WithMaxGoroutines(5)
		s.Go(func() stream.Callback {
			return func() {
				panic("something really bad happened in the callback")
			}
		})
		for i := 0; i < 100; i++ {
			s.Go(func() stream.Callback {
				return func() {}
			})
		}
		require.Panics(t, s.Wait)
	})

	t.Run("max in-flight limits pending callbacks", func(t *testing.T) {
		t.Parallel()
		const maxInFlight = 3
		s := stream.New().WithMaxGoroutines(10).WithMaxInFlight(maxInFlight)

		var pending atomic.Int64
		var maxPending atomic.Int64
		track := func(delta int64) {
			curr := pending.Add(delta)
			if curr > maxPending.Load() {
				maxPending.CompareAndSwap(maxPending.Load(), curr)
			}
		}

		// The first task is slow, so later tasks finish long before
		// their callbacks can be delivered.
		s.Go(func() stream.Callback {
			track(1)
			time.Sleep(100 * time.Millisecond)
			return func() { track(-1) }
		})
		// Later tasks complete immediately, returning large closures
		// that would pile up in memory without an in-flight limit.
		for i := 0; i < 100; i++ {
			s.Go(func() stream.Callback {
				track(1)
				buf := make([]byte, 1<<20)
				return func() {
					track(-1)
					_ = buf
				}
			})
		}
		s.Wait()
		require.LessOrEqual(t, maxPending.Load(), int64(maxInFlight))
		require.Equal(t, int64(0), pending.Load())
	})

	t.Run("max in-flight validation", func(t *testing.T) {
		t.Parallel()
		require.Panics(t, func() { stream.New().WithMaxInFlight(0) })
		s := stream.New()
		s.Go(func() stream.Callback { return func() {} })
		require.Panics(t, func() { s.WithMaxInFlight(1) })
		s.Wait()
	})

	t.Run("panic in task releases in-flight slots", func(t *testing.T) {
		t.Parallel()
		s := stream.New().WithMaxGoroutines(2).WithMaxInFlight(2)
		s.Go(func() stream.Callback {
			panic("something really bad happened in the task")
		})
		// If the panicking task did not release its in-flight slot,
		// these submissions would deadlock.
		for i := 0; i < 10; i++ {
			s.Go(func() stream.Callback {
				return func() {}
			})
		}
		recovered := waitRecovered(s)
		require.NotNil(t, recovered)
		require.Equal(t, "something really bad happened in the task", rootValue(recovered))
	})

	t.Run("panic in callback releases in-flight slots", func(t *testing.T) {
		t.Parallel()
		s := stream.New().WithMaxGoroutines(2).WithMaxInFlight(2)
		s.Go(func() stream.Callback {
			return func() {
				panic("something really bad happened in the callback")
			}
		})
		// If the panicking callback did not release its in-flight slot,
		// these submissions would deadlock.
		for i := 0; i < 10; i++ {
			s.Go(func() stream.Callback {
				return func() {}
			})
		}
		recovered := waitRecovered(s)
		require.NotNil(t, recovered)
		require.Equal(t, "something really bad happened in the callback", rootValue(recovered))
	})
}

func waitRecovered(s *stream.Stream) (recovered *panics.Recovered) {
	defer func() {
		if r := recover(); r != nil {
			recovered = r.(*panics.Recovered)
		}
	}()
	s.Wait()
	return nil
}

// rootValue unwraps nested panics.Recovered values down to the original
// panic value.
func rootValue(r *panics.Recovered) any {
	for {
		inner, ok := r.Value.(*panics.Recovered)
		if !ok {
			return r.Value
		}
		r = inner
	}
}

func BenchmarkStream(b *testing.B) {
	b.Run("startup and teardown", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			s := stream.New()
			s.Go(func() stream.Callback { return func() {} })
			s.Wait()
		}
	})

	b.Run("per task", func(b *testing.B) {
		n := 0
		s := stream.New()
		for i := 0; i < b.N; i++ {
			s.Go(func() stream.Callback {
				return func() {
					n += 1
				}
			})
		}
		s.Wait()
	})
}
