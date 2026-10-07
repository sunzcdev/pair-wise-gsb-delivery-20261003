package goqu

import (
	"fmt"

	"github.com/doug-martin/goqu/v9/exec"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/errors"
	"github.com/doug-martin/goqu/v9/internal/sb"
)

type (
	MergeDataset struct {
		dialect      SQLDialect
		clauses      exp.MergeClauses
		isPrepared   prepared
		queryFactory exec.QueryFactory
		err          error
	}

	// MergeBranchBuilder is the fluent builder returned by WhenMatched/WhenNotMatched.
	// It resolves into a single branch and returns the parent MergeDataset.
	MergeBranchBuilder struct {
		matchType exp.MergeMatchType
		condition exp.Expression
		dataset   *MergeDataset
	}
)

var ErrUnsupportedMergeTableType = errors.New("unsupported table type, a string or identifier expression is required")

func newMergeDataset(d string, queryFactory exec.QueryFactory) *MergeDataset {
	return &MergeDataset{
		clauses:      exp.NewMergeClauses(),
		dialect:      GetDialect(d),
		queryFactory: queryFactory,
	}
}

// Merge creates a new MergeDataset that merges rows into the provided target table.
func Merge(target interface{}) *MergeDataset {
	return newMergeDataset("default", nil).Target(target)
}

// Set the parameter interpolation behavior.
func (md *MergeDataset) Prepared(prepared bool) *MergeDataset {
	ret := md.copy(md.clauses)
	ret.isPrepared = preparedFromBool(prepared)
	return ret
}

func (md *MergeDataset) IsPrepared() bool {
	return md.isPrepared.Bool()
}

// WithDialect sets the adapter used to serialize values and create the SQL statement.
func (md *MergeDataset) WithDialect(dl string) *MergeDataset {
	ds := md.copy(md.GetClauses())
	ds.dialect = GetDialect(dl)
	return ds
}

// Dialect returns the current adapter on the dataset.
func (md *MergeDataset) Dialect() SQLDialect {
	return md.dialect
}

// SetDialect sets the current adapter on the dataset.
func (md *MergeDataset) SetDialect(dialect SQLDialect) *MergeDataset {
	cd := md.copy(md.GetClauses())
	cd.dialect = dialect
	return cd
}

func (md *MergeDataset) Expression() exp.Expression {
	return md
}

// Clone clones the dataset.
func (md *MergeDataset) Clone() exp.Expression {
	return md.copy(md.clauses)
}

// GetClauses returns the current clauses on the dataset.
func (md *MergeDataset) GetClauses() exp.MergeClauses {
	return md.clauses
}

func (md *MergeDataset) copy(clauses exp.MergeClauses) *MergeDataset {
	return &MergeDataset{
		dialect:      md.dialect,
		clauses:      clauses,
		isPrepared:   md.isPrepared,
		queryFactory: md.queryFactory,
		err:          md.err,
	}
}

func toMergeExpression(v interface{}) (exp.Expression, error) {
	switch t := v.(type) {
	case exp.Expression:
		return t, nil
	case string:
		return exp.ParseIdentifier(t), nil
	default:
		return nil, ErrUnsupportedMergeTableType
	}
}

// Target sets (or replaces) the target table of the MERGE statement.
func (md *MergeDataset) Target(target interface{}) *MergeDataset {
	e, err := toMergeExpression(target)
	if err != nil {
		return md.SetError(err)
	}
	return md.copy(md.clauses.SetTarget(e))
}

// Using sets the MERGE source. The source may be a table name/identifier or an
// aliased subquery (e.g. goqu.From(...).As("src")).
func (md *MergeDataset) Using(source interface{}) *MergeDataset {
	switch t := source.(type) {
	case *SelectDataset:
		if t.Dialect() != GetDialect("default") && md.Dialect() != t.Dialect() {
			return md.SetError(fmt.Errorf(
				"incompatible dialects for MERGE (%q) and source SELECT (%q)",
				md.dialect.Dialect(), t.Dialect().Dialect(),
			))
		}
		return md.copy(md.clauses.SetSource(t.SetDialect(md.dialect)))
	case exp.Expression:
		return md.copy(md.clauses.SetSource(t))
	case string:
		return md.copy(md.clauses.SetSource(exp.ParseIdentifier(t)))
	default:
		return md.SetError(ErrUnsupportedMergeTableType)
	}
}

// On sets the join condition used to decide whether a source row matches a target row.
func (md *MergeDataset) On(condition exp.Expression) *MergeDataset {
	return md.copy(md.clauses.SetOn(condition))
}

// WhenMatched begins a WHEN MATCHED branch with no extra condition.
func (md *MergeDataset) WhenMatched() *MergeBranchBuilder {
	return &MergeBranchBuilder{matchType: exp.MatchedMergeType, dataset: md}
}

// WhenMatchedAnd begins a conditional WHEN MATCHED AND <condition> branch.
func (md *MergeDataset) WhenMatchedAnd(condition exp.Expression) *MergeBranchBuilder {
	return &MergeBranchBuilder{matchType: exp.MatchedMergeType, condition: condition, dataset: md}
}

// WhenNotMatched begins a WHEN NOT MATCHED branch with no extra condition.
func (md *MergeDataset) WhenNotMatched() *MergeBranchBuilder {
	return &MergeBranchBuilder{matchType: exp.NotMatchedMergeType, dataset: md}
}

// WhenNotMatchedAnd begins a conditional WHEN NOT MATCHED AND <condition> branch.
func (md *MergeDataset) WhenNotMatchedAnd(condition exp.Expression) *MergeBranchBuilder {
	return &MergeBranchBuilder{matchType: exp.NotMatchedMergeType, condition: condition, dataset: md}
}

// AppendAction appends a pre-built action expression. It is an escape hatch; the
// WhenMatched/WhenNotMatched builders are preferred. Branch ordering and category
// are validated here.
func (md *MergeDataset) AppendAction(action exp.MergeActionExpression) *MergeDataset {
	clauses, err := md.clauses.AppendAction(action)
	if err != nil {
		return md.SetError(err)
	}
	return md.copy(clauses)
}

// Returning adds a RETURNING clause to the merge if the dialect supports it.
func (md *MergeDataset) Returning(returning ...interface{}) *MergeDataset {
	return md.copy(md.clauses.SetReturning(exp.NewColumnListExpression(returning...)))
}

// Error returns any error that has been set.
func (md *MergeDataset) Error() error {
	return md.err
}

// SetError records an error on the dataset if one has not already been set.
func (md *MergeDataset) SetError(err error) *MergeDataset {
	if md.err == nil {
		md.err = err
	}
	return md
}

// ToSQL generates the MERGE statement.
func (md *MergeDataset) ToSQL() (sql string, params []interface{}, err error) {
	return md.mergeSQLBuilder().ToSQL()
}

// AppendSQL appends this Dataset's MERGE statement to the SQLBuilder.
func (md *MergeDataset) AppendSQL(b sb.SQLBuilder) {
	if md.err != nil {
		b.SetError(md.err)
		return
	}
	md.dialect.ToMergeSQL(b, md.GetClauses())
}

// Executor generates the MERGE sql and returns a QueryExecutor.
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

func (bb *MergeBranchBuilder) append(action exp.MergeActionExpression) *MergeDataset {
	return bb.dataset.AppendAction(action)
}

// UpdateSet finishes the branch with a THEN UPDATE SET action.
func (bb *MergeBranchBuilder) UpdateSet(values interface{}) *MergeDataset {
	return bb.append(exp.NewMatchedUpdateAction(values, bb.condition))
}

// Delete finishes the branch with a THEN DELETE action.
func (bb *MergeBranchBuilder) Delete() *MergeDataset {
	return bb.append(exp.NewMatchedDeleteAction(bb.condition))
}

// Insert finishes the branch with a THEN INSERT (cols) VALUES (vals) action.
func (bb *MergeBranchBuilder) Insert(cols []interface{}, vals []interface{}) *MergeDataset {
	return bb.append(exp.NewNotMatchedInsertAction(exp.NewColumnListExpression(cols...), vals, bb.condition))
}

// InsertRow finishes the branch with a THEN INSERT derived from a Record/map/struct.
func (bb *MergeBranchBuilder) InsertRow(row interface{}) *MergeDataset {
	action, err := exp.NewNotMatchedRecordAction(row, bb.condition)
	if err != nil {
		return bb.dataset.SetError(err)
	}
	return bb.append(action)
}
