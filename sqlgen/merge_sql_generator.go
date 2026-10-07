package sqlgen

import (
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/errors"
	"github.com/doug-martin/goqu/v9/internal/sb"
)

type (
	// MergeSQLGenerator generates SQL for a MERGE statement.
	MergeSQLGenerator interface {
		Dialect() string
		Generate(b sb.SQLBuilder, clauses exp.MergeClauses)
	}

	mergeSQLGenerator struct {
		CommonSQLGenerator
	}
)

var (
	ErrNoTargetForMerge   = errors.New("no target found when generating merge sql")
	ErrNoSourceForMerge   = errors.New("no source found when generating merge sql")
	ErrNoOnForMerge       = errors.New("no ON condition found when generating merge sql")
	ErrNoBranchesForMerge = errors.New("at least one WHEN branch is required when generating merge sql")
)

func errMergeNotSupported(dialect string) error {
	return errors.New("dialect does not support MERGE statements [dialect=%s]", dialect)
}

func errMergeDeleteNotSupported(dialect string) error {
	return errors.New("dialect does not support the DELETE action in a MERGE statement [dialect=%s]", dialect)
}

func errMergeInsertInvalid() error {
	return errors.New("invalid MERGE INSERT payload, expected an exp.MergeInsertExpression")
}

// NewMergeSQLGenerator constructs the default MERGE SQL generator.
func NewMergeSQLGenerator(dialect string, do *SQLDialectOptions) MergeSQLGenerator {
	return &mergeSQLGenerator{NewCommonSQLGenerator(dialect, do)}
}

func (msg *mergeSQLGenerator) Generate(b sb.SQLBuilder, clauses exp.MergeClauses) {
	if !msg.DialectOptions().SupportsMerge {
		b.SetError(errMergeNotSupported(msg.Dialect()))
		return
	}
	if !clauses.HasTarget() {
		b.SetError(ErrNoTargetForMerge)
		return
	}
	if !clauses.HasSource() {
		b.SetError(ErrNoSourceForMerge)
		return
	}
	if !clauses.HasOn() {
		b.SetError(ErrNoOnForMerge)
		return
	}
	if len(clauses.Branches()) == 0 {
		b.SetError(ErrNoBranchesForMerge)
		return
	}

	for _, f := range msg.DialectOptions().MergeSQLOrder {
		if b.Error() != nil {
			return
		}
		switch f {
		case CommonTableSQLFragment:
			msg.ExpressionSQLGenerator().Generate(b, clauses.CommonTables())
		case MergeBeginSQLFragment:
			b.Write(msg.DialectOptions().MergeClause)
		case MergeTargetSQLFragment:
			msg.mergeTargetSQL(b, clauses)
		case MergeSourceSQLFragment:
			msg.mergeSourceSQL(b, clauses.Source())
		case MergeOnSQLFragment:
			b.Write(msg.DialectOptions().OnFragment)
			msg.ExpressionSQLGenerator().Generate(b, clauses.On())
		case MergeBranchesSQLFragment:
			msg.mergeBranchesSQL(b, clauses.Branches())
		case ReturningSQLFragment:
			msg.ReturningSQL(b, clauses.Returning())
		default:
			b.SetError(ErrNotSupportedFragment("MERGE", f))
		}
	}

	if b.Error() == nil {
		b.Write(msg.DialectOptions().MergeStatementEnd)
	}
}

func (msg *mergeSQLGenerator) mergeTargetSQL(b sb.SQLBuilder, clauses exp.MergeClauses) {
	b.WriteRunes(msg.DialectOptions().SpaceRune)
	msg.ExpressionSQLGenerator().Generate(b, clauses.Target())
	if clauses.HasTargetAlias() {
		b.Write(msg.DialectOptions().AsFragment)
		msg.ExpressionSQLGenerator().Generate(b, clauses.TargetAlias())
	}
}

func (msg *mergeSQLGenerator) mergeSourceSQL(b sb.SQLBuilder, source exp.Expression) {
	b.Write(msg.DialectOptions().MergeUsingFragment)
	msg.ExpressionSQLGenerator().Generate(b, source)
}

func (msg *mergeSQLGenerator) mergeBranchesSQL(b sb.SQLBuilder, branches []exp.MergeBranchExpression) {
	for _, branch := range branches {
		b.WriteRunes(msg.DialectOptions().SpaceRune)
		switch branch.Type() {
		case exp.MergeMatched:
			b.Write(msg.DialectOptions().MergeWhenMatchedFragment)
		case exp.MergeNotMatched:
			b.Write(msg.DialectOptions().MergeWhenNotMatchedFragment)
		}
		if branch.Condition() != nil {
			b.Write(msg.DialectOptions().MergeAndFragment)
			msg.ExpressionSQLGenerator().Generate(b, branch.Condition())
		}
		b.Write(msg.DialectOptions().ThenFragment)
		if err := msg.mergeActionSQL(b, branch); err != nil {
			b.SetError(err)
			return
		}
	}
}

func (msg *mergeSQLGenerator) mergeActionSQL(b sb.SQLBuilder, branch exp.MergeBranchExpression) error {
	switch branch.Action() {
	case exp.MergeUpdateAction:
		return msg.mergeUpdateSQL(b, branch.Update())
	case exp.MergeDeleteAction:
		if !msg.DialectOptions().SupportsMergeMatchedDelete {
			return errMergeDeleteNotSupported(msg.Dialect())
		}
		b.Write(msg.DialectOptions().MergeDeleteFragment)
	case exp.MergeInsertAction:
		return msg.mergeInsertSQL(b, branch.Insert())
	default:
		b.Write(msg.DialectOptions().MergeDoNothingFragment)
	}
	return nil
}

func (msg *mergeSQLGenerator) mergeUpdateSQL(b sb.SQLBuilder, update interface{}) error {
	updates, err := exp.NewUpdateExpressions(update)
	if err != nil {
		return err
	}
	if len(updates) == 0 {
		return ErrNoUpdatedValuesProvided
	}
	b.Write(msg.DialectOptions().MergeUpdateFragment)
	msg.UpdateExpressionSQL(b, updates...)
	return b.Error()
}

func (msg *mergeSQLGenerator) mergeInsertSQL(b sb.SQLBuilder, row interface{}) error {
	ie, ok := row.(exp.MergeInsertExpression)
	if !ok {
		return errMergeInsertInvalid()
	}
	cols := ie.Cols()
	vals := ie.Vals()

	b.Write(msg.DialectOptions().MergeInsertFragment)
	b.WriteRunes(msg.DialectOptions().SpaceRune, msg.DialectOptions().LeftParenRune)
	msg.ExpressionSQLGenerator().Generate(b, cols)
	b.WriteRunes(msg.DialectOptions().RightParenRune)
	b.Write(msg.DialectOptions().ValuesFragment)
	msg.ExpressionSQLGenerator().Generate(b, vals[0])
	return b.Error()
}
