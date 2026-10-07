package reduce

import (
	"database/sql/driver"
	"reflect"

	"github.com/doug-martin/goqu/v9/exp"
)

// isPure reports whether dropping e could not change any Go-visible
// side effect of generating SQL: e must not contain Literal SQL, a
// driver.Valuer argument, an undeclared function, a subquery, or another
// opaque node that the SQL generator evaluates.
//
// Purity only gates operand elimination. Columns and reducer constants are
// pure. The reducer never calls Value() itself.
func (r *Reducer) isPure(e exp.Expression) bool {
	switch t := e.(type) {
	case nil:
		return true
	case exp.IdentifierExpression:
		return true
	case exp.LiteralExpression:
		// Reducer constants render a fixed literal; caller-authored literals
		// may embed arbitrary expressions and args.
		if _, ok := exp.ConstKindOf(t); ok {
			return true
		}
		return false
	case exp.BooleanExpression:
		return r.isPure(t.LHS()) && r.isPureValue(t.RHS())
	case exp.BitwiseExpression:
		if t.LHS() != nil && !r.isPure(t.LHS()) {
			return false
		}
		return r.isPureValue(t.RHS())
	case exp.RangeExpression:
		rv := t.RHS()
		return r.isPure(t.LHS()) && r.isPureValue(rv.Start()) && r.isPureValue(rv.End())
	case exp.ExpressionList:
		for _, child := range t.Expressions() {
			if !r.isPure(child) {
				return false
			}
		}
		return true
	case exp.SQLFunctionExpression:
		if !r.isPureFunc(t.Name()) {
			return false
		}
		for _, arg := range t.Args() {
			if !r.isPureValue(arg) {
				return false
			}
		}
		return true
	case exp.SQLWindowFunctionExpression:
		return false
	case exp.CastExpression:
		return r.isPure(t.Casted())
	case exp.AliasedExpression:
		return r.isPure(t.Aliased())
	case exp.ColumnListExpression:
		for _, col := range t.Columns() {
			if !r.isPure(col) {
				return false
			}
		}
		return true
	}
	return false
}

func (r *Reducer) isPureValue(val interface{}) bool {
	if val == nil {
		return true
	}
	if e, ok := val.(exp.Expression); ok {
		return r.isPure(e)
	}
	if isValuer(val) {
		return false
	}
	rv := reflect.ValueOf(val)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.IsNil() {
			return true
		}
		for i := 0; i < rv.Len(); i++ {
			if !r.isPureValue(rv.Index(i).Interface()) {
				return false
			}
		}
		return true
	case reflect.Ptr:
		if rv.IsNil() {
			return true
		}
		return r.isPureValue(rv.Elem().Interface())
	}
	// Plain literals (numbers, strings, bools, time.Time, []byte, ...) are
	// encoded without invoking user code.
	return true
}

func (r *Reducer) isPureFunc(name string) bool {
	_, ok := r.pureFuncs[normalizeName(name)]
	return ok
}

func isValuer(val interface{}) bool {
	if _, ok := val.(driver.Valuer); ok {
		return true
	}
	rv := reflect.ValueOf(val)
	return rv.Kind() == reflect.Ptr && rv.Type().Implements(valuerType)
}

var valuerType = reflect.TypeOf((*driver.Valuer)(nil)).Elem()
