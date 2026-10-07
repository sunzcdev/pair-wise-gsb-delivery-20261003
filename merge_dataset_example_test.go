package goqu_test

import (
	"fmt"

	"github.com/doug-martin/goqu/v9"
	_ "github.com/doug-martin/goqu/v9/dialect/mysql"
	_ "github.com/doug-martin/goqu/v9/dialect/postgres"
	_ "github.com/doug-martin/goqu/v9/dialect/sqlserver"
)

func ExampleMerge() {
	ds := goqu.Dialect("postgres").
		Merge("accounts").
		Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenMatched().UpdateSet(goqu.Record{"name": goqu.I("staging.name")}).
		WhenNotMatched().InsertRow(goqu.Record{"id": goqu.I("staging.id"), "name": goqu.I("staging.name")})

	sql, args, _ := ds.ToSQL()
	fmt.Println(sql)
	fmt.Println(args)

	// Output:
	// MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN MATCHED THEN UPDATE SET "name"="staging"."name" WHEN NOT MATCHED THEN INSERT ("id", "name") VALUES ("staging"."id", "staging"."name");
	// []
}

func ExampleMerge_conditionalBranches() {
	ds := goqu.Dialect("postgres").
		Merge(goqu.T("accounts").As("a")).
		Using(goqu.T("staging").As("s")).
		On(goqu.I("a.id").Eq(goqu.I("s.id"))).
		WhenMatchedAnd(goqu.I("s.deleted").Is(true)).Delete().
		WhenMatched().UpdateSet(goqu.Record{"name": goqu.I("s.name")}).
		WhenNotMatchedAnd(goqu.I("s.active").Is(true)).
		Insert([]interface{}{"id", "name"}, []interface{}{goqu.I("s.id"), goqu.I("s.name")}).
		Returning(goqu.I("a.id"))

	sql, _, _ := ds.ToSQL()
	fmt.Println(sql)

	// Output:
	// MERGE INTO "accounts" AS "a" USING "staging" AS "s" ON ("a"."id" = "s"."id") WHEN MATCHED AND ("s"."deleted" IS TRUE) THEN DELETE WHEN MATCHED THEN UPDATE SET "name"="s"."name" WHEN NOT MATCHED AND ("s"."active" IS TRUE) THEN INSERT ("id", "name") VALUES ("s"."id", "s"."name"); RETURNING "a"."id"
}

func ExampleMerge_parameterizedSubquery() {
	source := goqu.From("employees").
		Where(goqu.I("updated_at").Gt("2020-01-01")).
		As("e")

	ds := goqu.Dialect("postgres").
		Merge(goqu.T("accounts").As("a")).
		Using(source).
		On(goqu.I("a.id").Eq(goqu.I("e.account_id"))).
		WhenMatched().UpdateSet(goqu.Record{"name": goqu.I("e.name")}).
		WhenNotMatched().InsertRow(goqu.Record{"id": goqu.I("e.account_id"), "name": goqu.I("e.name")}).
		Prepared(true)

	sql, args, _ := ds.ToSQL()
	fmt.Println(sql)
	fmt.Println(args)

	// Output:
	// MERGE INTO "accounts" AS "a" USING (SELECT * FROM "employees" WHERE ("updated_at" > $1)) AS "e" ON ("a"."id" = "e"."account_id") WHEN MATCHED THEN UPDATE SET "name"="e"."name" WHEN NOT MATCHED THEN INSERT ("id", "name") VALUES ("e"."account_id", "e"."name");
	// [2020-01-01]
}

func ExampleMerge_unsupportedDialect() {
	_, _, err := goqu.Dialect("mysql").
		Merge("accounts").
		Using("staging").
		On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
		WhenNotMatched().Insert([]interface{}{"id"}, []interface{}{1}).
		ToSQL()
	fmt.Println(err)

	// Output:
	// goqu: dialect does not support MERGE statement [dialect=mysql]
}
