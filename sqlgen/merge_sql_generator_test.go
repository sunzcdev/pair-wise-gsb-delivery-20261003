package sqlgen_test

import (
	"testing"

	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/sb"
	"github.com/doug-martin/goqu/v9/sqlgen"
	"github.com/stretchr/testify/suite"
)

type (
	mergeTestCase struct {
		clause     exp.MergeClauses
		sql        string
		isPrepared bool
		args       []interface{}
		err        string
	}
	mergeSQLGeneratorSuite struct {
		baseSQLGeneratorSuite
	}
)

func (mgs *mergeSQLGeneratorSuite) assertCases(gen sqlgen.MergeSQLGenerator, testCases ...mergeTestCase) {
	for _, tc := range testCases {
		b := sb.NewSQLBuilder(tc.isPrepared)
		gen.Generate(b, tc.clause)
		switch {
		case len(tc.err) > 0:
			mgs.assertErrorSQL(b, tc.err)
		case tc.isPrepared:
			mgs.assertPreparedSQL(b, tc.sql, tc.args)
		default:
			mgs.assertNotPreparedSQL(b, tc.sql)
		}
	}
}

func (mgs *mergeSQLGeneratorSuite) newClauses() exp.MergeClauses {
	return exp.NewMergeClauses().
		SetTarget(exp.NewIdentifierExpression("", "accounts", "")).
		SetTargetAlias(exp.NewIdentifierExpression("", "a", "")).
		SetSource(exp.NewIdentifierExpression("", "transactions", "")).
		SetOn(exp.NewExpressionList(exp.AndType, exp.ParseIdentifier("a.id").Eq(exp.ParseIdentifier("t.id"))))
}

func (mgs *mergeSQLGeneratorSuite) TestDialect() {
	opts := sqlgen.DefaultDialectOptions()
	d := sqlgen.NewMergeSQLGenerator("test", opts)
	mgs.Equal("test", d.Dialect())
}

func (mgs *mergeSQLGeneratorSuite) TestGenerate_unsupportedFragment() {
	opts := sqlgen.DefaultDialectOptions()
	opts.MergeSQLOrder = []sqlgen.SQLFragmentType{sqlgen.UpdateBeginSQLFragment}
	d := sqlgen.NewMergeSQLGenerator("test", opts)

	b := sb.NewSQLBuilder(true)
	clauses := mgs.newClauses()
	branch, err := exp.NewMergeMatchedBranch(nil).SetDelete()
	mgs.NoError(err)
	clauses, err = clauses.AppendBranch(branch)
	mgs.NoError(err)
	d.Generate(b, clauses)
	mgs.assertErrorSQL(b, `goqu: unsupported MERGE SQL fragment UpdateBeginSQLFragment`)
}

func (mgs *mergeSQLGeneratorSuite) TestGenerate_validation() {
	d := sqlgen.NewMergeSQLGenerator("test", sqlgen.DefaultDialectOptions())

	mgs.assertCases(d,
		mergeTestCase{
			clause: exp.NewMergeClauses(),
			err:    "goqu: no target found when generating merge sql",
		},
		mergeTestCase{
			clause: exp.NewMergeClauses().
				SetTarget(exp.NewIdentifierExpression("", "accounts", "")),
			err: "goqu: no source found when generating merge sql",
		},
		mergeTestCase{
			clause: exp.NewMergeClauses().
				SetTarget(exp.NewIdentifierExpression("", "accounts", "")).
				SetSource(exp.NewIdentifierExpression("", "transactions", "")),
			err: "goqu: no ON condition found when generating merge sql",
		},
		mergeTestCase{
			clause: mgs.newClauses(),
			err:    "goqu: at least one WHEN branch is required when generating merge sql",
		},
	)
}

func (mgs *mergeSQLGeneratorSuite) TestGenerate_mergeNotSupported() {
	opts := sqlgen.DefaultDialectOptions()
	opts.SupportsMerge = false
	d := sqlgen.NewMergeSQLGenerator("mysql", opts)

	b := sb.NewSQLBuilder(true)
	d.Generate(b, mgs.newClauses())
	mgs.assertErrorSQL(b, "goqu: dialect does not support MERGE statements [dialect=mysql]")
}

func (mgs *mergeSQLGeneratorSuite) TestGenerate_matchedUpdate() {
	clauses := mgs.newClauses().
		SetReturning(exp.NewColumnListExpression(exp.ParseIdentifier("a.id")))
	branch, err := exp.NewMergeMatchedBranch(exp.ParseIdentifier("t.amount").Gt(int64(100))).
		SetUpdate(exp.Record{"name": "Bob", "amount": int64(10)})
	mgs.NoError(err)
	clauses, err = clauses.AppendBranch(branch)
	mgs.NoError(err)

	mgs.assertCases(
		sqlgen.NewMergeSQLGenerator("test", sqlgen.DefaultDialectOptions()),
		mergeTestCase{
			clause: clauses,
			sql: `MERGE INTO "accounts" AS "a" USING "transactions" ON ("a"."id" = "t"."id") ` +
				`WHEN MATCHED AND ("t"."amount" > 100) THEN UPDATE SET "amount"=10,"name"='Bob' RETURNING "a"."id"`,
		},
		mergeTestCase{
			clause: clauses,
			sql: `MERGE INTO "accounts" AS "a" USING "transactions" ON ("a"."id" = "t"."id") ` +
				`WHEN MATCHED AND ("t"."amount" > ?) THEN UPDATE SET "amount"=?,"name"=? RETURNING "a"."id"`,
			isPrepared: true,
			args:       []interface{}{int64(100), int64(10), "Bob"},
		},
	)
}

func (mgs *mergeSQLGeneratorSuite) TestGenerate_matchedUpdateUnconditional() {
	clauses := mgs.newClauses()
	branch, err := exp.NewMergeMatchedBranch(nil).
		SetUpdate(exp.Record{"name": "Bob"})
	mgs.NoError(err)
	clauses, err = clauses.AppendBranch(branch)
	mgs.NoError(err)

	mgs.assertCases(
		sqlgen.NewMergeSQLGenerator("test", sqlgen.DefaultDialectOptions()),
		mergeTestCase{
			clause: clauses,
			sql: `MERGE INTO "accounts" AS "a" USING "transactions" ON ("a"."id" = "t"."id") ` +
				`WHEN MATCHED THEN UPDATE SET "name"='Bob'`,
		},
	)
}

func (mgs *mergeSQLGeneratorSuite) TestGenerate_matchedDelete() {
	clauses := mgs.newClauses()
	branch, err := exp.NewMergeMatchedBranch(nil).SetDelete()
	mgs.NoError(err)
	clauses, err = clauses.AppendBranch(branch)
	mgs.NoError(err)

	mgs.assertCases(
		sqlgen.NewMergeSQLGenerator("test", sqlgen.DefaultDialectOptions()),
		mergeTestCase{
			clause: clauses,
			sql: `MERGE INTO "accounts" AS "a" USING "transactions" ON ("a"."id" = "t"."id") ` +
				`WHEN MATCHED THEN DELETE`,
		},
	)
}

func (mgs *mergeSQLGeneratorSuite) TestGenerate_matchedDeleteNotSupported() {
	opts := sqlgen.DefaultDialectOptions()
	opts.SupportsMergeMatchedDelete = false

	clauses := mgs.newClauses()
	branch, err := exp.NewMergeMatchedBranch(nil).SetDelete()
	mgs.NoError(err)
	clauses, err = clauses.AppendBranch(branch)
	mgs.NoError(err)

	b := sb.NewSQLBuilder(true)
	sqlgen.NewMergeSQLGenerator("test", opts).Generate(b, clauses)
	mgs.assertErrorSQL(b, "goqu: dialect does not support the DELETE action in a MERGE statement [dialect=test]")
}

func (mgs *mergeSQLGeneratorSuite) TestGenerate_notMatchedInsert() {
	clauses := mgs.newClauses()
	branch, err := exp.NewMergeNotMatchedBranch(nil).
		SetInsert(exp.Record{"name": "Bob", "amount": int64(10)})
	mgs.NoError(err)
	clauses, err = clauses.AppendBranch(branch)
	mgs.NoError(err)

	mgs.assertCases(
		sqlgen.NewMergeSQLGenerator("test", sqlgen.DefaultDialectOptions()),
		mergeTestCase{
			clause: clauses,
			sql: `MERGE INTO "accounts" AS "a" USING "transactions" ON ("a"."id" = "t"."id") ` +
				`WHEN NOT MATCHED THEN INSERT ("amount", "name") VALUES (10, 'Bob')`,
		},
		mergeTestCase{
			clause: clauses,
			sql: `MERGE INTO "accounts" AS "a" USING "transactions" ON ("a"."id" = "t"."id") ` +
				`WHEN NOT MATCHED THEN INSERT ("amount", "name") VALUES (?, ?)`,
			isPrepared: true,
			args:       []interface{}{int64(10), "Bob"},
		},
	)
}

func (mgs *mergeSQLGeneratorSuite) TestGenerate_doNothing() {
	clauses := mgs.newClauses()
	branch := exp.NewMergeNotMatchedBranch(nil)
	var err error
	clauses, err = clauses.AppendBranch(branch)
	mgs.NoError(err)

	mgs.assertCases(
		sqlgen.NewMergeSQLGenerator("test", sqlgen.DefaultDialectOptions()),
		mergeTestCase{
			clause: clauses,
			sql: `MERGE INTO "accounts" AS "a" USING "transactions" ON ("a"."id" = "t"."id") ` +
				`WHEN NOT MATCHED THEN DO NOTHING`,
		},
	)
}

func (mgs *mergeSQLGeneratorSuite) TestGenerate_multipleBranchesInOrder() {
	clauses := mgs.newClauses()

	b1, err := exp.NewMergeMatchedBranch(exp.ParseIdentifier("t.active").Is(true)).
		SetUpdate(exp.Record{"name": "Active"})
	mgs.NoError(err)
	clauses, err = clauses.AppendBranch(b1)
	mgs.NoError(err)

	b2, err := exp.NewMergeMatchedBranch(nil).SetDelete()
	mgs.NoError(err)
	clauses, err = clauses.AppendBranch(b2)
	mgs.NoError(err)

	b3, err := exp.NewMergeNotMatchedBranch(nil).
		SetInsert(exp.Record{"name": "New"})
	mgs.NoError(err)
	clauses, err = clauses.AppendBranch(b3)
	mgs.NoError(err)

	mgs.assertCases(
		sqlgen.NewMergeSQLGenerator("test", sqlgen.DefaultDialectOptions()),
		mergeTestCase{
			clause: clauses,
			sql: `MERGE INTO "accounts" AS "a" USING "transactions" ON ("a"."id" = "t"."id") ` +
				`WHEN MATCHED AND ("t"."active" IS TRUE) THEN UPDATE SET "name"='Active' ` +
				`WHEN MATCHED THEN DELETE WHEN NOT MATCHED THEN INSERT ("name") VALUES ('New')`,
		},
	)
}

func TestMergeSQLGeneratorSuite(t *testing.T) {
	suite.Run(t, new(mergeSQLGeneratorSuite))
}
