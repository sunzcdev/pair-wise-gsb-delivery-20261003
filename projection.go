package jsonquery

import (
	"fmt"
	"strconv"

	"github.com/antchfx/xpath"
)

// ColumnType is the expected value type of a projected column.
type ColumnType int

const (
	// AnyType accepts values of any JSON type without conversion.
	AnyType ColumnType = iota
	// StringType expects string values.
	StringType
	// NumberType expects float64 values.
	NumberType
	// BoolType expects bool values.
	BoolType
)

// ConversionRule is a bitmask of explicitly allowed type conversions.
// A value whose type does not match the column type is only converted
// when the matching rule was declared on the column; otherwise the
// row records a diagnostic for that column.
type ConversionRule uint

const (
	// NoConversion disables all implicit type conversions.
	NoConversion ConversionRule = 0
	// NumberToString allows converting float64 values to string.
	NumberToString ConversionRule = 1 << iota
	// BoolToString allows converting bool values to string.
	BoolToString
	// StringToNumber allows parsing string values as float64.
	StringToNumber
)

// Column describes one projected column of a Plan.
type Column struct {
	// Name is the caller-facing column name used in diagnostics.
	Name string
	// XPath is evaluated relative to each row node.
	XPath string
	// Required reports a diagnostic when the column matches no node.
	Required bool
	// Type is the expected value type.
	Type ColumnType
	// Multi allows the column to collect multiple matched nodes into
	// a []interface{}; otherwise more than one match is a diagnostic.
	Multi bool
	// Rules lists the explicitly allowed type conversions.
	Rules ConversionRule
	// Default is used when the column matches no node. It never
	// overrides an explicit JSON null.
	Default    interface{}
	HasDefault bool
}

// Diagnostic describes a problem found while projecting one column of
// one row. A row with diagnostics still carries the values that could
// be projected, and does not affect other rows.
type Diagnostic struct {
	Column  string
	Message string
}

func (d Diagnostic) String() string {
	return fmt.Sprintf("%s: %s", d.Column, d.Message)
}

// Row is the projection result of a single record node. Values are in
// the same order as the columns were declared on the Plan.
type Row struct {
	Values      []interface{}
	Diagnostics []Diagnostic
}

// OK reports whether the row was projected without diagnostics.
func (r Row) OK() bool {
	return len(r.Diagnostics) == 0
}

// Result is the outcome of applying a Plan to a document.
type Result struct {
	Rows []Row
}

type compiledColumn struct {
	Column
	expr *xpath.Expr
}

// A Plan projects every node matched by a row XPath into a fixed set
// of columns. A Plan holds only compiled XPath expressions and never
// caches nodes, so it can be reused across documents.
type Plan struct {
	rowExpr *xpath.Expr
	columns []compiledColumn
}

// NewPlan compiles a projection plan from a row-selection XPath and an
// ordered list of columns whose XPaths are relative to each row node.
func NewPlan(rowXPath string, columns ...Column) (*Plan, error) {
	rowExpr, err := xpath.Compile(rowXPath)
	if err != nil {
		return nil, fmt.Errorf("jsonquery: invalid row XPath %q: %w", rowXPath, err)
	}
	compiled := make([]compiledColumn, len(columns))
	for i, col := range columns {
		expr, err := xpath.Compile(col.XPath)
		if err != nil {
			return nil, fmt.Errorf("jsonquery: invalid XPath for column %q: %w", col.Name, err)
		}
		compiled[i] = compiledColumn{Column: col, expr: expr}
	}
	return &Plan{rowExpr: rowExpr, columns: compiled}, nil
}

// MustPlan is like NewPlan but panics on invalid XPath expressions.
func MustPlan(rowXPath string, columns ...Column) *Plan {
	p, err := NewPlan(rowXPath, columns...)
	if err != nil {
		panic(err)
	}
	return p
}

// Project applies the plan to doc and returns one Row per matched
// record node. A failing row does not prevent other rows from being
// projected.
func (p *Plan) Project(doc *Node) *Result {
	rowNodes := QuerySelectorAll(doc, p.rowExpr)
	result := &Result{Rows: make([]Row, 0, len(rowNodes))}
	for _, rowNode := range rowNodes {
		result.Rows = append(result.Rows, p.projectRow(rowNode))
	}
	return result
}

func (p *Plan) projectRow(rowNode *Node) Row {
	row := Row{Values: make([]interface{}, len(p.columns))}
	for i := range p.columns {
		col := &p.columns[i]
		nodes := QuerySelectorAll(rowNode, col.expr)
		value, diag := col.project(nodes)
		row.Values[i] = value
		row.Diagnostics = append(row.Diagnostics, diag...)
	}
	return row
}

func (c *compiledColumn) project(nodes []*Node) (interface{}, []Diagnostic) {
	if len(nodes) == 0 {
		var diags []Diagnostic
		if c.Required {
			diags = append(diags, Diagnostic{
				Column:  c.Name,
				Message: fmt.Sprintf("required column matched no node with XPath %q", c.XPath),
			})
		}
		if c.HasDefault {
			return c.Default, diags
		}
		if c.Multi {
			return []interface{}{}, diags
		}
		return nil, diags
	}
	if !c.Multi && len(nodes) > 1 {
		return nil, []Diagnostic{{
			Column:  c.Name,
			Message: fmt.Sprintf("single-value column matched %d nodes with XPath %q", len(nodes), c.XPath),
		}}
	}
	if c.Multi {
		values := make([]interface{}, 0, len(nodes))
		var diags []Diagnostic
		for _, n := range nodes {
			v, ok := c.convert(n.Value())
			if !ok {
				diags = append(diags, c.typeDiagnostic(n.Value()))
				continue
			}
			values = append(values, v)
		}
		return values, diags
	}
	v, ok := c.convert(nodes[0].Value())
	if !ok {
		return nil, []Diagnostic{c.typeDiagnostic(nodes[0].Value())}
	}
	return v, nil
}

// convert applies the column's expected type. Explicit JSON nulls pass
// through untouched. Any other conversion requires a matching rule to
// have been declared on the column.
func (c *compiledColumn) convert(value interface{}) (interface{}, bool) {
	if value == nil || c.Type == AnyType {
		return value, true
	}
	switch c.Type {
	case StringType:
		switch v := value.(type) {
		case string:
			return v, true
		case float64:
			if c.Rules&NumberToString != 0 {
				return strconv.FormatFloat(v, 'f', -1, 64), true
			}
		case bool:
			if c.Rules&BoolToString != 0 {
				return strconv.FormatBool(v), true
			}
		}
	case NumberType:
		switch v := value.(type) {
		case float64:
			return v, true
		case string:
			if c.Rules&StringToNumber != 0 {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					return f, true
				}
			}
		}
	case BoolType:
		if v, ok := value.(bool); ok {
			return v, true
		}
	}
	return nil, false
}

func (c *compiledColumn) typeDiagnostic(value interface{}) Diagnostic {
	return Diagnostic{
		Column:  c.Name,
		Message: fmt.Sprintf("value %v of type %T does not match column type %v", value, value, c.Type),
	}
}

func (t ColumnType) String() string {
	switch t {
	case StringType:
		return "string"
	case NumberType:
		return "number"
	case BoolType:
		return "bool"
	default:
		return "any"
	}
}
