package squirrel

import (
	"fmt"
	"io"
	"strings"

	"github.com/lann/builder"
)

type cteData struct {
	Name      string
	Columns   []string
	Recursive bool
	Body      Sqlizer
}

// CTEBuilder builds a single common table expression (CTE) definition for use
// in a WITH clause. CTEs are attached to a statement with the With method of
// SelectBuilder, InsertBuilder, UpdateBuilder or DeleteBuilder.
type CTEBuilder builder.Builder

func init() {
	builder.Register(CTEBuilder{}, cteData{})
}

// CTE returns a new CTEBuilder for a common table expression with the given
// name and, optionally, an explicit list of output columns.
//
// When explicit columns are given and the CTE body is a SelectBuilder, the
// declared column count is checked against the body's result columns when the
// SQL is generated. Bodies built from raw SQL fragments cannot be verified
// statically and are accepted as-is.
func CTE(name string, columns ...string) CTEBuilder {
	b := builder.Set(CTEBuilder(builder.EmptyBuilder), "Name", name).(CTEBuilder)
	if len(columns) > 0 {
		b = builder.Set(b, "Columns", columns).(CTEBuilder)
	}
	return b
}

// Columns adds explicit output columns to the CTE definition.
func (b CTEBuilder) Columns(columns ...string) CTEBuilder {
	return builder.Extend(b, "Columns", columns).(CTEBuilder)
}

// Recursive marks the CTE as recursive, allowing its body to reference the
// CTE itself. If any CTE in a WITH clause is recursive, the clause is
// generated as WITH RECURSIVE.
func (b CTEBuilder) Recursive() CTEBuilder {
	return builder.Set(b, "Recursive", true).(CTEBuilder)
}

// As sets the query that produces the CTE's rows. It accepts a SelectBuilder
// or a raw SQL fragment with optional placeholder args.
func (b CTEBuilder) As(pred interface{}, args ...interface{}) CTEBuilder {
	var body Sqlizer
	switch p := pred.(type) {
	case SelectBuilder:
		// Prevent misnumbered parameters in nested selects (#183).
		body = p.PlaceholderFormat(Question)
	case Sqlizer:
		body = p
	case string:
		body = Expr(p, args...)
	default:
		body = newPart(pred, args...)
	}
	return builder.Set(b, "Body", body).(CTEBuilder)
}

func cteName(c CTEBuilder) string {
	return builder.GetStruct(c).(cteData).Name
}

// selectColumnCount returns the number of result columns of a SelectBuilder.
// The second return value is false if the count cannot be determined
// statically, e.g. a column is a raw fragment containing a comma or a
// wildcard; in that case no column count validation can be performed.
func selectColumnCount(b SelectBuilder) (int, bool) {
	data := builder.GetStruct(b).(selectData)
	if len(data.Columns) == 0 {
		return 0, false
	}
	count := 0
	for _, col := range data.Columns {
		p, ok := col.(*part)
		if !ok {
			return 0, false
		}
		switch pred := p.pred.(type) {
		case string:
			if strings.ContainsAny(pred, ",*") {
				return 0, false
			}
		case nil:
			return 0, false
		}
		count++
	}
	return count, true
}

// sqlIdentifiers returns the identifier-like tokens appearing in a SQL
// fragment. Single-quoted string literals are ignored.
func sqlIdentifiers(sql string) map[string]struct{} {
	ids := map[string]struct{}{}
	start := -1
	flush := func(end int) {
		if start >= 0 {
			ids[sql[start:end]] = struct{}{}
			start = -1
		}
	}
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case c == '\'':
			flush(i)
			for i++; i < len(sql); i++ {
				if sql[i] == '\'' {
					if i+1 < len(sql) && sql[i+1] == '\'' {
						i++
						continue
					}
					break
				}
			}
		case c == '_' || c == '$' ||
			c >= 'a' && c <= 'z' ||
			c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9':
			if start < 0 {
				start = i
			}
		default:
			flush(i)
		}
	}
	flush(len(sql))
	return ids
}

// appendCTEs validates a WITH clause and writes it to w, appending any args
// bound by the CTE bodies.
//
// Names are resolved against the CTEs of the current statement only; a
// reference that matches no CTE in the current scope is assumed to resolve in
// an outer query scope or to a regular table. A CTE defined by a nested
// statement shadows CTEs of outer scopes within that statement.
func appendCTEs(ctes []CTEBuilder, w io.Writer, args []interface{}) ([]interface{}, error) {
	datas := make([]cteData, len(ctes))
	bodySQLs := make([]string, len(ctes))
	bodyArgs := make([][]interface{}, len(ctes))
	positions := make(map[string]int, len(ctes))
	recursive := false

	for i, c := range ctes {
		d := builder.GetStruct(c).(cteData)
		if d.Name == "" {
			return nil, fmt.Errorf("cte at position %d is missing a name", i)
		}
		if _, ok := positions[d.Name]; ok {
			return nil, fmt.Errorf("cte %q is defined more than once in the same WITH clause", d.Name)
		}
		if d.Body == nil {
			return nil, fmt.Errorf("cte %q has no query; call As to set its body", d.Name)
		}
		positions[d.Name] = i
		recursive = recursive || d.Recursive
		datas[i] = d

		bodySQL, bArgs, err := nestedToSql(d.Body)
		if err != nil {
			return nil, fmt.Errorf("cte %q: %v", d.Name, err)
		}
		bodySQLs[i] = bodySQL
		bodyArgs[i] = bArgs
	}

	for i, d := range datas {
		refs := sqlIdentifiers(bodySQLs[i])
		// Names defined by the body's own WITH clause shadow this scope.
		if sb, ok := d.Body.(SelectBuilder); ok {
			for _, inner := range builder.GetStruct(sb).(selectData).CTEs {
				delete(refs, cteName(inner))
			}
		}
		for ref := range refs {
			j, ok := positions[ref]
			if !ok {
				// Resolves in an outer scope or names a regular table.
				continue
			}
			switch {
			case j == i && !d.Recursive:
				return nil, fmt.Errorf("cte %q references itself but is not marked Recursive", d.Name)
			case j > i:
				return nil, fmt.Errorf("cte %q references %q before it is defined", d.Name, ref)
			}
		}
		if len(d.Columns) > 0 {
			if sb, ok := d.Body.(SelectBuilder); ok {
				if n, ok := selectColumnCount(sb); ok && n != len(d.Columns) {
					return nil, fmt.Errorf("cte %q declares %d columns but its query produces %d",
						d.Name, len(d.Columns), n)
				}
			}
		}
	}

	if _, err := io.WriteString(w, "WITH "); err != nil {
		return nil, err
	}
	if recursive {
		if _, err := io.WriteString(w, "RECURSIVE "); err != nil {
			return nil, err
		}
	}
	for i, d := range datas {
		if i > 0 {
			if _, err := io.WriteString(w, ", "); err != nil {
				return nil, err
			}
		}
		head := d.Name
		if len(d.Columns) > 0 {
			head += " (" + strings.Join(d.Columns, ", ") + ")"
		}
		if _, err := io.WriteString(w, head+" AS ("+bodySQLs[i]+")"); err != nil {
			return nil, err
		}
		args = append(args, bodyArgs[i]...)
	}
	_, err := io.WriteString(w, " ")
	return args, err
}
