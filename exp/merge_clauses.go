package exp

type (
	MergeClauses interface {
		clone() *mergeClauses

		Target() Expression
		SetTarget(e Expression) MergeClauses

		Source() Expression
		SetSource(e Expression) MergeClauses

		On() Expression
		HasOn() bool
		SetOn(e Expression) MergeClauses

		Actions() []MergeActionExpression
		AppendAction(action MergeActionExpression) (MergeClauses, error)

		Returning() ColumnListExpression
		HasReturning() bool
		SetReturning(cl ColumnListExpression) MergeClauses

		// Validate runs the structural checks that must succeed before SQL is generated.
		Validate() error
	}
	mergeClauses struct {
		target    Expression
		source    Expression
		on        Expression
		actions   []MergeActionExpression
		returning ColumnListExpression
	}
)

func NewMergeClauses() MergeClauses {
	return &mergeClauses{}
}

func (mc *mergeClauses) clone() *mergeClauses {
	return &mergeClauses{
		target:    mc.target,
		source:    mc.source,
		on:        mc.on,
		actions:   mc.actions,
		returning: mc.returning,
	}
}

func (mc *mergeClauses) Target() Expression { return mc.target }

func (mc *mergeClauses) SetTarget(e Expression) MergeClauses {
	ret := mc.clone()
	ret.target = e
	return ret
}

func (mc *mergeClauses) Source() Expression { return mc.source }

func (mc *mergeClauses) SetSource(e Expression) MergeClauses {
	ret := mc.clone()
	ret.source = e
	return ret
}

func (mc *mergeClauses) On() Expression { return mc.on }

func (mc *mergeClauses) HasOn() bool { return mc.on != nil }

func (mc *mergeClauses) SetOn(e Expression) MergeClauses {
	ret := mc.clone()
	ret.on = e
	return ret
}

func (mc *mergeClauses) Actions() []MergeActionExpression { return mc.actions }

// AppendAction validates branch ordering and action/category compatibility before
// recording the branch. Branches of the same category are matched in declaration
// order, so appending any branch after an unconditional branch of the same category
// is unreachable and rejected.
func (mc *mergeClauses) AppendAction(action MergeActionExpression) (MergeClauses, error) {
	if err := mc.validateAction(action); err != nil {
		return nil, err
	}
	ret := mc.clone()
	ret.actions = append(ret.actions, action)
	return ret, nil
}

func (mc *mergeClauses) validateAction(action MergeActionExpression) error {
	matchType := action.MatchType()
	if err := validateMergeActionCategory(matchType, action.Action()); err != nil {
		return err
	}
	for _, existing := range mc.actions {
		if existing.MatchType() == matchType && existing.IsUnconditional() {
			return errUnreachableMergeBranch(matchType)
		}
	}
	return nil
}

func validateMergeActionCategory(matchType MergeMatchType, action MergeActionType) error {
	switch matchType {
	case MatchedMergeType:
		if action != MergeUpdateAction && action != MergeDeleteAction {
			return errIllegalMergeAction(matchType, action)
		}
	case NotMatchedMergeType:
		if action != MergeInsertAction {
			return errIllegalMergeAction(matchType, action)
		}
	}
	return nil
}

func (mc *mergeClauses) Returning() ColumnListExpression { return mc.returning }

func (mc *mergeClauses) HasReturning() bool {
	return mc.returning != nil && !mc.returning.IsEmpty()
}

func (mc *mergeClauses) SetReturning(cl ColumnListExpression) MergeClauses {
	ret := mc.clone()
	ret.returning = cl
	return ret
}

// Validate verifies the structural requirements that are evaluated right before SQL
// generation: required clauses and per-branch insert/update value consistency.
func (mc *mergeClauses) Validate() error {
	if mc.target == nil {
		return errNoTargetForMerge
	}
	if mc.source == nil {
		return errNoSourceForMerge
	}
	if mc.on == nil {
		return errNoOnForMerge
	}
	if len(mc.actions) == 0 {
		return errNoMergeActions
	}
	for _, action := range mc.actions {
		switch action.Action() {
		case MergeInsertAction:
			if err := validateMergeInsertAction(action); err != nil {
				return err
			}
		case MergeUpdateAction:
			if _, err := NewUpdateExpressions(action.UpdateValues()); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateMergeInsertAction(action MergeActionExpression) error {
	cols := action.InsertCols()
	vals := action.InsertVals()
	if cols == nil || len(vals) == 0 {
		return errMergeInsertNoValues
	}
	colCount := len(cols.Columns())
	if colCount != len(vals) {
		return errMergeInsertColumnMismatch(colCount, len(vals))
	}
	return nil
}
