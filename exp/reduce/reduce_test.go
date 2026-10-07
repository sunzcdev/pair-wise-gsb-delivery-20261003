package reduce_test

import (
	"database/sql/driver"
	"testing"

	"github.com/doug-martin/goqu/v9"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/exp/reduce"
	"github.com/stretchr/testify/suite"
)

type reducerSuite struct {
	suite.Suite
}

func TestReducerSuite(t *testing.T) {
	suite.Run(t, new(reducerSuite))
}

func (s *reducerSuite) sql(e exp.Expression) string {
	sql, _, err := goqu.From("t").Where(e).ToSQL()
	s.NoError(err)
	return sql
}

func (s *reducerSuite) TestAndFolding() {
	T := exp.NewConstExpression(exp.ConstTrue)
	F := exp.NewConstExpression(exp.ConstFalse)
	U := exp.NewConstExpression(exp.ConstUnknown)
	a := goqu.I("a").Eq(1)

	cases := []struct {
		name string
		in   exp.Expression
		out  exp.Expression
	}{
		{"false dominates AND", goqu.And(F, a), F},
		{"true identity removed AND", goqu.And(T, a), a},
		{"all true AND", goqu.And(T, T), T},
		{"false and unknown AND", goqu.And(F, U), F},
		// UNKNOWN AND p is not a constant: p could be FALSE.
		{"unknown with predicate AND kept", goqu.And(U, a), goqu.And(U, a)},
		{"only unknown constants AND -> unknown", goqu.And(T, U, U), U},
		{"nested same connective flattened", goqu.And(T, goqu.And(T, a)), a},
	}
	for _, tc := range cases {
		s.Run(tc.name, func() {
			s.Equal(tc.out, reduce.Fold(tc.in))
		})
	}
}

func (s *reducerSuite) TestOrFolding() {
	T := exp.NewConstExpression(exp.ConstTrue)
	F := exp.NewConstExpression(exp.ConstFalse)
	U := exp.NewConstExpression(exp.ConstUnknown)
	a := goqu.I("a").Eq(1)

	cases := []struct {
		name string
		in   exp.Expression
		out  exp.Expression
	}{
		{"true dominates OR", goqu.Or(T, a), T},
		{"false identity removed OR", goqu.Or(F, a), a},
		{"all false OR", goqu.Or(F, F), F},
		{"true and unknown OR", goqu.Or(T, U), T},
		{"unknown does not dominate OR", goqu.Or(U, a), goqu.Or(U, a)},
		{"unknown with false OR", goqu.Or(F, U), U},
		{"nested flatten OR", goqu.Or(F, goqu.Or(F, a)), a},
	}
	for _, tc := range cases {
		s.Run(tc.name, func() {
			s.Equal(tc.out, reduce.Fold(tc.in))
		})
	}
}

func (s *reducerSuite) TestNullConstantDoesNotEliminateImpureSibling() {
	U := exp.NewConstExpression(exp.ConstUnknown)
	v := &countingValuer{}
	impure := goqu.I("a").Eq(v)

	// AND with UNKNOWN would erase the list, but the sibling calls Value(),
	// so the whole list must be kept to preserve side effects.
	in := goqu.And(U, impure)
	out := reduce.Fold(in).(exp.ExpressionList)
	s.Len(out.Expressions(), 2)

	// Same for OR with TRUE.
	T := exp.NewConstExpression(exp.ConstTrue)
	in = goqu.Or(T, impure)
	out = reduce.Fold(in).(exp.ExpressionList)
	s.Len(out.Expressions(), 2)

	// With purity declared for the subtree (no Valuer, no Literal, no
	// functions), elimination is allowed.
	pure := goqu.I("a").Eq(1)
	s.Equal(T, reduce.Fold(goqu.Or(T, pure)))
}

func (s *reducerSuite) TestLiteralIsOpaque() {
	T := exp.NewConstExpression(exp.ConstTrue)
	lit := goqu.L("some_side_effect() = 1")

	// OR TRUE would normally erase the list, but L(...) is opaque SQL.
	out := reduce.Fold(goqu.Or(T, lit)).(exp.ExpressionList)
	s.Len(out.Expressions(), 2)

	// A lone literal is returned untouched.
	s.Equal(lit, reduce.Fold(lit))
}

func (s *reducerSuite) TestUnknownFunctionConservative() {
	r := reduce.New(reduce.WithPureFunctions("COALESCE"))
	T := exp.NewConstExpression(exp.ConstTrue)

	unknown := goqu.Func("MY_FUNC", goqu.I("a")).Eq(1)
	out := reduce.Fold(goqu.Or(T, unknown)).(exp.ExpressionList)
	s.Len(out.Expressions(), 2)

	// Declaring the function pure permits elimination.
	s.Equal(T, r.Reduce(goqu.Or(T, goqu.Func("COALESCE", goqu.I("a"), 1).Eq(1))))
}

func (s *reducerSuite) TestNullableSelfEquality() {
	col := goqu.I("a")

	// Without nullability info x = x is kept.
	s.Equal(col.Eq(col), reduce.Fold(col.Eq(col)))

	// Nullable (explicit or implicit) keeps it too.
	rNullable := reduce.New(reduce.WithNullability(map[string]reduce.Nullability{"a": reduce.Nullable}))
	s.Equal(col.Eq(col), rNullable.Reduce(col.Eq(col)))

	// Non-null proof folds it.
	rNN := reduce.New(reduce.WithNullability(map[string]reduce.Nullability{"a": reduce.NonNullable}))
	s.Equal(exp.NewConstExpression(exp.ConstTrue), rNN.Reduce(col.Eq(col)))
	s.Equal(exp.NewConstExpression(exp.ConstFalse), rNN.Reduce(col.Neq(col)))
}

func (s *reducerSuite) TestNullTestsNeedNonNullProof() {
	col := goqu.I("a")
	rNN := reduce.New(reduce.WithNullability(map[string]reduce.Nullability{"a": reduce.NonNullable}))

	s.Equal(exp.NewConstExpression(exp.ConstFalse), rNN.Reduce(col.Is(nil)))
	s.Equal(exp.NewConstExpression(exp.ConstTrue), rNN.Reduce(col.IsNot(nil)))

	// Without proof the predicates stay.
	s.Equal(col.Is(nil), reduce.Fold(col.Is(nil)))
	s.Equal(col.IsNot(nil), reduce.Fold(col.IsNot(nil)))
}

func (s *reducerSuite) TestConstantComparison() {
	T := exp.NewConstExpression(exp.ConstTrue)
	F := exp.NewConstExpression(exp.ConstFalse)
	U := exp.NewConstExpression(exp.ConstUnknown)

	s.Equal(T, reduce.Fold(T.Eq(true)))
	s.Equal(F, reduce.Fold(T.Eq(false)))
	s.Equal(F, reduce.Fold(T.Neq(true)))
	s.Equal(U, reduce.Fold(U.Eq(1)))
	s.Equal(U, reduce.Fold(T.Eq(U)))
	s.Equal(T, reduce.Fold(U.Is(nil)))
	s.Equal(F, reduce.Fold(U.Is(true)))
	s.Equal(T, reduce.Fold(T.Is(true)))
	s.Equal(F, reduce.Fold(T.IsNot(true)))
}

func (s *reducerSuite) TestNestedConstantPropagation() {
	T := exp.NewConstExpression(exp.ConstTrue)
	F := exp.NewConstExpression(exp.ConstFalse)
	a := goqu.I("a").Eq(1)
	b := goqu.I("b").Eq(2)

	// (a AND FALSE) OR b  -> b
	in := goqu.Or(goqu.And(a, F), b)
	s.Equal(b, reduce.Fold(in))

	// (a OR TRUE) AND b -> FALSE-free simplification -> b
	in = goqu.And(goqu.Or(a, T), b)
	s.Equal(b, reduce.Fold(in))

	// (a OR b) AND FALSE -> FALSE (both siblings are pure column predicates)
	in = goqu.And(goqu.Or(a, b), F)
	s.Equal(F, reduce.Fold(in))
}

func (s *reducerSuite) TestNotConstantAndOperators() {
	T := exp.NewConstExpression(exp.ConstTrue)
	F := exp.NewConstExpression(exp.ConstFalse)
	U := exp.NewConstExpression(exp.ConstUnknown)
	r := reduce.New()

	s.Equal(F, r.Not(T))
	s.Equal(T, r.Not(F))
	s.Equal(U, r.Not(U))

	s.Equal(goqu.I("a").Neq(1), r.Not(goqu.I("a").Eq(1)))
	s.Equal(goqu.I("a").Eq(1), r.Not(goqu.I("a").Neq(1)))
	s.Equal(goqu.I("a").Lte(1), r.Not(goqu.I("a").Gt(1)))
	s.Equal(goqu.I("a").IsNot(nil), r.Not(goqu.I("a").Is(nil)))
	s.Equal(goqu.I("a").NotIn(1, 2), r.Not(goqu.I("a").In(1, 2)))
	s.Equal(goqu.I("a").NotLike("x%"), r.Not(goqu.I("a").Like("x%")))
}

func (s *reducerSuite) TestNotDeMorgan() {
	r := reduce.New()
	a := goqu.I("a").Eq(1)
	b := goqu.I("b").Eq(2)

	out := r.Not(goqu.And(a, b)).(exp.ExpressionList)
	s.Equal(exp.OrType, out.Type())
	s.Len(out.Expressions(), 2)

	out = r.Not(goqu.Or(a, b)).(exp.ExpressionList)
	s.Equal(exp.AndType, out.Type())
	s.Len(out.Expressions(), 2)

	// Double negation through operators returns to the original.
	notA := r.Not(a)
	s.Equal(a, r.Not(notA))
}

func (s *reducerSuite) TestNotOpaqueFallback() {
	r := reduce.New()
	lit := goqu.L("custom_predicate(x)")
	// Without a structural negation the expression is reduced but unchanged.
	s.Equal(lit, r.Not(lit))

	// One opaque operand blocks De Morgan rewriting of the whole list.
	mixed := goqu.Or(goqu.I("a").Eq(1), lit)
	s.Equal(mixed, r.Not(mixed))
}

func (s *reducerSuite) TestEvaluationOrderAndValuerCalls() {
	v := &countingValuer{}
	r := reduce.New()

	in := goqu.And(
		goqu.I("a").Eq(v),
		goqu.I("b").Eq(v),
		goqu.I("c").Eq(1),
	)
	out := r.Reduce(in)

	// Reduction itself never calls Value.
	s.Equal(0, v.calls)

	// Generating SQL from the reduced expression calls Value exactly twice,
	// once per occurrence, same as the original.
	s.sql(out)
	s.Equal(2, v.calls)

	// Original order is preserved.
	list := out.(exp.ExpressionList)
	s.Equal("a", list.Expressions()[0].(exp.BooleanExpression).
		LHS().(exp.IdentifierExpression).GetCol())
	s.Equal("b", list.Expressions()[1].(exp.BooleanExpression).
		LHS().(exp.IdentifierExpression).GetCol())
}

func (s *reducerSuite) TestEmptyAndUnknownListShapes() {
	empty := exp.NewExpressionList(exp.AndType)
	s.Equal(empty, reduce.Fold(empty))
}

func (s *reducerSuite) TestQualifiedNullabilityKey() {
	col := goqu.I("t.a")
	r := reduce.New(reduce.WithNullability(map[string]reduce.Nullability{
		"a":   reduce.NonNullable, // unqualified key must not match t.a
		"t.a": reduce.NonNullable,
	}))
	s.Equal(exp.NewConstExpression(exp.ConstTrue), r.Reduce(col.Eq(col)))

	col2 := goqu.I("other.a")
	s.Equal(col2.Eq(col2), r.Reduce(col2.Eq(col2)))
}

func (s *reducerSuite) TestReducedExpressionRendersSQL() {
	r := reduce.New(reduce.WithNullability(map[string]reduce.Nullability{
		"id": reduce.NonNullable,
	}))

	in := goqu.And(
		exp.NewConstExpression(exp.ConstTrue),
		goqu.I("id").Eq(goqu.I("id")),
		goqu.I("status").Eq("active"),
		goqu.Or(exp.NewConstExpression(exp.ConstFalse), goqu.I("kind").Eq("x")),
	)
	reduced := r.Reduce(in)

	sql, args, err := goqu.From("users").Where(reduced).ToSQL()
	s.NoError(err)
	s.Empty(args)
	s.Equal(`SELECT * FROM "users" WHERE (("status" = 'active') AND ("kind" = 'x'))`, sql)

	// Prepared statements keep placeholders in operand order.
	prep, prepArgs, err := goqu.From("users").Where(reduced).Prepared(true).ToSQL()
	s.NoError(err)
	s.Equal([]interface{}{"active", "x"}, prepArgs)
	s.Equal(`SELECT * FROM "users" WHERE (("status" = ?) AND ("kind" = ?))`, prep)
}

func (s *reducerSuite) TestFalseWholeWhere() {
	F := exp.NewConstExpression(exp.ConstFalse)
	sql, _, err := goqu.From("t").Where(goqu.And(F, goqu.I("a").Eq(1))).ToSQL()
	s.NoError(err)
	// Without reducing, nothing is folded; the reducer yields just FALSE.
	reduced := reduce.Fold(goqu.And(F, goqu.I("a").Eq(1)))
	rsql, _, err := goqu.From("t").Where(reduced).ToSQL()
	s.NoError(err)
	s.Equal("SELECT * FROM \"t\" WHERE FALSE", rsql)
	s.NotEqual(sql, rsql)
}

type countingValuer struct {
	calls int
}

func (c *countingValuer) Value() (driver.Value, error) {
	c.calls++
	return 1, nil
}
