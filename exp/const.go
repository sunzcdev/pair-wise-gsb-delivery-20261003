package exp

// ConstKind identifies a boolean constant under SQL three-valued logic.
//
// Unlike an arbitrary LiteralExpression, a ConstExpression is a known boolean
// result produced by the reducer itself. Reducers can therefore safely fold
// it inside AND/OR/NOT without having to parse raw SQL.
type ConstKind int

const (
	// ConstTrue renders as TRUE and evaluates to TRUE even when a column is NULL.
	ConstTrue ConstKind = iota + 1
	// ConstFalse renders as FALSE.
	ConstFalse
	// ConstUnknown renders as NULL, the third truth value of SQL logic.
	// It is distinct from an unknown NULL produced by a nullable column.
	ConstUnknown
)

// NewConstExpression creates a known TRUE, FALSE, or NULL boolean constant.
//
// The returned expression renders with the default dialect fragments
// (TRUE / FALSE / NULL). Use it as a reduction result rather than as input
// that must round-trip on dialects without a boolean data type (e.g. MSSQL,
// where a standalone TRUE/FALSE in a WHERE clause is not valid SQL).
func NewConstExpression(kind ConstKind) LiteralExpression {
	return constFor(kind)
}

// ConstKindOf reports whether e is a reducer constant and, when it is, its kind.
// Literal SQL written by the caller (NewLiteralExpression) is never reported
// as a constant because its text is opaque.
func ConstKindOf(e Expression) (ConstKind, bool) {
	if c, ok := e.(constExpression); ok {
		return c.kind, true
	}
	return 0, false
}

type constExpression struct {
	LiteralExpression
	kind ConstKind
}

// Comparisons must carry the constExpression as the LHS; relying on the
// embedded LiteralExpression methods would lose the constant marker.
func (c constExpression) Eq(val interface{}) BooleanExpression {
	return NewBooleanExpression(EqOp, c, val)
}
func (c constExpression) Neq(val interface{}) BooleanExpression {
	return NewBooleanExpression(NeqOp, c, val)
}
func (c constExpression) Gt(val interface{}) BooleanExpression {
	return NewBooleanExpression(GtOp, c, val)
}
func (c constExpression) Gte(val interface{}) BooleanExpression {
	return NewBooleanExpression(GteOp, c, val)
}
func (c constExpression) Lt(val interface{}) BooleanExpression {
	return NewBooleanExpression(LtOp, c, val)
}
func (c constExpression) Lte(val interface{}) BooleanExpression {
	return NewBooleanExpression(LteOp, c, val)
}
func (c constExpression) Is(val interface{}) BooleanExpression {
	return NewBooleanExpression(IsOp, c, val)
}
func (c constExpression) IsNot(val interface{}) BooleanExpression {
	return NewBooleanExpression(IsNotOp, c, val)
}
func (c constExpression) In(vals ...interface{}) BooleanExpression {
	return NewBooleanExpression(InOp, c, vals)
}
func (c constExpression) NotIn(vals ...interface{}) BooleanExpression {
	return NewBooleanExpression(NotInOp, c, vals)
}

func (c constExpression) Expression() Expression { return c }

// Clone returns the same value: a constant is immutable and shared safely,
// so reducing never has to copy it.
func (c constExpression) Clone() Expression { return c }

var (
	constTrueExpr    = constExpression{kind: ConstTrue, LiteralExpression: NewLiteralExpression("TRUE")}
	constFalseExpr   = constExpression{kind: ConstFalse, LiteralExpression: NewLiteralExpression("FALSE")}
	constUnknownExpr = constExpression{kind: ConstUnknown, LiteralExpression: NewLiteralExpression("NULL")}
)

func constFor(kind ConstKind) constExpression {
	switch kind {
	case ConstTrue:
		return constTrueExpr
	case ConstFalse:
		return constFalseExpr
	default:
		return constUnknownExpr
	}
}
