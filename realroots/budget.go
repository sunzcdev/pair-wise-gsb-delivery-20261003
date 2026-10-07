package realroots

// Reason states why the isolation process could not finish a region.
type Reason string

const (
	// ReasonMaxDegree means the polynomial degree exceeds Budget.MaxDegree
	// and no interval was searched for that polynomial.
	ReasonMaxDegree Reason = "max-degree-exceeded"

	// ReasonMaxBisections means the bound on adaptive bisections was spent
	// while unresolved regions still remained.
	ReasonMaxBisections Reason = "max-bisections-exceeded"

	// ReasonMaxOperations means the bound on exact arithmetic operations was
	// spent while unresolved regions still remained.
	ReasonMaxOperations Reason = "max-operations-exceeded"
)

// Budget bounds the exact work the isolator performs. The zero value applies
// safe defaults; callers that want an unlimited search (apart from the
// built-in guardrails) may set the relevant fields to -1.
//
//   - MaxDegree: polynomials strictly above this degree are not searched.
//   - MaxBisections: maximum number of adaptive interval halvings.
//   - MaxOperations: maximum number of charged exact polynomial operations
//     (evaluation, remainder step). This bounds big-integer growth.
type Budget struct {
	MaxDegree     int
	MaxBisections int
	MaxOperations int
}

// DefaultBudget is applied whenever an Options leaves a field zero.
var DefaultBudget = Budget{
	MaxDegree:     256,
	MaxBisections: 200000,
	MaxOperations: 50000000,
}

type budget struct {
	degreeLimit    int
	bisectionLimit int
	opLimit        int

	bisections int
	ops        int

	exhausted Reason
}

func newBudget(b Budget) *budget {
	resolve := func(v, def int) int {
		switch {
		case v < 0:
			return 0
		case v == 0:
			return def
		default:
			return v
		}
	}
	return &budget{
		degreeLimit:    resolve(b.MaxDegree, DefaultBudget.MaxDegree),
		bisectionLimit: resolve(b.MaxBisections, DefaultBudget.MaxBisections),
		opLimit:        resolve(b.MaxOperations, DefaultBudget.MaxOperations),
	}
}

// chargeBisection reports whether one more halving is allowed.
func (b *budget) chargeBisection() bool {
	if b.exhausted != "" {
		return false
	}
	if b.bisectionLimit > 0 && b.bisections >= b.bisectionLimit {
		b.exhausted = ReasonMaxBisections
		return false
	}
	b.bisections++
	return true
}

// polyEval charges the work of evaluating a polynomial with n coefficients.
func (b *budget) polyEval(q ratPoly) {
	if b.exhausted != "" {
		return
	}
	b.ops += len(q)
	if b.opLimit > 0 && b.ops >= b.opLimit {
		b.exhausted = ReasonMaxOperations
	}
}

// remainderStep charges one extended-Euclidean remainder computation.
func (b *budget) remainderStep(degree int) {
	if b.exhausted != "" {
		return
	}
	// A remainder on degree d polynomials costs O(d) rational operations,
	// each touching coefficients of growing size; charge conservatively.
	b.ops += (degree + 1) * (degree + 2)
	if b.opLimit > 0 && b.ops >= b.opLimit {
		b.exhausted = ReasonMaxOperations
	}
}
