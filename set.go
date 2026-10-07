package squirrel

import (
	"bytes"
	"database/sql"
	"fmt"
	"regexp"

	"github.com/lann/builder"
)

// setOperator is one of the SQL set operations supported by SetBuilder.
type setOperator string

const (
	unionOperator     setOperator = "UNION"
	unionAllOperator  setOperator = "UNION ALL"
	intersectOperator setOperator = "INTERSECT"
	exceptOperator    setOperator = "EXCEPT"
)

// lockClauseRe matches row-level locking clauses, which PostgreSQL forbids in
// any query participating in a set operation.
var lockClauseRe = regexp.MustCompile(`(?i)\bFOR\s+(?:UPDATE|SHARE)\b`)

type setData struct {
	PlaceholderFormat PlaceholderFormat
	RunWith           BaseRunner
	Operator          setOperator
	Branches          []Sqlizer
	Prefixes          []Sqlizer
	OrderByParts      []Sqlizer
	Limit             string
	Offset            string
	Suffixes          []Sqlizer
}

func (d setData) Exec() (sql.Result, error) {
	if d.RunWith == nil {
		return nil, RunnerNotSet
	}
	return ExecWith(d.RunWith, d)
}

func (d setData) Query() (*sql.Rows, error) {
	if d.RunWith == nil {
		return nil, RunnerNotSet
	}
	return QueryWith(d.RunWith, d)
}

func (d setData) QueryRow() RowScanner {
	if d.RunWith == nil {
		return &Row{err: RunnerNotSet}
	}
	queryRower, ok := d.RunWith.(QueryRower)
	if !ok {
		return &Row{err: RunnerNotQueryRunner}
	}
	return QueryRowWith(queryRower, d)
}

// ToSql builds the query into a SQL string and bound args.
func (d setData) ToSql() (sqlStr string, args []interface{}, err error) {
	sqlStr, args, err = d.toSqlRaw()
	if err != nil {
		return
	}

	sqlStr, err = d.PlaceholderFormat.ReplacePlaceholders(sqlStr)
	return
}

// toSqlRaw builds the query without finalizing placeholder numbering, so the
// result can be embedded into an outer query.
func (d setData) toSqlRaw() (sqlStr string, args []interface{}, err error) {
	if len(d.Branches) < 2 {
		err = fmt.Errorf("set operations must combine at least two queries, got %d", len(d.Branches))
		return
	}
	if err = d.validate(); err != nil {
		return
	}

	sql := &bytes.Buffer{}

	if len(d.Prefixes) > 0 {
		args, err = appendToSql(d.Prefixes, sql, " ", args)
		if err != nil {
			return
		}
		sql.WriteString(" ")
	}

	for i, branch := range d.Branches {
		if branch == nil {
			err = fmt.Errorf("set operation branch %d is nil", i)
			return
		}

		var partSQL string
		var partArgs []interface{}
		partSQL, partArgs, err = nestedToSql(branch)
		if err != nil {
			return
		}

		if i > 0 {
			sql.WriteString(" ")
			sql.WriteString(string(d.Operator))
			sql.WriteString(" ")
		}

		if setBranchNeedsParens(branch) {
			sql.WriteString("(")
			sql.WriteString(partSQL)
			sql.WriteString(")")
		} else {
			sql.WriteString(partSQL)
		}
		args = append(args, partArgs...)
	}

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

	if len(d.Suffixes) > 0 {
		sql.WriteString(" ")
		args, err = appendToSql(d.Suffixes, sql, " ", args)
		if err != nil {
			return
		}
	}

	sqlStr = sql.String()
	return
}

// validate rejects combinations PostgreSQL does not allow in set operations.
func (d setData) validate() error {
	for _, suffix := range d.Suffixes {
		sql, _, err := suffix.ToSql()
		if err != nil {
			return err
		}
		if lockClauseRe.MatchString(sql) {
			return fmt.Errorf("FOR UPDATE/FOR SHARE is not allowed on a %s query", d.Operator)
		}
	}
	for _, branch := range d.Branches {
		if err := validateSetBranch(branch); err != nil {
			return err
		}
	}
	return nil
}

func validateSetBranch(branch Sqlizer) error {
	switch b := branch.(type) {
	case SelectBuilder:
		data := builder.GetStruct(b).(selectData)
		for _, suffix := range data.Suffixes {
			sql, _, err := suffix.ToSql()
			if err != nil {
				return err
			}
			if lockClauseRe.MatchString(sql) {
				return fmt.Errorf("FOR UPDATE/FOR SHARE is not allowed on a branch of a set operation")
			}
		}
	case SetBuilder:
		data := builder.GetStruct(b).(setData)
		return data.validate()
	}
	return nil
}

// setBranchNeedsParens reports whether a branch must be rendered inside
// parentheses. PostgreSQL requires parentheses around branches carrying their
// own ORDER BY/LIMIT/OFFSET or a leading WITH clause; nested set operations
// are parenthesized to make their grouping explicit instead of relying on the
// left-to-right default associativity.
func setBranchNeedsParens(branch Sqlizer) bool {
	if _, ok := branch.(SetBuilder); ok {
		return true
	}

	selectBranch, ok := branch.(SelectBuilder)
	if !ok {
		return false
	}

	data := builder.GetStruct(selectBranch).(selectData)
	switch {
	case len(data.Prefixes) > 0,
		len(data.OrderByParts) > 0,
		len(data.Limit) > 0,
		len(data.Offset) > 0:
		return true
	}
	return false
}

// SetBuilder builds SQL set operations (UNION, UNION ALL, INTERSECT and
// EXCEPT). The operator applies strictly left to right; pass another
// SetBuilder as a branch to group parts with explicit parentheses.
type SetBuilder builder.Builder

func init() {
	builder.Register(SetBuilder{}, setData{})
}

// PlaceholderFormat sets PlaceholderFormat (e.g. Question or Dollar) for the
// query.
func (b SetBuilder) PlaceholderFormat(f PlaceholderFormat) SetBuilder {
	return builder.Set(b, "PlaceholderFormat", f).(SetBuilder)
}

// RunWith sets a Runner (like database/sql.DB) to be used with e.g. Exec.
func (b SetBuilder) RunWith(runner BaseRunner) SetBuilder {
	return setRunWith(b, runner).(SetBuilder)
}

// Exec builds and Execs the query with the Runner set by RunWith.
func (b SetBuilder) Exec() (sql.Result, error) {
	return builder.GetStruct(b).(setData).Exec()
}

// Query builds and Querys the query with the Runner set by RunWith.
func (b SetBuilder) Query() (*sql.Rows, error) {
	return builder.GetStruct(b).(setData).Query()
}

// QueryRow builds and QueryRows the query with the Runner set by RunWith.
func (b SetBuilder) QueryRow() RowScanner {
	return builder.GetStruct(b).(setData).QueryRow()
}

// Scan is a shortcut for QueryRow().Scan.
func (b SetBuilder) Scan(dest ...interface{}) error {
	return b.QueryRow().Scan(dest...)
}

// ToSql builds the query into a SQL string and bound args.
func (b SetBuilder) ToSql() (string, []interface{}, error) {
	return builder.GetStruct(b).(setData).ToSql()
}

func (b SetBuilder) toSqlRaw() (string, []interface{}, error) {
	return builder.GetStruct(b).(setData).toSqlRaw()
}

// MustSql builds the query into a SQL string and bound args.
// It panics if there are any errors.
func (b SetBuilder) MustSql() (string, []interface{}) {
	sql, args, err := b.ToSql()
	if err != nil {
		panic(err)
	}
	return sql, args
}

// Prefix adds an expression to the beginning of the query.
func (b SetBuilder) Prefix(sql string, args ...interface{}) SetBuilder {
	return b.PrefixExpr(Expr(sql, args...))
}

// PrefixExpr adds an expression to the very beginning of the query.
func (b SetBuilder) PrefixExpr(expr Sqlizer) SetBuilder {
	return builder.Append(b, "Prefixes", expr).(SetBuilder)
}

// OrderByClause adds an ORDER BY clause to the result of the whole set
// operation. To sort a single branch, order that SelectBuilder directly.
func (b SetBuilder) OrderByClause(pred interface{}, args ...interface{}) SetBuilder {
	return builder.Append(b, "OrderByParts", newPart(pred, args...)).(SetBuilder)
}

// OrderBy adds ORDER BY expressions to the result of the whole set
// operation.
func (b SetBuilder) OrderBy(orderBys ...string) SetBuilder {
	for _, orderBy := range orderBys {
		b = b.OrderByClause(orderBy)
	}
	return b
}

// Limit sets a LIMIT clause on the result of the whole set operation.
func (b SetBuilder) Limit(limit uint64) SetBuilder {
	return builder.Set(b, "Limit", fmt.Sprintf("%d", limit)).(SetBuilder)
}

// RemoveLimit removes the LIMIT clause from the set operation.
func (b SetBuilder) RemoveLimit() SetBuilder {
	return builder.Delete(b, "Limit").(SetBuilder)
}

// Offset sets an OFFSET clause on the result of the whole set operation.
func (b SetBuilder) Offset(offset uint64) SetBuilder {
	return builder.Set(b, "Offset", fmt.Sprintf("%d", offset)).(SetBuilder)
}

// RemoveOffset removes the OFFSET clause from the set operation.
func (b SetBuilder) RemoveOffset() SetBuilder {
	return builder.Delete(b, "Offset").(SetBuilder)
}

// Suffix adds an expression to the end of the query.
func (b SetBuilder) Suffix(sql string, args ...interface{}) SetBuilder {
	return b.SuffixExpr(Expr(sql, args...))
}

// SuffixExpr adds an expression to the end of the query.
func (b SetBuilder) SuffixExpr(expr Sqlizer) SetBuilder {
	return builder.Append(b, "Suffixes", expr).(SetBuilder)
}

func newSetBuilder(operator setOperator, branches ...Sqlizer) SetBuilder {
	b := SetBuilder(builder.EmptyBuilder)
	b = builder.Set(b, "PlaceholderFormat", Question).(SetBuilder)
	b = builder.Set(b, "Operator", operator).(SetBuilder)
	b = builder.Extend(b, "Branches", sanitizeSetBranches(branches)).(SetBuilder)
	return b
}

// errSqlizer carries an error through SQL generation. It is used to reject nil
// branches without putting a nil interface into a reflected []Sqlizer, which
// would panic inside github.com/lann/builder.
type errSqlizer struct{ err error }

func (e errSqlizer) ToSql() (string, []interface{}, error) { return "", nil, e.err }

func sanitizeSetBranches(branches []Sqlizer) []Sqlizer {
	out := make([]Sqlizer, len(branches))
	for i, branch := range branches {
		if branch == nil {
			out[i] = errSqlizer{err: fmt.Errorf("set operation branch %d is nil", i)}
		} else {
			out[i] = branch
		}
	}
	return out
}

// Union combines queries with UNION, which removes duplicate rows from the
// result.
//
// The operator associates left to right; nest SetBuilders to force a
// different grouping.
func Union(branches ...Sqlizer) SetBuilder {
	return newSetBuilder(unionOperator, branches...)
}

// UnionAll combines queries with UNION ALL, which preserves duplicate rows.
func UnionAll(branches ...Sqlizer) SetBuilder {
	return newSetBuilder(unionAllOperator, branches...)
}

// Intersect combines queries with INTERSECT, which returns rows present in
// every branch.
func Intersect(branches ...Sqlizer) SetBuilder {
	return newSetBuilder(intersectOperator, branches...)
}

// Except combines queries with EXCEPT, which returns rows present in the
// first branch but not in any later branch.
func Except(branches ...Sqlizer) SetBuilder {
	return newSetBuilder(exceptOperator, branches...)
}
