package hyperloglog

import "math"

// Representation identifies which internal storage a Sketch currently uses.
type Representation uint8

const (
	// Uninitialized is the representation of a zero-value Sketch: no
	// precision, no registers, no sparse state.
	Uninitialized Representation = iota
	// Sparse is the sparse representation: a tmp set of pending keys plus a
	// compressed list.
	Sparse
	// Dense is the dense representation: one register byte per index.
	Dense
)

func (r Representation) String() string {
	switch r {
	case Sparse:
		return "sparse"
	case Dense:
		return "dense"
	default:
		return "uninitialized"
	}
}

// Diagnostics is a read-only snapshot of a Sketch's internal state, intended
// for observing memory layout and data distribution.
type Diagnostics struct {
	// Precision is the sketch's p; it is 0 for a zero-value Sketch.
	Precision uint8
	// Representation reports whether the sketch is currently sparse, dense,
	// or uninitialized.
	Representation Representation
	// Pending is the number of tmp set keys not yet merged into the
	// compressed list. It is always 0 for dense and uninitialized sketches.
	Pending int
	// ZeroRegisters is the number of registers that still hold 0. For a
	// sparse sketch it counts every register no sparse key maps to, matching
	// what the dense representation would report.
	ZeroRegisters uint32
	// RhoHistogram has maxRho(Precision)+1 entries; entry r is the number of
	// registers holding rho r, with entry 0 counting zero registers. It is
	// computed over the effective register set, so duplicate sparse keys and
	// registers touched by both the tmp set and the compressed list are
	// counted once, at the maximum rho, exactly as converting to the dense
	// representation would. It is nil for a zero-value Sketch.
	RhoHistogram []uint32
	// StandardError is the illustrative relative standard error of the
	// estimate at this precision, 1.04/sqrt(m). It is a distributional
	// scale, not a bound: any individual estimate may deviate from the true
	// cardinality by more than this, and it is not a confidence guarantee
	// that holds for every sample. It is 0 for a zero-value Sketch.
	StandardError float64
}

// Diagnostics returns a read-only snapshot of sk's internal state. It never
// mutates sk: unlike Estimate it does not compact the sparse representation,
// so the tmp set, the compressed list, the registers, and the serialized form
// of sk are all exactly the same before and after the call. A zero-value
// Sketch can be diagnosed directly.
func (sk *Sketch) Diagnostics() Diagnostics {
	d := Diagnostics{
		Precision:      sk.p,
		Representation: Uninitialized,
	}
	if sk.p == 0 {
		return d
	}
	d.StandardError = 1.04 / math.Sqrt(float64(sk.m))
	d.RhoHistogram = make([]uint32, maxRho(sk.p)+1)

	if !sk.sparse() {
		d.Representation = Dense
		for _, r := range sk.regs {
			d.RhoHistogram[r]++
		}
		d.ZeroRegisters = d.RhoHistogram[0]
		return d
	}

	d.Representation = Sparse
	d.Pending = sk.tmpSet.Len()

	// Fold the tmp set and the compressed list into the effective register
	// set, keeping the maximum rho per index just as toNormal's insert
	// would, so duplicates across and within the two are counted once.
	touched := make(map[uint32]uint8, sk.tmpSet.Len()+int(sk.sparseList.count))
	fold := func(k uint32) {
		i, r := decodeHash(k, sk.p, pp)
		if r > touched[i] {
			touched[i] = r
		}
	}
	sk.tmpSet.ForEach(fold)
	for iter := sk.sparseList.Iter(); iter.HasNext(); {
		fold(iter.Next())
	}
	for _, r := range touched {
		d.RhoHistogram[r]++
	}
	d.ZeroRegisters = sk.m - uint32(len(touched))
	d.RhoHistogram[0] = d.ZeroRegisters
	return d
}
