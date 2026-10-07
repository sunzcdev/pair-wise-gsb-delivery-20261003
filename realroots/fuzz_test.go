package realroots

import (
	"math"
	"math/rand"
	"testing"

	"github.com/shopspring/decimal"
)

// buildPolyFromRoots returns the polynomial product_i (x - root_i)^mult_i as
// exact decimal coefficients, alongside the (root, multiplicity) list.
func buildPolyFromRoots(roots []int, mults []int) []decimal.Decimal {
	// Start from 1, multiply by (x - r) repeatedly (all integer arithmetic).
	cur := []int64{1}
	for i, r := range roots {
		factor := []int64{int64(-r), 1}
		for m := 0; m < mults[i]; m++ {
			next := make([]int64, len(cur)+len(factor)-1)
			for a := range cur {
				for b := range factor {
					next[a+b] += cur[a] * factor[b]
				}
			}
			cur = next
		}
	}
	out := make([]decimal.Decimal, len(cur))
	for i, v := range cur {
		out[i] = decimal.NewFromInt(v)
	}
	return out
}

func TestRandomRootInventory(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 200; trial++ {
		// Pick distinct integer roots in a small range.
		perm := rng.Perm(21)
		n := 1 + rng.Intn(5)
		roots := make([]int, n)
		used := map[int]bool{}
		for i := range roots {
			r := perm[i] - 10
			for used[r] {
				r = rng.Intn(21) - 10
			}
			used[r] = true
			roots[i] = r
		}
		mults := make([]int, n)
		for i := range mults {
			mults[i] = 1 + rng.Intn(3)
		}
		p := buildPolyFromRoots(roots, mults)

		o := Options{
			Low:         d("-10.5"),
			High:        d("10.5"),
			IncludeHigh: true,
			TargetWidth: d("0.0001"),
		}
		res, err := Isolate(p, o)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Complete {
			t.Fatalf("trial %d: expected complete, pending %+v", trial, res.Pending)
		}
		if len(res.Roots) != n {
			t.Fatalf("trial %d roots=%v mults=%v: got %d roots %+v",
				trial, roots, mults, len(res.Roots), res.Roots)
		}

		matched := make([]bool, n)
		var prevHigh float64 = math.Inf(-1)
		for _, ri := range res.Roots {
			if ri.Width().Cmp(o.TargetWidth) > 0 {
				t.Fatalf("trial %d: width %s too big", trial, ri.Width())
			}
			lo, _ := ri.Low.Float64()
			hi, _ := ri.High.Float64()
			if lo < prevHigh {
				t.Fatalf("trial %d: overlapping intervals", trial)
			}
			prevHigh = hi
			found := -1
			for j, r := range roots {
				rf := float64(r)
				if lo <= rf && rf <= hi && !matched[j] {
					found = j
					break
				}
			}
			if found < 0 {
				t.Fatalf("trial %d roots=%v: interval %s..%s matches no root",
					trial, roots, ri.Low, ri.High)
			}
			if ri.Multiplicity != mults[found] {
				t.Fatalf("trial %d: root %d mult want %d got %d",
					trial, roots[found], mults[found], ri.Multiplicity)
			}
			matched[found] = true
		}
	}
}
