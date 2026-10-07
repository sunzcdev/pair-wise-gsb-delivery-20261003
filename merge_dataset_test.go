package goqu_test

import (
	"testing"

	"github.com/doug-martin/goqu/v9"
	_ "github.com/doug-martin/goqu/v9/dialect/mysql"
	_ "github.com/doug-martin/goqu/v9/dialect/postgres"
	_ "github.com/doug-martin/goqu/v9/dialect/sqlite3"
	_ "github.com/doug-martin/goqu/v9/dialect/sqlserver"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/stretchr/testify/suite"
)

type mergeDatasetSuite struct {
	suite.Suite
}

func (mds *mergeDatasetSuite) assertSQL(ds *goqu.MergeDataset, expectedSQL string, expectedArgs []interface{}) {
	actualSQL, actualArgs, err := ds.ToSQL()
	mds.NoError(err)
	mds.Equal(expectedSQL, actualSQL)
	if len(expectedArgs) == 0 {
		mds.Empty(actualArgs)
	} else {
		mds.Equal(expectedArgs, actualArgs)
	}
}

func (mds *mergeDatasetSuite) assertPreparedSQL(ds *goqu.MergeDataset, expectedSQL string, expectedArgs []interface{}) {
	actualSQL, actualArgs, err := ds.Prepared(true).ToSQL()
	mds.NoError(err)
	mds.Equal(expectedSQL, actualSQL)
	if len(expectedArgs) == 0 {
		mds.Empty(actualArgs)
	} else {
		mds.Equal(expectedArgs, actualArgs)
	}
}

func (mds *mergeDatasetSuite) assertError(ds *goqu.MergeDataset, expectedErr string) {
	actualSQL, actualArgs, err := ds.ToSQL()
	mds.EqualError(err, expectedErr)
	mds.Empty(actualSQL)
	mds.Empty(actualArgs)
}

func (mds *mergeDatasetSuite) TestMerge() {
	ds := goqu.Merge("test")
	mds.IsType(&goqu.MergeDataset{}, ds)
	mds.Implements((*exp.Expression)(nil), ds)
	mds.Equal(ds, ds.Clone())
	mds.Equal(ds, ds.Expression())
	mds.NotNil(ds.Dialect())
	mds.False(ds.IsPrepared())
	mds.False(ds.ReturnsColumns())
}

func (mds *mergeDatasetSuite) TestWithDialect() {
	ds := goqu.Dialect("postgres").Merge("test")
	mds.Equal("postgres", ds.Dialect().Dialect())
	mds.True(ds.Prepared(true).IsPrepared())
}

func (mds *mergeDatasetSuite) TestFullStatement() {
	src := goqu.From("transactions").
		Select(goqu.I("user_id"), goqu.SUM(goqu.I("amount")).As("amount")).
		Where(goqu.I("date").Gt("2020-01-01")).
		GroupBy(goqu.I("user_id"))

	ds := goqu.Dialect("postgres").
		Merge("accounts").
		TargetAs("a").
		UsingAs(src, "t").
		On(goqu.I("a.user_id").Eq(goqu.I("t.user_id"))).
		WhenMatched(goqu.I("t.amount").Gt(100)).
		ThenUpdate(goqu.Record{"balance": goqu.L("? + ?", goqu.I("a.balance"), goqu.I("t.amount"))}).
		WhenMatched().
		ThenDelete().
		WhenNotMatched(goqu.I("t.kind").Eq("new")).
		ThenInsert(goqu.Record{"user_id": goqu.I("t.user_id"), "balance": goqu.I("t.amount")}).
		WhenNotMatched().
		ThenDoNothing().
		Returning(goqu.I("a.user_id"))

	mds.assertPreparedSQL(
		ds,
		`MERGE INTO "accounts" AS "a" USING (SELECT "user_id", SUM("amount") AS "amount" FROM "transactions" `+
			`WHERE ("date" > $1) GROUP BY "user_id") AS "t" ON ("a"."user_id" = "t"."user_id") `+
			`WHEN MATCHED AND ("t"."amount" > $2) THEN UPDATE SET "balance"="a"."balance" + "t"."amount" `+
			`WHEN MATCHED THEN DELETE WHEN NOT MATCHED AND ("t"."kind" = $3) THEN INSERT `+
			`("balance", "user_id") VALUES ("t"."amount", "t"."user_id") WHEN NOT MATCHED THEN DO NOTHING `+
			`RETURNING "a"."user_id"`,
		[]interface{}{"2020-01-01", int64(100), "new"},
	)
}

func (mds *mergeDatasetSuite) TestSimplePostgres() {
	ds := goqu.Dialect("postgres").
		Merge("accounts").
		Using("transactions").
		On(goqu.I("accounts.id").Eq(goqu.I("transactions.id"))).
		WhenMatched().
		ThenUpdate(goqu.Record{"name": goqu.I("transactions.name")}).
		WhenNotMatched().
		ThenInsert(goqu.Record{"id": goqu.I("transactions.id"), "name": goqu.I("transactions.name")})

	mds.assertPreparedSQL(
		ds,
		`MERGE INTO "accounts" USING "transactions" ON ("accounts"."id" = "transactions"."id") `+
			`WHEN MATCHED THEN UPDATE SET "name"="transactions"."name" WHEN NOT MATCHED THEN INSERT `+
			`("id", "name") VALUES ("transactions"."id", "transactions"."name")`,
		nil,
	)
}

func (mds *mergeDatasetSuite) TestMultipleOnConditionsAreJoinedWithAnd() {
	ds := goqu.Merge("a").Using("b").
		On(goqu.I("a.x").Eq(goqu.I("b.x")), goqu.I("a.y").Eq(goqu.I("b.y"))).
		WhenMatched().ThenDelete()

	mds.assertSQL(
		ds,
		`MERGE INTO "a" USING "b" ON (("a"."x" = "b"."x") AND ("a"."y" = "b"."y")) WHEN MATCHED THEN DELETE`,
		nil,
	)
}

func (mds *mergeDatasetSuite) TestUnreachableBranches() {
	// conditional then unconditional then conditional (same type) is unreachable
	ds := goqu.Merge("a").Using("b").On(goqu.I("a.x").Eq(goqu.I("b.x"))).
		WhenMatched(goqu.I("b.v").Gt(1)).ThenUpdate(goqu.Record{"v": goqu.I("b.v")}).
		WhenMatched().ThenDelete().
		WhenMatched(goqu.I("b.v").Lt(0)).ThenDelete()

	mds.assertError(
		ds,
		"goqu: unreachable WHEN MATCHED branch: a branch of the same type with no condition was already declared",
	)
}

func (mds *mergeDatasetSuite) TestIllegalActions() {
	// INSERT on a WHEN MATCHED branch
	ds := goqu.Merge("a").Using("b").On(goqu.I("a.x").Eq(goqu.I("b.x"))).
		WhenMatched().ThenInsert(goqu.Record{"x": 1})
	mds.assertError(ds, "goqu: INSERT action is only allowed on WHEN NOT MATCHED branches")

	// UPDATE on a WHEN NOT MATCHED branch
	ds = goqu.Merge("a").Using("b").On(goqu.I("a.x").Eq(goqu.I("b.x"))).
		WhenNotMatched().ThenUpdate(goqu.Record{"x": 1})
	mds.assertError(ds, "goqu: UPDATE action is only allowed on WHEN MATCHED branches")

	// DELETE on a WHEN NOT MATCHED branch
	ds = goqu.Merge("a").Using("b").On(goqu.I("a.x").Eq(goqu.I("b.x"))).
		WhenNotMatched().ThenDelete()
	mds.assertError(ds, "goqu: DELETE action is only allowed on WHEN MATCHED branches")
}

func (mds *mergeDatasetSuite) TestUnsupportedDialects() {
	ds := goqu.Dialect("mysql").Merge("a").Using("b").
		On(goqu.I("a.x").Eq(goqu.I("b.x"))).WhenMatched().ThenDelete()
	mds.assertError(ds, "goqu: dialect does not support MERGE statements [dialect=mysql]")

	ds = goqu.Dialect("sqlite3").Merge("a").Using("b").
		On(goqu.I("a.x").Eq(goqu.I("b.x"))).WhenNotMatched().
		ThenInsert(goqu.Record{"x": 1})
	mds.assertError(ds, "goqu: dialect does not support MERGE statements [dialect=sqlite3]")
}

func (mds *mergeDatasetSuite) TestSqlServerStatementEnd() {
	ds := goqu.Dialect("sqlserver").Merge("a").Using("b").
		On(goqu.I("a.x").Eq(goqu.I("b.x"))).WhenMatched().
		ThenUpdate(goqu.Record{"x": goqu.I("b.x")})

	mds.assertPreparedSQL(
		ds,
		`MERGE INTO "a" USING "b" ON ("a"."x" = "b"."x") WHEN MATCHED THEN UPDATE SET "x"="b"."x";`,
		nil,
	)
}

func (mds *mergeDatasetSuite) TestSqlServerDeleteSupported() {
	ds := goqu.Dialect("sqlserver").Merge("a").Using("b").
		On(goqu.I("a.x").Eq(goqu.I("b.x"))).WhenMatched().ThenDelete()

	mds.assertPreparedSQL(
		ds,
		`MERGE INTO "a" USING "b" ON ("a"."x" = "b"."x") WHEN MATCHED THEN DELETE;`,
		nil,
	)
}

func (mds *mergeDatasetSuite) TestValidationErrors() {
	mds.assertError(goqu.Merge("a"), "goqu: no source found when generating merge sql")

	mds.assertError(
		goqu.Merge("a").Using("b"),
		"goqu: no ON condition found when generating merge sql",
	)

	mds.assertError(
		goqu.Merge("a").Using("b").On(goqu.I("x").Eq(goqu.I("y"))),
		"goqu: at least one WHEN branch is required when generating merge sql",
	)
}

func (mds *mergeDatasetSuite) TestManualWhenAppended() {
	branch, err := exp.NewMergeMatchedBranch(nil).SetDelete()
	mds.NoError(err)
	ds := goqu.Merge("a").Using("b").On(goqu.I("a.x").Eq(goqu.I("b.x"))).When(branch)
	mds.assertSQL(ds, `MERGE INTO "a" USING "b" ON ("a"."x" = "b"."x") WHEN MATCHED THEN DELETE`, nil)
}

func (mds *mergeDatasetSuite) TestInterpolationAndArgs() {
	ds := goqu.Merge("a").Using("b").On(goqu.I("a.x").Eq(goqu.I("b.x"))).
		WhenNotMatched().ThenInsert(goqu.Record{"name": "Bob", "n": 42})

	mds.assertSQL(
		ds,
		`MERGE INTO "a" USING "b" ON ("a"."x" = "b"."x") WHEN NOT MATCHED THEN INSERT ("n", "name") VALUES (42, 'Bob')`,
		nil,
	)
}

func (mds *mergeDatasetSuite) TestSubqueryInsertRejected() {
	sub := goqu.From("other").Select(goqu.I("id"))
	ds := goqu.Merge("a").Using("b").On(goqu.I("a.x").Eq(goqu.I("b.x"))).
		WhenNotMatched().ThenInsert(sub)

	mds.assertError(ds, "goqu: a subquery cannot be used as the INSERT row in a MERGE statement")
}

func (mds *mergeDatasetSuite) TestEmptyInsertRowRejected() {
	ds := goqu.Merge("a").Using("b").On(goqu.I("a.x").Eq(goqu.I("b.x"))).
		WhenNotMatched().ThenInsert(goqu.Record{})

	mds.assertError(ds, "goqu: a MERGE INSERT action requires an explicit column list")
}

func (mds *mergeDatasetSuite) TestInvalidUpdateRejected() {
	ds := goqu.Merge("a").Using("b").On(goqu.I("a.x").Eq(goqu.I("b.x"))).
		WhenMatched().ThenUpdate([]string{"not-valid"})

	_, _, err := ds.ToSQL()
	mds.Error(err)
}

func (mds *mergeDatasetSuite) TestStructInsert() {
	type item struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
	}
	ds := goqu.Merge("a").Using("b").On(goqu.I("a.id").Eq(goqu.I("b.id"))).
		WhenNotMatched().ThenInsert(item{ID: 1, Name: "Bob"})

	mds.assertSQL(
		ds,
		`MERGE INTO "a" USING "b" ON ("a"."id" = "b"."id") WHEN NOT MATCHED THEN INSERT ("id", "name") VALUES (1, 'Bob')`,
		nil,
	)
}

func TestMergeDatasetSuite(t *testing.T) {
	suite.Run(t, new(mergeDatasetSuite))
}
