package squirrel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecursiveEmployeeTree(t *testing.T) {
	subordinates := CTE("subordinates", "id", "name", "manager_id", "depth").
		Anchor(
			Select("id", "name", "manager_id", "0").
				From("employees").
				Where(Eq{"id": 1}),
		).
		RecursiveTerm(
			Select("e.id", "e.name", "e.manager_id", "s.depth + 1").
				From("employees e").
				Join("subordinates s ON e.manager_id = s.id"),
		)

	sql, args, err := With(subordinates).
		Select("id", "name", "depth").
		From("subordinates").
		OrderBy("depth", "id").
		ToSql()

	assert.NoError(t, err)
	assert.Equal(t,
		"WITH RECURSIVE subordinates (id, name, manager_id, depth) AS ("+
			"SELECT id, name, manager_id, 0 FROM employees WHERE id = ? "+
			"UNION ALL "+
			"SELECT e.id, e.name, e.manager_id, s.depth + 1 FROM employees e "+
			"JOIN subordinates s ON e.manager_id = s.id"+
			") SELECT id, name, depth FROM subordinates ORDER BY depth, id",
		sql)
	assert.Equal(t, []interface{}{1}, args)
}

func TestNestedCTEShadowing(t *testing.T) {
	inner := With(CTE("t").As(Select("id").From("inner_table"))).
		Select("id").
		From("t")

	sql, args, err := With(CTE("t").As(inner)).
		Select("*").
		From("t").
		ToSql()

	assert.NoError(t, err)
	assert.Equal(t,
		"WITH t AS (WITH t AS (SELECT id FROM inner_table) SELECT id FROM t) SELECT * FROM t",
		sql)
	assert.Empty(t, args)
}

func TestSharedSubqueryBetweenCTEs(t *testing.T) {
	active := Select("id", "department").From("employees").Where(Eq{"active": true})
	engineers := CTE("engineers").As(active.Where(Eq{"department": "eng"}))
	salespeople := CTE("salespeople").As(active.Where(Eq{"department": "sales"}))

	sql, args, err := With(engineers, salespeople).
		Select("id").
		From("engineers").
		Suffix("UNION ALL SELECT id FROM salespeople").
		ToSql()

	assert.NoError(t, err)
	assert.Equal(t,
		"WITH engineers AS (SELECT id, department FROM employees WHERE active = ? AND department = ?), "+
			"salespeople AS (SELECT id, department FROM employees WHERE active = ? AND department = ?) "+
			"SELECT id FROM engineers UNION ALL SELECT id FROM salespeople",
		sql)
	assert.Equal(t, []interface{}{true, "eng", true, "sales"}, args)

	// The shared subquery builder must not be affected by the derived CTEs.
	baseSQL, baseArgs, err := active.ToSql()
	assert.NoError(t, err)
	assert.Equal(t, "SELECT id, department FROM employees WHERE active = ?", baseSQL)
	assert.Equal(t, []interface{}{true}, baseArgs)
}

func TestWithBuilderImmutability(t *testing.T) {
	base := With(CTE("t").As(Select("id").From("employees"))).
		Select("*").
		From("t")

	q1 := base.Where(Eq{"a": 1})
	q2 := base.Where(Eq{"b": 2})

	sql0, args0, err := base.ToSql()
	assert.NoError(t, err)
	assert.Equal(t, "WITH t AS (SELECT id FROM employees) SELECT * FROM t", sql0)
	assert.Empty(t, args0)

	sql1, args1, err := q1.ToSql()
	assert.NoError(t, err)
	assert.Equal(t, "WITH t AS (SELECT id FROM employees) SELECT * FROM t WHERE a = ?", sql1)
	assert.Equal(t, []interface{}{1}, args1)

	sql2, args2, err := q2.ToSql()
	assert.NoError(t, err)
	assert.Equal(t, "WITH t AS (SELECT id FROM employees) SELECT * FROM t WHERE b = ?", sql2)
	assert.Equal(t, []interface{}{2}, args2)
}

func TestMultipleCTEsBackwardReference(t *testing.T) {
	sql, args, err := With(
		CTE("a").As(Select("id").From("t1")),
		CTE("b").As(Select("id").From("a")),
	).Select("*").From("b").ToSql()

	assert.NoError(t, err)
	assert.Equal(t,
		"WITH a AS (SELECT id FROM t1), b AS (SELECT id FROM a) SELECT * FROM b",
		sql)
	assert.Empty(t, args)
}

func TestCTEPlaceholderFormat(t *testing.T) {
	sql, args, err := With(
		CTE("t").As(Select("id").From("employees").Where(Eq{"id": 7})),
	).
		Select("id").
		From("t").
		Where(Eq{"id": 8}).
		PlaceholderFormat(Dollar).
		ToSql()

	assert.NoError(t, err)
	assert.Equal(t,
		"WITH t AS (SELECT id FROM employees WHERE id = $1) SELECT id FROM t WHERE id = $2",
		sql)
	assert.Equal(t, []interface{}{7, 8}, args)
}

func TestCTEWithInsert(t *testing.T) {
	sql, args, err := With(
		CTE("src", "id").As(Select("id").From("employees").Where(Eq{"active": true})),
	).
		InsertInto("archive").
		Columns("id").
		Select(Select("id").From("src")).
		ToSql()

	assert.NoError(t, err)
	assert.Equal(t,
		"WITH src (id) AS (SELECT id FROM employees WHERE active = ?) "+
			"INSERT INTO archive (id) SELECT id FROM src",
		sql)
	assert.Equal(t, []interface{}{true}, args)
}

func TestCTEWithUpdate(t *testing.T) {
	sql, args, err := With(
		CTE("avg_sal").As(Select("AVG(salary) AS s").From("employees")),
	).
		Update("employees").
		Set("salary", Expr("salary * ?", 1.1)).
		Where("salary < (SELECT s FROM avg_sal)").
		ToSql()

	assert.NoError(t, err)
	assert.Equal(t,
		"WITH avg_sal AS (SELECT AVG(salary) AS s FROM employees) "+
			"UPDATE employees SET salary = salary * ? WHERE salary < (SELECT s FROM avg_sal)",
		sql)
	assert.Equal(t, []interface{}{1.1}, args)
}

func TestCTEWithDelete(t *testing.T) {
	sql, args, err := With(
		CTE("old_logs").As(Select("id").From("logs").Where(Lt{"created_at": "2020-01-01"})),
	).
		DeleteFrom("logs").
		Where("id IN (SELECT id FROM old_logs)").
		ToSql()

	assert.NoError(t, err)
	assert.Equal(t,
		"WITH old_logs AS (SELECT id FROM logs WHERE created_at < ?) "+
			"DELETE FROM logs WHERE id IN (SELECT id FROM old_logs)",
		sql)
	assert.Equal(t, []interface{}{"2020-01-01"}, args)
}

func TestCTEDuplicateName(t *testing.T) {
	_, _, err := With(
		CTE("dup").As(Select("id").From("a")),
		CTE("dup").As(Select("id").From("b")),
	).Select("*").From("dup").ToSql()

	assert.EqualError(t, err, `duplicate cte name "dup"`)
}

func TestCTEForwardReference(t *testing.T) {
	_, _, err := With(
		CTE("first").As(Select("id").From("second")),
		CTE("second").As(Select("id").From("t")),
	).Select("*").From("first").ToSql()

	assert.EqualError(t, err, `cte "first" references cte "second" before it is defined`)
}

func TestCTESelfReferenceNonRecursive(t *testing.T) {
	_, _, err := With(
		CTE("t").As(Select("id").From("t")),
	).Select("*").From("t").ToSql()

	assert.EqualError(t, err, `cte "t" references itself but has no recursive terms`)
}

func TestCTEColumnCountMismatch(t *testing.T) {
	_, _, err := With(
		CTE("t", "a", "b").As(Select("only_one").From("src")),
	).Select("*").From("t").ToSql()

	assert.EqualError(t, err, `cte "t" declares 2 columns but its anchor query produces 1`)
}

func TestCTERecursiveTermColumnCountMismatch(t *testing.T) {
	_, _, err := With(
		CTE("t", "a").
			Anchor(Select("one").From("src")).
			RecursiveTerm(Select("x", "y").From("t")),
	).Select("*").From("t").ToSql()

	assert.EqualError(t, err, `cte "t" declares 1 columns but recursive term 1 produces 2`)
}

func TestCTEUnknownColumnCountNotValidated(t *testing.T) {
	// Raw SQL fragments cannot be inspected; no column count validation happens.
	sql, _, err := With(
		CTE("t", "a", "b").As(Expr("SELECT * FROM src")),
	).Select("*").From("t").ToSql()

	assert.NoError(t, err)
	assert.Equal(t, "WITH t (a, b) AS (SELECT * FROM src) SELECT * FROM t", sql)

	// "*" columns cannot be counted either.
	sql, _, err = With(
		CTE("t", "a").As(Select("*").From("src")),
	).Select("*").From("t").ToSql()

	assert.NoError(t, err)
	assert.Equal(t, "WITH t (a) AS (SELECT * FROM src) SELECT * FROM t", sql)
}

func TestCTEMissingBody(t *testing.T) {
	_, _, err := With(CTE("t")).Select("*").From("t").ToSql()

	assert.EqualError(t, err, `cte "t" has no query`)
}
