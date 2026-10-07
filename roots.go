package decimal

import (
	"math/big"
	"strconv"
)

// IncompleteReason explains why a root isolation run did not produce a
// complete answer. Use RootIsolationResult.Complete to test for success.
type IncompleteReason int

const (
	// IsolationComplete means every real root in the search interval was
	// isolated and no unsearched region remains.
	IsolationComplete IncompleteReason = iota
	// IsolationZeroPolynomial means every coefficient is zero, so every
	// point of the interval is a root and isolation is meaningless.
	IsolationZeroPolynomial
	// IsolationDegreeLimit means the polynomial degree exceeds
	// RootIsolationOptions.MaxDegree; nothing was searched.
	IsolationDegreeLimit
	// IsolationBudgetExhausted means the operation budget
	// (RootIsolationOptions.MaxOps) ran out. Intervals that were not
	// fully searched are reported in RootIsolationResult.Pending; they
	// are NOT certified root-free.
	IsolationBudgetExhausted
	// IsolationInvalidInput means the options are inconsistent (nil or
	// misordered endpoints, non-positive target width).
	IsolationInvalidInput
)

func (r IncompleteReason) String() string {
	switch r {
	case IsolationComplete:
		return "complete"
	case IsolationZeroPolynomial:
		return "zero polynomial: every point of the interval is a root"
	case IsolationDegreeLimit:
		return "polynomial degree exceeds MaxDegree"
	case IsolationBudgetExhausted:
		return "operation budget exhausted"
	case IsolationInvalidInput:
		return "invalid isolation options"
	}
	return "unknown reason"
}

// RootInterval isolates exactly one real root of the polynomial. The root
// lies in the closed interval [Lower, Upper]; Lower == Upper means the
// root was determined exactly (e.g. an interval endpoint or a bisection
// point hit exactly). Multiplicity is the algebraic multiplicity of the
// root.
//
// Intervals returned by IsolateRealRoots are pairwise non-overlapping
// and sorted by Lower.
type RootInterval struct {
	Lower        *big.Rat
	Upper        *big.Rat
	Multiplicity int
}

// Width returns Upper - Lower.
func (r RootInterval) Width() *big.Rat {
	return new(big.Rat).Sub(r.Upper, r.Lower)
}

// IsExact reports whether the root was determined exactly (Lower == Upper).
func (r RootInterval) IsExact() bool {
	return r.Lower.Cmp(r.Upper) == 0
}

// Contains reports whether x lies in the isolating interval.
func (r RootInterval) Contains(x *big.Rat) bool {
	return r.Lower.Cmp(x) <= 0 && x.Cmp(r.Upper) <= 0
}

func (r RootInterval) String() string {
	if r.IsExact() {
		return "{" + r.Lower.RatString() + "} multiplicity " + strconv.Itoa(r.Multiplicity)
	}
	return "(" + r.Lower.RatString() + ", " + r.Upper.RatString() + ") multiplicity " + strconv.Itoa(r.Multiplicity)
}

// PendingInterval is an open interval (Lower, Upper) whose root content
// could not be determined within the resource budget. It must not be
// treated as root-free.
type PendingInterval struct {
	Lower *big.Rat
	Upper *big.Rat
}

func (p PendingInterval) String() string {
	return "(" + p.Lower.RatString() + ", " + p.Upper.RatString() + ")"
}

// RootIsolationOptions configures IsolateRealRoots. Endpoints and the
// target width are exact rationals; use NewRootIsolationOptions for
// decimal endpoints or NewRootIsolationOptionsRat for rational ones.
type RootIsolationOptions struct {
	// Lower and Upper bound the search interval [Lower, Upper]. Roots
	// exactly at an endpoint are reported as exact point roots.
	Lower *big.Rat
	Upper *big.Rat
	// TargetWidth is the desired maximum width of non-exact isolating
	// intervals. Must be positive. Intervals may end up narrower when
	// needed to separate close roots.
	TargetWidth *big.Rat
	// MaxOps bounds the number of exact-arithmetic operations
	// (polynomial evaluations, Euclidean division steps, ...). Zero or
	// negative means unlimited. When the budget runs out, the result
	// carries IsolationBudgetExhausted and the unsearched remainder in
	// Pending.
	MaxOps int64
	// MaxDegree bounds the polynomial degree. Zero or negative means
	// unlimited. When exceeded, the result carries IsolationDegreeLimit.
	MaxDegree int
}

// NewRootIsolationOptions builds options from decimal endpoints and a
// decimal target width. Decimals are converted to exact rationals.
func NewRootIsolationOptions(lower, upper, targetWidth Decimal) RootIsolationOptions {
	return RootIsolationOptions{
		Lower:       lower.Rat(),
		Upper:       upper.Rat(),
		TargetWidth: targetWidth.Rat(),
	}
}

// NewRootIsolationOptionsRat builds options from exact rational endpoints
// and a rational target width. The arguments are copied defensively.
func NewRootIsolationOptionsRat(lower, upper, targetWidth *big.Rat) RootIsolationOptions {
	return RootIsolationOptions{
		Lower:       cloneRat(lower),
		Upper:       cloneRat(upper),
		TargetWidth: cloneRat(targetWidth),
	}
}

// RootIsolationResult is the outcome of IsolateRealRoots.
type RootIsolationResult struct {
	// Roots are the isolated real roots, sorted and non-overlapping.
	Roots []RootInterval
	// Pending lists intervals whose root content is unknown because the
	// resource budget was exhausted. Empty when Complete() is true.
	Pending []PendingInterval
	// Reason tells why the run is incomplete; IsolationComplete on success.
	Reason IncompleteReason
	// OpsUsed reports how many exact-arithmetic operations were consumed.
	OpsUsed int64
}

// Complete reports whether every root in the search interval was isolated
// and no unsearched region remains.
func (r RootIsolationResult) Complete() bool {
	return r.Reason == IsolationComplete
}

// IsolateRealRoots finds all real roots of the polynomial
//
//	p(x) = coeffs[0] + coeffs[1]*x + ... + coeffs[n]*x^n
//
// inside [opts.Lower, opts.Upper] and returns pairwise non-overlapping
// isolating intervals with multiplicities.
//
// The computation is entirely symbolic: coefficients and endpoints are
// converted to exact rationals, roots are counted with Sturm sequences
// and multiplicities are obtained from a square-free factorization
// (Yun's algorithm). No float64 is used at any point, so the reported
// intervals are certified: each contains exactly one distinct root, and
// intervals not listed contain none, except intervals reported in
// Pending when the resource budget is exhausted.
//
// Special cases: the zero polynomial yields IsolationZeroPolynomial; a
// constant non-zero polynomial trivially has no roots; roots exactly at
// the interval endpoints are reported as exact point intervals.
func IsolateRealRoots(coeffs []Decimal, opts RootIsolationOptions) RootIsolationResult {
	res := RootIsolationResult{Reason: IsolationComplete}
	if opts.Lower == nil || opts.Upper == nil || opts.TargetWidth == nil ||
		opts.TargetWidth.Sign() <= 0 || opts.Lower.Cmp(opts.Upper) > 0 {
		res.Reason = IsolationInvalidInput
		return res
	}

	p := ratPolyFromDecimals(coeffs)
	deg := ratPolyDegree(p)
	switch {
	case deg < 0:
		res.Reason = IsolationZeroPolynomial
		return res
	case deg == 0:
		// Non-zero constant: no roots anywhere.
		return res
	}
	if opts.MaxDegree > 0 && deg > opts.MaxDegree {
		res.Reason = IsolationDegreeLimit
		res.Pending = []PendingInterval{{cloneRat(opts.Lower), cloneRat(opts.Upper)}}
		return res
	}

	budget := &opBudget{limit: opts.MaxOps}
	if !budget.charge(int64(len(coeffs))) {
		res.Reason = IsolationBudgetExhausted
		res.Pending = []PendingInterval{{cloneRat(opts.Lower), cloneRat(opts.Upper)}}
		res.OpsUsed = budget.used
		return res
	}

	iso := &rootIsolator{
		width:  opts.TargetWidth,
		budget: budget,
		lo:     opts.Lower,
		hi:     opts.Upper,
	}

	factors, ok := squareFreeFactorize(ratPolyMonic(p), budget)
	if !ok {
		iso.fail()
	} else {
		for i, f := range factors {
			if ratPolyDegree(f.factor) <= 0 {
				continue
			}
			if !iso.isolateFactor(f.factor, f.multiplicity) {
				// The remaining factors are unsearched as well.
				for _, rest := range factors[i+1:] {
					if ratPolyDegree(rest.factor) > 0 {
						iso.fail()
						break
					}
				}
				break
			}
		}
	}

	if iso.failed {
		res.Reason = IsolationBudgetExhausted
	}
	res.Roots = iso.finishRoots()
	res.Pending = mergePending(iso.pending)
	res.OpsUsed = budget.used
	return res
}
