package integration

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

  sqrl "github.com/Masterminds/squirrel"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

const (
	testSchema = `
		CREATE TABLE squirrel_integration ( k INT, v TEXT )`
	testData = `
		INSERT INTO squirrel_integration VALUES
			(1, 'foo'),
			(3, 'bar'),
			(2, 'foo'),
			(4, 'baz')
		`
)

var (
	sb         sqrl.StatementBuilderType
	dbConn     *sql.DB
	driverName string
)

func TestMain(m *testing.M) {
	var driver, dataSource string
	flag.StringVar(&driver, "driver", "", "integration database driver")
	flag.StringVar(&dataSource, "dataSource", "", "integration database data source")
	flag.Parse()

  if driver == "" {
    driver = "sqlite3"
  }

	if driver == "sqlite3" && dataSource == "" {
		dataSource = ":memory:"
	}

	db, err := sql.Open(driver, dataSource)
	if err != nil {
		fmt.Printf("error opening database: %v\n", err)
		os.Exit(-1)
	}
	dbConn = db
	driverName = driver

	_, err = db.Exec(testSchema)
	if err != nil {
		fmt.Printf("error creating test schema: %v\n", err)
		os.Exit(-2)
	}

	defer func() {
		_, err = db.Exec("DROP TABLE squirrel_integration")
		fmt.Printf("error removing test schema: %v\n", err)
	}()

	_, err = db.Exec(testData)
	if err != nil {
		fmt.Printf("error inserting test data: %v\n", err)
		os.Exit(-3)
	}

	sb = sqrl.StatementBuilder.RunWith(db)

	if driver == "postgres" {
		sb = sb.PlaceholderFormat(sqrl.Dollar)
	}

	os.Exit(m.Run())
}

func assertVals(t *testing.T, s sqrl.SelectBuilder, expected ...string) {
	rows, err := s.Query()
	assert.NoError(t, err)
	defer rows.Close()

	vals := make([]string, len(expected))
	for i := range vals {
		assert.True(t, rows.Next())
		assert.NoError(t, rows.Scan(&vals[i]))
	}
	assert.False(t, rows.Next())

	if expected != nil {
		assert.Equal(t, expected, vals)
	}
}

func TestSimpleSelect(t *testing.T) {
	assertVals(
		t,
		sb.Select("v").From("squirrel_integration"),
		"foo", "bar", "foo", "baz")
}

func TestEq(t *testing.T) {
	s := sb.Select("v").From("squirrel_integration")
	assertVals(t, s.Where(sqrl.Eq{"k": 4}), "baz")
	assertVals(t, s.Where(sqrl.NotEq{"k": 2}), "foo", "bar", "baz")
	assertVals(t, s.Where(sqrl.Eq{"k": []int{1, 4}}), "foo", "baz")
	assertVals(t, s.Where(sqrl.NotEq{"k": []int{1, 4}}), "bar", "foo")
	assertVals(t, s.Where(sqrl.Eq{"k": nil}))
	assertVals(t, s.Where(sqrl.NotEq{"k": nil}), "foo", "bar", "foo", "baz")
	assertVals(t, s.Where(sqrl.Eq{"k": []int{}}))
	assertVals(t, s.Where(sqrl.NotEq{"k": []int{}}), "foo", "bar", "foo", "baz")
}

func TestIneq(t *testing.T) {
	s := sb.Select("v").From("squirrel_integration")
	assertVals(t, s.Where(sqrl.Lt{"k": 3}), "foo", "foo")
	assertVals(t, s.Where(sqrl.Gt{"k": 3}), "baz")
}

func TestConj(t *testing.T) {
	s := sb.Select("v").From("squirrel_integration")
	assertVals(t, s.Where(sqrl.And{sqrl.Gt{"k": 1}, sqrl.Lt{"k": 4}}), "bar", "foo")
	assertVals(t, s.Where(sqrl.Or{sqrl.Gt{"k": 3}, sqrl.Lt{"k": 2}}), "foo", "baz")
}

func TestContext(t *testing.T) {
	s := sb.Select("v").From("squirrel_integration")
	ctx := context.Background()
	_, err := s.QueryContext(ctx)
	assert.NoError(t, err)
}

// runStrings executes a Sqlizer with the package runner and collects the
// first column of every row as a string.
func runStrings(t *testing.T, s sqrl.Sqlizer) []string {
	query, args, err := s.ToSql()
	assert.NoError(t, err)

	rows, err := dbConn.Query(query, args...)
	assert.NoError(t, err)
	defer rows.Close()

	var vals []string
	for rows.Next() {
		var v string
		assert.NoError(t, rows.Scan(&v))
		vals = append(vals, v)
	}
	assert.NoError(t, rows.Err())
	return vals
}

// TestSetOperations verifies UNION ALL/UNION and, for PostgreSQL, that moving
// the grouping parentheses in an EXCEPT expression changes the result when the
// input contains duplicate rows.
func TestSetOperations(t *testing.T) {
	if dbConn == nil {
		t.Skip("set operation tests require a database connection")
	}

	if driverName != "postgres" {
		t.Skip("set operation EXCEPT semantics are pinned for the postgres dialect")
	}

	// Constant branches with deliberately duplicated rows:
	//	formal  -> 1, 1, 2
	//	reentry -> 1
	//	archived -> 1
	values := func(alias string, ids ...int) sqrl.Sqlizer {
		q := sb.Select("column1 AS id").
			From("(VALUES "+placeholdersGrouped(ids)+") AS "+alias, toArgs(ids)...)
		return q
	}

	formalQ := values("formal(id)", 1, 1, 2)
	reentryQ := values("reentry(id)", 1)
	archivedQ := values("archived(id)", 1)

	// (formal UNION ALL reentry) EXCEPT archived -> 2 only
	left := sqrl.Except(sqrl.UnionAll(formalQ, reentryQ), archivedQ)
	assert.Equal(t, []string{"2"}, runStrings(t, left))

	// formal UNION ALL (reentry EXCEPT archived) -> 1, 1, 2
	right := sqrl.UnionAll(formalQ, sqrl.Except(reentryQ, archivedQ))
	assert.Equal(t, []string{"1", "1", "2"}, runStrings(t, right))
}

func placeholdersGrouped(ids []int) string {
	groups := make([]string, len(ids))
	for i := range ids {
		groups[i] = "(?)"
	}
	return strings.Join(groups, ", ")
}

func toArgs(ids []int) []interface{} {
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}
