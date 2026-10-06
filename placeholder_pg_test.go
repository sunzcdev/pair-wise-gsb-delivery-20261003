package squirrel

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDollarPGBasic(t *testing.T) {
	sql := "x = ? AND y = ?"
	s, err := DollarPG.ReplacePlaceholders(sql)
	assert.NoError(t, err)
	assert.Equal(t, "x = $1 AND y = $2", s)
}

func TestDollarPGStringLiterals(t *testing.T) {
	sql := "a = 'it''s ?' AND b = E'esc\\'?' AND c = ?"
	s, err := DollarPG.ReplacePlaceholders(sql)
	assert.NoError(t, err)
	assert.Equal(t, "a = 'it''s ?' AND b = E'esc\\'?' AND c = $1", s)
}

func TestDollarPGQuotedIdentifier(t *testing.T) {
	sql := `SELECT "weird?col" FROM t WHERE "what" = ?`
	s, err := DollarPG.ReplacePlaceholders(sql)
	assert.NoError(t, err)
	assert.Equal(t, `SELECT "weird?col" FROM t WHERE "what" = $1`, s)
}

func TestDollarPGDollarQuotedString(t *testing.T) {
	sql := "f = $func$body ? $func$ AND g = $$??$$ AND h = ?"
	s, err := DollarPG.ReplacePlaceholders(sql)
	assert.NoError(t, err)
	assert.Equal(t, "f = $func$body ? $func$ AND g = $$??$$ AND h = $1", s)
}

func TestDollarPGComments(t *testing.T) {
	sql := "SELECT a -- what?\n/* block ? /* nested ? */ still ? */ FROM t WHERE a = ? AND b = ?"
	s, err := DollarPG.ReplacePlaceholders(sql)
	assert.NoError(t, err)
	assert.Equal(t, "SELECT a -- what?\n/* block ? /* nested ? */ still ? */ FROM t WHERE a = $1 AND b = $2", s)
}

func TestDollarPGLineCommentAtEOF(t *testing.T) {
	sql := "a = ? -- trailing ?"
	s, err := DollarPG.ReplacePlaceholders(sql)
	assert.NoError(t, err)
	assert.Equal(t, "a = $1 -- trailing ?", s)
}

func TestDollarPGJSONOperators(t *testing.T) {
	sql := `data ??| array['?'] AND data ?? 'key' AND enabled = ?`
	s, err := DollarPG.ReplacePlaceholders(sql)
	assert.NoError(t, err)
	assert.Equal(t, `data ?| array['?'] AND data ? 'key' AND enabled = $1`, s)
}

func TestDollarPGPositionalParameterLeftAlone(t *testing.T) {
	sql := "a = $1 AND b = ?"
	s, err := DollarPG.ReplacePlaceholders(sql)
	assert.NoError(t, err)
	assert.Equal(t, "a = $1 AND b = $1", s)
}

func TestDollarPGUnterminated(t *testing.T) {
	cases := []struct {
		name      string
		sql       string
		construct string
		offset    int
		line      int
		column    int
	}{
		{"string", "a = 'oops", "string literal", 4, 1, 5},
		{"escape string", "a = E'oops", "escape string literal", 4, 1, 5},
		{"identifier", `a = "oops`, "double-quoted identifier", 4, 1, 5},
		{"block comment", "a = 1 /* oops /* nested */", "block comment", 6, 1, 7},
		{"dollar quote", "a = $tag$oops", "dollar-quoted string", 4, 1, 5},
		{"multiline", "a = 1\nb = 2\nc = 'oops", "string literal", 16, 3, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DollarPG.ReplacePlaceholders(tc.sql)
			assert.Error(t, err)
			var lexErr *LexError
			assert.True(t, errors.As(err, &lexErr), "expected *LexError, got %T", err)
			assert.Equal(t, tc.construct, lexErr.Construct)
			assert.Equal(t, tc.offset, lexErr.Offset)
			assert.Equal(t, tc.line, lexErr.Line)
			assert.Equal(t, tc.column, lexErr.Column)
		})
	}
}

func TestDollarPGNestedSelectAndExpr(t *testing.T) {
	inner := StatementBuilder.Select("id").From("orders").
		Where("status = ?", "paid").
		Where("total > ?", 100)

	sql, args, err := StatementBuilder.PlaceholderFormat(DollarPG).
		Select("*").From("users").
		Where("/* report: who? when? */ active = ?", true).
		Where(Expr("age > ? AND city = ?", 18, "beijing")).
		Where(Expr("id IN (?)", inner)).
		Where("note = '-- not a comment? '").
		ToSql()

	assert.NoError(t, err)
	assert.Equal(t,
		"SELECT * FROM users WHERE /* report: who? when? */ active = $1 "+
			"AND age > $2 AND city = $3 "+
			"AND id IN (SELECT id FROM orders WHERE status = $4 AND total > $5) "+
			"AND note = '-- not a comment? '",
		sql)
	assert.Equal(t, []interface{}{true, 18, "beijing", "paid", 100}, args)
}

func TestDollarPGNestedSelectNumberingNotShiftedByComment(t *testing.T) {
	nested := StatementBuilder.Select("*").Prefix("NOT EXISTS (").
		From("bar").Where("y = ?", 42).Suffix(")")

	sql, args, err := StatementBuilder.PlaceholderFormat(DollarPG).Select("*").
		From("foo").
		Where("-- why?\nx = ?", 7).
		Where(nested).
		ToSql()

	assert.NoError(t, err)
	assert.Equal(t,
		"SELECT * FROM foo WHERE -- why?\nx = $1 AND NOT EXISTS ( SELECT * FROM bar WHERE y = $2 )",
		sql)
	assert.Equal(t, []interface{}{7, 42}, args)
}
