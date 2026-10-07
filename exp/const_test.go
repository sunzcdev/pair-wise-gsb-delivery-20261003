package exp_test

import (
	"testing"

	"github.com/doug-martin/goqu/v9"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/stretchr/testify/suite"
)

type constSuite struct {
	suite.Suite
}

func TestConstSuite(t *testing.T) {
	suite.Run(t, new(constSuite))
}

func (s *constSuite) TestConstKinds() {
	cases := []struct {
		kind exp.ConstKind
		sql  string
	}{
		{exp.ConstTrue, "TRUE"},
		{exp.ConstFalse, "FALSE"},
		{exp.ConstUnknown, "NULL"},
	}
	for _, tc := range cases {
		e := exp.NewConstExpression(tc.kind)
		sql, _, err := goqu.From("t").Where(e).ToSQL()
		s.NoError(err)
		s.Equal("SELECT * FROM \"t\" WHERE "+tc.sql, sql)

		kind, ok := exp.ConstKindOf(e)
		s.True(ok)
		s.Equal(tc.kind, kind)

		// The constant is immutable; Clone returns the shared singleton.
		s.Equal(e, e.Clone())
	}
}

func (s *constSuite) TestConstKindOfLiteral() {
	// Caller-authored literal SQL is never a known constant even if the text
	// happens to spell TRUE.
	_, ok := exp.ConstKindOf(exp.NewLiteralExpression("TRUE"))
	s.False(ok)
}
