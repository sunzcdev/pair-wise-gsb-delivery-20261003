package buntdb

import (
	"errors"
	"strings"

	"github.com/tidwall/gjson"
)

// JSON type names accepted by JSONField.TypeOrder.
const (
	// JSONMissing represents a JSON path that does not exist in a document.
	// It is a virtual type and never matches an actual JSON literal such as
	// an explicit "null".
	JSONMissing = "missing"
	JSONNull    = "null"
	JSONFalse   = "false"
	JSONTrue    = "true"
	JSONNumber  = "number"
	JSONString  = "string"
	// JSONValue covers both objects and arrays, matching gjson's "JSON" type.
	JSONValue = "json"
)

// DefaultJSONTypeOrder is the type precedence used by IndexJSONFields when a
// field does not declare an explicit TypeOrder. Documents whose indexed paths
// resolve to different JSON types are ordered by type first; values of the
// same type are then compared by value.
//
// The fixed order is:
//
//	missing < null < false < true < number < string < object/array
var DefaultJSONTypeOrder = []string{
	JSONMissing,
	JSONNull,
	JSONFalse,
	JSONTrue,
	JSONNumber,
	JSONString,
	JSONValue,
}

// JSONField describes a single component of a structured, compound JSON
// index. Fields are compared in declaration order. The first field that
// differs decides the ordering; later fields are only consulted as
// tie-breakers.
type JSONField struct {
	// Path is a gjson path such as "category" or "name.last".
	Path string
	// Desc reverses the ordering of this field.
	Desc bool
	// CaseSensitive switches string comparisons from the default
	// case-insensitive comparison to a case-sensitive one. It has no effect
	// on non-string values.
	CaseSensitive bool
	// TypeOrder optionally overrides DefaultJSONTypeOrder for this field.
	// List type names in ascending precedence, e.g.
	//
	//	[]string{JSONNull, JSONNumber, JSONString}
	//
	// Types that are not listed sort after every listed type. An empty list
	// falls back to DefaultJSONTypeOrder.
	TypeOrder []string
}

// IndexJSONFields builds a compound index comparator over one or more
// structured JSON fields. Unlike IndexJSON, numbers are compared with exact
// decimal precision instead of being converted to float64, so integers beyond
// 2^53 retain their ordering, and scientific notation and negative numbers are
// compared by their mathematical value.
//
// Every field applies a fixed ordering across missing paths, null, booleans,
// numbers, strings and composite JSON values. The type precedence can be
// customized per field with JSONField.TypeOrder.
func IndexJSONFields(fields ...JSONField) func(a, b string) bool {
	comparators := make([]func(a, b string) bool, len(fields))
	for i := range fields {
		comparators[i] = jsonFieldLess(fields[i])
	}
	return func(a, b string) bool {
		last := len(comparators) - 1
		for i := 0; i < last; i++ {
			if comparators[i](a, b) {
				return true
			}
			if comparators[i](b, a) {
				return false
			}
		}
		if last < 0 {
			return false
		}
		return comparators[last](a, b)
	}
}

// IndexJSONFieldExact returns a comparator for a single JSON path with exact
// numeric precision and the default type order. Strings are compared
// case-sensitively. It is a convenience wrapper around IndexJSONFields.
func IndexJSONFieldExact(path string) func(a, b string) bool {
	return IndexJSONFields(JSONField{Path: path, CaseSensitive: true})
}

// jsonTypeRank maps a resolved gjson Result to its rank under the configured
// type order. Ranks start at zero and increase with precedence. The "missing"
// virtual type is reported when the path does not exist.
func jsonTypeRank(res gjson.Result, order map[string]int, fallback int) int {
	if !res.Exists() {
		if rank, ok := order[JSONMissing]; ok {
			return rank
		}
		return fallback
	}
	name := jsonTypeName(res.Type)
	if rank, ok := order[name]; ok {
		return rank
	}
	return fallback
}

func jsonTypeName(typ gjson.Type) string {
	switch typ {
	case gjson.Null:
		return JSONNull
	case gjson.False:
		return JSONFalse
	case gjson.True:
		return JSONTrue
	case gjson.Number:
		return JSONNumber
	case gjson.String:
		return JSONString
	default:
		return JSONValue
	}
}

func jsonFieldLess(field JSONField) func(a, b string) bool {
	order := make(map[string]int)
	src := field.TypeOrder
	if len(src) == 0 {
		src = DefaultJSONTypeOrder
	}
	used := make(map[string]bool)
	for _, name := range src {
		if !validJSONTypeName(name) || used[name] {
			continue
		}
		used[name] = true
		order[name] = len(order)
	}
	// Unlisted types sort after every listed type.
	fallbackRank := len(order)
	less := func(a, b string) bool {
		ra := gjson.Get(a, field.Path)
		rb := gjson.Get(b, field.Path)
		rankA := jsonTypeRank(ra, order, fallbackRank)
		rankB := jsonTypeRank(rb, order, fallbackRank)
		if rankA != rankB {
			return rankA < rankB
		}
		return jsonValueLess(ra, rb, field.CaseSensitive)
	}
	if field.Desc {
		return func(a, b string) bool { return less(b, a) }
	}
	return less
}

func validJSONTypeName(name string) bool {
	switch name {
	case JSONMissing, JSONNull, JSONFalse, JSONTrue, JSONNumber,
		JSONString, JSONValue:
		return true
	}
	return false
}

// jsonValueLess compares two results that share the same type rank.
func jsonValueLess(a, b gjson.Result, caseSensitive bool) bool {
	switch a.Type {
	case gjson.Number:
		return jsonNumberLess(a.Raw, b.Raw)
	case gjson.String:
		if caseSensitive {
			return a.Str < b.Str
		}
		return jsonStringLessInsensitive(a.Str, b.Str)
	default:
		// Null and each boolean rank hold a single value, so reaching here
		// means both are equal; objects and arrays fall back to raw lexical
		// order.
		return a.Raw < b.Raw
	}
}

func jsonStringLessInsensitive(a, b string) bool {
	la := strings.ToLower(a)
	lb := strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}

// jsonDecimal is an exact sign/magnitude representation of a JSON number.
// digits holds the significant coefficient digits with no leading or trailing
// zeros; decimalPoint is the number of coefficient digits that lie before the
// decimal point after applying the exponent (it may be negative or exceed
// len(digits)). The numeric value is:
//
//	sign * 0.digits * 10^decimalPoint
type jsonDecimal struct {
	negative     bool
	digits       []byte
	decimalPoint int
}

// parseJSONDecimal parses any number accepted by the JSON grammar, including
// scientific notation such as 1e309, without converting to float64.
func parseJSONDecimal(raw string) jsonDecimal {
	s := raw
	neg := false
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	mantissa := s
	exponent := ""
	if i := strings.IndexAny(mantissa, "eE"); i >= 0 {
		exponent = mantissa[i+1:]
		mantissa = mantissa[:i]
	}
	intPart := mantissa
	fracPart := ""
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		intPart = mantissa[:i]
		fracPart = mantissa[i+1:]
	}
	digits := make([]byte, 0, len(intPart)+len(fracPart))
	digits = append(digits, intPart...)
	digits = append(digits, fracPart...)
	point := len(intPart)
	if exponent != "" {
		if n, err := parseExponent(exponent); err == nil {
			point += n
		}
	}
	// Strip leading zeros while keeping the decimal point aligned.
	start := 0
	for start < len(digits)-1 && digits[start] == '0' {
		start++
		point--
	}
	digits = digits[start:]
	// Strip trailing zeros.
	end := len(digits)
	for end > 1 && digits[end-1] == '0' {
		end--
	}
	digits = digits[:end]
	if len(digits) == 1 && digits[0] == '0' {
		neg = false
	}
	return jsonDecimal{negative: neg, digits: digits, decimalPoint: point}
}

var errInvalidJSONNumber = errors.New("invalid JSON number exponent")

// parseExponent converts an exponent body such as "-3" or "+12" to an int,
// saturating at a large bound so arbitrary-length exponents cannot overflow.
func parseExponent(s string) (int, error) {
	neg := false
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, errInvalidJSONNumber
	}
	const cap = 1 << 30
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errInvalidJSONNumber
		}
		if n > (cap-int(s[i]-'0'))/10 {
			n = cap
			continue
		}
		n = n*10 + int(s[i]-'0')
	}
	if neg {
		return -n, nil
	}
	return n, nil
}

// absCmp compares the magnitudes of two decimals without float conversion.
func (d jsonDecimal) absCmp(o jsonDecimal) int {
	if d.decimalPoint != o.decimalPoint {
		if d.decimalPoint < o.decimalPoint {
			return -1
		}
		return 1
	}
	n := len(d.digits)
	if len(o.digits) > n {
		n = len(o.digits)
	}
	for i := 0; i < n; i++ {
		var ca, cb byte
		if i < len(d.digits) {
			ca = d.digits[i]
		}
		if i < len(o.digits) {
			cb = o.digits[i]
		}
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	return 0
}

func jsonNumberLess(a, b string) bool {
	da := parseJSONDecimal(a)
	db := parseJSONDecimal(b)
	if da.negative != db.negative {
		return da.negative
	}
	cmp := da.absCmp(db)
	if da.negative {
		return cmp > 0
	}
	return cmp < 0
}
