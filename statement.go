package squirrel

import "github.com/lann/builder"

// StatementBuilderType is the type of StatementBuilder.
type StatementBuilderType builder.Builder

// Select returns a SelectBuilder for this StatementBuilderType.
func (b StatementBuilderType) Select(columns ...string) SelectBuilder {
	return SelectBuilder(b).Columns(columns...)
}

// Insert returns a InsertBuilder for this StatementBuilderType.
func (b StatementBuilderType) Insert(into string) InsertBuilder {
	return InsertBuilder(b).Into(into)
}

// Replace returns a InsertBuilder for this StatementBuilderType with the
// statement keyword set to "REPLACE".
func (b StatementBuilderType) Replace(into string) InsertBuilder {
	return InsertBuilder(b).statementKeyword("REPLACE").Into(into)
}

// Update returns a UpdateBuilder for this StatementBuilderType.
func (b StatementBuilderType) Update(table string) UpdateBuilder {
	return UpdateBuilder(b).Table(table)
}

// Delete returns a DeleteBuilder for this StatementBuilderType.
func (b StatementBuilderType) Delete(from string) DeleteBuilder {
	return DeleteBuilder(b).From(from)
}

// newSetBuilderWith returns a SetBuilder that inherits the Runner and
// PlaceholderFormat configured on this StatementBuilderType.
func (b StatementBuilderType) newSetBuilderWith(operator setOperator, branches []Sqlizer) SetBuilder {
	out := SetBuilder(builder.EmptyBuilder)
	if f, ok := builder.Get(b, "PlaceholderFormat"); ok && f != nil {
		out = out.PlaceholderFormat(f.(PlaceholderFormat))
	}
	if r, ok := builder.Get(b, "RunWith"); ok && r != nil {
		out = builder.Set(out, "RunWith", r).(SetBuilder)
	}
	out = builder.Set(out, "Operator", operator).(SetBuilder)
	out = builder.Extend(out, "Branches", sanitizeSetBranches(branches)).(SetBuilder)
	return out
}

// Union returns a SetBuilder for this StatementBuilderType.
//
// See the package-level Union function for details.
func (b StatementBuilderType) Union(branches ...Sqlizer) SetBuilder {
	return b.newSetBuilderWith(unionOperator, branches)
}

// UnionAll returns a SetBuilder for this StatementBuilderType.
func (b StatementBuilderType) UnionAll(branches ...Sqlizer) SetBuilder {
	return b.newSetBuilderWith(unionAllOperator, branches)
}

// Intersect returns a SetBuilder for this StatementBuilderType.
func (b StatementBuilderType) Intersect(branches ...Sqlizer) SetBuilder {
	return b.newSetBuilderWith(intersectOperator, branches)
}

// Except returns a SetBuilder for this StatementBuilderType.
func (b StatementBuilderType) Except(branches ...Sqlizer) SetBuilder {
	return b.newSetBuilderWith(exceptOperator, branches)
}

// PlaceholderFormat sets the PlaceholderFormat field for any child builders.
func (b StatementBuilderType) PlaceholderFormat(f PlaceholderFormat) StatementBuilderType {
	return builder.Set(b, "PlaceholderFormat", f).(StatementBuilderType)
}

// RunWith sets the RunWith field for any child builders.
func (b StatementBuilderType) RunWith(runner BaseRunner) StatementBuilderType {
	return setRunWith(b, runner).(StatementBuilderType)
}

// Where adds WHERE expressions to the query.
//
// See SelectBuilder.Where for more information.
func (b StatementBuilderType) Where(pred interface{}, args ...interface{}) StatementBuilderType {
	return builder.Append(b, "WhereParts", newWherePart(pred, args...)).(StatementBuilderType)
}

// StatementBuilder is a parent builder for other builders, e.g. SelectBuilder.
var StatementBuilder = StatementBuilderType(builder.EmptyBuilder).PlaceholderFormat(Question)

// Select returns a new SelectBuilder, optionally setting some result columns.
//
// See SelectBuilder.Columns.
func Select(columns ...string) SelectBuilder {
	return StatementBuilder.Select(columns...)
}

// Insert returns a new InsertBuilder with the given table name.
//
// See InsertBuilder.Into.
func Insert(into string) InsertBuilder {
	return StatementBuilder.Insert(into)
}

// Replace returns a new InsertBuilder with the statement keyword set to
// "REPLACE" and with the given table name.
//
// See InsertBuilder.Into.
func Replace(into string) InsertBuilder {
	return StatementBuilder.Replace(into)
}

// Update returns a new UpdateBuilder with the given table name.
//
// See UpdateBuilder.Table.
func Update(table string) UpdateBuilder {
	return StatementBuilder.Update(table)
}

// Delete returns a new DeleteBuilder with the given table name.
//
// See DeleteBuilder.Table.
func Delete(from string) DeleteBuilder {
	return StatementBuilder.Delete(from)
}

// Case returns a new CaseBuilder
// "what" represents case value
func Case(what ...interface{}) CaseBuilder {
	b := CaseBuilder(builder.EmptyBuilder)

	switch len(what) {
	case 0:
	case 1:
		b = b.what(what[0])
	default:
		b = b.what(newPart(what[0], what[1:]...))

	}
	return b
}
