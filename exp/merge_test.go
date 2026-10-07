package exp_test

import (
	"testing"

	"github.com/doug-martin/goqu/v9/exp"
	"github.com/stretchr/testify/suite"
)

type mergeSuite struct {
	suite.Suite
}

func TestMergeSuite(t *testing.T) {
	suite.Run(t, new(mergeSuite))
}

func (ms *mergeSuite) TestActionTypeString() {
	ms.Equal("UPDATE", exp.MergeUpdateAction.String())
	ms.Equal("DELETE", exp.MergeDeleteAction.String())
	ms.Equal("INSERT", exp.MergeInsertAction.String())
	ms.Equal("MATCHED", exp.MatchedMergeType.String())
	ms.Equal("NOT MATCHED", exp.NotMatchedMergeType.String())
}

func (ms *mergeSuite) TestNewMatchedUpdateAction() {
	cond := exp.ParseIdentifier("a").Eq(1)
	a := exp.NewMatchedUpdateAction(exp.Record{"x": 1}, cond)
	ms.Equal(exp.MergeUpdateAction, a.Action())
	ms.Equal(exp.MatchedMergeType, a.MatchType())
	ms.Equal(cond, a.Condition())
	ms.False(a.IsUnconditional())
	ms.Equal(exp.Record{"x": 1}, a.UpdateValues())
	ms.NotNil(a.Clone())
}

func (ms *mergeSuite) TestNewMatchedDeleteAction() {
	a := exp.NewMatchedDeleteAction(nil)
	ms.Equal(exp.MergeDeleteAction, a.Action())
	ms.Equal(exp.MatchedMergeType, a.MatchType())
	ms.True(a.IsUnconditional())
	ms.Nil(a.Condition())
}

func (ms *mergeSuite) TestNewNotMatchedInsertAction() {
	cols := exp.NewColumnListExpression("a", "b")
	a := exp.NewNotMatchedInsertAction(cols, []interface{}{1, 2}, nil)
	ms.Equal(exp.MergeInsertAction, a.Action())
	ms.Equal(exp.NotMatchedMergeType, a.MatchType())
	ms.True(a.IsUnconditional())
	ms.Equal(cols, a.InsertCols())
	ms.Equal([]interface{}{1, 2}, a.InsertVals())
	ms.NotNil(a.Clone())
}

func (ms *mergeSuite) TestNewNotMatchedRecordAction() {
	a, err := exp.NewNotMatchedRecordAction(exp.Record{"b": 2, "a": 1}, nil)
	ms.NoError(err)
	ms.Equal(exp.MergeInsertAction, a.Action())
	ms.Equal([]string{"a", "b"}, identifierNames(a.InsertCols()))
	ms.Equal([]interface{}{1, 2}, a.InsertVals())

	_, err = exp.NewNotMatchedRecordAction(123, nil)
	ms.Error(err)
}

func identifierNames(cl exp.ColumnListExpression) []string {
	names := make([]string, 0, len(cl.Columns()))
	for _, c := range cl.Columns() {
		names = append(names, c.(exp.IdentifierExpression).GetCol().(string))
	}
	return names
}

type mergeClausesSuite struct {
	suite.Suite
}

func TestMergeClausesSuite(t *testing.T) {
	suite.Run(t, new(mergeClausesSuite))
}

func (mcs *mergeClausesSuite) TestTargetSourceOn() {
	c := exp.NewMergeClauses()
	target := exp.ParseIdentifier("t")
	source := exp.ParseIdentifier("s")
	on := exp.ParseIdentifier("t.id").Eq(exp.ParseIdentifier("s.id"))

	mcs.False(c.HasOn())
	c = c.SetTarget(target).SetSource(source).SetOn(on)
	mcs.Equal(target, c.Target())
	mcs.Equal(source, c.Source())
	mcs.Equal(on, c.On())
	mcs.True(c.HasOn())
}

func (mcs *mergeClausesSuite) TestReturning() {
	c := exp.NewMergeClauses()
	mcs.False(c.HasReturning())
	c = c.SetReturning(exp.NewColumnListExpression("a"))
	mcs.True(c.HasReturning())
}

func (mcs *mergeClausesSuite) TestAppendActionsInDeclarationOrder() {
	c := exp.NewMergeClauses()
	var err error

	// conditional matched branches are fine before the unconditional one.
	c, err = c.AppendAction(exp.NewMatchedDeleteAction(exp.ParseIdentifier("x").Is(true)))
	mcs.NoError(err)
	c, err = c.AppendAction(exp.NewMatchedUpdateAction(exp.Record{"x": 1}, nil))
	mcs.NoError(err)
	mcs.Len(c.Actions(), 2)

	// an unconditional matched branch cannot be followed by another matched branch.
	_, err = c.AppendAction(exp.NewMatchedDeleteAction(nil))
	mcs.Error(err)
	mcs.Contains(err.Error(), "unreachable MERGE branch")

	// ... but a not-matched branch is a different category and allowed.
	c, err = c.AppendAction(exp.NewNotMatchedInsertAction(
		exp.NewColumnListExpression("a"), []interface{}{1}, nil,
	))
	mcs.NoError(err)

	// and a further not-matched branch is now unreachable.
	_, err = c.AppendAction(exp.NewNotMatchedInsertAction(
		exp.NewColumnListExpression("a"), []interface{}{1}, nil,
	))
	mcs.Error(err)
	mcs.Contains(err.Error(), "unreachable MERGE branch")
}

func (mcs *mergeClausesSuite) TestAppendActionRejectsIllegalCategory() {
	c := exp.NewMergeClauses()

	// INSERT is not valid on a WHEN MATCHED branch.
	illegal := exp.NewMergeActionForTest(exp.MatchedMergeType, exp.MergeInsertAction, nil)
	_, err := c.AppendAction(illegal)
	mcs.Error(err)
	mcs.Contains(err.Error(), "illegal MERGE action INSERT for a WHEN MATCHED branch")

	// UPDATE is not valid on a WHEN NOT MATCHED branch.
	illegalUpdate := exp.NewMergeActionForTest(exp.NotMatchedMergeType, exp.MergeUpdateAction, nil)
	_, err = c.AppendAction(illegalUpdate)
	mcs.Error(err)
	mcs.Contains(err.Error(), "illegal MERGE action UPDATE for a WHEN NOT MATCHED branch")

	// DELETE is not valid on a WHEN NOT MATCHED branch.
	illegalDelete := exp.NewMergeActionForTest(exp.NotMatchedMergeType, exp.MergeDeleteAction, nil)
	_, err = c.AppendAction(illegalDelete)
	mcs.Error(err)
	mcs.Contains(err.Error(), "illegal MERGE action DELETE for a WHEN NOT MATCHED branch")
}

func (mcs *mergeClausesSuite) TestValidateMissingClauses() {
	c := exp.NewMergeClauses()
	mcs.EqualError(c.Validate(), "goqu: no target found when generating merge sql")

	c = c.SetTarget(exp.ParseIdentifier("t"))
	mcs.EqualError(c.Validate(), "goqu: no source found when generating merge sql")

	c = c.SetSource(exp.ParseIdentifier("s"))
	mcs.EqualError(c.Validate(), "goqu: no ON condition found when generating merge sql")

	c = c.SetOn(exp.ParseIdentifier("t.id").Eq(exp.ParseIdentifier("s.id")))
	mcs.EqualError(c.Validate(), "goqu: at least one WHEN branch is required when generating merge sql")
}

func (mcs *mergeClausesSuite) TestValidateInsertColumnMismatch() {
	c := exp.NewMergeClauses().
		SetTarget(exp.ParseIdentifier("t")).
		SetSource(exp.ParseIdentifier("s")).
		SetOn(exp.ParseIdentifier("t.id").Eq(exp.ParseIdentifier("s.id")))

	c2, err := c.AppendAction(exp.NewNotMatchedInsertAction(
		exp.NewColumnListExpression("a", "b"), []interface{}{1}, nil,
	))
	mcs.NoError(err)
	mcs.EqualError(c2.Validate(), "goqu: MERGE INSERT has 2 columns but 1 values")
}

func (mcs *mergeClausesSuite) TestValidateInsertEmpty() {
	c := exp.NewMergeClauses().
		SetTarget(exp.ParseIdentifier("t")).
		SetSource(exp.ParseIdentifier("s")).
		SetOn(exp.ParseIdentifier("t.id").Eq(exp.ParseIdentifier("s.id")))

	c2, err := c.AppendAction(exp.NewNotMatchedInsertAction(nil, nil, nil))
	mcs.NoError(err)
	mcs.EqualError(c2.Validate(), "goqu: MERGE INSERT action requires at least one value")
}

func (mcs *mergeClausesSuite) TestValidateSuccess() {
	c := exp.NewMergeClauses().
		SetTarget(exp.ParseIdentifier("t")).
		SetSource(exp.ParseIdentifier("s")).
		SetOn(exp.ParseIdentifier("t.id").Eq(exp.ParseIdentifier("s.id")))

	var err error
	c, err = c.AppendAction(exp.NewMatchedUpdateAction(exp.Record{"x": 1}, nil))
	mcs.NoError(err)
	c, err = c.AppendAction(exp.NewNotMatchedInsertAction(
		exp.NewColumnListExpression("x"), []interface{}{1}, nil,
	))
	mcs.NoError(err)
	mcs.NoError(c.Validate())
}
