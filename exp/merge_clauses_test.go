package exp

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

type mergeClausesSuite struct {
	suite.Suite
}

func (mcs *mergeClausesSuite) TestBranch_TypeAndDefaults() {
	matched := NewMergeMatchedBranch(nil)
	mcs.Equal(MergeMatched, matched.Type())
	mcs.Nil(matched.Condition())
	mcs.Equal(MergeDoNothing, matched.Action())

	cond := ParseIdentifier("a").Eq(ParseIdentifier("b"))
	notMatched := NewMergeNotMatchedBranch(cond)
	mcs.Equal(MergeNotMatched, notMatched.Type())
	mcs.Equal(cond, notMatched.Condition())
}

func (mcs *mergeClausesSuite) TestBranch_Update() {
	b, err := NewMergeMatchedBranch(nil).SetUpdate(Record{"a": 1})
	mcs.NoError(err)
	mcs.Equal(MergeUpdateAction, b.Action())
	mcs.Equal(Record{"a": 1}, b.Update())

	_, err = NewMergeNotMatchedBranch(nil).SetUpdate(Record{"a": 1})
	mcs.EqualError(err, "goqu: UPDATE action is only allowed on WHEN MATCHED branches")

	_, err = NewMergeMatchedBranch(nil).SetUpdate(nil)
	mcs.EqualError(err, "goqu: values are required for a MERGE UPDATE action")

	_, err = NewMergeMatchedBranch(nil).SetUpdate([]string{"not-valid"})
	mcs.Error(err)
}

func (mcs *mergeClausesSuite) TestBranch_Delete() {
	b, err := NewMergeMatchedBranch(nil).SetDelete()
	mcs.NoError(err)
	mcs.Equal(MergeDeleteAction, b.Action())

	_, err = NewMergeNotMatchedBranch(nil).SetDelete()
	mcs.EqualError(err, "goqu: DELETE action is only allowed on WHEN MATCHED branches")
}

func (mcs *mergeClausesSuite) TestBranch_Insert() {
	b, err := NewMergeNotMatchedBranch(nil).SetInsert(Record{"a": 1})
	mcs.NoError(err)
	mcs.Equal(MergeInsertAction, b.Action())
	insert, ok := b.Insert().(MergeInsertExpression)
	mcs.True(ok)
	mcs.Len(insert.Cols().Columns(), 1)
	mcs.Equal([][]interface{}{{1}}, insert.Vals())

	_, err = NewMergeMatchedBranch(nil).SetInsert(Record{"a": 1})
	mcs.EqualError(err, "goqu: INSERT action is only allowed on WHEN NOT MATCHED branches")

	_, err = NewMergeNotMatchedBranch(nil).SetInsert(nil)
	mcs.EqualError(err, "goqu: a row is required for a MERGE INSERT action")
}

func (mcs *mergeClausesSuite) TestMergeInsertExpression_validation() {
	insert, err := NewMergeInsertExpression(Record{"a": 1, "b": 2})
	mcs.NoError(err)
	mcs.Len(insert.Cols().Columns(), 2)
	mcs.Equal([][]interface{}{{1, 2}}, insert.Vals())
	mcs.Equal(insert, insert.Expression())
	mcs.Equal(insert, insert.Clone())

	_, err = NewMergeInsertExpression(nil)
	mcs.EqualError(err, "goqu: a row is required for a MERGE INSERT action")

	_, err = NewMergeInsertExpression(Record{})
	mcs.EqualError(err, "goqu: a MERGE INSERT action requires an explicit column list")
}

func (mcs *mergeClausesSuite) TestBranch_CloneAndExpression() {
	cond := ParseIdentifier("a").Eq(int64(1))
	b, err := NewMergeMatchedBranch(cond).SetUpdate(Record{"a": 2})
	mcs.NoError(err)

	cloned := b.Clone()
	mcs.Equal(b, cloned)
	mcs.Equal(b, b.Expression())
}

func (mcs *mergeClausesSuite) TestClauses_appendBranches() {
	clauses := NewMergeClauses()

	b1 := NewMergeMatchedBranch(ParseIdentifier("a").Eq(int64(1)))
	var err error
	clauses, err = clauses.AppendBranch(b1)
	mcs.NoError(err)

	// a second conditional matched branch after a conditional branch is reachable
	b2 := NewMergeMatchedBranch(ParseIdentifier("a").Eq(int64(2)))
	clauses, err = clauses.AppendBranch(b2)
	mcs.NoError(err)

	// an unconditional matched branch after conditional branches is reachable
	b3 := NewMergeMatchedBranch(nil)
	clauses, err = clauses.AppendBranch(b3)
	mcs.NoError(err)

	// a further conditional matched branch after the unconditional branch is unreachable
	_, err = clauses.AppendBranch(NewMergeMatchedBranch(ParseIdentifier("a").Eq(int64(3))))
	mcs.EqualError(
		err,
		"goqu: unreachable WHEN MATCHED branch: a branch of the same type with no condition was already declared",
	)

	// not matched branches are tracked independently
	nm1 := NewMergeNotMatchedBranch(ParseIdentifier("b").Eq(int64(1)))
	clauses, err = clauses.AppendBranch(nm1)
	mcs.NoError(err)
	nm2 := NewMergeNotMatchedBranch(nil)
	clauses, err = clauses.AppendBranch(nm2)
	mcs.NoError(err)
	_, err = clauses.AppendBranch(NewMergeNotMatchedBranch(ParseIdentifier("b").Eq(int64(2))))
	mcs.EqualError(
		err,
		"goqu: unreachable WHEN NOT MATCHED branch: a branch of the same type with no condition was already declared",
	)

	mcs.Len(clauses.Branches(), 5)

	_, err = clauses.AppendBranch(nil)
	mcs.EqualError(err, "goqu: cannot append a nil MERGE branch")
}

func (mcs *mergeClausesSuite) TestClauses_targetSourceOnReturning() {
	target := ParseIdentifier("accounts")
	source := ParseIdentifier("transactions")
	on := NewExpressionList(AndType, ParseIdentifier("a.id").Eq(ParseIdentifier("t.id")))
	returning := NewColumnListExpression("id")
	alias := NewIdentifierExpression("", "a", "")

	clauses := NewMergeClauses().
		SetTarget(target).
		SetSource(source).
		SetOn(on).
		SetReturning(returning).
		SetTargetAlias(alias)

	mcs.True(clauses.HasTarget())
	mcs.Equal(target, clauses.Target())
	mcs.True(clauses.HasSource())
	mcs.Equal(source, clauses.Source())
	mcs.True(clauses.HasOn())
	mcs.Equal(on, clauses.On())
	mcs.True(clauses.HasReturning())
	mcs.Equal(returning, clauses.Returning())
	mcs.True(clauses.HasTargetAlias())
	mcs.Equal(alias, clauses.TargetAlias())

	mcs.False(NewMergeClauses().HasOn())
	mcs.False(NewMergeClauses().HasReturning())
	mcs.False(NewMergeClauses().HasTargetAlias())
}

func (mcs *mergeClausesSuite) TestClauses_commonTables() {
	cte := NewCommonTableExpression(false, "cte", ParseIdentifier("other"))
	clauses := NewMergeClauses().CommonTablesAppend(cte)
	mcs.Len(clauses.CommonTables(), 1)
	mcs.Equal(cte, clauses.CommonTables()[0])
}

func TestMergeClausesSuite(t *testing.T) {
	suite.Run(t, new(mergeClausesSuite))
}
