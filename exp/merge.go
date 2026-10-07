package exp

import (
	"github.com/doug-martin/goqu/v9/internal/errors"
)

// MergeActionType identifies the DML operation performed by a MERGE branch.
type MergeActionType int

const (
	// MergeUpdateAction is the WHEN ... THEN UPDATE SET action.
	MergeUpdateAction MergeActionType = iota
	// MergeDeleteAction is the WHEN ... THEN DELETE action.
	MergeDeleteAction
	// MergeInsertAction is the WHEN ... THEN INSERT action.
	MergeInsertAction
)

// MergeMatchType identifies the kind of WHEN clause a branch belongs to.
type MergeMatchType int

const (
	// MatchedMergeType is a WHEN MATCHED (source row matched a target row) branch.
	MatchedMergeType MergeMatchType = iota
	// NotMatchedMergeType is a WHEN NOT MATCHED (source row had no target row) branch.
	NotMatchedMergeType
)

type (
	// MergeActionExpression is a single THEN action of a MERGE statement.
	MergeActionExpression interface {
		Expression
		// Action returns the DML operation the branch performs.
		Action() MergeActionType
		// MatchType returns the WHEN category (MATCHED or NOT MATCHED) the action belongs to.
		MatchType() MergeMatchType
		// IsUnconditional returns true when the branch has no WHEN AND condition.
		IsUnconditional() bool
		// Condition returns the optional WHEN AND condition (nil when unconditional).
		Condition() Expression

		// UpdateValues returns the SET mapping used by an UPDATE action.
		UpdateValues() interface{}
		// InsertCols returns the columns used by an INSERT action.
		InsertCols() ColumnListExpression
		// InsertVals returns the values used by an INSERT action.
		InsertVals() []interface{}
	}
	mergeAction struct {
		actionType MergeActionType
		matchType  MergeMatchType
		condition  Expression

		update interface{}
		cols   ColumnListExpression
		vals   []interface{}
	}
)

func (m MergeActionType) String() string {
	switch m {
	case MergeUpdateAction:
		return "UPDATE"
	case MergeDeleteAction:
		return "DELETE"
	case MergeInsertAction:
		return "INSERT"
	}
	return "UNKNOWN"
}

func (m MergeMatchType) String() string {
	switch m {
	case MatchedMergeType:
		return "MATCHED"
	case NotMatchedMergeType:
		return "NOT MATCHED"
	}
	return "UNKNOWN"
}

func newMergeAction(
	matchType MergeMatchType,
	actionType MergeActionType,
	condition Expression,
) *mergeAction {
	return &mergeAction{matchType: matchType, actionType: actionType, condition: condition}
}

// NewMatchedUpdateAction creates a WHEN MATCHED THEN UPDATE SET action.
func NewMatchedUpdateAction(update interface{}, condition Expression) MergeActionExpression {
	a := newMergeAction(MatchedMergeType, MergeUpdateAction, condition)
	a.update = update
	return a
}

// NewMatchedDeleteAction creates a WHEN MATCHED THEN DELETE action.
func NewMatchedDeleteAction(condition Expression) MergeActionExpression {
	return newMergeAction(MatchedMergeType, MergeDeleteAction, condition)
}

// NewNotMatchedInsertAction creates a WHEN NOT MATCHED THEN INSERT action using an
// explicit column list and matching values.
func NewNotMatchedInsertAction(cols ColumnListExpression, vals []interface{}, condition Expression) MergeActionExpression {
	a := newMergeAction(NotMatchedMergeType, MergeInsertAction, condition)
	a.cols = cols
	a.vals = vals
	return a
}

// NewNotMatchedRecordAction creates a WHEN NOT MATCHED THEN INSERT action from a
// Record/map or struct. Columns are derived in a stable (sorted) order.
func NewNotMatchedRecordAction(row interface{}, condition Expression) (MergeActionExpression, error) {
	cols, vals, err := mergeRecordColsVals(row)
	if err != nil {
		return nil, err
	}
	return NewNotMatchedInsertAction(cols, vals, condition), nil
}

func mergeRecordColsVals(row interface{}) (ColumnListExpression, []interface{}, error) {
	record, err := mergeRecord(row)
	if err != nil {
		return nil, nil, err
	}
	colNames := record.Cols()
	colArgs := make([]interface{}, 0, len(colNames))
	vals := make([]interface{}, 0, len(colNames))
	for _, col := range colNames {
		colArgs = append(colArgs, col)
		vals = append(vals, record[col])
	}
	return NewColumnListExpression(colArgs...), vals, nil
}

func mergeRecord(row interface{}) (Record, error) {
	switch r := row.(type) {
	case Record:
		return r, nil
	case map[string]interface{}:
		return Record(r), nil
	default:
		record, err := NewRecordFromStruct(row, true, false)
		if err != nil {
			return nil, err
		}
		return record, nil
	}
}

func (a *mergeAction) Expression() Expression { return a }

func (a *mergeAction) Clone() Expression {
	clone := &mergeAction{
		actionType: a.actionType,
		matchType:  a.matchType,
		update:     a.update,
		cols:       a.cols,
		vals:       a.vals,
	}
	if a.condition != nil {
		clone.condition = a.condition.Clone()
	}
	if a.cols != nil {
		clone.cols = a.cols.Clone().(ColumnListExpression)
	}
	return clone
}

func (a *mergeAction) Action() MergeActionType { return a.actionType }

func (a *mergeAction) MatchType() MergeMatchType { return a.matchType }

func (a *mergeAction) IsUnconditional() bool { return a.condition == nil }

func (a *mergeAction) Condition() Expression { return a.condition }

func (a *mergeAction) UpdateValues() interface{} { return a.update }

func (a *mergeAction) InsertCols() ColumnListExpression { return a.cols }

func (a *mergeAction) InsertVals() []interface{} { return a.vals }

var (
	errNoTargetForMerge    = errors.New("no target found when generating merge sql")
	errNoSourceForMerge    = errors.New("no source found when generating merge sql")
	errNoOnForMerge        = errors.New("no ON condition found when generating merge sql")
	errNoMergeActions      = errors.New("at least one WHEN branch is required when generating merge sql")
	errMergeInsertNoValues = errors.New("MERGE INSERT action requires at least one value")
)

const (
	errMergeUnmatchedActionMsg = "unreachable MERGE branch: a WHEN %s branch with no condition cannot be followed by another WHEN %s branch"
	errMergeIllegalActionMsg   = "illegal MERGE action %s for a WHEN %s branch"
)

func errUnreachableMergeBranch(matchType MergeMatchType) error {
	return errors.New(errMergeUnmatchedActionMsg, matchType, matchType)
}

func errIllegalMergeAction(matchType MergeMatchType, action MergeActionType) error {
	return errors.New(errMergeIllegalActionMsg, action, matchType)
}

func errMergeInsertColumnMismatch(colCount, valCount int) error {
	return errors.New("MERGE INSERT has %d columns but %d values", colCount, valCount)
}
