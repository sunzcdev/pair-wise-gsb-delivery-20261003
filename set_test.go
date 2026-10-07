package squirrel

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSetBuilderBasicOperations pins the SQL and argument ordering for each of
// the four supported set operators.
func TestSetBuilderBasicOperations(t *testing.T) {
	cases := []struct {
		name     string
		builder  SetBuilder
		expected string
	}{
		{
			name:     "union",
			builder:  Union(Select("a").From("t"), Select("b").From("u")),
			expected: "SELECT a FROM t UNION SELECT b FROM u",
		},
		{
			name:     "union all",
			builder:  UnionAll(Select("a").From("t"), Select("b").From("u")),
			expected: "SELECT a FROM t UNION ALL SELECT b FROM u",
		},
		{
			name:     "intersect",
			builder:  Intersect(Select("a").From("t"), Select("b").From("u")),
			expected: "SELECT a FROM t INTERSECT SELECT b FROM u",
		},
		{
			name:     "except",
			builder:  Except(Select("a").From("t"), Select("b").From("u")),
			expected: "SELECT a FROM t EXCEPT SELECT b FROM u",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := c.builder.ToSql()
			assert.NoError(t, err)
			assert.Equal(t, c.expected, sql)
			assert.Empty(t, args)
		})
	}
}

// TestSetBuilderArgsAndDollarPlaceholders pins the order in which branch
// arguments appear, both for question marks and for dollar placeholders.
func TestSetBuilderArgsAndDollarPlaceholders(t *testing.T) {
	b := UnionAll(
		Select("id").From("orders").Where("kind = ?", "formal"),
		Select("id").From("orders").Where("kind = ?", "reentry"),
		Select("id").From("orders").Where("kind = ?", "manual"),
	)

	sql, args, err := b.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"SELECT id FROM orders WHERE kind = ? UNION ALL SELECT id FROM orders WHERE kind = ? UNION ALL SELECT id FROM orders WHERE kind = ?",
		sql)
	assert.Equal(t, []interface{}{"formal", "reentry", "manual"}, args)

	sql, args, err = b.PlaceholderFormat(Dollar).ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"SELECT id FROM orders WHERE kind = $1 UNION ALL SELECT id FROM orders WHERE kind = $2 UNION ALL SELECT id FROM orders WHERE kind = $3",
		sql)
	assert.Equal(t, []interface{}{"formal", "reentry", "manual"}, args)
}

// TestSetBuilderBranchOrderAndLimit pins the rule that ordering or pagination
// on a single branch belongs to that branch (and is parenthesized), while the
// final ORDER BY/LIMIT applies to the whole set result.
func TestSetBuilderBranchOrderAndLimit(t *testing.T) {
	formal := Select("id").From("orders").Where("kind = ?", "formal")
	reentry := Select("id").From("orders").
		Where("kind = ?", "reentry").
		OrderBy("created_at DESC").
		Limit(50).
		Offset(10)

	b := UnionAll(formal, reentry).OrderBy("1").Limit(100)

	sql, args, err := b.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"SELECT id FROM orders WHERE kind = ? UNION ALL "+
			"(SELECT id FROM orders WHERE kind = ? ORDER BY created_at DESC LIMIT 50 OFFSET 10) "+
			"ORDER BY 1 LIMIT 100",
		sql)
	assert.Equal(t, []interface{}{"formal", "reentry"}, args)
}

// TestSetBuilderGroupingParentheses verifies that a nested SetBuilder is
// grouped with explicit parentheses, rather than being flattened by string
// concatenation.
func TestSetBuilderGroupingParentheses(t *testing.T) {
	a := Select("a").From("t")
	b := Select("b").From("u")
	c := Select("c").From("v")

	sql, _, err := Except(UnionAll(a, b), c).ToSql()
	assert.NoError(t, err)
	assert.Equal(t, "(SELECT a FROM t UNION ALL SELECT b FROM u) EXCEPT SELECT c FROM v", sql)

	sql, _, err = Except(a, Except(b, c)).ToSql()
	assert.NoError(t, err)
	assert.Equal(t, "SELECT a FROM t EXCEPT (SELECT b FROM u EXCEPT SELECT c FROM v)", sql)

	// A branch that starts with WITH must be parenthesized as well.
	withBranch := Select("a").From("t").Prefix("WITH q AS (SELECT 1 AS a)")
	sql, _, err = UnionAll(withBranch, Select("b").From("u")).ToSql()
	assert.NoError(t, err)
	assert.Equal(t, "(WITH q AS (SELECT 1 AS a) SELECT a FROM t) UNION ALL SELECT b FROM u", sql)

	// Operators of different precedence are expressed structurally.
	sql, _, err = Union(a, Intersect(b, c)).ToSql()
	assert.NoError(t, err)
	assert.Equal(t, "SELECT a FROM t UNION (SELECT b FROM u INTERSECT SELECT c FROM v)", sql)
}

// TestSetBuilderNestedErrors verifies that errors inside a nested set are
// surfaced instead of producing partial SQL.
func TestSetBuilderNestedErrors(t *testing.T) {
	_, _, err := UnionAll(Select("a").From("t"), Union(Select("b").From("u"))).ToSql()
	assert.Error(t, err)

	locked := Union(
		Select("b").From("u"),
		Select("c").From("v").Suffix("FOR UPDATE"),
	)
	_, _, err = UnionAll(Select("a").From("t"), locked).ToSql()
	assert.Error(t, err)
}

// TestSetBuilderFromSet pins use of a whole set result as a derived table,
// including placeholder renumbering through the outer query.
func TestSetBuilderFromSet(t *testing.T) {
	formal := Select("id").From("orders").Where("kind = ?", "formal")
	reentry := Select("id").From("orders").Where("kind = ?", "reentry")
	archived := Select("id").From("orders").Where("archived = ?", true)

	set := Except(UnionAll(formal, reentry), archived).PlaceholderFormat(Dollar)

	b := Select("n.id").FromSet(set, "n").Where("n.id > ?", 9).PlaceholderFormat(Dollar)
	sql, args, err := b.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"SELECT n.id FROM ((SELECT id FROM orders WHERE kind = $1 UNION ALL SELECT id FROM orders WHERE kind = $2) "+
			"EXCEPT SELECT id FROM orders WHERE archived = $3) AS n WHERE n.id > $4",
		sql)
	assert.Equal(t, []interface{}{"formal", "reentry", true, 9}, args)
}

// TestSetBuilderErrors covers combinations that must be rejected up front.
func TestSetBuilderErrors(t *testing.T) {
	t.Run("single branch", func(t *testing.T) {
		_, _, err := Union(Select("a").From("t")).ToSql()
		assert.Error(t, err)
	})

	t.Run("no branch", func(t *testing.T) {
		_, _, err := Intersect().ToSql()
		assert.Error(t, err)
	})

	t.Run("nil branch", func(t *testing.T) {
		_, _, err := UnionAll(Select("a").From("t"), nil).ToSql()
		assert.Error(t, err)
	})

	t.Run("for update on branch", func(t *testing.T) {
		locked := Select("a").From("t").Suffix("FOR UPDATE")
		_, _, err := Union(Select("a").From("t"), locked).ToSql()
		assert.Error(t, err)
	})

	t.Run("for share on whole set", func(t *testing.T) {
		b := Union(Select("a").From("t"), Select("b").From("u")).Suffix("FOR SHARE")
		_, _, err := b.ToSql()
		assert.Error(t, err)
	})
}

// TestSetBuilderRunners checks RunWith wiring.
func TestSetBuilderRunners(t *testing.T) {
	db := &DBStub{}
	b := UnionAll(Select("a").From("t"), Select("b").From("u")).RunWith(db)

	_, err := b.Query()
	assert.NoError(t, err)
	assert.Equal(t, "SELECT a FROM t UNION ALL SELECT b FROM u", db.LastQuerySql)

	_, err = b.Exec()
	assert.NoError(t, err)
	assert.Equal(t, "SELECT a FROM t UNION ALL SELECT b FROM u", db.LastExecSql)

	b = UnionAll(Select("a").From("t"), Select("b").From("u"))
	_, err = b.Query()
	assert.Equal(t, RunnerNotSet, err)
}

// TestStatementBuilderTypeSetOperators verifies Runner/Placeholder inheritance.
func TestStatementBuilderTypeSetOperators(t *testing.T) {
	db := &DBStub{}
	sb := StatementBuilder.RunWith(db).PlaceholderFormat(Dollar)

	b := sb.Except(
		sb.Select("a").From("t").Where("x = ?", 1),
		sb.Select("b").From("u"),
	)
	sql, args, err := b.ToSql()
	assert.NoError(t, err)
	assert.Equal(t, "SELECT a FROM t WHERE x = $1 EXCEPT SELECT b FROM u", sql)
	assert.Equal(t, []interface{}{1}, args)

	_, err = b.Query()
	assert.NoError(t, err)
	assert.Equal(t, "SELECT a FROM t WHERE x = $1 EXCEPT SELECT b FROM u", db.LastQuerySql)
}

// exceptMultiset models PostgreSQL EXCEPT (without ALL): it deduplicates its
// result, returning one copy of every row present in left that is absent from
// right.
func exceptMultiset(left, right []string) []string {
	excluded := map[string]bool{}
	for _, r := range right {
		excluded[r] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range left {
		if !excluded[l] && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

func unionAllMultiset(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestSetParenthesesChangeResultWithDuplicates is the acceptance case: with
// duplicate rows in the input, moving one pair of parentheses changes both the
// generated SQL and the rows PostgreSQL returns.
//
// The report query is: (formal orders UNION ALL re-entered orders)
// EXCEPT archived orders. Parenthesizing the UNION ALL side is required;
// grouping the EXCEPT side instead silently changes the result.
func TestSetParenthesesChangeResultWithDuplicates(t *testing.T) {
	// Duplicate-rich data: one order was recorded twice in different branches.
	formal := []string{"1", "1", "2"}
	reentry := []string{"1"}
	archived := []string{"1"}

	// Left grouping: (formal UNION ALL reentry) EXCEPT archived
	leftRows := exceptMultiset(unionAllMultiset(formal, reentry), archived)

	// Right grouping: formal UNION ALL (reentry EXCEPT archived)
	inner := exceptMultiset(reentry, archived)
	rightRows := unionAllMultiset(formal, inner)

	// The duplicate rows are what make the two parenthesizations differ.
	assert.Equal(t, []string{"1", "1", "2", "1"}, unionAllMultiset(formal, reentry))
	assert.Equal(t, []string{"2"}, leftRows)
	assert.Equal(t, []string{"1", "1", "2"}, rightRows)
	assert.NotEqual(t, leftRows, rightRows)

	formalQ := Select("id").From("orders").Where("kind = ?", "formal")
	reentryQ := Select("id").From("orders").Where("kind = ?", "reentry")
	archivedQ := Select("id").From("orders").Where("archived = ?", true)

	leftSQL, leftArgs, err := Except(UnionAll(formalQ, reentryQ), archivedQ).
		PlaceholderFormat(Dollar).ToSql()
	assert.NoError(t, err)

	rightSQL, rightArgs, err := UnionAll(formalQ, Except(reentryQ, archivedQ)).
		PlaceholderFormat(Dollar).ToSql()
	assert.NoError(t, err)

	assert.Equal(t,
		"(SELECT id FROM orders WHERE kind = $1 UNION ALL SELECT id FROM orders WHERE kind = $2) "+
			"EXCEPT SELECT id FROM orders WHERE archived = $3",
		leftSQL)
	assert.Equal(t,
		"SELECT id FROM orders WHERE kind = $1 UNION ALL "+
			"(SELECT id FROM orders WHERE kind = $2 EXCEPT SELECT id FROM orders WHERE archived = $3)",
		rightSQL)

	// Parameter positions are identical; only the parentheses move.
	assert.Equal(t, []interface{}{"formal", "reentry", true}, leftArgs)
	assert.Equal(t, leftArgs, rightArgs)

	// The only textual difference between the two statements is the placement
	// of parentheses and operators, proving they are the same query grouped two
	// different ways.
	normalize := func(s string) string {
		s = strings.NewReplacer("(", "", ")", "").Replace(s)
		s = strings.ReplaceAll(s, " ", "")
		return s
	}
	assert.Equal(t, normalize(leftSQL), normalize(rightSQL))
	assert.NotEqual(t, leftSQL, rightSQL)
}
