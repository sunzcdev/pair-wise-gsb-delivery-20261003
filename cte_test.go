package squirrel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCTERecursiveEmployeeTree(t *testing.T) {
	empTree := CTE("emp_tree", "id", "name", "depth").Recursive().As(
		Select("id", "name", "0 AS depth").
			From("employees").
			Where("manager_id IS NULL").
			Suffix("UNION ALL " +
				"SELECT e.id, e.name, t.depth + 1 " +
				"FROM employees e JOIN emp_tree t ON e.manager_id = t.id"),
	)

	q := Select("id", "name", "depth").
		From("emp_tree").
		Where("depth < ?", 3).
		OrderBy("depth", "id").
		With(empTree)

	sql, args, err := q.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH RECURSIVE emp_tree (id, name, depth) AS ("+
			"SELECT id, name, 0 AS depth FROM employees WHERE manager_id IS NULL "+
			"UNION ALL SELECT e.id, e.name, t.depth + 1 "+
			"FROM employees e JOIN emp_tree t ON e.manager_id = t.id"+
			") "+
			"SELECT id, name, depth FROM emp_tree WHERE depth < ? ORDER BY depth, id",
		sql)
	assert.Equal(t, []interface{}{3}, args)
}

func TestCTEMultipleSharingParameterizedSubquery(t *testing.T) {
	// One parameterized subquery shared by two CTEs.
	base := Select("id", "dept").From("employees").Where("dept = ?", "eng")

	q := Select("s.id", "j.id").
		From("seniors s").
		Join("juniors j ON j.dept = s.dept").
		With(
			CTE("seniors").As(base.Where("level > ?", 5)),
			CTE("juniors").As(base.Where("level <= ?", 5)),
		)

	sql, args, err := q.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH seniors AS (SELECT id, dept FROM employees WHERE dept = ? AND level > ?), "+
			"juniors AS (SELECT id, dept FROM employees WHERE dept = ? AND level <= ?) "+
			"SELECT s.id, j.id FROM seniors s JOIN juniors j ON j.dept = s.dept",
		sql)
	assert.Equal(t, []interface{}{"eng", 5, "eng", 5}, args)

	// Placeholders are renumbered across CTE bodies and the main query.
	sql, args, err = q.Where("s.id > ?", 100).PlaceholderFormat(Dollar).ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH seniors AS (SELECT id, dept FROM employees WHERE dept = $1 AND level > $2), "+
			"juniors AS (SELECT id, dept FROM employees WHERE dept = $3 AND level <= $4) "+
			"SELECT s.id, j.id FROM seniors s JOIN juniors j ON j.dept = s.dept WHERE s.id > $5",
		sql)
	assert.Equal(t, []interface{}{"eng", 5, "eng", 5, 100}, args)
}

func TestCTEInnerScopeShadowsOuter(t *testing.T) {
	// The inner WITH clause reuses the name "x"; within the inner query it
	// shadows the outer CTE of the same name.
	q := Select("x.n").From("x").With(
		CTE("x").As(
			Select("n").From("x").With(
				CTE("x").As(Select("n").From("inner_src")),
			),
		),
	)

	sql, args, err := q.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH x AS (WITH x AS (SELECT n FROM inner_src) SELECT n FROM x) SELECT x.n FROM x",
		sql)
	assert.Empty(t, args)
}

func TestCTEConsumedByInsert(t *testing.T) {
	q := Insert("archive").
		Columns("id", "name").
		Select(Select("id", "name").From("departed")).
		With(CTE("departed").As(
			Select("id", "name").From("employees").Where("left_at IS NOT NULL"),
		))

	sql, args, err := q.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH departed AS (SELECT id, name FROM employees WHERE left_at IS NOT NULL) "+
			"INSERT INTO archive (id,name) SELECT id, name FROM departed",
		sql)
	assert.Empty(t, args)
}

func TestCTEConsumedByUpdate(t *testing.T) {
	q := Update("employees").
		Set("archived", true).
		Where("id IN (SELECT id FROM departed)").
		With(CTE("departed").As(
			Select("id").From("employees").Where("left_at < ?", "2026-01-01"),
		))

	sql, args, err := q.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH departed AS (SELECT id FROM employees WHERE left_at < ?) "+
			"UPDATE employees SET archived = ? WHERE id IN (SELECT id FROM departed)",
		sql)
	assert.Equal(t, []interface{}{"2026-01-01", true}, args)
}

func TestCTEConsumedByDelete(t *testing.T) {
	q := Delete("employees").
		Where("id IN (SELECT id FROM departed)").
		With(CTE("departed").As(
			Select("id").From("employees").Where("left_at < ?", "2026-01-01"),
		))

	sql, args, err := q.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH departed AS (SELECT id FROM employees WHERE left_at < ?) "+
			"DELETE FROM employees WHERE id IN (SELECT id FROM departed)",
		sql)
	assert.Equal(t, []interface{}{"2026-01-01"}, args)
}

func TestCTEBuilderCopyDoesNotMutateOriginal(t *testing.T) {
	base := Select("id").From("employees").With(
		CTE("managers").As(Select("id").From("employees").Where("is_manager")),
	)

	byDept := base.Where("dept = ?", "eng")
	byLevel := base.Where("level > ?", 5)

	sql, args, err := base.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH managers AS (SELECT id FROM employees WHERE is_manager) SELECT id FROM employees",
		sql)
	assert.Empty(t, args)

	sql, args, err = byDept.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH managers AS (SELECT id FROM employees WHERE is_manager) "+
			"SELECT id FROM employees WHERE dept = ?",
		sql)
	assert.Equal(t, []interface{}{"eng"}, args)

	sql, args, err = byLevel.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH managers AS (SELECT id FROM employees WHERE is_manager) "+
			"SELECT id FROM employees WHERE level > ?",
		sql)
	assert.Equal(t, []interface{}{5}, args)

	// The original is unchanged after derived builders appended conditions.
	sql, args, err = base.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH managers AS (SELECT id FROM employees WHERE is_manager) SELECT id FROM employees",
		sql)
	assert.Empty(t, args)
}

func TestCTEDuplicateName(t *testing.T) {
	q := Select("1").With(
		CTE("dup").As(Select("1")),
		CTE("dup").As(Select("2")),
	)
	_, _, err := q.ToSql()
	assert.EqualError(t, err, `cte "dup" is defined more than once in the same WITH clause`)
}

func TestCTEForwardReference(t *testing.T) {
	q := Select("a").From("first").With(
		CTE("first").As(Select("n").From("second")),
		CTE("second").As(Select("n").From("src")),
	)
	_, _, err := q.ToSql()
	assert.EqualError(t, err, `cte "first" references "second" before it is defined`)
}

func TestCTEBackwardReferenceIsAllowed(t *testing.T) {
	q := Select("n").From("second").With(
		CTE("first").As(Select("n").From("src")),
		CTE("second").As(Select("n").From("first")),
	)
	sql, _, err := q.ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"WITH first AS (SELECT n FROM src), second AS (SELECT n FROM first) SELECT n FROM second",
		sql)
}

func TestCTENonRecursiveSelfReference(t *testing.T) {
	q := Select("n").From("r").With(
		CTE("r").As(Select("n").From("r")),
	)
	_, _, err := q.ToSql()
	assert.EqualError(t, err, `cte "r" references itself but is not marked Recursive`)
}

func TestCTEColumnCountMismatch(t *testing.T) {
	q := Select("a").From("c").With(
		CTE("c", "a", "b").As(Select("1")),
	)
	_, _, err := q.ToSql()
	assert.EqualError(t, err, `cte "c" declares 2 columns but its query produces 1`)
}

func TestCTERawSQLColumnCountNotVerified(t *testing.T) {
	// The column count of a raw SQL fragment cannot be determined statically,
	// so it is not validated against the declared columns.
	q := Select("a").From("c").With(
		CTE("c", "a", "b").As("SELECT 1, 2, 3"),
	)
	sql, _, err := q.ToSql()
	assert.NoError(t, err)
	assert.Equal(t, "WITH c (a, b) AS (SELECT 1, 2, 3) SELECT a FROM c", sql)
}

func TestCTEMissingName(t *testing.T) {
	q := Select("1").With(CTE("").As(Select("1")))
	_, _, err := q.ToSql()
	assert.EqualError(t, err, "cte at position 0 is missing a name")
}

func TestCTEMissingBody(t *testing.T) {
	q := Select("1").With(CTE("bodiless"))
	_, _, err := q.ToSql()
	assert.EqualError(t, err, `cte "bodiless" has no query; call As to set its body`)
}
