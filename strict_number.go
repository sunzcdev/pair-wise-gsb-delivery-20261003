package gojq

import "math"

// strictNumberCheck validates the value returned by an internal function in
// strict number mode. It returns an error when the value is a non-finite
// number (NaN or infinities), including values nested in arrays or objects.
// Values of int, *big.Int, and json.Number types are passed through as-is,
// so arbitrary-precision integers keep their exactness.
func strictNumberCheck(source string, v any) any {
	if err := checkNonFiniteNumber(source, v, nil); err != nil {
		return err
	}
	return v
}

func checkNonFiniteNumber(source string, v any, path []any) error {
	switch v := v.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			p := append([]any(nil), path...)
			return &nonFiniteNumberError{source: source, value: v, path: p}
		}
	case []any:
		for i, x := range v {
			if err := checkNonFiniteNumber(source, x, appendPath(path, i)); err != nil {
				return err
			}
		}
	case map[string]any:
		for k, x := range v {
			if err := checkNonFiniteNumber(source, x, appendPath(path, k)); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendPath(path []any, p any) []any {
	q := make([]any, 0, len(path)+1)
	q = append(q, path...)
	return append(q, p)
}

// strictIter wraps an iterator returned by an internal function in strict
// number mode, validating every emitted value at the function boundary.
type strictIter struct {
	iter   Iter
	source string
}

func (iter *strictIter) Next() (any, bool) {
	v, ok := iter.iter.Next()
	if !ok {
		return nil, false
	}
	if e, ok := v.(error); ok {
		return e, true
	}
	if err := checkNonFiniteNumber(iter.source, v, nil); err != nil {
		return err, true
	}
	return v, true
}
