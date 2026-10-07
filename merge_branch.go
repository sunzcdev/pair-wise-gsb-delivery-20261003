package goqu

import "github.com/doug-martin/goqu/v9/exp"

// MergeBranch is a fluent builder for a single WHEN branch of a MERGE statement.
// It is terminated by one of the action methods (ThenUpdate, ThenDelete,
// ThenInsert, or ThenDoNothing) which return the parent MergeDataset.
type MergeBranch struct {
	dataset *MergeDataset
	branch  exp.MergeBranchExpression
	err     error
}

// Configures the branch to UPDATE the target row.
//
// The values may be a goqu.Record, a map, a struct, or one or more UpdateExpression.
func (mb *MergeBranch) ThenUpdate(values interface{}) *MergeDataset {
	if mb.err != nil {
		return mb.dataset.SetError(mb.err)
	}
	branch, err := mb.branch.SetUpdate(values)
	if err != nil {
		return mb.dataset.SetError(err)
	}
	return mb.append(branch)
}

// Configures the branch to DELETE the target row.
func (mb *MergeBranch) ThenDelete() *MergeDataset {
	if mb.err != nil {
		return mb.dataset.SetError(mb.err)
	}
	branch, err := mb.branch.SetDelete()
	if err != nil {
		return mb.dataset.SetError(err)
	}
	return mb.append(branch)
}

// Configures the branch to INSERT a new target row.
//
// The row may be a goqu.Record, a map, or a struct.
func (mb *MergeBranch) ThenInsert(row interface{}) *MergeDataset {
	if mb.err != nil {
		return mb.dataset.SetError(mb.err)
	}
	branch, err := mb.branch.SetInsert(row)
	if err != nil {
		return mb.dataset.SetError(err)
	}
	return mb.append(branch)
}

// Configures the branch to take no action.
func (mb *MergeBranch) ThenDoNothing() *MergeDataset {
	if mb.err != nil {
		return mb.dataset.SetError(mb.err)
	}
	return mb.append(mb.branch)
}

func (mb *MergeBranch) append(branch exp.MergeBranchExpression) *MergeDataset {
	clauses, err := mb.dataset.clauses.AppendBranch(branch)
	if err != nil {
		return mb.dataset.SetError(err)
	}
	return mb.dataset.copy(clauses)
}
