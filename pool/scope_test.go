package pool_test

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/conc/pool"

	"github.com/stretchr/testify/require"
)

func ExamplePool_GoScoped() {
	var sum atomic.Int64
	p := pool.New().WithMaxGoroutines(1)
	p.GoScoped(func(s *pool.Scope) {
		for i := 0; i < 3; i++ {
			i := i
			s.Go(func() {
				sum.Add(int64(i + 1))
			})
		}
	})
	p.Wait()
	fmt.Println(sum.Load())
	// Output:
	// 6
}

func TestPoolGoScoped(t *testing.T) {
	t.Parallel()

	t.Run("basic children", func(t *testing.T) {
		t.Parallel()

		var count atomic.Int64
		p := pool.New()
		p.GoScoped(func(s *pool.Scope) {
			for i := 0; i < 50; i++ {
				s.Go(func() {
					count.Add(1)
				})
			}
		})
		p.Wait()
		require.Equal(t, int64(50), count.Load())
	})

	t.Run("capacity one does not deadlock", func(t *testing.T) {
		t.Parallel()

		var count atomic.Int64
		p := pool.New().WithMaxGoroutines(1)
		p.GoScoped(func(s *pool.Scope) {
			for i := 0; i < 100; i++ {
				s.Go(func() {
					time.Sleep(time.Millisecond)
					count.Add(1)
				})
			}
		})

		done := make(chan struct{})
		go func() {
			p.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("capacity-one pool with a scope deadlocked")
		}
		require.Equal(t, int64(100), count.Load())
	})

	t.Run("deep nesting does not deadlock", func(t *testing.T) {
		t.Parallel()

		const depth = 200

		var leaf atomic.Int64
		var spawn func(scope *pool.Scope, level int)
		p := pool.New().WithMaxGoroutines(1)

		spawn = func(scope *pool.Scope, level int) {
			if level == depth {
				leaf.Add(1)
				return
			}
			scope.GoScoped(func(child *pool.Scope) {
				spawn(child, level+1)
			})
		}

		p.GoScoped(func(s *pool.Scope) {
			spawn(s, 0)
		})

		done := make(chan struct{})
		go func() {
			p.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("deeply nested scope deadlocked")
		}
		require.Equal(t, int64(1), leaf.Load())
	})

	t.Run("siblings run concurrently within the limit", func(t *testing.T) {
		t.Parallel()

		for _, maxConcurrent := range []int{1, 2, 4, 8} {
			maxConcurrent := maxConcurrent
			t.Run(strconv.Itoa(maxConcurrent), func(t *testing.T) {
				t.Parallel()

				var current atomic.Int64
				var peak atomic.Int64
				var violations atomic.Int64

				p := pool.New().WithMaxGoroutines(maxConcurrent)
				p.GoScoped(func(s *pool.Scope) {
					for i := 0; i < maxConcurrent*4; i++ {
						s.Go(func() {
							cur := current.Add(1)
							for {
								old := peak.Load()
								if cur <= old || peak.CompareAndSwap(old, cur) {
									break
								}
							}
							if cur > int64(maxConcurrent) {
								violations.Add(1)
							}
							time.Sleep(2 * time.Millisecond)
							current.Add(-1)
						})
					}
				})
				p.Wait()

				require.Equal(t, int64(0), violations.Load(), "children exceeded the pool limit")
				require.Equal(t, int64(0), current.Load())
				if maxConcurrent > 1 {
					require.GreaterOrEqual(t, peak.Load(), int64(2), "siblings never ran concurrently")
				}
			})
		}
	})

	t.Run("top level Wait reaps all descendants", func(t *testing.T) {
		t.Parallel()

		var done atomic.Int64
		p := pool.New().WithMaxGoroutines(3)

		p.GoScoped(func(parent *pool.Scope) {
			for i := 0; i < 10; i++ {
				parent.GoScoped(func(child *pool.Scope) {
					for j := 0; j < 10; j++ {
						child.Go(func() {
							time.Sleep(time.Millisecond)
							done.Add(1)
						})
					}
				})
			}
		})

		p.Wait()
		require.Equal(t, int64(100), done.Load())
	})

	t.Run("parent panic reaps accepted children", func(t *testing.T) {
		t.Parallel()

		var childrenDone atomic.Int64
		var childDoneMu sync.Mutex
		var finished []chan struct{}

		p := pool.New().WithMaxGoroutines(1)
		p.GoScoped(func(s *pool.Scope) {
			for i := 0; i < 20; i++ {
				ch := make(chan struct{})
				childDoneMu.Lock()
				finished = append(finished, ch)
				childDoneMu.Unlock()

				s.Go(func() {
					time.Sleep(2 * time.Millisecond)
					childrenDone.Add(1)
					close(ch)
				})
			}
			panic("boom")
		})

		require.Panics(t, p.Wait)
		require.Equal(t, int64(20), childrenDone.Load())
		for _, ch := range finished {
			select {
			case <-ch:
			default:
				t.Fatal("an accepted child was not reaped after the parent panicked")
			}
		}
	})

	t.Run("child panic is propagated", func(t *testing.T) {
		t.Parallel()

		var otherDone atomic.Int64
		p := pool.New().WithMaxGoroutines(2)
		p.GoScoped(func(s *pool.Scope) {
			for i := 0; i < 10; i++ {
				i := i
				s.Go(func() {
					if i == 5 {
						panic("child boom")
					}
					otherDone.Add(1)
				})
			}
		})
		require.Panics(t, p.Wait)
	})

	t.Run("saved scope rejects use after return", func(t *testing.T) {
		t.Parallel()

		var saved *pool.Scope
		p := pool.New()
		p.GoScoped(func(s *pool.Scope) {
			saved = s
			s.Go(func() {})
		})
		p.Wait()

		require.Panics(t, func() {
			saved.Go(func() {})
		})
	})

	t.Run("explicit Wait then more children", func(t *testing.T) {
		t.Parallel()

		var count atomic.Int64
		p := pool.New().WithMaxGoroutines(1)
		p.GoScoped(func(s *pool.Scope) {
			s.Go(func() { count.Add(1) })
			s.Wait()
			require.Equal(t, int64(1), count.Load())

			s.Go(func() { count.Add(1) })
		})
		p.Wait()
		require.Equal(t, int64(2), count.Load())
	})

	t.Run("scoped and plain tasks share the limit", func(t *testing.T) {
		t.Parallel()

		const limit = 3
		var current atomic.Int64
		var violations atomic.Int64

		p := pool.New().WithMaxGoroutines(limit)
		for i := 0; i < 10; i++ {
			p.Go(func() {
				cur := current.Add(1)
				if cur > limit {
					violations.Add(1)
				}
				time.Sleep(2 * time.Millisecond)
				current.Add(-1)
			})
		}
		p.GoScoped(func(s *pool.Scope) {
			for i := 0; i < 30; i++ {
				s.Go(func() {
					cur := current.Add(1)
					if cur > limit {
						violations.Add(1)
					}
					time.Sleep(2 * time.Millisecond)
					current.Add(-1)
				})
			}
		})
		p.Wait()
		require.Equal(t, int64(0), violations.Load())
	})

	t.Run("unlimited pool runs siblings concurrently", func(t *testing.T) {
		t.Parallel()

		var current atomic.Int64
		var peak atomic.Int64

		p := pool.New()
		p.GoScoped(func(s *pool.Scope) {
			for i := 0; i < 16; i++ {
				s.Go(func() {
					cur := current.Add(1)
					for {
						old := peak.Load()
						if cur <= old || peak.CompareAndSwap(old, cur) {
							break
						}
					}
					time.Sleep(2 * time.Millisecond)
					current.Add(-1)
				})
			}
		})
		p.Wait()
		require.GreaterOrEqual(t, peak.Load(), int64(2), "children never ran concurrently")
		require.Equal(t, int64(0), current.Load())
	})

	t.Run("pool is reusable after a scoped panic", func(t *testing.T) {
		t.Parallel()

		p := pool.New().WithMaxGoroutines(2)
		p.GoScoped(func(s *pool.Scope) {
			s.Go(func() {})
			panic("first")
		})
		require.Panics(t, p.Wait)

		var count atomic.Int64
		p.GoScoped(func(s *pool.Scope) {
			for i := 0; i < 10; i++ {
				s.Go(func() { count.Add(1) })
			}
		})
		p.Wait()
		require.Equal(t, int64(10), count.Load())
	})
}
