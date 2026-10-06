package squirrel

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/lann/builder"
)

// cte is the internal representation of a single common table expression.
type cte struct {
	Name           string
	Columns        []string
	Anchor         Sqlizer
	RecursiveTerms []cteRecursiveTerm
}

// cteRecursiveTerm is one recursive member of a recursive cte.
type cteRecursiveTerm struct {
	Query    Sqlizer
	UnionAll bool
}

// CTEBuilder builds a single common table expression for use with With.
type CTEBuilder builder.Builder

func init() {
	builder.Register(CTEBuilder{}, cte{})
}

// CTE starts a common table expression with the given name and, optionally,
// an explicit list of output columns.
//
// If columns are given and the column count of the body query can be
// determined (e.g. it is a SelectBuilder), a mismatch is reported when the
// SQL is generated. Bodies built from raw SQL fragments cannot be inspected,
// so their column counts are not validated.
func CTE(name string, columns ...string) CTEBuilder {
	b := CTEBuilder(builder.EmptyBuilder)
	b = builder.Set(b, "Name", name).(CTEBuilder)
	if len(columns) > 0 {
		b = builder.Set(b, "Columns", columns).(CTEBuilder)
	}
	return b
}

// As sets the body of a non-recursive common table expression.
func (b CTEBuilder) As(query Sqlizer) CTEBuilder {
	return builder.Set(b, "Anchor", query).(CTEBuilder)
}

// Anchor sets the anchor (non-recursive) member of a recursive common table
// expression.
func (b CTEBuilder) Anchor(query Sqlizer) CTEBuilder {
	return builder.Set(b, "Anchor", query).(CTEBuilder)
}

// RecursiveTerm appends a recursive member joined to the previous members
// with UNION ALL. The anchor member must be set with Anchor.
func (b CTEBuilder) RecursiveTerm(query Sqlizer) CTEBuilder {
	return builder.Append(b, "RecursiveTerms", cteRecursiveTerm{Query: query, UnionAll: true}).(CTEBuilder)
}

// RecursiveTermUnion appends a recursive member joined to the previous
// members with UNION (duplicates eliminated).
func (b CTEBuilder) RecursiveTermUnion(query Sqlizer) CTEBuilder {
	return builder.Append(b, "RecursiveTerms", cteRecursiveTerm{Query: query}).(CTEBuilder)
}

func getCTE(b CTEBuilder) cte {
	return builder.GetStruct(b).(cte)
}

// bodySQL renders the cte body, combining the anchor and any recursive
// terms, and validates column counts where they can be determined.
func (c cte) bodySQL() (string, []interface{}, error) {
	if c.Anchor == nil {
		if len(c.RecursiveTerms) > 0 {
			return "", nil, fmt.Errorf("cte %q has recursive terms but no anchor member", c.Name)
		}
		return "", nil, fmt.Errorf("cte %q has no query", c.Name)
	}

	anchorSQL, args, err := nestedToSql(c.Anchor)
	if err != nil {
		return "", nil, err
	}
	sql := anchorSQL

	declared := len(c.Columns)
	anchorCount, anchorOK := queryColumnCount(c.Anchor)
	if declared > 0 && anchorOK && anchorCount != declared {
		return "", nil, fmt.Errorf("cte %q declares %d columns but its anchor query produces %d",
			c.Name, declared, anchorCount)
	}

	for i, term := range c.RecursiveTerms {
		termSQL, termArgs, err := nestedToSql(term.Query)
		if err != nil {
			return "", nil, err
		}
		if termCount, ok := queryColumnCount(term.Query); ok {
			if declared > 0 && termCount != declared {
				return "", nil, fmt.Errorf("cte %q declares %d columns but recursive term %d produces %d",
					c.Name, declared, i+1, termCount)
			}
			if anchorOK && termCount != anchorCount {
				return "", nil, fmt.Errorf("cte %q recursive term %d produces %d columns but the anchor produces %d",
					c.Name, i+1, termCount, anchorCount)
			}
		}
		if term.UnionAll {
			sql += " UNION ALL "
		} else {
			sql += " UNION "
		}
		sql += termSQL
		args = append(args, termArgs...)
	}
	return sql, args, nil
}

// innerCTENames returns the names of ctes defined directly by q when q is a
// statement carrying its own WITH clause. Those names shadow same-named ctes
// of enclosing queries.
func (c cte) innerCTENames() []string {
	var names []string
	queries := []Sqlizer{c.Anchor}
	for _, term := range c.RecursiveTerms {
		queries = append(queries, term.Query)
	}
	for _, q := range queries {
		names = append(names, nestedCTENames(q)...)
	}
	return names
}

func nestedCTENames(q Sqlizer) []string {
	var ctes []cte
	switch b := q.(type) {
	case SelectBuilder:
		ctes = builder.GetStruct(b).(selectData).CTEs
	case InsertBuilder:
		ctes = builder.GetStruct(b).(insertData).CTEs
	case UpdateBuilder:
		ctes = builder.GetStruct(b).(updateData).CTEs
	case DeleteBuilder:
		ctes = builder.GetStruct(b).(deleteData).CTEs
	default:
		return nil
	}
	names := make([]string, 0, len(ctes))
	for _, c := range ctes {
		names = append(names, c.Name)
	}
	return names
}

// queryColumnCount reports the number of result columns of q when it can be
// determined from the builder state. Raw SQL fragments, "*" columns and
// non-SELECT bodies are reported as unknown.
func queryColumnCount(q Sqlizer) (int, bool) {
	sb, ok := q.(SelectBuilder)
	if !ok {
		return 0, false
	}
	d := builder.GetStruct(sb).(selectData)
	if len(d.Columns) == 0 {
		return 0, false
	}
	for _, col := range d.Columns {
		colSQL, _, err := nestedToSql(col)
		if err != nil {
			return 0, false
		}
		if strings.ContainsAny(colSQL, "*,") {
			return 0, false
		}
	}
	return len(d.Columns), true
}

func sqlReferencesName(sql, name string) bool {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`)
	return re.MatchString(sql)
}

// appendCTEsSQL renders a WITH clause for ctes into buf, including a
// trailing space. Names are resolved per query scope: a cte may reference
// ctes defined earlier in the same clause and names from enclosing queries,
// while nested WITH clauses may shadow outer names. Duplicate names, illegal
// forward references and non-recursive self references are reported as
// errors.
func appendCTEsSQL(buf *bytes.Buffer, args []interface{}, ctes []cte, recursive bool) ([]interface{}, error) {
	if len(ctes) == 0 {
		return args, nil
	}

	bodies := make([]string, len(ctes))
	bodyArgs := make([][]interface{}, len(ctes))
	anyRecursive := false

	for i := range ctes {
		c := &ctes[i]
		if c.Name == "" {
			return nil, fmt.Errorf("cte %d has no name", i+1)
		}
		for j := 0; j < i; j++ {
			if strings.EqualFold(ctes[j].Name, c.Name) {
				return nil, fmt.Errorf("duplicate cte name %q", c.Name)
			}
		}
		body, bArgs, err := c.bodySQL()
		if err != nil {
			return nil, err
		}
		bodies[i] = body
		bodyArgs[i] = bArgs
		if len(c.RecursiveTerms) > 0 {
			anyRecursive = true
		}
	}

	for i := range ctes {
		c := &ctes[i]
		shadowed := map[string]bool{}
		for _, name := range c.innerCTENames() {
			shadowed[strings.ToLower(name)] = true
		}
		for j := i; j < len(ctes); j++ {
			name := ctes[j].Name
			if shadowed[strings.ToLower(name)] {
				continue
			}
			if !sqlReferencesName(bodies[i], name) {
				continue
			}
			if j == i {
				if len(c.RecursiveTerms) == 0 {
					return nil, fmt.Errorf("cte %q references itself but has no recursive terms", name)
				}
				continue
			}
			return nil, fmt.Errorf("cte %q references cte %q before it is defined", c.Name, name)
		}
	}

	buf.WriteString("WITH ")
	if recursive || anyRecursive {
		buf.WriteString("RECURSIVE ")
	}
	for i := range ctes {
		if i > 0 {
			buf.WriteString(", ")
		}
		c := &ctes[i]
		buf.WriteString(c.Name)
		if len(c.Columns) > 0 {
			buf.WriteString(" (")
			buf.WriteString(strings.Join(c.Columns, ", "))
			buf.WriteString(")")
		}
		buf.WriteString(" AS (")
		buf.WriteString(bodies[i])
		buf.WriteString(")")
		args = append(args, bodyArgs[i]...)
	}
	buf.WriteString(" ")
	return args, nil
}

// withCTEs appends ctes to the CTEs field of a statement builder.
func withCTEs(b interface{}, ctes []CTEBuilder) interface{} {
	for _, c := range ctes {
		b = builder.Append(b, "CTEs", getCTE(c))
	}
	return b
}

// WithBuilder starts a statement with a WITH clause. Use Select, InsertInto,
// Update or DeleteFrom to choose the statement that consumes the ctes.
type WithBuilder struct {
	ctes      []CTEBuilder
	recursive bool
}

// With starts a WITH clause with the given common table expressions.
func With(ctes ...CTEBuilder) WithBuilder {
	return WithBuilder{ctes: ctes}
}

// Recursive marks the WITH clause as WITH RECURSIVE. This is implied when
// any cte has recursive terms.
func (b WithBuilder) Recursive() WithBuilder {
	b.recursive = true
	return b
}

// Select returns a SelectBuilder consuming the ctes.
func (b WithBuilder) Select(columns ...string) SelectBuilder {
	sb := Select(columns...).With(b.ctes...)
	if b.recursive {
		sb = sb.WithRecursive()
	}
	return sb
}

// InsertInto returns an InsertBuilder consuming the ctes.
func (b WithBuilder) InsertInto(table string) InsertBuilder {
	ib := Insert(table).With(b.ctes...)
	if b.recursive {
		ib = ib.WithRecursive()
	}
	return ib
}

// Update returns an UpdateBuilder consuming the ctes.
func (b WithBuilder) Update(table string) UpdateBuilder {
	ub := Update(table).With(b.ctes...)
	if b.recursive {
		ub = ub.WithRecursive()
	}
	return ub
}

// DeleteFrom returns a DeleteBuilder consuming the ctes.
func (b WithBuilder) DeleteFrom(table string) DeleteBuilder {
	db := Delete(table).With(b.ctes...)
	if b.recursive {
		db = db.WithRecursive()
	}
	return db
}
