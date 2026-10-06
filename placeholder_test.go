package squirrel

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestQuestion(t *testing.T) {
	sql := "x = ? AND y = ?"
	s, _ := Question.ReplacePlaceholders(sql)
	assert.Equal(t, sql, s)
}

func TestDollar(t *testing.T) {
	sql := "x = ? AND y = ?"
	s, _ := Dollar.ReplacePlaceholders(sql)
	assert.Equal(t, "x = $1 AND y = $2", s)
}

func TestColon(t *testing.T) {
	sql := "x = ? AND y = ?"
	s, _ := Colon.ReplacePlaceholders(sql)
	assert.Equal(t, "x = :1 AND y = :2", s)
}

func TestAtp(t *testing.T) {
	sql := "x = ? AND y = ?"
	s, _ := AtP.ReplacePlaceholders(sql)
	assert.Equal(t, "x = @p1 AND y = @p2", s)
}

func TestPlaceholders(t *testing.T) {
	assert.Equal(t, Placeholders(2), "?,?")
}

func TestEscapeDollar(t *testing.T) {
	sql := "SELECT uuid, \"data\" #> '{tags}' AS tags FROM nodes WHERE  \"data\" -> 'tags' ??| array['?'] AND enabled = ?"
	s, _ := Dollar.ReplacePlaceholders(sql)
	assert.Equal(t, "SELECT uuid, \"data\" #> '{tags}' AS tags FROM nodes WHERE  \"data\" -> 'tags' ?| array['$1'] AND enabled = $2", s)
}

func TestEscapeColon(t *testing.T) {
	sql := "SELECT uuid, \"data\" #> '{tags}' AS tags FROM nodes WHERE  \"data\" -> 'tags' ??| array['?'] AND enabled = ?"
	s, _ := Colon.ReplacePlaceholders(sql)
	assert.Equal(t, "SELECT uuid, \"data\" #> '{tags}' AS tags FROM nodes WHERE  \"data\" -> 'tags' ?| array[':1'] AND enabled = :2", s)
}

func TestEscapeAtp(t *testing.T) {
	sql := "SELECT uuid, \"data\" #> '{tags}' AS tags FROM nodes WHERE  \"data\" -> 'tags' ??| array['?'] AND enabled = ?"
	s, _ := AtP.ReplacePlaceholders(sql)
	assert.Equal(t, "SELECT uuid, \"data\" #> '{tags}' AS tags FROM nodes WHERE  \"data\" -> 'tags' ?| array['@p1'] AND enabled = @p2", s)
}

func TestPostgreSQL(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"plain", "x = ? AND y = ?", "x = $1 AND y = $2"},
		{"string literal", "a = '?' AND b = ?", "a = '?' AND b = $1"},
		{"string doubled quote", "a = 'it''s ?' AND b = ?", "a = 'it''s ?' AND b = $1"},
		{"string with comment text", "a = '-- ?' AND b = ?", "a = '-- ?' AND b = $1"},
		{"e-string", `a = E'it\'s ?' AND b = ?`, `a = E'it\'s ?' AND b = $1`},
		{"e-string escaped backslash", `a = E'\\' AND b = ?`, `a = E'\\' AND b = $1`},
		{"quoted identifier", `"my?col" = ?`, `"my?col" = $1`},
		{"quoted identifier doubled quote", `"my""?col" = ?`, `"my""?col" = $1`},
		{"dollar quote", "f = $$body ? $$ AND x = ?", "f = $$body ? $$ AND x = $1"},
		{"tagged dollar quote", "f = $func$x ? $func$ AND y = ?", "f = $func$x ? $func$ AND y = $1"},
		{"dollar quote with quotes", "f = $tag$'?' -- not a comment$tag$ AND y = ?",
			"f = $tag$'?' -- not a comment$tag$ AND y = $1"},
		{"line comment", "-- what?\nx = ?", "-- what?\nx = $1"},
		{"line comment at eof", "x = ? -- done?", "x = $1 -- done?"},
		{"block comment", "/* what? */ x = ?", "/* what? */ x = $1"},
		{"nested block comment", "/* a ? /* b ? */ c ? */ x = ?", "/* a ? /* b ? */ c ? */ x = $1"},
		{"json operator escape", "data ??| array['x'] AND enabled = ?", "data ?| array['x'] AND enabled = $1"},
		{"json operator inside string untouched", "a = '??' AND b = ?", "a = '??' AND b = $1"},
		{"dollar param not a tag", "x = ?::text", "x = $1::text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := PostgreSQL.ReplacePlaceholders(c.sql)
			assert.NoError(t, err)
			assert.Equal(t, c.want, s)
		})
	}
}

func TestPostgreSQLErrors(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		kind   string
		offset int
		line   int
		column int
	}{
		{"string literal", "SELECT 'abc", "string literal", 7, 1, 8},
		{"e-string", "SELECT E'abc", "string literal", 8, 1, 9},
		{"quoted identifier", "SELECT \"abc", "quoted identifier", 7, 1, 8},
		{"block comment", "SELECT 1 /* outer /* inner */", "block comment", 9, 1, 10},
		{"dollar-quoted string", "SELECT $$ body", "dollar-quoted string", 7, 1, 8},
		{"tagged dollar-quoted string", "SELECT\n$f$ body", "dollar-quoted string", 7, 2, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := PostgreSQL.ReplacePlaceholders(c.sql)
			assert.Error(t, err)
			var synErr *PlaceholderSyntaxError
			if assert.True(t, errors.As(err, &synErr), "expected *PlaceholderSyntaxError, got %T", err) {
				assert.Equal(t, c.kind, synErr.Kind)
				assert.Equal(t, c.offset, synErr.Offset)
				assert.Equal(t, c.line, synErr.Line)
				assert.Equal(t, c.column, synErr.Column)
			}
		})
	}
}

func TestPostgreSQLNestedAndExpr(t *testing.T) {
	sub := Select("user_id").From("orders").Where("total > ?", 100)
	sql, args, err := Select("*").From("users").
		Where("/* active? only */ active = ?", true).
		Where(Expr("note = '-- ?' AND role = ?", "admin")).
		Where(Expr("id IN (?)", sub)).
		Where("tags ??| array[?]", "vip").
		PlaceholderFormat(PostgreSQL).
		ToSql()

	assert.NoError(t, err)
	assert.Equal(t,
		"SELECT * FROM users WHERE /* active? only */ active = $1 AND "+
			"note = '-- ?' AND role = $2 AND "+
			"id IN (SELECT user_id FROM orders WHERE total > $3) AND "+
			"tags ?| array[$4]",
		sql)
	assert.Equal(t, []interface{}{true, "admin", 100, "vip"}, args)
}

func BenchmarkPlaceholdersArray(b *testing.B) {
	var count = b.N
	placeholders := make([]string, count)
	for i := 0; i < count; i++ {
		placeholders[i] = "?"
	}
	var _ = strings.Join(placeholders, ",")
}

func BenchmarkPlaceholdersStrings(b *testing.B) {
	Placeholders(b.N)
}
