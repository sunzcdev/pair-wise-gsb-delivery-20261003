package pool_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/conc/pool"
)

func TestStressScopes(t *testing.T) {
	caps := []int{1, 2, 3, 7}
	for iter := 0; iter < 30; iter++ {
		for _, c := range caps {
			var cur, peak, viol atomic.Int64
			p := pool.New().WithMaxGoroutines(c)

			for root := 0; root < 5; root++ {
				p.Go(func() {
					v := cur.Add(1)
					updatePeak(&peak, v)
					if v > int64(c) {
						viol.Add(1)
					}
					time.Sleep(time.Millisecond)
					cur.Add(-1)
				})
			}

			p.GoScoped(func(s *pool.Scope) {
				for i := 0; i < 8; i++ {
					s.GoScoped(func(child *pool.Scope) {
						v := cur.Add(1)
						updatePeak(&peak, v)
						if v > int64(c) {
							viol.Add(1)
						}
						for k := 0; k < 3; k++ {
							child.Go(func() {
								time.Sleep(time.Millisecond)
							})
						}
						cur.Add(-1)
					})
				}
			})
			p.Wait()
			if viol.Load() != 0 {
				t.Fatalf("cap=%d iter=%d limit violated", c, iter)
			}
			if cur.Load() != 0 {
				t.Fatalf("cap=%d iter=%d tasks still running", c, iter)
			}
		}
	}
}

func updatePeak(peak *atomic.Int64, v int64) {
	for {
		old := peak.Load()
		if v <= old || peak.CompareAndSwap(old, v) {
			return
		}
	}
}
