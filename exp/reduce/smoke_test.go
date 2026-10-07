package reduce_test

import (
	"testing"

	"github.com/doug-martin/goqu/v9"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/exp/reduce"
	"github.com/stretchr/testify/suite"
)

type smokeSuite struct{ suite.Suite }

func TestSmokeSuite(t *testing.T) { suite.Run(t, new(smokeSuite)) }

func (s *smokeSuite) TestRenderConstVariants() {
	r := reduce.New()
	cases := []struct {
		e   exp.Expression
		sql string
	}{
		{exp.NewConstExpression(exp.ConstTrue), "TRUE"},
		{exp.NewConstExpression(exp.ConstFalse), "FALSE"},
		{exp.NewConstExpression(exp.ConstUnknown), "NULL"},
		{r.Not(exp.NewConstExpression(exp.ConstUnknown)), "NULL"},
	}
	for _, tc := range cases {
		sql, _, err := goqu.From("t").Where(tc.e).ToSQL()
		s.NoError(err)
		s.Equal("SELECT * FROM \"t\" WHERE "+tc.sql, sql)
	}
}

func (s *smokeSuite) TestFoldInsideNestedOr() {
	F := exp.NewConstExpression(exp.ConstFalse)
	// OR(FALSE, a=1) inside an AND with b=2 -> a=1 AND b=2
	in := goqu.And(
		goqu.Or(F, goqu.I("a").Eq(1)),
		goqu.I("b").Eq(2),
	)
	out := reduce.Fold(in)
	sql, _, err := goqu.From("t").Where(out).ToSQL()
	s.NoError(err)
	s.Equal(`SELECT * FROM "t" WHERE (("a" = 1) AND ("b" = 2))`, sql)
}

func (s *smokeSuite) TestNotBetweenRoundTrip() {
	r := reduce.New()
	be := goqu.I("a").Between(exp.NewRangeVal(1, 10))
	nb := r.Not(be)
	bb := r.Not(nb)
	sql, _, err := goqu.From("t").Where(bb).ToSQL()
	s.NoError(err)
	s.Equal(`SELECT * FROM "t" WHERE ("a" BETWEEN 1 AND 10)`, sql)
}

func (s *smokeSuite) TestUnknownTypesUnchanged() {
	r := reduce.New()
	cases := []exp.Expression{
		goqu.L("raw sql = 1"),
		goqu.Ex{"a": 1},
		goqu.Func("NOW").Eq(1),
		goqu.I("a").As("x"),
	}
	for _, e := range cases {
		s.Equal(e, r.Reduce(e))
	}
}
