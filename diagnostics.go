package hyperloglog

import (
	"math"
	"slices"
)

// Diagnostics is a read-only snapshot of a Sketch's internal state, intended
// for observing memory layout and data distribution.
type Diagnostics struct {
	// Precision is the sketch's precision p, so the register count is
	// 1<<Precision. It is 0 for a zero-value Sketch.
	Precision uint8

	// Sparse reports whether the sketch currently uses the sparse
	// representation. A zero-value Sketch reports false: it has no
	// representation until the first insert or merge initializes one.
	Sparse bool

	// PendingMerges is the number of keys staged in the sparse temporary
	// set that have not yet been merged into the compressed list. It is
	// always 0 for dense and zero-value sketches.
	PendingMerges int

	// ZeroRegisters is the number of registers still holding 0. For a
	// sparse sketch it counts the registers no effective key maps to,
	// exactly as if the sketch had been converted to the dense
	// representation.
	ZeroRegisters uint64

	// RhoHistogram counts registers by value: RhoHistogram[r] is the
	// number of registers holding r, so RhoHistogram[0] equals
	// ZeroRegisters and the entries sum to 1<<Precision. Staging keys
	// that duplicate keys already in the compressed list are counted
	// once, by the effective key set, so the histogram is identical to
	// the one reported after conversion to the dense representation. It
	// is nil for a zero-value Sketch.
	RhoHistogram []uint64

	// RelativeStdErr is the illustrative scale 1.04/sqrt(2^Precision) of
	// the estimator's typical relative standard error at this precision.
	// It is an order-of-magnitude guide for interpreting estimates, not a
	// confidence interval: individual estimates can deviate by more, and
	// no fraction of samples is guaranteed to fall within it. It is 0 for
	// a zero-value Sketch.
	RelativeStdErr float64
}

// Diagnostics returns a read-only snapshot of sk's internal state. It never
// mutates sk: unlike Estimate it neither compacts the sparse staging set
// into the compressed list nor converts the representation, so the binary
// encoding and the internal representation are the same before and after
// the call. A zero-value Sketch can be diagnosed directly and reports
// Precision 0.
func (sk *Sketch) Diagnostics() Diagnostics {
	d := Diagnostics{
		Precision: sk.p,
		Sparse:    sk.sparse(),
	}
	if sk.p == 0 {
		return d
	}

	d.RelativeStdErr = 1.04 / math.Sqrt(float64(sk.m))
	d.RhoHistogram = make([]uint64, maxRho(sk.p)+1)

	if !sk.sparse() {
		for _, r := range sk.regs {
			d.RhoHistogram[r]++
		}
		d.ZeroRegisters = d.RhoHistogram[0]
		return d
	}

	d.PendingMerges = sk.tmpSet.Len()

	// Walk the staged keys and the compressed list in key order without
	// mutating either, observing each key of the effective merged set
	// exactly once.
	keys := make([]uint32, 0, sk.tmpSet.Len())
	sk.tmpSet.ForEach(func(k uint32) {
		keys = append(keys, k)
	})
	slices.Sort(keys)

	regValue := make(map[uint32]uint8)
	observe := func(k uint32) {
		i, r := decodeHash(k, sk.p, pp)
		if r > regValue[i] {
			regValue[i] = r
		}
	}
	iter, i := sk.sparseList.Iter(), 0
	for iter.HasNext() || i < len(keys) {
		switch {
		case !iter.HasNext():
			observe(keys[i])
			i++
		case i >= len(keys):
			observe(iter.Next())
		default:
			x1, adv := iter.Peek()
			switch x2 := keys[i]; {
			case x1 == x2:
				observe(x1)
				iter.Advance(x1, adv)
				i++
			case x1 > x2:
				observe(x2)
				i++
			default:
				observe(x1)
				iter.Advance(x1, adv)
			}
		}
	}

	for _, r := range regValue {
		d.RhoHistogram[r]++
	}
	d.ZeroRegisters = uint64(sk.m) - uint64(len(regValue))
	d.RhoHistogram[0] = d.ZeroRegisters
	return d
}
