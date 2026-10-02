package reedsolomon

import (
	"errors"
)

// ErrUncorrectable is returned by CorrectErrors when the number or pattern of
// errors and erasures cannot be corrected reliably.
var ErrUncorrectable = errors.New("too many errors or erasures to correct")

// ErrorCorrection extends Encoder with combined error-and-erasure decoding.
//
// Only the default GF(256) systematic Reed-Solomon encoding matrix supports
// the correction guarantee below. Encoders created with WithJerasureMatrix,
// WithPAR1Matrix, WithCauchyMatrix, WithCustomMatrix or
// WithFastOneParityMatrix return ErrNotSupported.
type ErrorCorrection interface {
	Encoder

	// CorrectErrors detects and repairs shards whose contents are wrong,
	// as well as shards that are missing.
	//
	// Missing shards are signalled with nil or zero-length slices and act as
	// known erasures. Every non-nil shard is treated as potentially corrupt:
	// CorrectErrors never assumes that a present shard is trustworthy.
	//
	// Let e be the number of missing (erased) shards and v the number of
	// silently corrupted shards. With p parity shards the decoder is
	// guaranteed to locate every corrupted shard and restore all original
	// data when
	//
	//	2*v + e <= p
	//
	// except that, because evaluation point 0 is used, when shard 0 is itself
	// missing the guaranteed bound is
	//
	//	2*v + e <= p - 1.
	//
	// Every candidate repair is verified by a full re-encode before it is
	// accepted, so a returned nil error means all data and parity shards match
	// a valid codeword. Outside the guarantee no detection is promised:
	// CorrectErrors may return ErrUncorrectable, or it may be unable to tell
	// that the data was altered. A parity re-check alone is not proof that the
	// reconstructed bytes equal the original data.
	//
	// The returned slice lists the indices (among the originally non-nil
	// shards) that were found corrupted and rewritten. Indices of shards that
	// were simply missing are not included.
	CorrectErrors(shards [][]byte) ([]int, error)
}

var _ ErrorCorrection = (*reedSolomon)(nil)

// correctWeights returns the column multipliers w_i of the alternant parity
// check matrix for evaluation points 0..n-1:
//
//	w_i = 1 / prod_{m != i} (i XOR m).
//
// With these weights the rows H[j][i] = w_i * i^j (j = 0..p-1) annihilate
// every codeword of the standard systematic Vandermonde matrix.
func correctWeights(n int) []byte {
	w := make([]byte, n)
	for i := 0; i < n; i++ {
		var prod byte = 1
		for m := 0; m < n; m++ {
			if m != i {
				prod = galMultiply(prod, byte(i)^byte(m))
			}
		}
		w[i] = galOneOver(prod)
	}
	return w
}

// paritySyndromes computes the p check equations over the present shards.
func (r *reedSolomon) paritySyndromes(shards [][]byte, w []byte, size int) [][]byte {
	syn := make([][]byte, r.parityShards)
	for j := range syn {
		syn[j] = make([]byte, size)
	}
	for i := 0; i < r.totalShards; i++ {
		if len(shards[i]) == 0 {
			continue
		}
		wi := w[i]
		for j := 0; j < r.parityShards; j++ {
			if c := galMultiply(wi, galExp(byte(i), j)); c != 0 {
				galMulSliceXor(c, shards[i][:size], syn[j], &r.o)
			}
		}
	}
	return syn
}

// gfSolve solves m*x = b over GF(256) by Gauss-Jordan elimination. It returns
// nil when the matrix is singular.
func gfSolve(m [][]byte, b []byte) []byte {
	nu := len(m)
	a := make([][]byte, nu)
	for i := range a {
		a[i] = make([]byte, nu+1)
		copy(a[i], m[i])
		a[i][nu] = b[i]
	}
	for col := 0; col < nu; col++ {
		if a[col][col] == 0 {
			for row := col + 1; row < nu; row++ {
				if a[row][col] != 0 {
					a[row], a[col] = a[col], a[row]
					break
				}
			}
			if a[col][col] == 0 {
				return nil
			}
		}
		inv := galOneOver(a[col][col])
		for c := col; c <= nu; c++ {
			a[col][c] = galMultiply(a[col][c], inv)
		}
		for row := 0; row < nu; row++ {
			if row != col && a[row][col] != 0 {
				f := a[row][col]
				for c := col; c <= nu; c++ {
					a[row][c] ^= galMultiply(f, a[col][c])
				}
			}
		}
	}
	x := make([]byte, nu)
	for i := range x {
		x[i] = a[i][nu]
	}
	return x
}

// locateErrors runs Peterson-Gorenstein-Zierler and returns the corrupted
// shard positions (excluding shard 0, which is handled by the caller).
//
// A per-column solution is only accepted when its locator explains every byte
// column, which prevents false positions from degenerate solves.
func (r *reedSolomon) locateErrors(syn [][]byte, erased []bool) []int {
	n, p := r.totalShards, r.parityShards
	size := len(syn[0])

	// Erasure locator over nonzero erased positions:
	// loc(x) = prod (1 + x*z).
	eNz := 0
	loc := []byte{1}
	for i := 1; i < n; i++ {
		if erased[i] {
			eNz++
			loc = append(loc, 0)
			for d := len(loc) - 1; d >= 1; d-- {
				loc[d] ^= galMultiply(loc[d-1], byte(i))
			}
		}
	}

	// When shard 0 is unavailable one check row cannot be used for locating.
	off := 0
	if erased[0] {
		off = 1
	}
	maxV := (p - eNz - off) / 2

	// T_j = sum_l loc[l] * S_{j-l}.
	tVal := func(j, c int) byte {
		var v byte
		for l := 0; l <= eNz; l++ {
			if idx := j - l; idx >= 0 && idx < p && loc[l] != 0 {
				v ^= galMultiply(loc[l], syn[idx][c])
			}
		}
		return v
	}

	// globalOK checks whether a candidate sigma (convolved with the erasure
	// locator) explains every byte column.
	explains := func(sigma []byte) bool {
		prod := make([]byte, eNz+len(sigma))
		for a := 0; a <= eNz; a++ {
			for b := 0; b < len(sigma); b++ {
				prod[a+b] ^= galMultiply(loc[a], sigma[b])
			}
		}
		d := len(prod) - 1
		conv := func(j, c int) byte {
			var v byte
			for l := 0; l <= d; l++ {
				if idx := j - l; idx >= 0 && idx < p && prod[l] != 0 {
					v ^= galMultiply(prod[l], syn[idx][c])
				}
			}
			return v
		}
		for c := 0; c < size; c++ {
			for j := d + 1; j < p; j++ {
				if conv(j, c) != 0 {
					return false
				}
			}
		}
		return true
	}

	var global []int
	for c := 0; c < size; c++ {
		useful := false
		for j := eNz + off + 1; j < p; j++ {
			if tVal(j, c) != 0 {
				useful = true
				break
			}
		}
		if !useful {
			continue
		}
		for nu := maxV; nu >= 1; nu-- {
			// Highest syndrome index used must stay below p.
			if eNz+off+2*nu-1 > p-1 {
				continue
			}
			m := make([][]byte, nu)
			b := make([]byte, nu)
			for a := 0; a < nu; a++ {
				m[a] = make([]byte, nu)
				for b2 := 0; b2 < nu; b2++ {
					m[a][b2] = tVal(eNz+off+nu-1+a-b2, c)
				}
				b[a] = tVal(eNz+off+nu+a, c)
			}
			sg := gfSolve(m, b)
			if sg == nil {
				continue
			}
			// sigma(x) = 1 + sg[0]x + ... + sg[nu-1]x^(nu-1) + x^nu,
			// whose roots are z^-1 of the corrupted shard positions.
			sigma := make([]byte, nu+1)
			sigma[0] = 1
			for a := 0; a < nu; a++ {
				sigma[a+1] = sg[a]
			}
			var roots []int
			for z := 1; z < n; z++ {
				if erased[z] {
					continue
				}
				var val, pow byte = 0, 1
				iz := galOneOver(byte(z))
				for d := 0; d <= nu; d++ {
					val ^= galMultiply(sigma[d], pow)
					pow = galMultiply(pow, iz)
				}
				if val == 0 {
					roots = append(roots, z)
				}
			}
			if len(roots) != nu {
				continue
			}
			// Verify the locator against all columns before accepting it.
			if !explains(sigma) {
				continue
			}
			global = roots
			break
		}
		if global != nil {
			break
		}
	}
	return global
}

func (r *reedSolomon) CorrectErrors(shards [][]byte) ([]int, error) {
	if len(shards) != r.totalShards {
		return nil, ErrTooFewShards
	}
	if err := checkShards(shards, true); err != nil {
		return nil, err
	}
	if r.matrixKind != matrixStandard {
		return nil, ErrNotSupported
	}
	if r.parityShards == 0 {
		return nil, ErrNotSupported
	}

	size := shardSize(shards)
	w := correctWeights(r.totalShards)

	// attempt repairs shards under one hypothesis for shard 0.
	// assumedBad lists non-nil shards that are treated as corrupted.
	attempt := func(assumeZeroBad bool) ([]int, bool) {
		zeroWasPresent := len(shards[0]) != 0
		erased := make([]bool, r.totalShards)
		for i := 0; i < r.totalShards; i++ {
			if len(shards[i]) == 0 || assumeZeroBad && i == 0 {
				erased[i] = true
			}
		}
		zeroAssumedBad := assumeZeroBad && zeroWasPresent
		report := func(fixed []int) []int {
			if !zeroAssumedBad {
				return fixed
			}
			out := append([]int{0}, fixed...)
			return out
		}

		rebuild := func(bad map[int]bool) ([][]byte, bool) {
			present := 0
			wk := make([][]byte, r.totalShards)
			for i := range shards {
				if !erased[i] && !bad[i] {
					wk[i] = append([]byte(nil), shards[i]...)
					present++
				}
			}
			if present < r.dataShards {
				return nil, false
			}
			if err := r.Reconstruct(wk); err != nil {
				return nil, false
			}
			syn := r.paritySyndromes(wk, w, size)
			for j := range syn {
				for _, x := range syn[j] {
					if x != 0 {
						return nil, false
					}
				}
			}
			return wk, true
		}

		syn := r.paritySyndromes(shards, w, size)
		allzero := true
		for j := range syn {
			for _, x := range syn[j] {
				if x != 0 {
					allzero = false
				}
			}
		}
		if allzero {
			if wk, ok := rebuild(map[int]bool{}); ok {
				copy(shards, wk)
				if zeroAssumedBad {
					return []int{0}, true
				}
				return []int{}, true
			}
			return nil, false
		}

		cand := r.locateErrors(syn, erased)

		eNz := 0
		for i := 1; i < r.totalShards; i++ {
			if erased[i] {
				eNz++
			}
		}
		off := 0
		if erased[0] {
			off = 1
		}
		maxV := (r.parityShards - eNz - off) / 2
		if len(cand) == 0 {
			// No corrupted shard could be located; the present data must be
			// treated as erasure-only rebuild input.
			if wk, ok := rebuild(map[int]bool{}); ok {
				copy(shards, wk)
				return report([]int{}), true
			}
			return nil, false
		}
		if len(cand) > maxV {
			return nil, false
		}
		bad := make(map[int]bool, len(cand))
		for _, z := range cand {
			bad[z] = true
		}
		wk, ok := rebuild(bad)
		if !ok {
			return nil, false
		}
		copy(shards, wk)
		return report(append([]int(nil), cand...)), true
	}

	if fixed, ok := attempt(false); ok {
		return fixed, nil
	}
	if fixed, ok := attempt(true); ok {
		return fixed, nil
	}
	return nil, ErrUncorrectable
}
