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

func (msgs *mergeSQLGeneratorSuite) assertCases(msg sqlgen.MergeSQLGenerator, testCases ...mergeTestCase) {
	for _, tc := range testCases {
		b := sb.NewSQLBuilder(tc.isPrepared)
		msg.Generate(b, tc.clause)
		switch {
		case len(tc.err) > 0:
			msgs.assertErrorSQL(b, tc.err)
		case tc.isPrepared:
			msgs.assertPreparedSQL(b, tc.sql, tc.args)
		default:
			msgs.assertNotPreparedSQL(b, tc.sql)
		}
	}
}

func TestMergeSQLGeneratorSuite(t *testing.T) {
	suite.Run(t, new(mergeSQLGeneratorSuite))
}

func (msgs *mergeSQLGeneratorSuite) baseClauses() exp.MergeClauses {
	return exp.NewMergeClauses().
		SetTarget(exp.ParseIdentifier("accounts")).
		SetSource(exp.ParseIdentifier("staging")).
		SetOn(exp.ParseIdentifier("accounts.id").Eq(exp.ParseIdentifier("staging.id")))
}

func (msgs *mergeSQLGeneratorSuite) supportedOptions() *sqlgen.SQLDialectOptions {
	opts := sqlgen.DefaultDialectOptions()
	opts.SupportsMerge = true
	return opts
}

func (msgs *mergeSQLGeneratorSuite) TestDialect() {
	opts := msgs.supportedOptions()
	msgs.Equal("test", sqlgen.NewMergeSQLGenerator("test", opts).Dialect())
}

func (msgs *mergeSQLGeneratorSuite) TestUnsupportedDialect() {
	opts := sqlgen.DefaultDialectOptions()
	gen := sqlgen.NewMergeSQLGenerator("mysql", opts)
	msgs.assertCases(
		gen,
		mergeTestCase{clause: msgs.baseClauses(), err: "goqu: dialect does not support MERGE statement [dialect=mysql]"},
		mergeTestCase{clause: msgs.baseClauses(), isPrepared: true, err: "goqu: dialect does not support MERGE statement [dialect=mysql]"},
	)
}

func (msgs *mergeSQLGeneratorSuite) TestMissingClauses() {
	gen := sqlgen.NewMergeSQLGenerator("test", msgs.supportedOptions())
	withTarget := exp.NewMergeClauses().SetTarget(exp.ParseIdentifier("t"))
	withSource := withTarget.SetSource(exp.ParseIdentifier("s"))
	withOn := withSource.SetOn(exp.ParseIdentifier("t.id").Eq(exp.ParseIdentifier("s.id")))
	msgs.assertCases(
		gen,
		mergeTestCase{clause: exp.NewMergeClauses(), err: "goqu: no target found when generating merge sql"},
		mergeTestCase{clause: withTarget, err: "goqu: no source found when generating merge sql"},
		mergeTestCase{clause: withSource, err: "goqu: no ON condition found when generating merge sql"},
		mergeTestCase{clause: withOn, err: "goqu: at least one WHEN branch is required when generating merge sql"},
	)
}

func (msgs *mergeSQLGeneratorSuite) TestMatchedUpdate() {
	opts := msgs.supportedOptions()
	gen := sqlgen.NewMergeSQLGenerator("test", opts)

	clauses, err := msgs.baseClauses().AppendAction(
		exp.NewMatchedUpdateAction(exp.Record{"name": exp.ParseIdentifier("staging.name")}, nil),
	)
	msgs.NoError(err)

	msgs.assertCases(
		gen,
		mergeTestCase{
			clause: clauses,
			sql:    `MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN MATCHED THEN UPDATE SET "name"="staging"."name";`,
		},
		mergeTestCase{
			clause:     clauses,
			isPrepared: true,
			sql:        `MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN MATCHED THEN UPDATE SET "name"="staging"."name";`,
		},
	)
}

func (msgs *mergeSQLGeneratorSuite) TestMatchedDelete() {
	gen := sqlgen.NewMergeSQLGenerator("test", msgs.supportedOptions())

	clauses, err := msgs.baseClauses().AppendAction(exp.NewMatchedDeleteAction(nil))
	msgs.NoError(err)

	msgs.assertCases(
		gen,
		mergeTestCase{
			clause: clauses,
			sql:    `MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN MATCHED THEN DELETE;`,
		},
	)
}

func (msgs *mergeSQLGeneratorSuite) TestNotMatchedInsert() {
	gen := sqlgen.NewMergeSQLGenerator("test", msgs.supportedOptions())

	clauses, err := msgs.baseClauses().AppendAction(exp.NewNotMatchedInsertAction(
		exp.NewColumnListExpression("id", "name"),
		[]interface{}{1, "Bob"},
		nil,
	))
	msgs.NoError(err)

	msgs.assertCases(
		gen,
		mergeTestCase{
			clause: clauses,
			sql:    `MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN NOT MATCHED THEN INSERT ("id", "name") VALUES (1, 'Bob');`,
		},
		mergeTestCase{
			clause:     clauses,
			isPrepared: true,
			sql:        `MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN NOT MATCHED THEN INSERT ("id", "name") VALUES (?, ?);`,
			args:       []interface{}{int64(1), "Bob"},
		},
	)
}

func (msgs *mergeSQLGeneratorSuite) TestFullStatementWithConditionsAndOrdering() {
	gen := sqlgen.NewMergeSQLGenerator("test", msgs.supportedOptions())

	c := msgs.baseClauses()
	var err error
	c, err = c.AppendAction(exp.NewMatchedDeleteAction(exp.ParseIdentifier("staging.deleted").Is(true)))
	msgs.NoError(err)
	c, err = c.AppendAction(exp.NewMatchedUpdateAction(exp.Record{"name": exp.ParseIdentifier("staging.name")}, nil))
	msgs.NoError(err)
	c, err = c.AppendAction(exp.NewNotMatchedInsertAction(
		exp.NewColumnListExpression("id", "name"),
		[]interface{}{7, exp.ParseIdentifier("staging.name")},
		nil,
	))
	msgs.NoError(err)

	msgs.assertCases(
		gen,
		mergeTestCase{
			clause: c,
			sql: `MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") ` +
				`WHEN MATCHED AND ("staging"."deleted" IS TRUE) THEN DELETE ` +
				`WHEN MATCHED THEN UPDATE SET "name"="staging"."name" ` +
				`WHEN NOT MATCHED THEN INSERT ("id", "name") VALUES (7, "staging"."name");`,
		},
	)
}

func (msgs *mergeSQLGeneratorSuite) TestInsertColumnCountMismatch() {
	gen := sqlgen.NewMergeSQLGenerator("test", msgs.supportedOptions())

	c, err := msgs.baseClauses().AppendAction(exp.NewNotMatchedInsertAction(
		exp.NewColumnListExpression("id", "name"),
		[]interface{}{1},
		nil,
	))
	msgs.NoError(err)

	msgs.assertCases(
		gen,
		mergeTestCase{clause: c, err: "goqu: MERGE INSERT has 2 columns but 1 values"},
	)
}

func (msgs *mergeSQLGeneratorSuite) TestActionNotSupportedByDialect() {
	opts := msgs.supportedOptions()
	opts.SupportsMergeMatchedDelete = false
	gen := sqlgen.NewMergeSQLGenerator("test", opts)

	c, err := msgs.baseClauses().AppendAction(exp.NewMatchedDeleteAction(nil))
	msgs.NoError(err)

	msgs.assertCases(
		gen,
		mergeTestCase{clause: c, err: "goqu: dialect does not support MERGE DELETE action [dialect=test]"},
	)
}
