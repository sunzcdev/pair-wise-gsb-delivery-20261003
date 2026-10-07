package goqu

import (
	"fmt"

	"github.com/doug-martin/goqu/v9/exec"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/sb"
)

type MergeDataset struct {
	dialect      SQLDialect
	clauses      exp.MergeClauses
	isPrepared   prepared
	queryFactory exec.QueryFactory
	err          error
}

// used internally by database to create a MERGE dataset with a specific dialect
func newMergeDataset(d string, queryFactory exec.QueryFactory) *MergeDataset {
	return &MergeDataset{
		clauses:      exp.NewMergeClauses(),
		dialect:      GetDialect(d),
		queryFactory: queryFactory,
	}
}

// Creates a new MergeDataset for the provided target table.
//
//	string: Will automatically be turned into an identifier
//	Expression: Any valid expression (IdentifierExpression, AliasedExpression, Literal, etc.)
func Merge(target interface{}) *MergeDataset {
	return newMergeDataset("default", nil).Target(target)
}

// Set the parameter interpolation behavior. See examples.
//
// prepared: If true the dataset WILL NOT interpolate the parameters.
func (md *MergeDataset) Prepared(prepared bool) *MergeDataset {
	ret := md.copy(md.clauses)
	ret.isPrepared = preparedFromBool(prepared)
	return ret
}

func (md *MergeDataset) IsPrepared() bool {
	return md.isPrepared.Bool()
}

// Sets the adapter used to serialize values and create the SQL statement
func (md *MergeDataset) WithDialect(dl string) *MergeDataset {
	ds := md.copy(md.GetClauses())
	ds.dialect = GetDialect(dl)
	return ds
}

// Returns the current adapter on the dataset
func (md *MergeDataset) Dialect() SQLDialect {
	return md.dialect
}

// Returns the current adapter on the dataset
func (md *MergeDataset) SetDialect(dialect SQLDialect) *MergeDataset {
	cd := md.copy(md.GetClauses())
	cd.dialect = dialect
	return cd
}

func (md *MergeDataset) Expression() exp.Expression {
	return md
}

// Clones the dataset
func (md *MergeDataset) Clone() exp.Expression {
	return md.copy(md.clauses)
}

// Returns the current clauses on the dataset.
func (md *MergeDataset) GetClauses() exp.MergeClauses {
	return md.clauses
}

// used interally to copy the dataset
func (md *MergeDataset) copy(clauses exp.MergeClauses) *MergeDataset {
	return &MergeDataset{
		dialect:      md.dialect,
		clauses:      clauses,
		isPrepared:   md.isPrepared,
		queryFactory: md.queryFactory,
		err:          md.err,
	}
}

// Creates a WITH clause for a common table expression (CTE).
func (md *MergeDataset) With(name string, subquery exp.Expression) *MergeDataset {
	return md.copy(md.clauses.CommonTablesAppend(exp.NewCommonTableExpression(false, name, subquery)))
}

// Creates a WITH RECURSIVE clause for a common table expression (CTE).
func (md *MergeDataset) WithRecursive(name string, subquery exp.Expression) *MergeDataset {
	return md.copy(md.clauses.CommonTablesAppend(exp.NewCommonTableExpression(true, name, subquery)))
}

// Sets the target table of the MERGE statement, replacing any existing target.
//
//	string: Will automatically be turned into an identifier
//	Expression: Any valid expression (IdentifierExpression, AliasedExpression, Literal, etc.)
func (md *MergeDataset) Target(target interface{}) *MergeDataset {
	switch t := target.(type) {
	case exp.Expression:
		return md.copy(md.clauses.SetTarget(t))
	case string:
		return md.copy(md.clauses.SetTarget(exp.ParseIdentifier(t)))
	default:
		panic(ErrUnsupportedIntoType)
	}
}

// Sets an alias for the target table (e.g. MERGE INTO "table" AS "t").
func (md *MergeDataset) TargetAs(alias string) *MergeDataset {
	return md.copy(md.clauses.SetTargetAlias(T(alias)))
}

// Sets the USING source of the MERGE statement.
//
//	string: Will automatically be turned into an identifier
//	Expression: An identifier, aliased expression, or a subquery (use UsingAs to alias a subquery).
func (md *MergeDataset) Using(source interface{}) *MergeDataset {
	switch t := source.(type) {
	case exp.Expression:
		return md.usingExpression(t)
	case string:
		return md.copy(md.clauses.SetSource(exp.ParseIdentifier(t)))
	default:
		panic(ErrUnsupportedIntoType)
	}
}

// Sets a subquery or other expression as the USING source and aliases it.
func (md *MergeDataset) UsingAs(source exp.AppendableExpression, alias string) *MergeDataset {
	if sds, ok := source.(*SelectDataset); ok {
		if sds.dialect != GetDialect("default") && md.dialect != sds.dialect {
			panic(
				fmt.Errorf(
					"incompatible dialects for MERGE (%q) and source (%q)",
					md.dialect.Dialect(), sds.dialect.Dialect(),
				),
			)
		}
		sds.dialect = md.dialect
	}
	return md.copy(md.clauses.SetSource(exp.NewAliasExpression(source, alias)))
}

func (md *MergeDataset) usingExpression(source exp.Expression) *MergeDataset {
	if sds, ok := source.(*SelectDataset); ok {
		if sds.dialect != GetDialect("default") && md.dialect != sds.dialect {
			panic(
				fmt.Errorf(
					"incompatible dialects for MERGE (%q) and source (%q)",
					md.dialect.Dialect(), sds.dialect.Dialect(),
				),
			)
		}
		sds.dialect = md.dialect
	}
	return md.copy(md.clauses.SetSource(source))
}

// Sets the ON condition used to match the source and target rows.
func (md *MergeDataset) On(expressions ...exp.Expression) *MergeDataset {
	return md.copy(md.clauses.SetOn(exp.NewExpressionList(exp.AndType, expressions...)))
}

// Appends a fully configured branch to the MERGE statement.
func (md *MergeDataset) When(branch exp.MergeBranchExpression) *MergeDataset {
	clauses, err := md.clauses.AppendBranch(branch)
	if err != nil {
		return md.SetError(err)
	}
	return md.copy(clauses)
}

// Begins a new WHEN MATCHED branch with the supplied optional search condition.
func (md *MergeDataset) WhenMatched(condition ...exp.Expression) *MergeBranch {
	return md.newBranch(exp.NewMergeMatchedBranch(joinCondition(condition)))
}

// Begins a new WHEN NOT MATCHED branch with the supplied optional search condition.
func (md *MergeDataset) WhenNotMatched(condition ...exp.Expression) *MergeBranch {
	return md.newBranch(exp.NewMergeNotMatchedBranch(joinCondition(condition)))
}

func (md *MergeDataset) newBranch(branch exp.MergeBranchExpression) *MergeBranch {
	return &MergeBranch{
		dataset: md,
		branch:  branch,
		err:     nil,
	}
}

// Adds a RETURNING clause to the dataset if the adapter supports it.
func (md *MergeDataset) Returning(returning ...interface{}) *MergeDataset {
	return md.copy(md.clauses.SetReturning(exp.NewColumnListExpression(returning...)))
}

// Get any error that has been set or nil if no error has been set.
func (md *MergeDataset) Error() error {
	return md.err
}

// Set an error on the dataset if one has not already been set.
func (md *MergeDataset) SetError(err error) *MergeDataset {
	if md.err == nil {
		md.err = err
	}
	return md
}

// Generates the MERGE statement. If Prepared has been called with true then the statement will not be interpolated.
func (md *MergeDataset) ToSQL() (sql string, params []interface{}, err error) {
	return md.mergeSQLBuilder().ToSQL()
}

// Appends this Dataset's MERGE statement to the SQLBuilder.
func (md *MergeDataset) AppendSQL(b sb.SQLBuilder) {
	if md.err != nil {
		b.SetError(md.err)
		return
	}
	md.dialect.ToMergeSQL(b, md.GetClauses())
}

func (md *MergeDataset) ReturnsColumns() bool {
	return md.clauses.HasReturning()
}

// Generates the MERGE sql, and returns a QueryExecutor struct with the sql set to the MERGE statement.
func (md *MergeDataset) Executor() exec.QueryExecutor {
	return md.queryFactory.FromSQLBuilder(md.mergeSQLBuilder())
}

func (md *MergeDataset) mergeSQLBuilder() sb.SQLBuilder {
	buf := sb.NewSQLBuilder(md.isPrepared.Bool())
	if md.err != nil {
		return buf.SetError(md.err)
	}
	md.dialect.ToMergeSQL(buf, md.clauses)
	return buf
}

func joinCondition(condition []exp.Expression) exp.Expression {
	switch len(condition) {
	case 0:
		return nil
	case 1:
		return condition[0]
	default:
		return exp.NewExpressionList(exp.AndType, condition...)
	}
}
