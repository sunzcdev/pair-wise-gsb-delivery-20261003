package squirrel

import (
	"bytes"
	"database/sql"
	"fmt"

	"github.com/lann/builder"
)

// SetOperator represents a SELECT set operation.
type SetOperator string

const (
	// UnionOperator represents UNION.
	UnionOperator SetOperator = "UNION"
	// UnionAllOperator represents UNION ALL.
	UnionAllOperator SetOperator = "UNION ALL"
	// IntersectOperator represents INTERSECT.
	IntersectOperator SetOperator = "INTERSECT"
	// IntersectAllOperator represents INTERSECT ALL.
	IntersectAllOperator SetOperator = "INTERSECT ALL"
	// ExceptOperator represents EXCEPT.
	ExceptOperator SetOperator = "EXCEPT"
	// ExceptAllOperator represents EXCEPT ALL.
	ExceptAllOperator SetOperator = "EXCEPT ALL"
)

// SetQuery is a query that can be used as a branch of a set operation.
type SetQuery interface {
	Sqlizer
	isSetQuery()
}

type setData struct {
	PlaceholderFormat PlaceholderFormat
	RunWith           BaseRunner
	Left              SetQuery
	Right             SetQuery
	Operator          SetOperator
	OrderByParts      []Sqlizer
	Limit             string
	Offset            string
}

// SetBuilder builds SQL set operations such as UNION and EXCEPT.
type SetBuilder builder.Builder

func init() {
	builder.Register(SetBuilder{}, setData{})
}

func (SelectBuilder) isSetQuery() {}
func (SetBuilder) isSetQuery()    {}

func (d *setData) Query() (*sql.Rows, error) {
	if d.RunWith == nil {
		return nil, RunnerNotSet
	}
	return QueryWith(d.RunWith, d)
}

func (d *setData) QueryRow() RowScanner {
	if d.RunWith == nil {
		return &Row{err: RunnerNotSet}
	}
	queryRower, ok := d.RunWith.(QueryRower)
	if !ok {
		return &Row{err: RunnerNotQueryRunner}
	}
	return QueryRowWith(queryRower, d)
}

func (d *setData) ToSql() (sqlStr string, args []interface{}, err error) {
	sqlStr, args, err = d.toSqlRaw()
	if err != nil {
		return
	}

	format := d.PlaceholderFormat
	if format == nil {
		format = Question
	}
	sqlStr, err = format.ReplacePlaceholders(sqlStr)
	return
}

func (d *setData) toSqlRaw() (sqlStr string, args []interface{}, err error) {
	if err = d.validate(); err != nil {
		return
	}

	sql := &bytes.Buffer{}
	leftSQL, leftArgs, err := setBranchSql(d.Left)
	if err != nil {
		return "", nil, err
	}
	rightSQL, rightArgs, err := setBranchSql(d.Right)
	if err != nil {
		return "", nil, err
	}

	sql.WriteString(leftSQL)
	sql.WriteString(" ")
	sql.WriteString(string(d.Operator))
	sql.WriteString(" ")
	sql.WriteString(rightSQL)
	args = append(args, leftArgs...)
	args = append(args, rightArgs...)

	if len(d.OrderByParts) > 0 {
		sql.WriteString(" ORDER BY ")
		args, err = appendToSql(d.OrderByParts, sql, ", ", args)
		if err != nil {
			return
		}
	}

	if len(d.Limit) > 0 {
		sql.WriteString(" LIMIT ")
		sql.WriteString(d.Limit)
	}

	if len(d.Offset) > 0 {
		sql.WriteString(" OFFSET ")
		sql.WriteString(d.Offset)
	}

	return sql.String(), args, nil
}

func (d *setData) validate() error {
	if d.Left == nil || d.Right == nil {
		return fmt.Errorf("set queries require two branches")
	}
	switch d.Operator {
	case UnionOperator, UnionAllOperator, IntersectOperator, IntersectAllOperator, ExceptOperator, ExceptAllOperator:
	default:
		return fmt.Errorf("unsupported set operator: %q", d.Operator)
	}
	return nil
}

func setBranchSql(query SetQuery) (string, []interface{}, error) {
	switch query.(type) {
	case SelectBuilder, SetBuilder:
	default:
		return "", nil, fmt.Errorf("unsupported set query %T: use SelectBuilder or SetBuilder", query)
	}

	sql, args, err := nestedToSql(query)
	if err != nil {
		return "", nil, err
	}
	return "(" + sql + ")", args, nil
}

func newSetBuilder(operator SetOperator, left, right SetQuery) SetBuilder {
	b := builder.Set(builder.EmptyBuilder, "Left", left)
	b = builder.Set(b, "Right", right)
	b = builder.Set(b, "Operator", operator)

	var format PlaceholderFormat
	var runner BaseRunner
	switch query := left.(type) {
	case SelectBuilder:
		data := builder.GetStruct(query).(selectData)
		format = data.PlaceholderFormat
		runner = data.RunWith
	case SetBuilder:
		data := builder.GetStruct(query).(setData)
		format = data.PlaceholderFormat
		runner = data.RunWith
	}
	if format != nil {
		b = builder.Set(b, "PlaceholderFormat", format)
	}
	if runner != nil {
		b = builder.Set(b, "RunWith", runner)
	}
	return SetBuilder(b.(builder.Builder))
}

func newStatementSetBuilder(statement StatementBuilderType, operator SetOperator, left, right SetQuery) SetBuilder {
	b := newSetBuilder(operator, left, right)
	data := builder.GetStruct(SetBuilder(statement)).(setData)
	if data.PlaceholderFormat != nil {
		b = b.PlaceholderFormat(data.PlaceholderFormat)
	}
	if data.RunWith != nil {
		b = b.RunWith(data.RunWith)
	}
	return b
}

// Union combines two queries with UNION.
func Union(left, right SetQuery) SetBuilder {
	return newSetBuilder(UnionOperator, left, right)
}

// UnionAll combines two queries with UNION ALL.
func UnionAll(left, right SetQuery) SetBuilder {
	return newSetBuilder(UnionAllOperator, left, right)
}

// Intersect combines two queries with INTERSECT.
func Intersect(left, right SetQuery) SetBuilder {
	return newSetBuilder(IntersectOperator, left, right)
}

// IntersectAll combines two queries with INTERSECT ALL.
func IntersectAll(left, right SetQuery) SetBuilder {
	return newSetBuilder(IntersectAllOperator, left, right)
}

// Except combines two queries with EXCEPT.
func Except(left, right SetQuery) SetBuilder {
	return newSetBuilder(ExceptOperator, left, right)
}

// ExceptAll combines two queries with EXCEPT ALL.
func ExceptAll(left, right SetQuery) SetBuilder {
	return newSetBuilder(ExceptAllOperator, left, right)
}

// PlaceholderFormat sets PlaceholderFormat for the complete set query.
func (b SetBuilder) PlaceholderFormat(f PlaceholderFormat) SetBuilder {
	return builder.Set(b, "PlaceholderFormat", f).(SetBuilder)
}

// RunWith sets a Runner to be used with Query or QueryRow.
func (b SetBuilder) RunWith(runner BaseRunner) SetBuilder {
	return setRunWith(b, runner).(SetBuilder)
}

// Query builds and queries the complete set query.
func (b SetBuilder) Query() (*sql.Rows, error) {
	data := builder.GetStruct(b).(setData)
	return data.Query()
}

// QueryRow builds and queries a single row from the complete set query.
func (b SetBuilder) QueryRow() RowScanner {
	data := builder.GetStruct(b).(setData)
	return data.QueryRow()
}

// Scan is a shortcut for QueryRow().Scan.
func (b SetBuilder) Scan(dest ...interface{}) error {
	return b.QueryRow().Scan(dest...)
}

// ToSql builds the query into a SQL string and bound args.
func (b SetBuilder) ToSql() (string, []interface{}, error) {
	data := builder.GetStruct(b).(setData)
	return data.ToSql()
}

func (b SetBuilder) toSqlRaw() (string, []interface{}, error) {
	data := builder.GetStruct(b).(setData)
	return data.toSqlRaw()
}

// MustSql builds the query into a SQL string and bound args. It panics on error.
func (b SetBuilder) MustSql() (string, []interface{}) {
	sql, args, err := b.ToSql()
	if err != nil {
		panic(err)
	}
	return sql, args
}

// Union combines the set query with another query using UNION.
func (b SetBuilder) Union(right SetQuery) SetBuilder {
	return Union(b, right)
}

// UnionAll combines the set query with another query using UNION ALL.
func (b SetBuilder) UnionAll(right SetQuery) SetBuilder {
	return UnionAll(b, right)
}

// Intersect combines the set query with another query using INTERSECT.
func (b SetBuilder) Intersect(right SetQuery) SetBuilder {
	return Intersect(b, right)
}

// IntersectAll combines the set query with another query using INTERSECT ALL.
func (b SetBuilder) IntersectAll(right SetQuery) SetBuilder {
	return IntersectAll(b, right)
}

// Except combines the set query with another query using EXCEPT.
func (b SetBuilder) Except(right SetQuery) SetBuilder {
	return Except(b, right)
}

// ExceptAll combines the set query with another query using EXCEPT ALL.
func (b SetBuilder) ExceptAll(right SetQuery) SetBuilder {
	return ExceptAll(b, right)
}

// OrderByClause adds an ORDER BY expression to the complete set query.
func (b SetBuilder) OrderByClause(pred interface{}, args ...interface{}) SetBuilder {
	return builder.Append(b, "OrderByParts", newPart(pred, args...)).(SetBuilder)
}

// OrderBy adds ORDER BY expressions to the complete set query.
func (b SetBuilder) OrderBy(orderBys ...string) SetBuilder {
	for _, orderBy := range orderBys {
		b = b.OrderByClause(orderBy)
	}
	return b
}

// Limit sets a LIMIT clause on the complete set query.
func (b SetBuilder) Limit(limit uint64) SetBuilder {
	return builder.Set(b, "Limit", fmt.Sprintf("%d", limit)).(SetBuilder)
}

// RemoveLimit removes the LIMIT clause from the complete set query.
func (b SetBuilder) RemoveLimit() SetBuilder {
	return builder.Delete(b, "Limit").(SetBuilder)
}

// Offset sets an OFFSET clause on the complete set query.
func (b SetBuilder) Offset(offset uint64) SetBuilder {
	return builder.Set(b, "Offset", fmt.Sprintf("%d", offset)).(SetBuilder)
}

// RemoveOffset removes the OFFSET clause from the complete set query.
func (b SetBuilder) RemoveOffset() SetBuilder {
	return builder.Delete(b, "Offset").(SetBuilder)
}

// Union combines the select query with another query using UNION.
func (b SelectBuilder) Union(right SetQuery) SetBuilder {
	return Union(b, right)
}

// UnionAll combines the select query with another query using UNION ALL.
func (b SelectBuilder) UnionAll(right SetQuery) SetBuilder {
	return UnionAll(b, right)
}

// Intersect combines the select query with another query using INTERSECT.
func (b SelectBuilder) Intersect(right SetQuery) SetBuilder {
	return Intersect(b, right)
}

// IntersectAll combines the select query with another query using INTERSECT ALL.
func (b SelectBuilder) IntersectAll(right SetQuery) SetBuilder {
	return IntersectAll(b, right)
}

// Except combines the select query with another query using EXCEPT.
func (b SelectBuilder) Except(right SetQuery) SetBuilder {
	return Except(b, right)
}

// ExceptAll combines the set query with another query using EXCEPT ALL.
func (b SelectBuilder) ExceptAll(right SetQuery) SetBuilder {
	return ExceptAll(b, right)
}

type setAliasExpr struct {
	query interface {
		Sqlizer
		rawSqlizer
	}
	alias string
}

func (e setAliasExpr) ToSql() (sql string, args []interface{}, err error) {
	if len(e.alias) == 0 {
		return "", nil, fmt.Errorf("derived tables require an alias")
	}
	sql, args, err = nestedToSql(e.query)
	if err == nil {
		sql = fmt.Sprintf("(%s) AS %s", sql, e.alias)
	}
	return
}
