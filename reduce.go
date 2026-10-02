package hyperloglog

import (
	"errors"
	"fmt"
	"math/bits"
)

// ErrorPrecisionIncrease is returned, wrapped, by ReducePrecision when the
// requested precision exceeds the sketch's current precision. Precision can
// only be lowered: the hash bits a lower precision would have indexed on are
// gone and cannot be brought back.
var ErrorPrecisionIncrease = errors.New("precision cannot be increased")

// ReducePrecision returns a new Sketch at precision p, leaving sk unchanged.
// p must be a valid precision no greater than sk's: several old registers
// fold into each new one, and the index bits dropped by the fold were the
// top bits of w, so they lead the new rho run instead of being thrown away.
// Raising precision is impossible and returns ErrorPrecisionIncrease;
// reducing to sk's own precision returns a Clone. A zero-value sk behaves
// like New(): reducing it yields an empty sketch at p, or
// ErrorPrecisionIncrease for p > 14. The sparse or dense representation is
// preserved.
func (sk *Sketch) ReducePrecision(p uint8) (*Sketch, error) {
	if sk.p == 0 {
		return New().ReducePrecision(p)
	}
	if err := checkPrecision(p); err != nil {
		return nil, err
	}
	if p > sk.p {
		return nil, fmt.Errorf("hyperloglog: cannot reduce precision %d to %d: %w", sk.p, p, ErrorPrecisionIncrease)
	}
	if p == sk.p {
		return sk.Clone(), nil
	}

	reduced := &Sketch{
		p:     p,
		m:     uint32(1) << p,
		alpha: alpha(float64(uint32(1) << p)),
	}
	if !sk.sparse() {
		reduced.regs = make([]uint8, reduced.m)
		for i, r := range sk.regs {
			if r == 0 {
				continue
			}
			i2, r2 := foldIndexRho(uint32(i), r, sk.p, p)
			if r2 > reduced.regs[i2] {
				reduced.regs[i2] = r2
			}
		}
		return reduced, nil
	}

	reduced.tmpSet = makeSet(0)
	reduced.sparseList = newCompressedList(0)
	// Folding maps several old indices onto each new one; keep only the
	// largest rho per new index so the sparse keys stay canonical.
	folded := make(map[uint32]uint8, sk.tmpSet.Len())
	fold := func(k uint32) {
		i, r := decodeHash(k, sk.p, pp)
		i, r = foldIndexRho(i, r, sk.p, p)
		if r > folded[i] {
			folded[i] = r
		}
	}
	sk.tmpSet.ForEach(fold)
	for iter := sk.sparseList.Iter(); iter.HasNext(); {
		fold(iter.Next())
	}
	for i, r := range folded {
		reduced.tmpSet.add(encodeIndexRho(i, r, p, pp))
	}
	return reduced, nil
}

// MergeDown adds other to sk like Merge, but tolerates different precisions:
// whichever sketch has the higher precision is first folded with
// ReducePrecision, so sk ends up at the lower of the two precisions. Nil and
// zero-value others are treated as empty, and a zero-value sk adopts other's
// precision and representation, exactly as Merge does. Use Merge to reject
// precision mismatches instead.
func (sk *Sketch) MergeDown(other *Sketch) error {
	if other == nil || other.p == 0 {
		return nil
	}
	if sk.p == 0 || sk.p == other.p {
		return sk.Merge(other)
	}
	target := min(sk.p, other.p)
	if sk.p > target {
		reduced, err := sk.ReducePrecision(target)
		if err != nil {
			return err
		}
		*sk = *reduced
	}
	if other.p > target {
		reduced, err := other.ReducePrecision(target)
		if err != nil {
			return err
		}
		other = reduced
	}
	return sk.Merge(other)
}

// foldIndexRho maps a register index i and its rho at precision p to the
// index and rho they produce at the lower precision p2. The d = p - p2 index
// bits the fold drops were the top d bits of w, so they lead the new rho
// run: a set bit among them caps the new rho inside those d bits, and only
// when they are all zero does the old rho follow them.
func foldIndexRho(i uint32, r uint8, p, p2 uint8) (uint32, uint8) {
	d := p - p2
	k := i & (uint32(1)<<d - 1)
	if k == 0 {
		return i >> d, r + d
	}
	return i >> d, d - uint8(bits.Len32(k)) + 1
}

// encodeIndexRho is the inverse of decodeHash: it builds the sparse key for
// register index i with rho r at precision p. ReducePrecision needs it
// because folding recomputes index and rho without access to the hash they
// came from.
func encodeIndexRho(i uint32, r uint8, p, pp uint8) uint32 {
	if r <= pp-p {
		// The rho run ends inside the pp bit window: the short key form
		// stores the index and the run's terminating 1 bit.
		return (i<<(pp-p) | uint32(1)<<(pp-p-r)) << 1
	}
	// The rho run spans the whole window: the long key form stores the
	// index and the zeros beyond it.
	return i<<(pp-p)<<7 | uint32(r-(pp-p))<<1 | 1
}
