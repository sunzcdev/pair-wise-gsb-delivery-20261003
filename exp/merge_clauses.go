package exp

import "github.com/doug-martin/goqu/v9/internal/errors"

// MergeAction identifies the action performed by a MERGE branch.
type MergeAction int

const (
	// MergeDoNothing indicates that no action is taken for the branch.
	MergeDoNothing MergeAction = iota
	// MergeUpdateAction indicates an UPDATE action.
	MergeUpdateAction
	// MergeDeleteAction indicates a DELETE action.
	MergeDeleteAction
	// MergeInsertAction indicates an INSERT action.
	MergeInsertAction
)

// MergeBranchType identifies the category of a MERGE branch.
type MergeBranchType int

const (
	// MergeMatched represents a WHEN MATCHED branch.
	MergeMatched MergeBranchType = iota
	// MergeNotMatched represents a WHEN NOT MATCHED branch.
	MergeNotMatched
)

type (
	// MergeInsertExpression holds the explicit columns and single row of values
	// used by the INSERT action of a WHEN NOT MATCHED branch.
	MergeInsertExpression interface {
		Expression
		Cols() ColumnListExpression
		Vals() [][]interface{}
	}

	// MergeBranchExpression is a single WHEN branch of a MERGE statement.
	MergeBranchExpression interface {
		Expression

		// Type returns the branch category (WHEN MATCHED / WHEN NOT MATCHED).
		Type() MergeBranchType
		// Condition returns the optional search condition for the branch.
		Condition() Expression
		// Action returns the action performed when the branch matches.
		Action() MergeAction
		// Update returns the values used by an UPDATE action.
		Update() interface{}
		// Insert returns the row used by an INSERT action.
		Insert() interface{}

		// SetUpdate configures an UPDATE action for a WHEN MATCHED branch.
		SetUpdate(values interface{}) (MergeBranchExpression, error)
		// SetDelete configures a DELETE action for a WHEN MATCHED branch.
		SetDelete() (MergeBranchExpression, error)
		// SetInsert configures an INSERT action for a WHEN NOT MATCHED branch.
		SetInsert(row interface{}) (MergeBranchExpression, error)
	}

	// MergeClauses represents all of the clauses of a MERGE statement.
	MergeClauses interface {
		HasTarget() bool
		clone() *mergeClauses

		CommonTables() []CommonTableExpression
		CommonTablesAppend(cte CommonTableExpression) MergeClauses

		Target() Expression
		SetTarget(target Expression) MergeClauses

		HasTargetAlias() bool
		TargetAlias() IdentifierExpression
		SetTargetAlias(alias IdentifierExpression) MergeClauses

		Source() Expression
		HasSource() bool
		SetSource(source Expression) MergeClauses

		On() ExpressionList
		HasOn() bool
		SetOn(on ExpressionList) MergeClauses

		Branches() []MergeBranchExpression
		AppendBranch(branch MergeBranchExpression) (MergeClauses, error)

		Returning() ColumnListExpression
		HasReturning() bool
		SetReturning(cl ColumnListExpression) MergeClauses
	}

	mergeBranch struct {
		branchType MergeBranchType
		condition  Expression
		action     MergeAction
		update     interface{}
		insert     interface{}
	}

	mergeInsert struct {
		cols ColumnListExpression
		vals [][]interface{}
	}

	mergeClauses struct {
		commonTables []CommonTableExpression
		target       Expression
		targetAlias  IdentifierExpression
		source       Expression
		on           ExpressionList
		branches     []MergeBranchExpression
		returning    ColumnListExpression
	}
)

var (
	errMergeUpdateOnNotMatched     = errors.New("UPDATE action is only allowed on WHEN MATCHED branches")
	errMergeDeleteOnNotMatched     = errors.New("DELETE action is only allowed on WHEN MATCHED branches")
	errMergeInsertOnMatched        = errors.New("INSERT action is only allowed on WHEN NOT MATCHED branches")
	errMergeUpdateValuesMissing    = errors.New("values are required for a MERGE UPDATE action")
	errMergeInsertValuesMissing    = errors.New("a row is required for a MERGE INSERT action")
	errMergeInsertColsRequired     = errors.New("a MERGE INSERT action requires an explicit column list")
	errMergeInsertValuesRequired   = errors.New("a MERGE INSERT action requires values")
	errMergeInsertFromNotSupported = errors.New("a subquery cannot be used as the INSERT row in a MERGE statement")
)

func errMergeInsertRowLength(expectedL, actualL int) error {
	return errors.New("merge insert row with different value length expected %d got %d", expectedL, actualL)
}

func errMergeMultipleInsertRows(count int) error {
	return errors.New("a MERGE INSERT action supports a single row, got %d", count)
}

// NewMergeInsertExpression builds the validated INSERT payload for a MERGE WHEN NOT MATCHED branch.
// It accepts a single goqu.Record, map, or struct. Subqueries and multiple rows are rejected, and the
// column/value count is validated before SQL generation.
func NewMergeInsertExpression(row interface{}) (MergeInsertExpression, error) {
	if row == nil {
		return nil, errMergeInsertValuesMissing
	}
	if _, ok := row.(AppendableExpression); ok {
		return nil, errMergeInsertFromNotSupported
	}
	ie, err := NewInsertExpression(row)
	if err != nil {
		return nil, err
	}
	if ie.IsInsertFrom() {
		return nil, errMergeInsertFromNotSupported
	}
	cols := ie.Cols()
	if cols == nil || cols.IsEmpty() {
		return nil, errMergeInsertColsRequired
	}
	vals := ie.Vals()
	if len(vals) == 0 {
		return nil, errMergeInsertValuesRequired
	}
	if len(vals) != 1 {
		return nil, errMergeMultipleInsertRows(len(vals))
	}
	if len(vals[0]) != len(cols.Columns()) {
		return nil, errMergeInsertRowLength(len(cols.Columns()), len(vals[0]))
	}
	return &mergeInsert{cols: cols, vals: vals}, nil
}

func (mi *mergeInsert) Expression() Expression { return mi }
func (mi *mergeInsert) Clone() Expression {
	return &mergeInsert{cols: mi.cols, vals: mi.vals}
}
func (mi *mergeInsert) Cols() ColumnListExpression { return mi.cols }
func (mi *mergeInsert) Vals() [][]interface{}      { return mi.vals }

func errMergeUnreachableBranch(branchType MergeBranchType) error {
	kind := "WHEN MATCHED"
	if branchType == MergeNotMatched {
		kind = "WHEN NOT MATCHED"
	}
	return errors.New(
		"unreachable %s branch: a branch of the same type with no condition was already declared",
		kind,
	)
}

// NewMergeBranch creates a new MERGE branch. The condition is optional and may be nil.
func NewMergeBranch(branchType MergeBranchType, condition Expression) MergeBranchExpression {
	return &mergeBranch{branchType: branchType, condition: condition, action: MergeDoNothing}
}

// NewMergeMatchedBranch creates a new WHEN MATCHED branch with an optional search condition.
func NewMergeMatchedBranch(condition Expression) MergeBranchExpression {
	return NewMergeBranch(MergeMatched, condition)
}

// NewMergeNotMatchedBranch creates a new WHEN NOT MATCHED branch with an optional search condition.
func NewMergeNotMatchedBranch(condition Expression) MergeBranchExpression {
	return NewMergeBranch(MergeNotMatched, condition)
}

func (mb *mergeBranch) Expression() Expression {
	return mb
}

func (mb *mergeBranch) Clone() Expression {
	return &mergeBranch{
		branchType: mb.branchType,
		condition:  mb.condition,
		action:     mb.action,
		update:     mb.update,
		insert:     mb.insert,
	}
}

func (mb *mergeBranch) Type() MergeBranchType {
	return mb.branchType
}

func (mb *mergeBranch) Condition() Expression {
	return mb.condition
}

func (mb *mergeBranch) Action() MergeAction {
	return mb.action
}

func (mb *mergeBranch) Update() interface{} {
	return mb.update
}

func (mb *mergeBranch) Insert() interface{} {
	return mb.insert
}

func (mb *mergeBranch) SetUpdate(values interface{}) (MergeBranchExpression, error) {
	if mb.branchType != MergeMatched {
		return nil, errMergeUpdateOnNotMatched
	}
	if values == nil {
		return nil, errMergeUpdateValuesMissing
	}
	if _, err := NewUpdateExpressions(values); err != nil {
		return nil, err
	}
	ret := mb.clone()
	ret.action = MergeUpdateAction
	ret.update = values
	return ret, nil
}

func (mb *mergeBranch) SetDelete() (MergeBranchExpression, error) {
	if mb.branchType != MergeMatched {
		return nil, errMergeDeleteOnNotMatched
	}
	ret := mb.clone()
	ret.action = MergeDeleteAction
	ret.update = nil
	return ret, nil
}

func (mb *mergeBranch) SetInsert(row interface{}) (MergeBranchExpression, error) {
	if mb.branchType != MergeNotMatched {
		return nil, errMergeInsertOnMatched
	}
	if row == nil {
		return nil, errMergeInsertValuesMissing
	}
	insert, err := NewMergeInsertExpression(row)
	if err != nil {
		return nil, err
	}
	ret := mb.clone()
	ret.action = MergeInsertAction
	ret.insert = insert
	return ret, nil
}

func (mb *mergeBranch) clone() *mergeBranch {
	return (&mergeBranch{
		branchType: mb.branchType,
		condition:  mb.condition,
		action:     mb.action,
		update:     mb.update,
		insert:     mb.insert,
	})
}

// NewMergeClauses creates a new empty set of MERGE clauses.
func NewMergeClauses() MergeClauses {
	return &mergeClauses{}
}

func (mc *mergeClauses) HasTarget() bool {
	return mc.target != nil
}

func (mc *mergeClauses) clone() *mergeClauses {
	return &mergeClauses{
		commonTables: mc.commonTables,
		target:       mc.target,
		targetAlias:  mc.targetAlias,
		source:       mc.source,
		on:           mc.on,
		branches:     mc.branches,
		returning:    mc.returning,
	}
}

func (mc *mergeClauses) CommonTables() []CommonTableExpression {
	return mc.commonTables
}

func (mc *mergeClauses) CommonTablesAppend(cte CommonTableExpression) MergeClauses {
	ret := mc.clone()
	ret.commonTables = append(ret.commonTables, cte)
	return ret
}

func (mc *mergeClauses) Target() Expression {
	return mc.target
}

func (mc *mergeClauses) SetTarget(target Expression) MergeClauses {
	ret := mc.clone()
	ret.target = target
	return ret
}

func (mc *mergeClauses) HasTargetAlias() bool {
	return mc.targetAlias != nil
}

func (mc *mergeClauses) TargetAlias() IdentifierExpression {
	return mc.targetAlias
}

func (mc *mergeClauses) SetTargetAlias(alias IdentifierExpression) MergeClauses {
	ret := mc.clone()
	ret.targetAlias = alias
	return ret
}

func (mc *mergeClauses) Source() Expression {
	return mc.source
}

func (mc *mergeClauses) HasSource() bool {
	return mc.source != nil
}

func (mc *mergeClauses) SetSource(source Expression) MergeClauses {
	ret := mc.clone()
	ret.source = source
	return ret
}

func (mc *mergeClauses) On() ExpressionList {
	return mc.on
}

func (mc *mergeClauses) HasOn() bool {
	return mc.on != nil && !mc.on.IsEmpty()
}

func (mc *mergeClauses) SetOn(on ExpressionList) MergeClauses {
	ret := mc.clone()
	ret.on = on
	return ret
}

func (mc *mergeClauses) Branches() []MergeBranchExpression {
	return mc.branches
}

func (mc *mergeClauses) AppendBranch(branch MergeBranchExpression) (MergeClauses, error) {
	if branch == nil {
		return nil, errors.New("cannot append a nil MERGE branch")
	}
	for _, existing := range mc.branches {
		if existing.Type() == branch.Type() && existing.Condition() == nil {
			return nil, errMergeUnreachableBranch(branch.Type())
		}
	}
	ret := mc.clone()
	ret.branches = append(ret.branches, branch)
	return ret, nil
}

func (mc *mergeClauses) Returning() ColumnListExpression {
	return mc.returning
}

func (mc *mergeClauses) HasReturning() bool {
	return mc.returning != nil && !mc.returning.IsEmpty()
}

func (mc *mergeClauses) SetReturning(cl ColumnListExpression) MergeClauses {
	ret := mc.clone()
	ret.returning = cl
	return ret
}
