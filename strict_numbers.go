package gojq

import (
	"math"
	"reflect"
)

var strictNumberFuncNames = map[string]string{
	"_add":      "+",
	"_subtract": "-",
	"_multiply": "*",
	"_divide":   "/",
	"_modulo":   "%",
	"_negate":   "-",
}

func strictNumberFuncName(name string) string {
	if s, ok := strictNumberFuncNames[name]; ok {
		return s
	}
	if len(name) > 0 && name[0] == '_' {
		return name[1:]
	}
	return name
}

// checkStrictNumber returns an error when v is, or contains, a NaN or an
// infinity. The check is performed at the boundary of arithmetic operations,
// math built-in functions, and custom Go extension functions, so that
// non-finite numbers surface as ordinary query errors instead of being
// silently converted to null or extreme values at the JSON encoding phase.
// Integers (int and *big.Int) are never converted to float64 here, so large
// integers keep their exact values.
func checkStrictNumber(v any, name string) error {
	return checkStrictValue(v, name, nil)
}

func checkStrictValue(v any, name string, visited map[uintptr]struct{}) error {
	switch v := v.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return &nonFiniteNumberError{strictNumberFuncName(name), v}
		}
	case []any:
		if len(v) == 0 {
			return nil
		}
		p := reflect.ValueOf(v).Pointer()
		if p != 0 {
			if visited == nil {
				visited = map[uintptr]struct{}{}
			}
			if _, ok := visited[p]; ok {
				return nil
			}
			visited[p] = struct{}{}
		}
		for _, x := range v {
			if err := checkStrictValue(x, name, visited); err != nil {
				return err
			}
		}
	case map[string]any:
		if len(v) == 0 {
			return nil
		}
		p := reflect.ValueOf(v).Pointer()
		if p != 0 {
			if visited == nil {
				visited = map[uintptr]struct{}{}
			}
			if _, ok := visited[p]; ok {
				return nil
			}
			visited[p] = struct{}{}
		}
		for _, x := range v {
			if err := checkStrictValue(x, name, visited); err != nil {
				return err
			}
		}
	}
	return nil
}

// strictIter wraps an iterator returned by a custom Go extension function and
// checks every emitted value, including values nested in arrays and objects.
type strictIter struct {
	iter Iter
	name string
}

func (iter *strictIter) Next() (any, bool) {
	v, ok := iter.iter.Next()
	if !ok {
		return nil, false
	}
	if err, ok := v.(error); ok {
		return err, true
	}
	if err := checkStrictNumber(v, iter.name); err != nil {
		return err, true
	}
	return v, true
}
