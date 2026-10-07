package decimal

import (
	"fmt"
	"math"
	"math/big"
)

// DefaultFusedMaxDigits is the computation budget used by FusedMultiplyAdd
// and DotProduct when FusedOptions.MaxDigits is zero or negative.
//
// The budget caps how many decimal digits the intermediate integers may hold.
// It bounds the cost of exact exponent alignment: without it, adding terms with
// wildly different exponents would force the allocation of arbitrarily large
// big.Int values.
const DefaultFusedMaxDigits = 100000

// fusedError is a sentinel error. It is comparable directly with == (so the
// package keeps working on Go 1.10) and also implements Is, letting callers use
// errors.Is on Go 1.13 and later.
type fusedError string

func (e fusedError) Error() string { return string(e) }

func (e fusedError) Is(target error) bool {
	t, ok := target.(fusedError)
	return ok && t == e
}

var (
	// ErrFusedBudgetExceeded is returned when an exact intermediate result
	// (a product, an exponent alignment, or the running sum) would need more
	// than FusedOptions.MaxDigits decimal digits. The operation is rejected
	// instead of falling back to float64 or truncating the intermediates.
	ErrFusedBudgetExceeded fusedError = "decimal: fused computation budget exceeded"

	// ErrFusedExponentOverflow is returned when an exact product exponent
	// falls outside the range supported by Decimal (int32).
	ErrFusedExponentOverflow fusedError = "decimal: fused product exponent overflows int32"

	// ErrFusedInvalidRounding is returned when FusedOptions.Rounding does not
	// name one of the documented RoundingMode values.
	ErrFusedInvalidRounding fusedError = "decimal: invalid rounding mode"

	// ErrFusedInvalidPrecision is returned when FusedOptions.Precision is
	// math.MinInt32, because the result exponent -precision would overflow.
	ErrFusedInvalidPrecision fusedError = "decimal: invalid precision"
)

// RoundingMode selects how the single, final rounding step of a fused
// operation is performed. Intermediate products and sums are always kept
// exact; only the final result is rounded.
type RoundingMode int

const (
	// RoundHalfUp rounds towards the nearest value; a value exactly halfway
	// between two candidates is rounded away from zero.
	RoundHalfUp RoundingMode = iota

	// RoundHalfDown rounds towards the nearest value; a value exactly halfway
	// between two candidates is rounded towards zero.
	RoundHalfDown

	// RoundHalfEven rounds towards the nearest value; a value exactly halfway
	// between two candidates is rounded towards the candidate whose last digit
	// is even (banker's rounding).
	RoundHalfEven

	// RoundFusedFloor rounds towards negative infinity.
	RoundFusedFloor

	// RoundFusedCeil rounds towards positive infinity.
	RoundFusedCeil

	// RoundFusedDown rounds towards zero (truncation).
	RoundFusedDown
)

func (m RoundingMode) valid() bool {
	return m >= RoundHalfUp && m <= RoundFusedDown
}

// FusedOptions configures a fused multiply-add or dot product.
//
// Precision is the number of digits after the decimal point of the returned
// value; it may be negative to round the integer part (for example -2 rounds
// to hundreds). The result is always an exact multiple of 10^(-Precision).
//
// Rounding selects the rounding applied exactly once, after the exact sum has
// been computed.
//
// MaxDigits is the computation budget measured in decimal digits of the exact
// intermediate integers. If zero or negative, DefaultFusedMaxDigits is used.
// Exceeding the budget makes the operation fail with ErrFusedBudgetExceeded;
// it never silently degrades to float64.
type FusedOptions struct {
	Precision int32
	Rounding  RoundingMode
	MaxDigits int
}

func (opts FusedOptions) normalized() (FusedOptions, error) {
	if opts.Precision == math.MinInt32 {
		return opts, ErrFusedInvalidPrecision
	}
	if !opts.Rounding.valid() {
		return opts, ErrFusedInvalidRounding
	}
	if opts.MaxDigits <= 0 {
		opts.MaxDigits = DefaultFusedMaxDigits
	}
	return opts, nil
}

// FusedMultiplyAdd computes x*y+add exactly and rounds the result once.
//
// The product x*y and the addition are performed without any intermediate
// rounding, so small residuals left after large numbers cancel are preserved.
// The only rounding step is the final one, performed according to opts.
//
// Example: with Precision 2,
//
//	(2 * 500000000000000000.002) + (-999999999999999999.998)
//
// equals 0.006 exactly and therefore rounds to 0.01. Rounding the product and
// the addend separately first would erase the residual and return 0.
//
// An empty addend (the zero Decimal) is simply not added.
func FusedMultiplyAdd(x, y, add Decimal, opts FusedOptions) (Decimal, error) {
	product, err := exactProduct(x, y, opts)
	if err != nil {
		return Decimal{}, err
	}
	terms := make([]Decimal, 0, 2)
	if product.getValue().Sign() != 0 {
		terms = append(terms, product)
	}
	if add.getValue().Sign() != 0 {
		terms = append(terms, add)
	}
	return fusedEvaluate(terms, opts)
}

// DotProduct computes the finite dot product sum(a[i]*b[i]) exactly and rounds
// the result once.
//
// All products are kept exact and added exactly; only the final total is
// rounded according to opts. Products that cancel leave their true residual
// instead of the zero produced by rounding each product first.
//
// An empty pair of slices (or slices containing only zero products) yields the
// zero multiple of 10^(-Precision). Zero products are ignored entirely, so they
// never enlarge the exponent alignment. The slices must have equal length.
func DotProduct(a, b []Decimal, opts FusedOptions) (Decimal, error) {
	if len(a) != len(b) {
		return Decimal{}, fmt.Errorf("decimal: DotProduct slice length mismatch: %d != %d", len(a), len(b))
	}

	terms := make([]Decimal, 0, len(a))
	for i := range a {
		product, err := exactProduct(a[i], b[i], opts)
		if err != nil {
			return Decimal{}, err
		}
		if product.getValue().Sign() != 0 {
			terms = append(terms, product)
		}
	}
	return fusedEvaluate(terms, opts)
}

// exactProduct returns x*y with its exact exponent, enforcing the digit budget
// before the potentially large multiplication is carried out.
func exactProduct(x, y Decimal, opts FusedOptions) (Decimal, error) {
	xv := x.getValue()
	yv := y.getValue()

	// A zero product carries no magnitude: its exponent is irrelevant and it
	// will be dropped by the caller, so extreme operand exponents are harmless.
	if xv.Sign() == 0 || yv.Sign() == 0 {
		return Decimal{value: new(big.Int)}, nil
	}

	exp := int64(x.exp) + int64(y.exp)
	if exp < math.MinInt32 || exp > math.MaxInt32 {
		return Decimal{}, ErrFusedExponentOverflow
	}

	maxDigits := opts.MaxDigits
	if maxDigits <= 0 {
		maxDigits = DefaultFusedMaxDigits
	}

	dx := (Decimal{value: xv}).NumDigits()
	dy := (Decimal{value: yv}).NumDigits()

	// The product coefficient has dx+dy or dx+dy-1 digits and its exponent is
	// exp. Its overall magnitude therefore spans (coeffDigits + exp) digits
	// for a positive exp. Reject when even the smaller possibility cannot fit,
	// then verify the actual size afterwards.
	exp32 := int32(exp)
	minCoeffDigits := dx + dy - 1
	if decimalDigits(minCoeffDigits, exp32) > maxDigits {
		return Decimal{}, ErrFusedBudgetExceeded
	}

	value := new(big.Int).Mul(xv, yv)

	// Strip trailing zero factors so the exponent reflects the product's true
	// precision. Operands commonly carry trailing zeros (for example the parsed
	// string "1000000.006" is 1000000006 * 10^-3); keeping those zeros as
	// coefficient digits would let them be mistaken for the small residuals that
	// survive a cancellation.
	normalizedExp := exp32
	for normalizedExp < math.MaxInt32 {
		q, r := new(big.Int).QuoRem(value, tenInt, new(big.Int))
		if r.Sign() != 0 {
			break
		}
		value = q
		normalizedExp++
	}

	if decimalDigits((Decimal{value: value}).NumDigits(), normalizedExp) > maxDigits {
		return Decimal{}, ErrFusedBudgetExceeded
	}

	return Decimal{value: value, exp: normalizedExp}, nil
}

// decimalDigits bounds how many decimal digits a coefficient with coeffDigits
// digits contributes at the given exponent. A positive exponent shifts the
// value left and must be counted; a negative exponent does not enlarge the
// integer representation.
func decimalDigits(coeffDigits int, exp int32) int {
	if exp > 0 {
		return coeffDigits + int(exp)
	}
	return coeffDigits
}

// fusedEvaluate sums the already-exact terms at a single shared exponent and
// then performs the unique final rounding step.
func fusedEvaluate(terms []Decimal, opts FusedOptions) (Decimal, error) {
	opts, err := opts.normalized()
	if err != nil {
		return Decimal{}, err
	}

	value, exp, err := exactSum(terms, opts.MaxDigits)
	if err != nil {
		return Decimal{}, err
	}
	return fusedRound(value, exp, opts)
}

// exactSum adds terms without rounding. It returns the accumulator coefficient
// together with its exponent: the smallest (most negative) exponent of any
// nonzero term, the unique scale fine enough to represent every term exactly.
// Coarser terms gain exact trailing zeros; no term is truncated. Every
// alignment that would exceed maxDigits is rejected before the corresponding
// big.Int is allocated.
func exactSum(terms []Decimal, maxDigits int) (*big.Int, int32, error) {
	acc := new(big.Int)

	minExp := int32(0)
	first := true
	for _, term := range terms {
		v := term.getValue()
		if v.Sign() == 0 {
			continue
		}
		if first || term.exp < minExp {
			minExp = term.exp
			first = false
		}
	}
	if first {
		return acc, 0, nil
	}

	for _, term := range terms {
		v := term.getValue()
		if v.Sign() == 0 {
			continue
		}

		// v represents coeff * 10^term.exp and term.exp >= minExp. Express it at
		// the finer common exponent minExp by appending (term.exp-minExp)
		// exact zeros to its coefficient.
		if term.exp > minExp {
			shift := int64(term.exp) - int64(minExp)
			digits := (Decimal{value: v}).NumDigits()
			if digits+int(shift) > maxDigits {
				return nil, 0, ErrFusedBudgetExceeded
			}
			scale := new(big.Int).Exp(tenInt, big.NewInt(shift), nil)
			v = new(big.Int).Mul(v, scale)
		}
		acc.Add(acc, v)
		if acc.BitLen() > 0 && (Decimal{value: acc}).NumDigits() > maxDigits {
			return nil, 0, ErrFusedBudgetExceeded
		}
	}

	return acc, minExp, nil
}

// fusedRound rounds value*10^exp to an exact multiple of 10^(-opts.Precision)
// using opts.Rounding. No integer larger than the already-approved intermediate
// result is constructed.
func fusedRound(value *big.Int, exp int32, opts FusedOptions) (Decimal, error) {
	targetExp := -opts.Precision

	if value.Sign() == 0 {
		return Decimal{value: new(big.Int), exp: targetExp}, nil
	}

	// If the exact value is already expressed at a coarser (or equal) exponent,
	// it is already an exact multiple of 10^(-Precision): no rounding is needed
	// and no zeros must be appended. Keeping the original exponent avoids
	// building a runaway integer such as 1 * 10^100000 to "represent" an integer
	// at a fractional precision.
	if exp >= targetExp {
		return Decimal{value: value, exp: exp}, nil
	}

	// exp < targetExp: the exact value carries finer digits that must be
	// removed. Drop k digits with the requested rounding mode.
	k := int64(targetExp) - int64(exp)
	digits := (Decimal{value: value}).NumDigits()

	// The magnitude holds fewer digits than must be removed, so it is strictly
	// below half of one result unit. Nearest/down modes yield zero; only the
	// directed modes can move to an adjacent unit. No large integer is needed.
	if k > int64(digits) {
		switch opts.Rounding {
		case RoundFusedCeil:
			if value.Sign() > 0 {
				return Decimal{value: big.NewInt(1), exp: targetExp}, nil
			}
		case RoundFusedFloor:
			if value.Sign() < 0 {
				return Decimal{value: big.NewInt(-1), exp: targetExp}, nil
			}
		}
		return Decimal{value: new(big.Int), exp: targetExp}, nil
	}

	scale := new(big.Int).Exp(tenInt, big.NewInt(k), nil)
	q, r := new(big.Int).QuoRem(value, scale, new(big.Int))

	half := new(big.Int).Mul(fiveInt, new(big.Int).Exp(tenInt, big.NewInt(k-1), nil))
	cmp := new(big.Int).Abs(r).Cmp(half) // -1 below half, 0 tie, +1 above half

	increment := func() {
		if value.Sign() < 0 {
			q.Sub(q, oneInt)
		} else {
			q.Add(q, oneInt)
		}
	}

	switch opts.Rounding {
	case RoundHalfUp:
		if cmp >= 0 {
			increment()
		}
	case RoundHalfDown:
		if cmp > 0 {
			increment()
		}
	case RoundHalfEven:
		if cmp > 0 || (cmp == 0 && q.Bit(0) == 1) {
			increment()
		}
	case RoundFusedCeil:
		if r.Sign() > 0 {
			increment()
		}
	case RoundFusedFloor:
		if r.Sign() < 0 {
			increment()
		}
	case RoundFusedDown:
		// Truncate towards zero: q is already the answer.
	}

	return Decimal{value: q, exp: targetExp}, nil
}
