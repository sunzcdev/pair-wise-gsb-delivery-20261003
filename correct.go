package reedsolomon

import (
	"errors"
	"sort"
)

// ErrCannotCorrect is returned by CorrectErrors when the shards cannot be
// repaired, because the number of corrupted and missing shards exceeds the
// correction capability of the code, or the corruption could not be located.
var ErrCannotCorrect = errors.New("cannot correct shards: too many corrupted or missing shards")

// ErrorCorrection is an optional interface that adds correction of corrupted
// shards on top of reconstruction of missing shards.
//
// It is implemented by encoders created with New that use the default
// Vandermonde-based matrix and have at least one parity shard.
// Encoders built WithCauchyMatrix, WithPAR1Matrix, WithJerasureMatrix,
// WithCustomMatrix or WithFastOneParityMatrix, and the leopard based
// encoders, do not support it; CorrectErrors returns ErrNotSupported there.
type ErrorCorrection interface {
	// CorrectErrors repairs a shard set where some shards are missing and
	// some of the remaining shards may be corrupted.
	//
	// The length of the array must be equal to Shards. You indicate that a
	// shard is missing by setting it to nil or zero-length. Every non-empty
	// shard is treated as UNTRUSTED: its content may have been altered, and
	// CorrectErrors will locate and repair such corruption instead of
	// blindly trusting it.
	//
	// Correction capability: with p parity shards the code has minimum
	// distance p+1, so recovery of the original data is guaranteed while
	//
	//	2*e + s <= p
	//
	// where e is the number of corrupted (non-missing) shards and s is the
	// number of missing shards. Within this bound the returned slice holds
	// the sorted indices of the shards that were found corrupted and were
	// repaired (missing shards are not included), and all shards are
	// restored to the original encoded data.
	//
	// Beyond this bound detection is NOT guaranteed: CorrectErrors may
	// return ErrCannotCorrect, but it may also return success with data
	// that differs from the original. A successful return - like a
	// successful Verify or a re-encode comparison - only proves that the
	// result is a valid codeword consistent with the supplied shards, never
	// that it equals the original data. Only the 2*e+s <= p bound turns a
	// successful result into a guarantee.
	//
	// On success, missing shards are replaced by newly allocated slices.
	// On error the shards are not modified.
	//
	// If you know that intact shards can be trusted, Reconstruct is the
	// faster erasure-only path and should be preferred.
	CorrectErrors(shards [][]byte) (corrupted []int, err error)
}

var _ = ErrorCorrection(&reedSolomon{})

// CorrectErrors implements ErrorCorrection.
func (r *reedSolomon) CorrectErrors(shards [][]byte) ([]int, error) {
	if len(shards) != r.totalShards {
		return nil, ErrTooFewShards
	}
	// Error location relies on the code being the Reed-Solomon evaluation
	// code produced by the default Vandermonde-based matrix.
	if r.parityShards == 0 || r.o.customMatrix != nil || r.o.useCauchy ||
		r.o.usePAR1Matrix || r.o.useJerasureMatrix ||
		(r.o.fastOneParity && r.parityShards == 1) {
		return nil, ErrNotSupported
	}
	if err := checkShards(shards, true); err != nil {
		return nil, err
	}
	size := shardSize(shards)

	missing := make([]int, 0, r.parityShards)
	wasMissing := make([]bool, r.totalShards)
	for i := range shards {
		if len(shards[i]) == 0 {
			missing = append(missing, i)
			wasMissing[i] = true
		}
	}
	if len(missing) > r.parityShards {
		return nil, ErrTooFewShards
	}
	if len(missing) == 0 {
		// Fast path: if parity is consistent there is nothing to repair.
		ok, err := r.Verify(shards)
		if err != nil {
			return nil, err
		}
		if ok {
			return nil, nil
		}
	}

	// The default matrix encodes data d as y = V*(T^-1*d), i.e. shard i
	// holds f(i) for a polynomial f of degree < dataShards, evaluated at
	// the field element with representation byte(i). The dual code has
	// multipliers u[i] = 1/prod_{j!=i}(i^j), so the syndromes
	// S_j = sum_i u[i] * i^j * y_i, j = 0..parityShards-1, vanish exactly
	// for valid codewords and have the geometric structure needed for
	// Berlekamp-Massey below.
	u := make([]byte, r.totalShards)
	for i := range u {
		prod := byte(1)
		for j := range u {
			if i != j {
				prod = galMultiply(prod, byte(i)^byte(j))
			}
		}
		u[i] = galOneOver(prod)
	}

	// Syndromes of the received word; missing shards contribute zero.
	syn := AllocAligned(r.parityShards, size)
	for i, shard := range shards {
		if len(shard) == 0 {
			continue
		}
		coef := u[i]
		for j := 0; j < r.parityShards; j++ {
			if coef != 0 {
				galMulSliceXor(coef, shard, syn[j], &r.o)
			}
			coef = galMultiply(coef, byte(i))
		}
	}

	if allZeroShards(syn) {
		// The zero-filled word is a codeword. Since at most parityShards
		// shards were replaced by zeros and the minimum distance is
		// parityShards+1, it is the original codeword: the missing shards
		// contained zeros.
		for _, i := range missing {
			shards[i] = AllocAligned(1, size)[0]
		}
		return nil, nil
	}

	// Fast path: attempt erasure-only correction. If no shard is corrupted
	// this succeeds and avoids the error locating step entirely.
	if len(missing) > 0 && r.solveAndApply(shards, missing, wasMissing, u, syn, size) {
		return nil, nil
	}

	// Errors are present (or the erasure-only attempt was inconsistent).
	// Remove the erasure contribution from the syndromes (Forney syndromes)
	// so Berlekamp-Massey only sees the unknown error locations.
	erasures := len(missing)
	fsCount := r.parityShards - erasures
	if fsCount == 0 {
		return nil, ErrCannotCorrect
	}
	gamma := errLocatorPoly(missing)
	fs := AllocAligned(fsCount, size)
	for h := 0; h < fsCount; h++ {
		for i := 0; i <= erasures; i++ {
			if gamma[i] != 0 {
				galMulSliceXor(gamma[i], syn[h+i], fs[h], &r.o)
			}
		}
	}

	errLocs, err := r.locateErrors(fs)
	if err != nil {
		return nil, err
	}
	if !r.solveAndApply(shards, mergeSortedInts(missing, errLocs), wasMissing, u, syn, size) {
		return nil, ErrCannotCorrect
	}
	return errLocs, nil
}

// locateErrors finds the corrupted shard indices from Forney syndromes.
// fs[h] holds Forney syndrome h for every byte position; len(fs) is
// parityShards-erasures, so at most len(fs)/2 error locations can be found.
func (r *reedSolomon) locateErrors(fs [][]byte) ([]int, error) {
	nSyn := len(fs)
	size := len(fs[0])
	locator := []byte{1}
	var errLocs []int
	seq := make([]byte, nSyn)
	for pos := 0; pos < size; pos++ {
		zero := true
		for h := 0; h < nSyn; h++ {
			seq[h] = fs[h][pos]
			if seq[h] != 0 {
				zero = false
			}
		}
		if zero || explainsSeq(locator, seq) {
			continue
		}
		// The syndrome sequence at this position is not explained by the
		// locations found so far; locate the shards corrupted here.
		c, l := berlekampMassey(seq)
		// c is the connection polynomial prod(1 + a_i*x); reversing it
		// gives prod(x + a_i) with roots at the evaluation points, which
		// are the shard indices themselves (including shard 0).
		phi := make([]byte, l+1)
		for i := 0; i <= l; i++ {
			phi[i] = c[l-i]
		}
		var found []int
		for pt := 0; pt < r.totalShards; pt++ {
			if polyEval(phi, byte(pt)) == 0 {
				found = append(found, pt)
			}
		}
		if len(found) != l {
			return nil, ErrCannotCorrect
		}
		added := false
		for _, f := range found {
			if !containsInt(errLocs, f) {
				errLocs = append(errLocs, f)
				added = true
			}
		}
		// Within the guaranteed bound every located error is new; no
		// progress means the corruption is beyond repair.
		if !added || 2*len(errLocs) > nSyn {
			return nil, ErrCannotCorrect
		}
		sort.Ints(errLocs)
		locator = errLocatorPoly(errLocs)
	}
	return errLocs, nil
}

// solveAndApply solves for the error values at the given locations and, if
// the corrected word is a consistent codeword, applies the corrections to
// the shards and returns true. If the locations cannot explain the
// syndromes, nothing is modified and false is returned.
func (r *reedSolomon) solveAndApply(shards [][]byte, locs []int, wasMissing []bool, u []byte, syn [][]byte, size int) bool {
	t := len(locs)
	if t == 0 || t > r.parityShards {
		return false
	}
	// The first t syndromes give a t-by-t Vandermonde system (columns
	// scaled by the non-zero dual multipliers) for the error values.
	m, err := newMatrix(t, t)
	if err != nil {
		return false
	}
	for col, loc := range locs {
		coef := u[loc]
		for row := 0; row < t; row++ {
			m[row][col] = coef
			coef = galMultiply(coef, byte(loc))
		}
	}
	inv, err := m.Invert()
	if err != nil {
		return false
	}
	// corr[i] holds the value to XOR into shard locs[i].
	corr := AllocAligned(t, size)
	r.codeSomeShards(inv, syn[:t], corr, size, true)

	// The remaining syndromes must vanish after correction, otherwise the
	// assumed locations cannot explain the received word.
	if t < r.parityShards {
		tmp := AllocAligned(1, size)[0]
		for j := t; j < r.parityShards; j++ {
			copy(tmp, syn[j])
			for i, loc := range locs {
				coef := galMultiply(u[loc], galExp(byte(loc), j))
				if coef != 0 {
					galMulSliceXor(coef, corr[i], tmp, &r.o)
				}
			}
			if !isZero(tmp) {
				return false
			}
		}
	}

	for i, loc := range locs {
		if wasMissing[loc] {
			// The correction of a zero-filled shard is its content.
			shards[loc] = corr[i]
		} else {
			sliceXor(corr[i], shards[loc], &r.o)
		}
	}
	return true
}

// berlekampMassey finds the minimal connection polynomial c (c[0] == 1) of
// the syndrome sequence s and its degree l, such that
// sum_{i=0..l} c[i]*s[n-i] == 0 for all n >= l.
func berlekampMassey(s []byte) (c []byte, l int) {
	c = []byte{1}
	b := []byte{1}
	l = 0
	m := 1
	bb := byte(1)
	for n := 0; n < len(s); n++ {
		d := s[n]
		for i := 1; i <= l; i++ {
			d ^= galMultiply(c[i], s[n-i])
		}
		if d == 0 {
			m++
			continue
		}
		coef := galDivide(d, bb)
		if 2*l <= n {
			t := c
			c = polyShiftXor(c, b, m, coef)
			l = n + 1 - l
			b = t
			bb = d
			m = 1
		} else {
			c = polyShiftXor(c, b, m, coef)
			m++
		}
	}
	return c, l
}

// polyShiftXor returns c with coef*x^shift*b added (XORed) into it.
func polyShiftXor(c, b []byte, shift int, coef byte) []byte {
	if need := len(b) + shift; len(c) < need {
		grown := make([]byte, need)
		copy(grown, c)
		c = grown
	}
	for i, v := range b {
		c[i+shift] ^= galMultiply(coef, v)
	}
	return c
}

// errLocatorPoly returns prod_{i in locs}(x + byte(i)), coefficients from
// lowest to highest degree.
func errLocatorPoly(locs []int) []byte {
	poly := []byte{1}
	for _, loc := range locs {
		a := byte(loc)
		next := make([]byte, len(poly)+1)
		for i, v := range poly {
			next[i] ^= galMultiply(v, a)
			next[i+1] ^= v
		}
		poly = next
	}
	return poly
}

// explainsSeq reports whether the sequence seq satisfies the linear
// recurrence given by the monic locator polynomial phi.
func explainsSeq(phi, seq []byte) bool {
	d := len(phi) - 1
	for h := 0; h+d < len(seq); h++ {
		var acc byte
		for i := 0; i <= d; i++ {
			acc ^= galMultiply(phi[i], seq[h+i])
		}
		if acc != 0 {
			return false
		}
	}
	return true
}

// polyEval evaluates a polynomial with coefficients from lowest to highest
// degree at x.
func polyEval(p []byte, x byte) byte {
	r := p[len(p)-1]
	for i := len(p) - 2; i >= 0; i-- {
		r = galMultiply(r, x) ^ p[i]
	}
	return r
}

func allZeroShards(shards [][]byte) bool {
	for _, shard := range shards {
		if !isZero(shard) {
			return false
		}
	}
	return true
}

func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func containsInt(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// mergeSortedInts returns the sorted union of two sorted slices.
func mergeSortedInts(a, b []int) []int {
	out := make([]int, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return append(append(out, a[i:]...), b[j:]...)
}
