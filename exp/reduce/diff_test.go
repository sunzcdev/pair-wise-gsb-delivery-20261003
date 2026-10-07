package reduce_test

import (
	"math/rand"
	"testing"

	"github.com/doug-martin/goqu/v9"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/exp/reduce"
	"github.com/stretchr/testify/suite"
)

type tri int

const (
	triFalse tri = iota
	triUnknown
	triTrue
)

func (t tri) String() string {
	switch t {
	case triTrue:
		return "T"
	case triFalse:
		return "F"
	default:
		return "U"
	}
}

type diffSuite struct {
	suite.Suite
	columns []string
}

func TestDiffSuite(t *testing.T) {
	suite.Run(t, new(diffSuite))
}

// env maps a column name to its three-valued result.
type env map[string]tri

// eval computes the three-valued result of the (possibly reduced) expression.
// Every node used by the random generator is supported.
func eval(e exp.Expression, values env) tri {
	switch t := e.(type) {
	case nil:
		return triUnknown
	case exp.IdentifierExpression:
		return values[colName(t)]
	case exp.LiteralExpression:
		if kind, ok := exp.ConstKindOf(t); ok {
			return constToTri(kind)
		}
		return triUnknown
	case exp.BooleanExpression:
		return evalBool(t, values)
	case exp.ExpressionList:
		if t.IsEmpty() {
			if t.Type() == exp.AndType {
				return triTrue
			}
			return triFalse
		}
		out := triTrue
		if t.Type() == exp.OrType {
			out = triFalse
		}
		for _, child := range t.Expressions() {
			v := eval(child, values)
			if t.Type() == exp.AndType {
				out = kleeneAND(out, v)
			} else {
				out = kleeneOR(out, v)
			}
		}
		return out
	case exp.RangeExpression:
		l := eval(t.LHS(), values)
		start := triFromValue(t.RHS().Start())
		end := triFromValue(t.RHS().End())
		return evalBetween(t.Op(), l, start, end)
	}
	return triUnknown
}

func evalBool(be exp.BooleanExpression, values env) tri {
	l := eval(be.LHS(), values)
	rhs := be.RHS()
	r := triFromValue(rhs)
	if re, ok := rhs.(exp.Expression); ok {
		r = eval(re, values)
	}
	return evalOp(be.Op(), l, r, rhs)
}

func evalOp(op exp.BooleanOperation, l, r tri, rawRHS interface{}) tri {
	switch op {
	case exp.IsOp:
		return triFromBool(l == r)
	case exp.IsNotOp:
		return triFromBool(l != r)
	case exp.EqOp:
		if l == triUnknown || r == triUnknown {
			return triUnknown
		}
		return triFromBool(l == r)
	case exp.NeqOp:
		if l == triUnknown || r == triUnknown {
			return triUnknown
		}
		return triFromBool(l != r)
	case exp.GtOp, exp.GteOp, exp.LtOp, exp.LteOp:
		if l == triUnknown || r == triUnknown {
			return triUnknown
		}
		ord := map[tri]int{triFalse: 0, triTrue: 1}
		li, ri := ord[l], ord[r]
		var result bool
		switch op {
		case exp.GtOp:
			result = li > ri
		case exp.GteOp:
			result = li >= ri
		case exp.LtOp:
			result = li < ri
		case exp.LteOp:
			result = li <= ri
		}
		return triFromBool(result)
	case exp.InOp, exp.NotInOp:
		return evalIn(op, l, rawRHS)
	}
	return triUnknown
}

func evalIn(op exp.BooleanOperation, l tri, rawRHS interface{}) tri {
	vals, ok := rawRHS.([]interface{})
	if !ok {
		return triUnknown
	}
	if l == triUnknown {
		return triUnknown
	}
	anyNull := false
	for _, v := range vals {
		tv := triFromValue(v)
		if tv == triUnknown {
			anyNull = true
			continue
		}
		if tv == l {
			if op == exp.InOp {
				return triTrue
			}
			return triFalse
		}
	}
	if anyNull {
		return triUnknown
	}
	if op == exp.InOp {
		return triFalse
	}
	return triTrue
}

func evalBetween(op exp.RangeOperation, l, start, end tri) tri {
	if l == triUnknown || start == triUnknown || end == triUnknown {
		v := triUnknown
		if op == exp.NotBetweenOp {
			return kleeneNOT(v)
		}
		return v
	}
	// Encode the boolean domain as FALSE=0, TRUE=1 for range tests.
	ord := map[tri]int{triFalse: 0, triTrue: 1}
	between := triFalse
	if ord[l] >= ord[start] && ord[l] <= ord[end] {
		between = triTrue
	}
	if op == exp.NotBetweenOp {
		return kleeneNOT(between)
	}
	return between
}

func triFromValue(val interface{}) tri {
	switch v := val.(type) {
	case nil:
		return triUnknown
	case bool:
		if v {
			return triTrue
		}
		return triFalse
	case int:
		if v == 1 {
			return triTrue
		}
		return triFalse
	}
	return triUnknown
}

func triFromBool(v bool) tri {
	if v {
		return triTrue
	}
	return triFalse
}

func constToTri(kind exp.ConstKind) tri {
	switch kind {
	case exp.ConstTrue:
		return triTrue
	case exp.ConstFalse:
		return triFalse
	default:
		return triUnknown
	}
}

func colName(ident exp.IdentifierExpression) string {
	return ident.GetCol().(string)
}

// gen builds small boolean expressions over a handful of columns and the
// boolean domain {true, false, nil(=NULL)}. All generated predicates are
// ones the reducer can reason about.
func gen(rng *rand.Rand, depth int) exp.Expression {
	if depth <= 0 || rng.Intn(3) == 0 {
		return genAtom(rng)
	}
	switch rng.Intn(6) {
	case 0:
		op := exp.AndType
		if rng.Intn(2) == 1 {
			op = exp.OrType
		}
		return exp.NewExpressionList(op, gen(rng, depth-1), gen(rng, depth-1))
	case 1:
		return genCompare(rng, genAtom(rng))
	case 2:
		return genAtom(rng)
	case 3:
		return genIsNull(rng)
	case 4:
		return genIn(rng)
	default:
		return genSelfEq(rng)
	}
}

func genIn(rng *rand.Rand) exp.Expression {
	col := goqu.I([]string{"a", "b", "c"}[rng.Intn(3)])
	vals := []interface{}{true, false, nil}
	rng.Shuffle(len(vals), func(i, j int) { vals[i], vals[j] = vals[j], vals[i] })
	n := 1 + rng.Intn(2)
	if rng.Intn(2) == 0 {
		return col.In(vals[:n]...)
	}
	return col.NotIn(vals[:n]...)
}

func genAtom(rng *rand.Rand) exp.Expression {
	col := []string{"a", "b", "c"}[rng.Intn(3)]
	switch rng.Intn(6) {
	case 0:
		return goqu.I(col).Eq(true)
	case 1:
		return exp.NewConstExpression(exp.ConstTrue)
	case 2:
		return exp.NewConstExpression(exp.ConstFalse)
	case 3:
		return exp.NewConstExpression(exp.ConstUnknown)
	case 4:
		return goqu.I(col).Eq(true)
	default:
		return goqu.I(col).Eq(false)
	}
}

func genCompare(rng *rand.Rand, lhs exp.Expression) exp.Expression {
	rhs := []interface{}{true, false, nil}[rng.Intn(3)]
	ops := []exp.BooleanOperation{
		exp.EqOp, exp.NeqOp, exp.IsOp, exp.IsNotOp,
		exp.GtOp, exp.GteOp, exp.LtOp, exp.LteOp,
	}
	op := ops[rng.Intn(len(ops))]
	if op == exp.IsOp || op == exp.IsNotOp {
		// goqu normalizes Is(bool), but keep the normalized shape for eval.
		if rhs == nil {
			return exp.NewBooleanExpression(op, lhs, nil)
		}
	}
	return exp.NewBooleanExpression(op, lhs, rhs)
}

func genIsNull(rng *rand.Rand) exp.Expression {
	col := goqu.I([]string{"a", "b", "c"}[rng.Intn(3)])
	if rng.Intn(2) == 0 {
		return col.Is(nil)
	}
	return col.IsNot(nil)
}

func genSelfEq(rng *rand.Rand) exp.Expression {
	col := goqu.I([]string{"a", "b", "c"}[rng.Intn(3)])
	if rng.Intn(2) == 0 {
		return col.Eq(col)
	}
	return col.Neq(col)
}

func genBetween(rng *rand.Rand) exp.Expression {
	col := goqu.I([]string{"a", "b", "c"}[rng.Intn(3)])
	start := []interface{}{0, 1}[rng.Intn(2)]
	end := []interface{}{1, 2}[rng.Intn(2)]
	op := exp.BetweenOp
	if rng.Intn(2) == 1 {
		op = exp.NotBetweenOp
	}
	return exp.NewRangeExpression(op, col, exp.NewRangeVal(start, end))
}

func (s *diffSuite) TestRandomizedEquivalence() {
	const iterations = 4000
	rng := rand.New(rand.NewSource(42))

	// Two reducers: one without schema knowledge, one with all columns proven
	// NOT NULL so the nullable-only folds are exercised as well.
	reducers := map[string]*reduce.Reducer{
		"unknown": reduce.New(),
		"notnull": reduce.New(reduce.WithNullability(map[string]reduce.Nullability{
			"a": reduce.NonNullable,
			"b": reduce.NonNullable,
			"c": reduce.NonNullable,
		})),
	}

	domains := []tri{triTrue, triFalse, triUnknown}
	for i := 0; i < iterations; i++ {
		e := gen(rng, 3)

		// Equivalence of Reduce.
		for name, r := range reducers {
			reduced := r.Reduce(e)
			// Reduction is idempotent.
			s.Equal(reduced, r.Reduce(reduced), "non-idempotent under %s: %#v", name, reduced)

			for _, a := range domains {
				for _, b := range domains {
					for _, c := range domains {
						v := env{"a": a, "b": b, "c": c}
						// For the notnull reducer NULL assignments are out of
						// the promised domain; skip them.
						if name == "notnull" && (a == triUnknown || b == triUnknown || c == triUnknown) {
							continue
						}
						s.Equal(eval(e, v), eval(reduced, v),
							"reduce mismatch [%s] %#v env=%v%v%v", name, e, a, b, c)
					}
				}
			}
		}

		// Equivalence of structural Not on every generated expression, under
		// every assignment. The reducer falls back to no-op when it cannot
		// negate structurally, in which case results must match as well.
		r := reducers["unknown"]
		negated := r.Not(e)
		for _, a := range domains {
			for _, b := range domains {
				for _, c := range domains {
					v := env{"a": a, "b": b, "c": c}
					s.Equal(kleeneNOT(eval(e, v)), eval(negated, v),
						"not mismatch %#v env=%v%v%v", e, a, b, c)
				}
			}
		}
	}
}

// TestRandomizedNullableSelfEqualityMismatch demonstrates why the naive
// two-valued fold x = x -> TRUE is unsound: on the "unknown" reducer the
// expression must survive, and only the notnull reducer may fold it.
func (s *diffSuite) TestSelfEqualityFoldRequiresProof() {
	x := goqu.I("a")
	selfEq := x.Eq(x)

	unknown := reduce.New()
	kept := unknown.Reduce(selfEq)
	s.Equal(selfEq, kept)
	s.Equal(triUnknown, eval(kept, env{"a": triUnknown}))

	notnull := reduce.New(reduce.WithNullability(map[string]reduce.Nullability{"a": reduce.NonNullable}))
	folded := notnull.Reduce(selfEq)
	s.Equal(exp.NewConstExpression(exp.ConstTrue), folded)
	s.Equal(triTrue, eval(folded, env{"a": triTrue}))
	s.Equal(triTrue, eval(folded, env{"a": triFalse}))
}
