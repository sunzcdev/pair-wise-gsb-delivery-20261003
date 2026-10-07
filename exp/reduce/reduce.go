// Package reduce provides an optional, conservative simplifier for boolean
// expressions before they are handed to the SQL generator.
//
// It folds AND/OR/NOT over constants that are known under SQL three-valued
// logic (TRUE, FALSE and UNKNOWN/NULL) while leaving caller-authored Literal
// SQL, unknown functions and other opaque or potentially side-effecting nodes
// untouched unless the caller declares them pure.
//
// The reducer never evaluates driver.Valuer arguments, never reorders
// operands, and does not duplicate any node. When a short-circuiting constant
// would allow dropping sibling operands, the siblings are only removed if the
// caller proved them pure; otherwise the original shape is kept so that
// generated SQL preserves the same Value() calls.
package reduce

import (
	"strings"

	"github.com/doug-martin/goqu/v9/exp"
)

// Nullability states whether a named column can contain NULL.
type Nullability int

const (
	// UnknownNullability is the default: folds that require a non-null proof
	// are skipped for the column.
	UnknownNullability Nullability = iota
	// NonNullable proves the column is never NULL (e.g. constrained NOT NULL).
	NonNullable
	// Nullable states that the column may contain NULL. It blocks the same
	// folds as UnknownNullability and is useful for documenting assumptions.
	Nullable
)

// Reducer simplifies expressions. The zero value is usable and performs no
// folds that require column or function purity information.
type Reducer struct {
	nullability map[string]Nullability
	pureFuncs   map[string]struct{}
}

// Option configures a Reducer.
type Option func(*Reducer)

// WithNullability provides column nullability information.
//
// Keys identify columns the same way the expression identifiers do:
//
//   - "col" matches an unqualified identifier I("col")
//   - "table.col" matches I("table.col") or I("table").Col("col")
//   - "schema.table.col" matches the fully qualified form
//
// Later calls override earlier entries for the same key.
func WithNullability(cols map[string]Nullability) Option {
	return func(r *Reducer) {
		if r.nullability == nil {
			r.nullability = map[string]Nullability{}
		}
		for name, n := range cols {
			r.nullability[name] = n
		}
	}
}

// WithPureFunctions declares SQL function names whose results depend only on
// their arguments and which have no side effects (e.g. "COALESCE", "UPPER").
// Names are matched case-insensitively. Any function not listed is treated as
// potentially side-effecting and its expression is never eliminated.
func WithPureFunctions(names ...string) Option {
	return func(r *Reducer) {
		if r.pureFuncs == nil {
			r.pureFuncs = map[string]struct{}{}
		}
		for _, name := range names {
			r.pureFuncs[normalizeName(name)] = struct{}{}
		}
	}
}

// New builds a Reducer from the given options.
func New(opts ...Option) *Reducer {
	r := &Reducer{}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Fold reduces e with the zero-value Reducer: only structural constant folds
// that need no schema information are applied.
func Fold(e exp.Expression) exp.Expression {
	return New().Reduce(e)
}

// Reduce returns a semantically equivalent, possibly simpler expression.
//
// Unknown expression types, Literal SQL, Ex/ExOr maps and subqueries are
// returned unchanged. The original tree is never mutated; unchanged subtrees
// are shared rather than rebuilt.
func (r *Reducer) Reduce(e exp.Expression) exp.Expression {
	if e == nil {
		return nil
	}
	return r.reduce(e)
}

func (r *Reducer) reduce(e exp.Expression) exp.Expression {
	switch t := e.(type) {
	case exp.ExpressionList:
		return r.reduceList(t)
	case exp.BooleanExpression:
		return r.reduceBoolean(t)
	case exp.RangeExpression:
		return r.reduceRange(t)
	case exp.BitwiseExpression:
		return r.reduceBitwise(t)
	case exp.CastExpression:
		if casted := r.reduce(t.Casted()); !sameExp(casted, t.Casted()) {
			return exp.NewCastExpression(casted, t.Type().Literal())
		}
	case exp.AliasedExpression:
		if aliased := r.reduce(t.Aliased()); !sameExp(aliased, t.Aliased()) {
			return exp.NewAliasExpression(aliased, t.GetAs())
		}
	case exp.SQLFunctionExpression:
		return r.reduceFunc(t)
	case exp.SQLWindowFunctionExpression:
		// Window functions are never declared pure through WithPureFunctions
		// and their ordering/partition columns are not boolean predicates.
		return e
	}
	return e
}

func (r *Reducer) reduceList(el exp.ExpressionList) exp.Expression {
	typ := el.Type()

	// First pass: reduce children in order and flatten one level of lists of
	// the same connective. Flattening AND inside AND / OR inside OR is
	// associative even with UNKNOWN, so no non-null proof is required.
	flat := make([]exp.Expression, 0, len(el.Expressions()))
	for _, child := range el.Expressions() {
		reduced := r.reduce(child)
		if inner, ok := reduced.(exp.ExpressionList); ok && inner.Type() == typ {
			flat = append(flat, inner.Expressions()...)
		} else {
			flat = append(flat, reduced)
		}
	}

	// Second pass: short-circuit absorption. FALSE erases AND and TRUE erases
	// OR regardless of siblings; the list is erased only when every dropped
	// sibling is pure, otherwise the list shape is kept so SQL generation
	// performs the same Valuer calls in the same order. UNKNOWN needs the
	// third pass because UNKNOWN AND FALSE = FALSE. Operand order is never
	// changed.
	for i, child := range flat {
		kind, isConst := exp.ConstKindOf(child)
		if !isConst {
			continue
		}
		// Only TRUE in OR and FALSE in AND dominate regardless of other
		// operands. UNKNOWN in AND must not beat a sibling FALSE
		// (UNKNOWN AND FALSE = FALSE); it is handled after constants combine.
		dominates := typ == exp.OrType && kind == exp.ConstTrue ||
			typ == exp.AndType && kind == exp.ConstFalse
		if dominates && r.allPureExcept(flat, i) {
			return exp.NewConstExpression(kind)
		}
	}

	// Third pass: drop identities (TRUE in AND, FALSE in OR) and combine all
	// remaining constants into one. Keeping exactly one constant at the end
	// preserves the position of the first constant relative to non-constants.
	var combined exp.ConstKind
	hasConst := false
	insertAt := 0
	kept := make([]exp.Expression, 0, len(flat))
	for _, child := range flat {
		kind, isConst := exp.ConstKindOf(child)
		if !isConst {
			kept = append(kept, child)
			continue
		}
		if !hasConst {
			combined, hasConst, insertAt = kind, true, len(kept)
		} else {
			combined = combine(typ, combined, kind)
		}
	}

	if hasConst {
		identity := typ == exp.AndType && combined == exp.ConstTrue ||
			typ == exp.OrType && combined == exp.ConstFalse
		if identity {
			// The merged constant is the connective identity; dropping it
			// removes no sibling and no Value() call, so purity is irrelevant.
			if len(kept) == 0 {
				return exp.NewConstExpression(combined)
			}
			if len(kept) == 1 {
				return kept[0]
			}
			return exp.NewExpressionList(typ, kept...)
		}
		// The combined constant may itself dominate once identities merged.
		// For AND, FALSE always dominates. UNKNOWN dominates only when every
		// sibling is itself a known UNKNOWN: a non-constant predicate could
		// still be FALSE, and UNKNOWN AND FALSE = FALSE.
		dominates := typ == exp.OrType && combined == exp.ConstTrue ||
			typ == exp.AndType && combined == exp.ConstFalse
		if typ == exp.AndType && combined == exp.ConstUnknown &&
			r.allConstKind(kept, exp.ConstUnknown) && r.allPure(kept) {
			dominates = true
		}
		if dominates && r.allPure(kept) {
			return exp.NewConstExpression(combined)
		}
		// Emit the combined constant where the first surviving constant was.
		kept = append(kept, nil)
		copy(kept[insertAt+1:], kept[insertAt:])
		kept[insertAt] = exp.NewConstExpression(combined)
	}

	switch len(kept) {
	case 0:
		if hasConst {
			return exp.NewConstExpression(combined)
		}
		// An empty input list stays empty; it is the AND/OR neutral element
		// used internally by goqu and its SQL is empty.
		return el
	case 1:
		return kept[0]
	default:
		return exp.NewExpressionList(typ, kept...)
	}
}

// combine evaluates AND/OR under Kleene three-valued logic.
func combine(typ exp.ExpressionListType, a, b exp.ConstKind) exp.ConstKind {
	if typ == exp.AndType {
		if a == exp.ConstFalse || b == exp.ConstFalse {
			return exp.ConstFalse
		}
		if a == exp.ConstUnknown || b == exp.ConstUnknown {
			return exp.ConstUnknown
		}
		return exp.ConstTrue
	}
	if a == exp.ConstTrue || b == exp.ConstTrue {
		return exp.ConstTrue
	}
	if a == exp.ConstUnknown || b == exp.ConstUnknown {
		return exp.ConstUnknown
	}
	return exp.ConstFalse
}

// allPureExcept reports whether every node other than exceptIdx is free of
// opaque or side-effecting subtrees. Index comparison is used because some
// expression values (e.g. literals with args) are not comparable with ==.
func (r *Reducer) allPureExcept(nodes []exp.Expression, exceptIdx int) bool {
	for i, node := range nodes {
		if i == exceptIdx {
			continue
		}
		if !r.isPure(node) {
			return false
		}
	}
	return true
}

func (r *Reducer) allPure(nodes []exp.Expression) bool {
	for _, node := range nodes {
		if !r.isPure(node) {
			return false
		}
	}
	return true
}

// allConstKind reports whether every node is a reducer constant of kind k.
func (r *Reducer) allConstKind(nodes []exp.Expression, k exp.ConstKind) bool {
	for _, node := range nodes {
		kind, ok := exp.ConstKindOf(node)
		if !ok || kind != k {
			return false
		}
	}
	return true
}

func normalizeName(name string) string {
	return strings.ToUpper(name)
}
