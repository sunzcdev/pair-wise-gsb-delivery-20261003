package pool_test

import (
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/conc/pool"

	"github.com/stretchr/testify/require"
)

func ExamplePool_GoScope() {
	p := pool.New().WithMaxGoroutines(1)
	p.GoScope(func(s *pool.Scope) {
		for i := 0; i < 3; i++ {
			s.Go(func() {
				fmt.Println("child")
			})
		}
	})
	p.Wait()
	// Output:
	// child
	// child
	// child
}

func TestScope(t *testing.T) {
	t.Parallel()

	t.Run("children all run", func(t *testing.T) {
		t.Parallel()
		p := pool.New()
		var completed atomic.Int64
		p.GoScope(func(s *pool.Scope) {
			for i := 0; i < 100; i++ {
				s.Go(func() {
					time.Sleep(time.Millisecond)
					completed.Add(1)
				})
			}
		})
		p.Wait()
		require.Equal(t, int64(100), completed.Load())
	})

	t.Run("capacity 1 nesting does not deadlock", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithMaxGoroutines(1)
		var completed atomic.Int64
		done := make(chan struct{})
		go func() {
			p.GoScope(func(s *pool.Scope) {
				for i := 0; i < 50; i++ {
					s.Go(func() { completed.Add(1) })
				}
			})
			p.Wait()
			close(done)
		}()
		waitFor(t, done)
		require.Equal(t, int64(50), completed.Load())
	})

	t.Run("deep nesting with capacity 1", func(t *testing.T) {
		t.Parallel()
		const depth = 100
		p := pool.New().WithMaxGoroutines(1)
		var completed atomic.Int64
		done := make(chan struct{})

		var spawn func(s *pool.Scope, d int)
		spawn = func(s *pool.Scope, d int) {
			if d == depth {
				completed.Add(1)
				return
			}
			s.GoScope(func(child *pool.Scope) {
				child.Go(func() { completed.Add(1) })
				spawn(child, d+1)
			})
		}

		go func() {
			p.GoScope(func(s *pool.Scope) { spawn(s, 0) })
			p.Wait()
			close(done)
		}()
		waitFor(t, done)
		require.Equal(t, int64(depth+1), completed.Load())
	})

	t.Run("sibling scopes run concurrently up to the limit", func(t *testing.T) {
		t.Parallel()
		const maxConcurrent = 4
		p := pool.New().WithMaxGoroutines(maxConcurrent)

		var current atomic.Int64
		var observedConcurrent atomic.Bool
		var violations atomic.Int64

		p.GoScope(func(s *pool.Scope) {
			for i := 0; i < 16; i++ {
				s.GoScope(func(sib *pool.Scope) {
					cur := current.Add(1)
					if cur > maxConcurrent {
						violations.Add(1)
					}
					if cur >= 2 {
						observedConcurrent.Store(true)
					}
					time.Sleep(5 * time.Millisecond)
					current.Add(-1)
					// Each sibling has its own children.
					for j := 0; j < 3; j++ {
						sib.Go(func() {
							cur := current.Add(1)
							if cur > maxConcurrent {
								violations.Add(1)
							}
							time.Sleep(time.Millisecond)
							current.Add(-1)
						})
					}
				})
			}
			// Joining with the scope (rather than an external wait group)
			// lets this goroutine help run the queued siblings.
			s.Wait()
		})
		p.Wait()

		require.Equal(t, int64(0), violations.Load())
		require.True(t, observedConcurrent.Load(), "siblings should have run concurrently")
		require.Equal(t, int64(0), current.Load())
	})

	t.Run("concurrency limit is shared with descendants", func(t *testing.T) {
		t.Parallel()
		for _, limit := range []int{1, 2, 8} {
			limit := limit
			t.Run(strconv.Itoa(limit), func(t *testing.T) {
				t.Parallel()
				p := pool.New().WithMaxGoroutines(limit)
				var current atomic.Int64
				var violations atomic.Int64

				p.GoScope(func(s *pool.Scope) {
					for i := 0; i < 50; i++ {
						s.GoScope(func(child *pool.Scope) {
							cur := current.Add(1)
							if cur > int64(limit) {
								violations.Add(1)
							}
							time.Sleep(time.Millisecond)
							current.Add(-1)
							for j := 0; j < 5; j++ {
								child.Go(func() {
									cur := current.Add(1)
									if cur > int64(limit) {
										violations.Add(1)
									}
									time.Sleep(time.Millisecond)
									current.Add(-1)
								})
							}
						})
					}
				})
				p.Wait()
				require.Equal(t, int64(0), violations.Load())
				require.Equal(t, int64(0), current.Load())
			})
		}
	})

	t.Run("parent panic still joins accepted children", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithMaxGoroutines(1)
		var completed atomic.Int64
		started := make(chan struct{})

		require.Panics(t, func() {
			p.GoScope(func(s *pool.Scope) {
				for i := 0; i < 10; i++ {
					s.Go(func() {
						completed.Add(1)
						// Leave time for the parent to panic while children
						// are still queued.
						time.Sleep(time.Millisecond)
					})
				}
				close(started)
				panic("parent blew up")
			})
			p.Wait()
		})

		waitFor(t, started)
		require.Equal(t, int64(10), completed.Load())
	})

	t.Run("child panic propagates to pool wait", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithMaxGoroutines(1)
		var completed atomic.Int64
		require.Panics(t, func() {
			p.GoScope(func(s *pool.Scope) {
				for i := 0; i < 5; i++ {
					i := i
					s.Go(func() {
						if i == 2 {
							panic("child blew up")
						}
						completed.Add(1)
					})
				}
			})
			p.Wait()
		})
		// The panic must not stop the other accepted children from running.
		require.Equal(t, int64(4), completed.Load())
	})

	t.Run("explicit wait joins children", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithMaxGoroutines(1)
		var completed atomic.Int64
		p.GoScope(func(s *pool.Scope) {
			for i := 0; i < 10; i++ {
				s.Go(func() { completed.Add(1) })
			}
			s.Wait()
			require.Equal(t, int64(10), completed.Load())

			// The scope can be reused after an explicit wait.
			for i := 0; i < 5; i++ {
				s.Go(func() { completed.Add(1) })
			}
		})
		p.Wait()
		require.Equal(t, int64(15), completed.Load())
	})

	t.Run("saved submitter is rejected after scope ends", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithMaxGoroutines(1)
		var saved *pool.Scope
		p.GoScope(func(s *pool.Scope) {
			saved = s
			s.Go(func() {})
		})
		p.Wait()
		require.Panics(t, func() { saved.Go(func() {}) })
		require.Panics(t, func() {
			saved.GoScope(func(c *pool.Scope) {})
		})
		require.Panics(t, saved.Wait)
	})

	t.Run("leaked submitter cannot outlive scope concurrently", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithMaxGoroutines(1)
		var saved *pool.Scope
		ended := make(chan struct{})
		p.GoScope(func(s *pool.Scope) {
			saved = s
		})
		go func() {
			p.Wait()
			close(ended)
		}()
		waitFor(t, ended)
		require.Panics(t, func() { saved.Go(func() {}) })
	})

	t.Run("nested scope panic still joins grandchildren", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithMaxGoroutines(1)
		var completed atomic.Int64
		require.Panics(t, func() {
			p.GoScope(func(s *pool.Scope) {
				s.GoScope(func(child *pool.Scope) {
					for i := 0; i < 10; i++ {
						child.Go(func() {
							completed.Add(1)
							time.Sleep(time.Millisecond)
						})
					}
					panic("child blew up")
				})
			})
			p.Wait()
		})
		require.Equal(t, int64(10), completed.Load())
	})

	t.Run("pool is reusable with scopes", func(t *testing.T) {
		t.Parallel()
		var count atomic.Int64
		p := pool.New().WithMaxGoroutines(2)
		p.GoScope(func(s *pool.Scope) {
			for i := 0; i < 10; i++ {
				s.Go(func() { count.Add(1) })
			}
		})
		p.Wait()
		p.GoScope(func(s *pool.Scope) {
			for i := 0; i < 10; i++ {
				s.Go(func() { count.Add(1) })
			}
		})
		p.Wait()
		require.Equal(t, int64(20), count.Load())
	})

	t.Run("plain go and scope tasks share the limit", func(t *testing.T) {
		t.Parallel()
		p := pool.New().WithMaxGoroutines(3)
		var current atomic.Int64
		var violations atomic.Int64
		p.Go(func() {
			cur := current.Add(1)
			if cur > 3 {
				violations.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
			current.Add(-1)
		})
		p.GoScope(func(s *pool.Scope) {
			for i := 0; i < 20; i++ {
				s.Go(func() {
					cur := current.Add(1)
					if cur > 3 {
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
}

// waitFor fails the test if ch is not closed within a short timeout. It
// turns scheduler deadlocks into test failures instead of hangs.
func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for scoped work to complete")
	}
}
