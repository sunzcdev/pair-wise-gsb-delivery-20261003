package exp

// Tri is a value under SQL three-valued logic (3VL).
//
// In 3VL a predicate evaluates to TRUE, FALSE or UNKNOWN (the UNKNOWN state is
// produced whenever NULL participates in a comparison). SQL WHERE clauses only
// accept rows for which the predicate is TRUE; UNKNOWN is rejected the same way
// FALSE is, but UNKNOWN and FALSE are still distinct values for compound
// expressions (e.g. NOT UNKNOWN = UNKNOWN, UNKNOWN OR TRUE = TRUE).
type Tri uint8

const (
	// UnknownTri is the 3VL UNKNOWN state produced by comparisons involving NULL.
	UnknownTri Tri = iota
	// TrueTri is the 3VL TRUE state.
	TrueTri
	// FalseTri is the 3VL FALSE state.
	FalseTri
)

// Not returns the 3VL negation of t (SQL NOT).
//
//	NOT TRUE    = FALSE
//	NOT FALSE   = TRUE
//	NOT UNKNOWN = UNKNOWN
func NotTri(t Tri) Tri {
	switch t {
	case TrueTri:
		return FalseTri
	case FalseTri:
		return TrueTri
	default:
		return UnknownTri
	}
}

// AndTri returns the 3VL conjunction of a and b (SQL AND).
func AndTri(a, b Tri) Tri {
	switch {
	case a == FalseTri || b == FalseTri:
		return FalseTri
	case a == TrueTri && b == TrueTri:
		return TrueTri
	default:
		return UnknownTri
	}
}

// OrTri returns the 3VL disjunction of a and b (SQL OR).
func OrTri(a, b Tri) Tri {
	switch {
	case a == TrueTri || b == TrueTri:
		return TrueTri
	case a == FalseTri && b == FalseTri:
		return FalseTri
	default:
		return UnknownTri
	}
}

func (t Tri) String() string {
	switch t {
	case TrueTri:
		return "true"
	case FalseTri:
		return "false"
	default:
		return "unknown"
	}
}

// BoolConstantExpression is an explicit boolean constant known at reduction
// time. Unlike a LiteralExpression its meaning is unambiguous, so the reducer
// folds it when it appears inside AND/OR/NOT expressions.
//
// Use the package level True, False and Unknown values instead of implementing
// this interface yourself.
type BoolConstantExpression interface {
	Expression
	// BoolValue returns the 3VL value of this constant.
	BoolValue() Tri
}

type boolConstant struct{ tri Tri }

// NewBoolConstantExpression creates an explicit TRUE/FALSE/UNKNOWN predicate
// constant. Prefer the package level True, False and Unknown variables.
func NewBoolConstantExpression(tri Tri) BoolConstantExpression {
	return boolConstant{tri: tri}
}

func (bc boolConstant) Clone() Expression          { return bc }
func (bc boolConstant) Expression() Expression     { return bc }
func (bc boolConstant) BoolValue() Tri             { return bc.tri }

var (
	// True is the SQL boolean constant TRUE.
	True BoolConstantExpression = boolConstant{tri: TrueTri}
	// False is the SQL boolean constant FALSE.
	False BoolConstantExpression = boolConstant{tri: FalseTri}
	// Unknown is the 3VL UNKNOWN constant, equivalent to a NULL predicate
	// (e.g. CAST(NULL AS BOOLEAN)). It is distinct from FALSE.
	Unknown BoolConstantExpression = boolConstant{tri: UnknownTri}
)

// NotExpression is the logical negation of another boolean expression.
//
//	Not(I("a").Eq(1)) // NOT ("a" = 1)
type NotExpression interface {
	Expression
	// Operand returns the negated boolean expression.
	Operand() Expression
}

type notExpression struct {
	operand Expression
}

// NewNotExpression wraps operand in a NOT expression.
//
//	Not(I("a").Eq(1)) // NOT ("a" = 1)
func NewNotExpression(operand Expression) NotExpression {
	return notExpression{operand: operand}
}

func (n notExpression) Clone() Expression {
	return notExpression{operand: n.operand.Clone()}
}

func (n notExpression) Expression() Expression { return n }

func (n notExpression) Operand() Expression { return n.operand }

// PureExpression marks an expression node as deterministic and free of side
// effects. The reducer only removes ("absorbs") nodes that are pure, because
// deleting an impure node from a TRUE/FALSE branch would silently suppress a
// function call that SQL may otherwise evaluate.
//
// Only implement this for expressions whose evaluation is safe to drop and
// repeat. In particular, LiteralExpression, unknown SQLFunctionExpression
// values and anything backed by a driver.Valuer are treated as impure unless
// proven otherwise.
type PureExpression interface {
	IsPure() bool
}

// NotNullExpression marks an expression node as provably non-NULL. This is the
// caller-supplied proof counterpart to column nullability: nodes implementing
// the interface with a true result are considered non-nullable by the reducer,
// allowing rewrites such as x = x to TRUE.
type NotNullExpression interface {
	IsNotNull() bool
}
