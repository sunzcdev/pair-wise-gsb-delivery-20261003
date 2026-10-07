package sqlgen

import (
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/errors"
	"github.com/doug-martin/goqu/v9/internal/sb"
)

type (
	// MergeSQLGenerator generates MERGE statements for a specific dialect.
	MergeSQLGenerator interface {
		Dialect() string
		Generate(b sb.SQLBuilder, clauses exp.MergeClauses)
	}
	mergeSQLGenerator struct {
		CommonSQLGenerator
	}
)

func errMergeNotSupported(dialect string) error {
	return errors.New("dialect does not support MERGE statement [dialect=%s]", dialect)
}

func errMergeActionNotSupported(dialect string, action exp.MergeActionType) error {
	return errors.New("dialect does not support MERGE %s action [dialect=%s]", action, dialect)
}

func NewMergeSQLGenerator(dialect string, do *SQLDialectOptions) MergeSQLGenerator {
	return &mergeSQLGenerator{NewCommonSQLGenerator(dialect, do)}
}

func (msg *mergeSQLGenerator) Generate(b sb.SQLBuilder, clauses exp.MergeClauses) {
	if !msg.DialectOptions().SupportsMerge {
		b.SetError(errMergeNotSupported(msg.Dialect()))
		return
	}
	if err := clauses.Validate(); err != nil {
		b.SetError(err)
		return
	}
	for _, f := range msg.DialectOptions().MergeSQLOrder {
		if b.Error() != nil {
			return
		}
		switch f {
		case MergeBeginSQLFragment:
			b.Write(msg.DialectOptions().MergeClause)
		case MergeTargetSQLFragment:
			msg.targetSQL(b, clauses)
		case MergeUsingSQLFragment:
			msg.usingSQL(b, clauses)
		case MergeOnSQLFragment:
			msg.onSQL(b, clauses.On())
		case MergeActionsSQLFragment:
			msg.actionsSQL(b, clauses.Actions())
		case ReturningSQLFragment:
			msg.ReturningSQL(b, clauses.Returning())
		default:
			b.SetError(ErrNotSupportedFragment("MERGE", f))
		}
	}
}

func (msg *mergeSQLGenerator) targetSQL(b sb.SQLBuilder, clauses exp.MergeClauses) {
	b.WriteRunes(msg.DialectOptions().SpaceRune)
	msg.ExpressionSQLGenerator().Generate(b, clauses.Target())
}

func (msg *mergeSQLGenerator) usingSQL(b sb.SQLBuilder, clauses exp.MergeClauses) {
	b.Write(msg.DialectOptions().MergeUsingFragment)
	msg.ExpressionSQLGenerator().Generate(b, clauses.Source())
}

func (msg *mergeSQLGenerator) onSQL(b sb.SQLBuilder, on exp.Expression) {
	b.Write(msg.DialectOptions().OnFragment)
	msg.ExpressionSQLGenerator().Generate(b, on)
}

func (msg *mergeSQLGenerator) actionsSQL(b sb.SQLBuilder, actions []exp.MergeActionExpression) {
	for _, action := range actions {
		if b.Error() != nil {
			return
		}
		msg.actionSQL(b, action)
	}
	// SQL Server requires a MERGE statement to be terminated with a semicolon.
	b.Write(msg.DialectOptions().MergeTerminator)
}

func (msg *mergeSQLGenerator) actionSQL(b sb.SQLBuilder, action exp.MergeActionExpression) {
	switch action.MatchType() {
	case exp.MatchedMergeType:
		b.Write(msg.DialectOptions().WhenMatchedFragment)
	case exp.NotMatchedMergeType:
		b.Write(msg.DialectOptions().WhenNotMatchedFragment)
	}
	if action.Condition() != nil {
		b.Write(msg.DialectOptions().WhenAndFragment)
		msg.ExpressionSQLGenerator().Generate(b, action.Condition())
	}
	b.Write(msg.DialectOptions().ThenMergeFragment)

	switch action.Action() {
	case exp.MergeUpdateAction:
		if !msg.DialectOptions().SupportsMergeMatchedUpdate {
			b.SetError(errMergeActionNotSupported(msg.Dialect(), action.Action()))
			return
		}
		msg.updateActionSQL(b, action.UpdateValues())
	case exp.MergeDeleteAction:
		if !msg.DialectOptions().SupportsMergeMatchedDelete {
			b.SetError(errMergeActionNotSupported(msg.Dialect(), action.Action()))
			return
		}
		b.Write(msg.DialectOptions().DeleteClause)
	case exp.MergeInsertAction:
		if !msg.DialectOptions().SupportsMergeNotMatchedInsert {
			b.SetError(errMergeActionNotSupported(msg.Dialect(), action.Action()))
			return
		}
		msg.insertActionSQL(b, action)
	}
}

func (msg *mergeSQLGenerator) updateActionSQL(b sb.SQLBuilder, update interface{}) {
	updates, err := exp.NewUpdateExpressions(update)
	if err != nil {
		b.SetError(err)
		return
	}
	if len(updates) == 0 {
		b.SetError(ErrNoUpdatedValuesProvided)
		return
	}
	b.Write(msg.DialectOptions().UpdateClause)
	b.Write(msg.DialectOptions().SetFragment)
	msg.UpdateExpressionSQL(b, updates...)
}

func (msg *mergeSQLGenerator) insertActionSQL(b sb.SQLBuilder, action exp.MergeActionExpression) {
	b.Write(msg.DialectOptions().MergeInsertKeyword)
	b.WriteRunes(msg.DialectOptions().SpaceRune, msg.DialectOptions().LeftParenRune)
	msg.ExpressionSQLGenerator().Generate(b, action.InsertCols())
	b.WriteRunes(msg.DialectOptions().RightParenRune)
	b.Write(msg.DialectOptions().ValuesFragment)
	msg.ExpressionSQLGenerator().Generate(b, action.InsertVals())
}
