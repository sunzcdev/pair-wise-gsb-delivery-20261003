package reduce_test

import (
	"testing"

	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/exp/reduce"
	"github.com/stretchr/testify/suite"
)

type truthTableSuite struct {
	suite.Suite
}

func TestTruthTableSuite(t *testing.T) {
	suite.Run(t, new(truthTableSuite))
}

// kleeneAND / kleeneOR are the SQL truth tables:
//
//	AND | T     F     U        OR  | T     F     U
//	T   | T     F     U        T   | T     T     T
//	F   | F     F     F        F   | T     F     U
//	U   | U     F     U        U   | T     U     U
//
// NOT: T->F, F->T, U->U.
func kleeneAND(a, b tri) tri {
	if a == triFalse || b == triFalse {
		return triFalse
	}
	if a == triUnknown || b == triUnknown {
		return triUnknown
	}
	return triTrue
}

func kleeneOR(a, b tri) tri {
	if a == triTrue || b == triTrue {
		return triTrue
	}
	if a == triUnknown || b == triUnknown {
		return triUnknown
	}
	return triFalse
}

func kleeneNOT(a tri) tri {
	switch a {
	case triTrue:
		return triFalse
	case triFalse:
		return triTrue
	default:
		return triUnknown
	}
}

func (s *truthTableSuite) TestReducerMatchesTruthTableConstants() {
	r := reduce.New()
	values := []tri{triTrue, triFalse, triUnknown}
	consts := map[tri]exp.Expression{
		triTrue:    exp.NewConstExpression(exp.ConstTrue),
		triFalse:   exp.NewConstExpression(exp.ConstFalse),
		triUnknown: exp.NewConstExpression(exp.ConstUnknown),
	}

	for _, a := range values {
		for _, b := range values {
			andOut := r.Reduce(andList(consts[a], consts[b]))
			orOut := r.Reduce(orList(consts[a], consts[b]))
			s.Equal(kleeneAND(a, b), eval(andOut, nil), "AND %v %v", a, b)
			s.Equal(kleeneOR(a, b), eval(orOut, nil), "OR %v %v", a, b)
			s.Equal(kleeneNOT(a), eval(r.Not(consts[a]), nil), "NOT %v", a)
		}
	}
}

// TestNonNullProofRequirements pins down the identities that differ between
// two-valued boolean algebra and SQL three-valued logic. These are exactly
// the folds the reducer refuses without a NonNullable proof.
func (s *truthTableSuite) TestNonNullProofRequirements() {
	// x = x: TRUE for non-null rows but UNKNOWN for NULL rows.
	s.Equal(triTrue, evalSelfEq(triTrue))
	s.Equal(triTrue, evalSelfEq(triFalse))
	s.Equal(triUnknown, evalSelfEq(triUnknown))

	// x IS NULL and x IS NOT NULL are complements under 3VL (IS never yields
	// UNKNOWN), but neither is a constant without knowing the column.
	s.Equal(triFalse, isNull(triTrue))
	s.Equal(triFalse, isNull(triFalse))
	s.Equal(triTrue, isNull(triUnknown))

	// De Morgan and the absorption identities, by contrast, hold for U too,
	// so the reducer applies them with no nullability information.
	for _, a := range []tri{triTrue, triFalse, triUnknown} {
		for _, b := range []tri{triTrue, triFalse, triUnknown} {
			s.Equal(kleeneNOT(kleeneAND(a, b)),
				kleeneOR(kleeneNOT(a), kleeneNOT(b)), "De Morgan AND %v %v", a, b)
			s.Equal(kleeneAND(a, kleeneOR(a, b)), a, "absorption %v %v", a, b)
			s.Equal(kleeneOR(a, kleeneAND(a, b)), a, "absorption %v %v", a, b)
		}
	}
}

func evalSelfEq(x tri) tri {
	if x == triUnknown {
		return triUnknown
	}
	return triTrue
}

func isNull(x tri) tri {
	return map[tri]tri{triTrue: triFalse, triFalse: triFalse, triUnknown: triTrue}[x]
}

func andList(a, b exp.Expression) exp.Expression {
	return exp.NewExpressionList(exp.AndType, a, b)
}

func orList(a, b exp.Expression) exp.Expression {
	return exp.NewExpressionList(exp.OrType, a, b)
}
