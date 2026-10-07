package exp

// NewMergeActionForTest exposes the otherwise internal action constructor so the
// external (exp_test) package can build category-invalid actions in tests.
func NewMergeActionForTest(matchType MergeMatchType, actionType MergeActionType, condition Expression) MergeActionExpression {
	return newMergeAction(matchType, actionType, condition)
}
