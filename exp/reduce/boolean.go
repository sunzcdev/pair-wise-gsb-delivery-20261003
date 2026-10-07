package reduce

import (
	"reflect"

	"github.com/doug-martin/goqu/v9/exp"
)

func (r *Reducer) reduceBoolean(be exp.BooleanExpression) exp.Expression {
	op := be.Op()
	lhs := be.LHS()
	rhs := be.RHS()

	if reducedLHS := r.reduce(lhs); !reflect.DeepEqual(reducedLHS, lhs) {
		lhs = reducedLHS
	}
	if reducedRHS, ok := rhs.(exp.Expression); ok {
		if reduced := r.reduce(reducedRHS); !reflect.DeepEqual(reduced, reducedRHS) {
			rhs = reduced
		}
	}

	// A constant LHS (e.g. an inner AND that folded to FALSE) makes the whole
	// comparison evaluable.
	if c, ok := exp.ConstKindOf(lhs); ok {
		if folded, handled := r.constComparison(op, c, rhs); handled {
			return folded
		}
	}

	// IS NULL / IS NOT NULL on a column are constant only with a non-null
	// proof. A NULL in a nullable column keeps them row-dependent.
	if rhs == nil {
		if ident, ok := lhs.(exp.IdentifierExpression); ok {
			if r.nullabilityOf(ident) == NonNullable && (op == exp.IsOp || op == exp.IsNotOp) {
				return exp.NewConstExpression(boolConst(op == exp.IsNotOp))
			}
		}
	}

	// col = col / col <> col need a non-null proof: a NULL column makes them
	// UNKNOWN instead of TRUE/FALSE.
	if op == exp.EqOp || op == exp.NeqOp {
		if ident, ok := lhs.(exp.IdentifierExpression); ok {
			if sameIdentifier(ident, rhs) && r.nullabilityOf(ident) == NonNullable {
				return exp.NewConstExpression(boolConst(op == exp.EqOp))
			}
		}
	}

	if !sameExp(lhs, be.LHS()) || !sameRHS(rhs, be.RHS()) {
		return exp.NewBooleanExpression(op, lhs, rhs)
	}
	return be
}

func (r *Reducer) reduceRange(re exp.RangeExpression) exp.Expression {
	lhs := re.LHS()
	rv := re.RHS()
	reducedLHS := r.reduce(lhs)
	start, end := rv.Start(), rv.End()
	reducedStart := r.reduceValue(start)
	reducedEnd := r.reduceValue(end)
	if !sameExp(reducedLHS, lhs) || !sameRHS(reducedStart, start) || !sameRHS(reducedEnd, end) {
		return exp.NewRangeExpression(
			re.Op(),
			reducedLHS,
			exp.NewRangeVal(reducedStart, reducedEnd),
		)
	}
	return re
}

func (r *Reducer) reduceBitwise(be exp.BitwiseExpression) exp.Expression {
	lhs := be.LHS()
	rhs := be.RHS()
	reducedLHS := r.reduce(lhs)
	reducedRHS := r.reduceValue(rhs)
	if !sameExp(reducedLHS, lhs) || !sameRHS(reducedRHS, rhs) {
		return exp.NewBitwiseExpression(be.Op(), reducedLHS, reducedRHS)
	}
	return be
}

func (r *Reducer) reduceFunc(fe exp.SQLFunctionExpression) exp.Expression {
	args := fe.Args()
	changed := false
	newArgs := make([]interface{}, len(args))
	for i, arg := range args {
		reduced := r.reduceValue(arg)
		newArgs[i] = reduced
		if !sameRHS(reduced, arg) {
			changed = true
		}
	}
	if changed {
		return exp.NewSQLFunctionExpression(fe.Name(), newArgs...)
	}
	return fe
}

func (r *Reducer) reduceValue(val interface{}) interface{} {
	if e, ok := val.(exp.Expression); ok {
		return r.reduce(e)
	}
	return val
}

// constComparison handles the case where the LHS is a known TRUE/FALSE/NULL
// constant. It only returns a fold for comparisons whose SQL semantics are
// unambiguous under three-valued logic.
func (r *Reducer) constComparison(op exp.BooleanOperation, lhs exp.ConstKind, rhs interface{}) (exp.Expression, bool) {
	if op == exp.IsOp || op == exp.IsNotOp {
		rhsKind, ok := constKindOrNil(rhs)
		if !ok {
			return nil, false
		}
		result := isTest(lhs, rhsKind)
		if op == exp.IsNotOp {
			result = negate(result)
		}
		return exp.NewConstExpression(result), true
	}
	if lhs == exp.ConstUnknown {
		// NULL compared with anything using the comparison operators is
		// UNKNOWN.
		switch op {
		case exp.EqOp, exp.NeqOp, exp.GtOp, exp.GteOp, exp.LtOp, exp.LteOp,
			exp.InOp, exp.NotInOp, exp.LikeOp, exp.NotLikeOp, exp.ILikeOp,
			exp.NotILikeOp, exp.RegexpLikeOp, exp.RegexpNotLikeOp,
			exp.RegexpILikeOp, exp.RegexpNotILikeOp:
			return exp.NewConstExpression(exp.ConstUnknown), true
		}
	}
	if rhsKind, ok := constOfValue(rhs); ok {
		switch op {
		case exp.EqOp, exp.NeqOp, exp.IsOp, exp.IsNotOp:
			return foldConstConst(op, lhs, rhsKind), true
		}
	}
	return nil, false
}

func constOfValue(val interface{}) (exp.ConstKind, bool) {
	switch v := val.(type) {
	case exp.Expression:
		return exp.ConstKindOf(v)
	case bool:
		if v {
			return exp.ConstTrue, true
		}
		return exp.ConstFalse, true
	}
	return 0, false
}

func constKindOrNil(val interface{}) (exp.ConstKind, bool) {
	if val == nil {
		return exp.ConstUnknown, true
	}
	return constOfValue(val)
}

func foldConstConst(op exp.BooleanOperation, a, b exp.ConstKind) exp.Expression {
	switch op {
	case exp.IsOp:
		return exp.NewConstExpression(isTest(a, b))
	case exp.IsNotOp:
		return exp.NewConstExpression(negate(isTest(a, b)))
	}
	if a == exp.ConstUnknown || b == exp.ConstUnknown {
		return exp.NewConstExpression(exp.ConstUnknown)
	}
	switch op {
	case exp.EqOp:
		return exp.NewConstExpression(boolConst(a == b))
	case exp.NeqOp:
		return exp.NewConstExpression(boolConst(a != b))
	}
	// Unreachable: callers only pass Eq/Neq/Is/IsNot.
	return exp.NewConstExpression(exp.ConstUnknown)
}

func boolConst(v bool) exp.ConstKind {
	if v {
		return exp.ConstTrue
	}
	return exp.ConstFalse
}

// isTest evaluates x IS y for x,y in {TRUE, FALSE, UNKNOWN}.
// IS NULL, IS TRUE, IS FALSE never return UNKNOWN.
func isTest(x, y exp.ConstKind) exp.ConstKind {
	return boolConst(x == y)
}

func negate(kind exp.ConstKind) exp.ConstKind {
	switch kind {
	case exp.ConstTrue:
		return exp.ConstFalse
	case exp.ConstFalse:
		return exp.ConstTrue
	default:
		return exp.ConstUnknown
	}
}

func sameIdentifier(ident exp.IdentifierExpression, val interface{}) bool {
	other, ok := val.(exp.IdentifierExpression)
	if !ok {
		return false
	}
	return ident.GetSchema() == other.GetSchema() &&
		ident.GetTable() == other.GetTable() &&
		reflect.DeepEqual(ident.GetCol(), other.GetCol())
}

func sameRHS(a, b interface{}) bool {
	return reflect.DeepEqual(a, b)
}

func sameExp(a, b exp.Expression) bool {
	return reflect.DeepEqual(a, b)
}
