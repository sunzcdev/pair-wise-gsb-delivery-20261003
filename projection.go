package jsonquery

import (
	"fmt"
	"reflect"

	"github.com/antchfx/xpath"
)

// ValueType is the value type a projection column expects after conversion.
// Only conversions explicitly selected by the caller (see Column.Convert) are
// applied; native values that do not match the declared type are reported
// instead of being implicitly coerced.
type ValueType int

const (
	// TypeAny accepts any JSON value and performs no type checking.
	TypeAny ValueType = iota
	// TypeString matches JSON strings (Go string).
	TypeString
	// TypeNumber matches JSON numbers (Go float64).
	TypeNumber
	// TypeBool matches JSON booleans (Go bool).
	TypeBool
	// TypeObject matches JSON objects (Go map[string]interface{}).
	TypeObject
	// TypeArray matches JSON arrays (Go []interface{}).
	TypeArray
)

// String returns the name of the expected type.
func (t ValueType) String() string {
	switch t {
	case TypeAny:
		return "any"
	case TypeString:
		return "string"
	case TypeNumber:
		return "number"
	case TypeBool:
		return "boolean"
	case TypeObject:
		return "object"
	case TypeArray:
		return "array"
	default:
		return "unknown"
	}
}

// A ConvertRule is an explicitly selected conversion rule applied to a matched
// value before it is checked against the column's expected type. Conversion is
// never implicit: without a rule the native JSON value is used as-is.
type ConvertRule func(interface{}) (interface{}, error)

// AsText renders a scalar value as its textual form, mirroring the text
// produced by an XPath text() node. Explicit JSON null stays null; JSON
// objects and arrays are rejected.
func AsText(v interface{}) (interface{}, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return x, nil
	case float64:
		return fmt.Sprintf("%v", x), nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	default:
		return nil, fmt.Errorf("asText: cannot render %s as text", jsonTypeName(v))
	}
}

// AsNumber accepts native JSON numbers and passes them through unchanged.
// Every other non-null scalar is rejected so that strings and booleans are not
// silently parsed into numbers.
func AsNumber(v interface{}) (interface{}, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case float64:
		return x, nil
	default:
		return nil, fmt.Errorf("asNumber: cannot convert %s to number", jsonTypeName(v))
	}
}

// Column describes one fixed output column of a projection plan.
type Column struct {
	// Name is the key under which the column value is exposed. Column names
	// must be non-empty and unique within a plan.
	Name string
	// XPath is evaluated relative to each selected row. An expression without
	// an explicit axis (for example "name") resolves against the row itself.
	XPath string
	// Required reports the column as mandatory. A row where a required column
	// matches no node gets a diagnostic for that column.
	Required bool
	// Multi allows the column to collect every matched node. When false,
	// matching more than one node is reported as a diagnostic for the row.
	Multi bool
	// Type is the expected value type after conversion. Use TypeAny to accept
	// any value.
	Type ValueType
	// Default is used when an optional column matches no node. It never
	// replaces an explicit JSON null and is rejected for required columns.
	Default interface{}
	// Convert, when non-nil, is the only conversion applied to matched
	// values. A conversion failure is reported as a diagnostic for the row.
	Convert ConvertRule
}

// ProjectionPlan compiles a row-selection XPath expression together with a set
// of relative column expressions. A plan is stateless with respect to any
// document: it holds only compiled expressions and can be reused across
// documents without retaining node pointers from a previous one.
type ProjectionPlan struct {
	rowExpr *xpath.Expr
	columns []compiledColumn
}

type compiledColumn struct {
	column Column
	expr   *xpath.Expr
}

// DiagCode identifies the kind of projection diagnostic.
type DiagCode string

const (
	// DiagMissing means a required column matched no node.
	DiagMissing DiagCode = "missing"
	// DiagMultiple means a single-value column matched more than one node.
	DiagMultiple DiagCode = "multiple"
	// DiagType means a value did not match the column's expected type.
	DiagType DiagCode = "type"
	// DiagConvert means the explicitly selected conversion rule failed.
	DiagConvert DiagCode = "convert"
)

// Diagnostic describes why a column of a projected row could not be produced.
type Diagnostic struct {
	Column  string
	Code    DiagCode
	Message string
}

func (d Diagnostic) Error() string {
	return fmt.Sprintf("column %q: %s: %s", d.Column, d.Code, d.Message)
}

// Cell is one projected column value. Set distinguishes an explicit JSON null
// (Set is true and Value is nil) from a missing value (Set is false).
type Cell struct {
	Name  string
	Value interface{}
	Set   bool
	// Diagnostics holds the failures recorded while producing this cell. A
	// cell with a diagnostic has Value nil and Set false.
	Diagnostics []Diagnostic
}

// Row is the per-row result of applying a projection plan. Cells keep the
// column declaration order. A row that fails any column still carries the
// successfully projected columns alongside its diagnostics.
type Row struct {
	// Node is the row node selected by the plan's row XPath.
	Node *Node
	// Cells holds every declared column in declaration order.
	Cells []Cell
	// Diagnostics aggregates the diagnostics of all failed cells.
	Diagnostics []Diagnostic
}

// Values returns the projected column values keyed by column name. Missing
// cells map to nil; use Cells to distinguish an explicit null from a miss.
func (r *Row) Values() map[string]interface{} {
	values := make(map[string]interface{}, len(r.Cells))
	for _, cell := range r.Cells {
		values[cell.Name] = cell.Value
	}
	return values
}

// Failed reports whether any column of the row produced a diagnostic.
func (r *Row) Failed() bool {
	return len(r.Diagnostics) > 0
}

// CompileProjection validates the plan description and compiles every XPath
// expression. The returned plan can be applied to any number of documents.
func CompileProjection(rowXPath string, columns []Column) (*ProjectionPlan, error) {
	rowExpr, err := getQuery(rowXPath)
	if err != nil {
		return nil, fmt.Errorf("compile row xpath %q: %w", rowXPath, err)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("projection requires at least one column")
	}
	plan := &ProjectionPlan{
		rowExpr: rowExpr,
		columns: make([]compiledColumn, 0, len(columns)),
	}
	names := make(map[string]bool, len(columns))
	for _, col := range columns {
		if col.Name == "" {
			return nil, fmt.Errorf("column at index %d has no name", len(plan.columns))
		}
		if names[col.Name] {
			return nil, fmt.Errorf("duplicate column name %q", col.Name)
		}
		names[col.Name] = true
		if col.XPath == "" {
			return nil, fmt.Errorf("column %q has no xpath", col.Name)
		}
		expr, err := getQuery(col.XPath)
		if err != nil {
			return nil, fmt.Errorf("compile column %q xpath %q: %w", col.Name, col.XPath, err)
		}
		if col.Default != nil {
			if col.Required {
				return nil, fmt.Errorf("column %q is required and cannot declare a default", col.Name)
			}
			def := normalizeDefault(col.Default)
			if !valueMatchesType(def, col.Type) {
				return nil, fmt.Errorf("column %q default has type %s, want %s",
					col.Name, jsonTypeName(def), col.Type)
			}
			col.Default = def
		}
		plan.columns = append(plan.columns, compiledColumn{column: col, expr: expr})
	}
	return plan, nil
}

// Project selects the row nodes from doc using the plan's row XPath and
// evaluates every column relative to each row. A failure on one row never
// stops the remaining rows: each returned Row carries its own diagnostics.
func (p *ProjectionPlan) Project(doc *Node) []*Row {
	rowNodes := QuerySelectorAll(doc, p.rowExpr)
	rows := make([]*Row, 0, len(rowNodes))
	for _, rowNode := range rowNodes {
		rows = append(rows, p.projectRow(rowNode))
	}
	return rows
}

func (p *ProjectionPlan) projectRow(rowNode *Node) *Row {
	row := &Row{
		Node:  rowNode,
		Cells: make([]Cell, 0, len(p.columns)),
	}
	for _, compiled := range p.columns {
		col := compiled.column
		cell := Cell{Name: col.Name}
		nodes := QuerySelectorAll(rowNode, compiled.expr)

		if col.Multi {
			values := make([]interface{}, 0, len(nodes))
			for _, matched := range nodes {
				value, diag := convertAndCheck(col, matched.Value())
				if diag != nil {
					cell.Diagnostics = append(cell.Diagnostics, *diag)
					continue
				}
				values = append(values, value)
			}
			if len(cell.Diagnostics) > 0 {
				row.Diagnostics = append(row.Diagnostics, cell.Diagnostics...)
			} else if len(nodes) == 0 {
				applyMissing(col, &cell)
				if len(cell.Diagnostics) > 0 {
					row.Diagnostics = append(row.Diagnostics, cell.Diagnostics...)
				}
			} else {
				cell.Value, cell.Set = values, true
			}
		} else {
			switch len(nodes) {
			case 0:
				applyMissing(col, &cell)
				if len(cell.Diagnostics) > 0 {
					row.Diagnostics = append(row.Diagnostics, cell.Diagnostics...)
				}
			case 1:
				value, diag := convertAndCheck(col, nodes[0].Value())
				if diag != nil {
					cell.Diagnostics = append(cell.Diagnostics, *diag)
					row.Diagnostics = append(row.Diagnostics, *diag)
				} else {
					cell.Value, cell.Set = value, true
				}
			default:
				diag := Diagnostic{
					Column:  col.Name,
					Code:    DiagMultiple,
					Message: fmt.Sprintf("single-value column matched %d nodes", len(nodes)),
				}
				cell.Diagnostics = append(cell.Diagnostics, diag)
				row.Diagnostics = append(row.Diagnostics, diag)
			}
		}

		row.Cells = append(row.Cells, cell)
	}
	return row
}

func applyMissing(col Column, cell *Cell) {
	if col.Required {
		cell.Diagnostics = append(cell.Diagnostics, Diagnostic{
			Column:  col.Name,
			Code:    DiagMissing,
			Message: "required column matched no node",
		})
		return
	}
	if col.Default != nil {
		cell.Value = col.Default
		cell.Set = true
	}
}

func convertAndCheck(col Column, raw interface{}) (interface{}, *Diagnostic) {
	value := raw
	if col.Convert != nil {
		converted, err := col.Convert(raw)
		if err != nil {
			return nil, &Diagnostic{
				Column:  col.Name,
				Code:    DiagConvert,
				Message: err.Error(),
			}
		}
		value = converted
	}
	if !valueMatchesType(value, col.Type) {
		return nil, &Diagnostic{
			Column:  col.Name,
			Code:    DiagType,
			Message: fmt.Sprintf("value has type %s, want %s", jsonTypeName(value), col.Type),
		}
	}
	return value, nil
}

func valueMatchesType(value interface{}, expected ValueType) bool {
	if expected == TypeAny || value == nil {
		return true
	}
	switch expected {
	case TypeString:
		_, ok := value.(string)
		return ok
	case TypeNumber:
		_, ok := value.(float64)
		return ok
	case TypeBool:
		_, ok := value.(bool)
		return ok
	case TypeObject:
		_, ok := value.(map[string]interface{})
		return ok
	case TypeArray:
		_, ok := value.([]interface{})
		return ok
	default:
		return false
	}
}

func jsonTypeName(value interface{}) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	default:
		return reflect.TypeOf(value).String()
	}
}

// normalizeDefault accepts integer default literals and stores them as the
// JSON number type used throughout the package.
func normalizeDefault(value interface{}) interface{} {
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint())
	case reflect.Float32:
		return float64(rv.Float())
	default:
		return value
	}
}
