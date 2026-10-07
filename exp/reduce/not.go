package reduce

import "github.com/doug-martin/goqu/v9/exp"

// Not returns a logically negated, reduced expression.
//
// It applies the SQL-safe negations:
//
//   - NOT TRUE/FALSE/NULL follows three-valued logic (NOT NULL is NULL).
//   - Comparable operators flip to their inverse (= to <>, > to <=,
//     IS to IS NOT, IN to NOT IN, LIKE to NOT LIKE, ...). These inversions
//     are exact under three-valued logic without a non-null proof.
//   - BETWEEN flips to NOT BETWEEN.
//   - De Morgan: NOT (a AND b) becomes (NOT a) OR (NOT b) and vice versa,
//     which also holds in the presence of NULL.
//
// When no structural negation exists (Literal SQL, an unknown function, a
// CASE expression, ...) e is reduced and returned unchanged rather than
// emitting opaque "NOT (...)" SQL.
func (r *Reducer) Not(e exp.Expression) exp.Expression {
	ne := r.not(e)
	if ne == nil {
		return r.Reduce(e)
	}
	return r.reduce(ne)
}

func (r *Reducer) not(e exp.Expression) exp.Expression {
	switch t := e.(type) {
	case nil:
		return exp.NewConstExpression(exp.ConstUnknown)
	case exp.BooleanExpression:
		return r.notBoolean(t)
	case exp.RangeExpression:
		return exp.NewRangeExpression(notRangeOp(t.Op()), t.LHS(), t.RHS())
	case exp.ExpressionList:
		return r.notList(t)
	}
	if kind, ok := exp.ConstKindOf(e); ok {
		return exp.NewConstExpression(negateConst(kind))
	}
	return nil
}

func negateConst(kind exp.ConstKind) exp.ConstKind {
	switch kind {
	case exp.ConstTrue:
		return exp.ConstFalse
	case exp.ConstFalse:
		return exp.ConstTrue
	default:
		return exp.ConstUnknown
	}
}

func (r *Reducer) notBoolean(be exp.BooleanExpression) exp.Expression {
	inv, ok := inverseBooleanOp[be.Op()]
	if !ok {
		return nil
	}
	return exp.NewBooleanExpression(inv, be.LHS(), be.RHS())
}

func notRangeOp(op exp.RangeOperation) exp.RangeOperation {
	if op == exp.BetweenOp {
		return exp.NotBetweenOp
	}
	return exp.BetweenOp
}

func (r *Reducer) notList(el exp.ExpressionList) exp.Expression {
	children := el.Expressions()
	negated := make([]exp.Expression, 0, len(children))
	for _, child := range children {
		n := r.not(child)
		if n == nil {
			// If any operand cannot be negated structurally, fall back to
			// leaving the whole expression unchanged rather than generating
			// opaque NOT (...) SQL.
			return nil
		}
		negated = append(negated, n)
	}
	typ := exp.AndType
	if el.Type() == exp.AndType {
		typ = exp.OrType
	}
	return exp.NewExpressionList(typ, negated...)
}

// inverseBooleanOp mirrors exp.operatorInversions, which is unexported.
var inverseBooleanOp = map[exp.BooleanOperation]exp.BooleanOperation{
	exp.EqOp:             exp.NeqOp,
	exp.NeqOp:            exp.EqOp,
	exp.IsOp:             exp.IsNotOp,
	exp.IsNotOp:          exp.IsOp,
	exp.GtOp:             exp.LteOp,
	exp.GteOp:            exp.LtOp,
	exp.LtOp:             exp.GteOp,
	exp.LteOp:            exp.GtOp,
	exp.InOp:             exp.NotInOp,
	exp.NotInOp:          exp.InOp,
	exp.LikeOp:           exp.NotLikeOp,
	exp.NotLikeOp:        exp.LikeOp,
	exp.ILikeOp:          exp.NotILikeOp,
	exp.NotILikeOp:       exp.ILikeOp,
	exp.RegexpLikeOp:     exp.RegexpNotLikeOp,
	exp.RegexpNotLikeOp:  exp.RegexpLikeOp,
	exp.RegexpILikeOp:    exp.RegexpNotILikeOp,
	exp.RegexpNotILikeOp: exp.RegexpILikeOp,
}
