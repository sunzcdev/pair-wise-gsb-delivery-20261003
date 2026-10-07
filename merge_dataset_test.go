package goqu_test

import (
	"testing"

	"github.com/doug-martin/goqu/v9"
	_ "github.com/doug-martin/goqu/v9/dialect/mysql"
	_ "github.com/doug-martin/goqu/v9/dialect/postgres"
	_ "github.com/doug-martin/goqu/v9/dialect/sqlite3"
	_ "github.com/doug-martin/goqu/v9/dialect/sqlserver"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/errors"
	"github.com/doug-martin/goqu/v9/mocks"
	"github.com/stretchr/testify/suite"
)

type mergeDatasetSuite struct {
	suite.Suite
}

func TestMergeDatasetSuite(t *testing.T) {
	suite.Run(t, new(mergeDatasetSuite))
}

func (mds *mergeDatasetSuite) base() *goqu.MergeDataset {
	return goqu.Merge("accounts").
		Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id")))
}

func (mds *mergeDatasetSuite) TestClone() {
	ds := mds.base()
	mds.Equal(ds, ds.Clone())
}

func (mds *mergeDatasetSuite) TestExpression() {
	ds := mds.base()
	mds.Equal(ds, ds.Expression())
}

func (mds *mergeDatasetSuite) TestDialect() {
	ds := mds.base()
	mds.NotNil(ds.Dialect())

	md := new(mocks.SQLDialect)
	ds = ds.SetDialect(md)
	mds.Equal(md, ds.Dialect())

	dialectDs := ds.WithDialect("default")
	mds.Equal(goqu.GetDialect("default"), dialectDs.Dialect())
}

func (mds *mergeDatasetSuite) TestPrepared() {
	ds := mds.base().WhenMatched().Delete()
	prepared := ds.Prepared(true)
	mds.True(prepared.IsPrepared())
	mds.False(ds.IsPrepared())
	mds.True(prepared.WhenNotMatched().
		Insert([]interface{}{"id"}, []interface{}{1}).IsPrepared())
}

func (mds *mergeDatasetSuite) TestGetClauses() {
	ds := goqu.Merge("accounts")
	ce := exp.NewMergeClauses().SetTarget(exp.ParseIdentifier("accounts"))
	mds.Equal(ce, ds.GetClauses())
}

func (mds *mergeDatasetSuite) TestUnsupportedTargetType() {
	ds := goqu.Merge(123)
	mds.Error(ds.Error())

	src := goqu.Merge("accounts").Using(123)
	mds.Error(src.Error())
}

func (mds *mergeDatasetSuite) TestMatchedUpdatePostgres() {
	sql, args, err := goqu.Dialect("postgres").
		Merge("accounts").Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenMatched().UpdateSet(goqu.Record{"name": goqu.I("staging.name")}).
		ToSQL()
	mds.NoError(err)
	mds.Empty(args)
	mds.Equal(
		`MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN MATCHED THEN UPDATE SET "name"="staging"."name";`,
		sql,
	)
}

func (mds *mergeDatasetSuite) TestMatchedDeletePostgres() {
	sql, _, err := goqu.Dialect("postgres").
		Merge("accounts").Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenMatched().Delete().
		ToSQL()
	mds.NoError(err)
	mds.Equal(
		`MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN MATCHED THEN DELETE;`,
		sql,
	)
}

func (mds *mergeDatasetSuite) TestNotMatchedInsertPostgres() {
	sql, args, err := goqu.Dialect("postgres").
		Merge("accounts").Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenNotMatched().Insert([]interface{}{"id", "name"}, []interface{}{1, "Bob"}).
		Prepared(true).
		ToSQL()
	mds.NoError(err)
	mds.Equal([]interface{}{int64(1), "Bob"}, args)
	mds.Equal(
		`MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN NOT MATCHED THEN INSERT ("id", "name") VALUES ($1, $2);`,
		sql,
	)
}

func (mds *mergeDatasetSuite) TestNotMatchedInsertRow() {
	sql, _, err := goqu.Dialect("postgres").
		Merge("accounts").Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenNotMatchedAnd(goqu.I("staging.active").Is(true)).
		InsertRow(goqu.Record{"name": "Bob", "age": 30}).
		ToSQL()
	mds.NoError(err)
	mds.Equal(
		`MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN NOT MATCHED AND ("staging"."active" IS TRUE) THEN INSERT ("age", "name") VALUES (30, 'Bob');`,
		sql,
	)
}

func (mds *mergeDatasetSuite) TestAliasedTargetAndSource() {
	sql, _, err := goqu.Dialect("postgres").
		Merge(goqu.T("accounts").As("a")).
		Using(goqu.T("staging").As("s")).
		On(goqu.I("a.id").Eq(goqu.I("s.id"))).
		WhenMatched().Delete().
		ToSQL()
	mds.NoError(err)
	mds.Equal(
		`MERGE INTO "accounts" AS "a" USING "staging" AS "s" ON ("a"."id" = "s"."id") WHEN MATCHED THEN DELETE;`,
		sql,
	)
}

func (mds *mergeDatasetSuite) TestParameterizedSubquerySource() {
	src := goqu.From("employees").
		Where(goqu.I("updated_at").Gt("2020-01-01")).
		As("e")

	sql, args, err := goqu.Dialect("postgres").
		Merge(goqu.T("accounts").As("a")).
		Using(src).
		On(goqu.I("a.id").Eq(goqu.I("e.acc_id"))).
		WhenMatched().UpdateSet(goqu.Record{"name": goqu.I("e.name")}).
		WhenNotMatched().InsertRow(goqu.Record{"id": goqu.I("e.acc_id"), "name": "New"}).
		Prepared(true).
		ToSQL()
	mds.NoError(err)
	mds.Equal([]interface{}{"2020-01-01", "New"}, args)
	mds.Equal(
		`MERGE INTO "accounts" AS "a" USING (SELECT * FROM "employees" WHERE ("updated_at" > $1)) AS "e" ON ("a"."id" = "e"."acc_id") WHEN MATCHED THEN UPDATE SET "name"="e"."name" WHEN NOT MATCHED THEN INSERT ("id", "name") VALUES ("e"."acc_id", $2);`,
		sql,
	)
}

func (mds *mergeDatasetSuite) TestFullBranchOrdering() {
	sql, _, err := goqu.Dialect("postgres").
		Merge("accounts").Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenMatchedAnd(goqu.I("staging.deleted").Is(true)).Delete().
		WhenMatched().UpdateSet(goqu.Record{"name": goqu.I("staging.name")}).
		WhenNotMatched().InsertRow(goqu.Record{"id": 9, "name": "Zed"}).
		ToSQL()
	mds.NoError(err)
	mds.Equal(
		`MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN MATCHED AND ("staging"."deleted" IS TRUE) THEN DELETE WHEN MATCHED THEN UPDATE SET "name"="staging"."name" WHEN NOT MATCHED THEN INSERT ("id", "name") VALUES (9, 'Zed');`,
		sql,
	)
}

func (mds *mergeDatasetSuite) TestReturning() {
	sql, _, err := goqu.Dialect("postgres").
		Merge("accounts").Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenMatched().Delete().
		Returning(goqu.I("accounts.id")).
		ToSQL()
	mds.NoError(err)
	mds.Contains(sql, `; RETURNING "accounts"."id"`)
}

func (mds *mergeDatasetSuite) TestUnreachableBranchReportedAsError() {
	ds := goqu.Dialect("postgres").
		Merge("accounts").Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenMatched().Delete().
		WhenMatched().UpdateSet(goqu.Record{"name": "x"})

	mds.Error(ds.Error())
	mds.Contains(ds.Error().Error(), "unreachable MERGE branch")

	_, _, err := ds.ToSQL()
	mds.Error(err)
	mds.Contains(err.Error(), "unreachable MERGE branch")
}

func (mds *mergeDatasetSuite) TestInsertColumnValueMismatch() {
	ds := goqu.Dialect("postgres").
		Merge("accounts").Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenNotMatched().Insert([]interface{}{"id", "name"}, []interface{}{1})

	_, _, err := ds.ToSQL()
	mds.EqualError(err, "goqu: MERGE INSERT has 2 columns but 1 values")
}

func (mds *mergeDatasetSuite) TestUnsupportedDialectMysql() {
	_, _, err := goqu.Dialect("mysql").
		Merge("accounts").Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenMatched().Delete().
		ToSQL()
	mds.EqualError(err, "goqu: dialect does not support MERGE statement [dialect=mysql]")
}

func (mds *mergeDatasetSuite) TestUnsupportedDialectSqlite3() {
	_, _, err := goqu.Dialect("sqlite3").
		Merge("accounts").Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenNotMatched().Insert([]interface{}{"id"}, []interface{}{1}).
		ToSQL()
	mds.EqualError(err, "goqu: dialect does not support MERGE statement [dialect=sqlite3]")
}

func (mds *mergeDatasetSuite) TestSqlServerSupported() {
	sql, _, err := goqu.Dialect("sqlserver").
		Merge("accounts").Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenMatched().UpdateSet(goqu.Record{"name": goqu.L("staging.name")}).
		WhenNotMatched().Insert([]interface{}{"id"}, []interface{}{1}).
		ToSQL()
	mds.NoError(err)
	mds.Equal(
		`MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN MATCHED THEN UPDATE SET "name"=staging.name WHEN NOT MATCHED THEN INSERT ("id") VALUES (1);`,
		sql,
	)
}

func (mds *mergeDatasetSuite) TestMissingOnClause() {
	_, _, err := goqu.Dialect("postgres").
		Merge("accounts").Using("staging").
		WhenMatched().Delete().
		ToSQL()
	mds.EqualError(err, "goqu: no ON condition found when generating merge sql")
}

func (mds *mergeDatasetSuite) TestSetError() {
	ds := mds.base()
	mds.Nil(ds.Error())
	returned := ds.SetError(errors.New("boom"))
	mds.EqualError(returned.Error(), "goqu: boom")
	// second SetError does not overwrite the first.
	returned2 := returned.SetError(errors.New("second"))
	mds.EqualError(returned2.Error(), "goqu: boom")
}
