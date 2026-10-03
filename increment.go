// Package sjson increment support.
package sjson

import (
	"math/big"
	"unsafe"

	"github.com/tidwall/gjson"
)

// DefaultIncrementMaxLen is the default maximum length, in bytes, of a
// number written by Increment when no explicit limit is requested.
const DefaultIncrementMaxLen = 10000

// hardIncrementMaxLen guards against exponents that would expand into
// unreasonably long numbers, even when the caller raises MaxResultLen.
const hardIncrementMaxLen = 1000000

// jsonNumber is an exact, arbitrary precision decimal parsed from a json
// number literal. It is never converted to or from a float.
type jsonNumber struct {
	neg bool
	co  *big.Int // unscaled coefficient digits
	sc  int      // number of digits after the decimal point (scale >= 0)
}

// parseJSONNumber parses a single json number grammar token, as defined by
// https://www.json.org. Surrounding whitespace is not accepted. It never
// uses float64.
func parseJSONNumber(s string) (jsonNumber, bool) {
	i := 0
	n := jsonNumber{co: new(big.Int)}
	if len(s) == 0 {
		return n, false
	}
	if s[i] == '-' {
		n.neg = true
		i++
		if i >= len(s) {
			return n, false
		}
	}
	// integer part
	if s[i] < '0' || s[i] > '9' {
		return n, false
	}
	digits := make([]byte, 0, len(s))
	if s[i] == '0' {
		i++
		if i < len(s) && s[i] >= '0' && s[i] <= '9' {
			return n, false // leading zero is not allowed
		}
	} else {
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			digits = append(digits, s[i])
			i++
		}
	}
	// fraction part
	if i < len(s) && s[i] == '.' {
		i++
		start := len(digits)
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			digits = append(digits, s[i])
			i++
		}
		if len(digits) == start {
			return n, false // a digit is required after the dot
		}
		n.sc = len(digits) - start
	}
	// exponent part
	exp := 0
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		eneg := false
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			eneg = s[i] == '-'
			i++
		}
		if i >= len(s) || s[i] < '0' || s[i] > '9' {
			return n, false // a digit is required in the exponent
		}
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			exp = exp*10 + int(s[i]-'0')
			if exp > hardIncrementMaxLen {
				return n, false // exponent too large to ever render
			}
			i++
		}
		if eneg {
			exp = -exp
		}
	}
	if i != len(s) {
		return n, false
	}
	if len(digits) == 0 {
		// the literal was "0" (optionally signed); represent an exact zero
		n.co.SetInt64(0)
		n.neg = false
		return n, true
	}
	if _, ok := n.co.SetString(string(digits), 10); !ok {
		return n, false
	}
	if exp != 0 {
		if exp > 0 {
			shift := exp
			if n.sc > 0 {
				if shift >= n.sc {
					shift -= n.sc
					n.sc = 0
				} else {
					n.sc -= shift
					shift = 0
				}
			}
			if shift > 0 {
				n.co.Mul(n.co, new(big.Int).Exp(big.NewInt(10),
					big.NewInt(int64(shift)), nil))
			}
		} else {
			n.sc += -exp
		}
	}
	if n.co.Sign() == 0 {
		n.neg = false
	}
	return n, true
}

// addDecimal returns the exact sum of two decimal numbers. The result scale
// is the larger of the two operand scales, which aligns the fractional
// digits exactly like column arithmetic.
func addDecimal(a, b jsonNumber) jsonNumber {
	sc := a.sc
	if b.sc > sc {
		sc = b.sc
	}
	ac := new(big.Int).Set(a.co)
	bc := new(big.Int).Set(b.co)
	if a.sc < sc {
		ac.Mul(ac, new(big.Int).Exp(big.NewInt(10),
			big.NewInt(int64(sc-a.sc)), nil))
	}
	if b.sc < sc {
		bc.Mul(bc, new(big.Int).Exp(big.NewInt(10),
			big.NewInt(int64(sc-b.sc)), nil))
	}
	if a.neg {
		ac.Neg(ac)
	}
	if b.neg {
		bc.Neg(bc)
	}
	co := ac.Add(ac, bc)
	res := jsonNumber{co: co, sc: sc}
	if co.Sign() < 0 {
		res.neg = true
		co.Neg(co)
	}
	return res
}

// formatDecimal renders a decimal as a canonical plain (non-exponential)
// json number. Trailing fractional zeros are dropped and "-0" is never
// produced.
func formatDecimal(n jsonNumber) string {
	if n.co.Sign() == 0 {
		return "0"
	}
	digits := n.co.Text(10)
	sc := n.sc
	// drop trailing fractional zeros so 2.00 renders as 2
	for sc > 0 && len(digits) > 0 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
		sc--
	}
	if len(digits) == 0 {
		return "0"
	}
	var buf []byte
	if n.neg {
		buf = append(buf, '-')
	}
	if sc == 0 {
		return string(append(buf, digits...))
	}
	if len(digits) > sc {
		buf = append(buf, digits[:len(digits)-sc]...)
		buf = append(buf, '.')
		buf = append(buf, digits[len(digits)-sc:]...)
	} else {
		buf = append(buf, '0', '.')
		for pad := sc - len(digits); pad > 0; pad-- {
			buf = append(buf, '0')
		}
		buf = append(buf, digits...)
	}
	return string(buf)
}

// incrementNumber validates the operands, performs the exact addition and
// returns the rendered replacement number. The length limit is enforced
// before the caller rewrites anything, so an over-limit result leaves the
// input document untouched.
func incrementNumber(cur, delta string, initial *string,
	maxLen int) (string, error) {
	if maxLen <= 0 {
		maxLen = DefaultIncrementMaxLen
	}
	d, ok := parseJSONNumber(delta)
	if !ok {
		return "", &errorType{"increment delta is not a valid json number"}
	}
	var t jsonNumber
	if cur == "" {
		if initial == nil {
			t = jsonNumber{co: new(big.Int)} // missing target starts at 0
		} else {
			t, ok = parseJSONNumber(*initial)
			if !ok {
				return "", &errorType{
					"increment initial is not a valid json number"}
			}
		}
	} else {
		t, ok = parseJSONNumber(cur)
		if !ok {
			return "", &errorType{
				"target value is not a valid json number"}
		}
	}
	out := formatDecimal(addDecimal(t, d))
	if len(out) > maxLen {
		return "", &errorType{
			"increment result exceeds the maximum number length"}
	}
	return out, nil
}

// increment computes the new number and routes it through the same writer
// as Set, which keeps the surrounding document formatting byte-for-byte
// intact and supports simple paths, arrays and nested creation.
func increment(jstr, path, delta string, opts *Options) ([]byte, error) {
	initial, maxLen := incrementOptions(opts)
	res := gjson.Get(jstr, path)
	var cur string
	rootScalar := res.Exists() && res.Index == 0 && len(res.Indexes) == 0
	present := rootScalar ||
		(res.Exists() && (res.Index > 0 || len(res.Indexes) > 0))
	if present {
		if len(res.Indexes) == 0 && res.Type != gjson.Number {
			return nil, &errorType{
				"cannot increment a non-number json value"}
		}
		if len(res.Indexes) > 0 {
			return incrementComplex(jstr, res, delta, initial, maxLen)
		}
		cur = res.Raw
	}
	raw, err := incrementNumber(cur, delta, initial, maxLen)
	if err != nil {
		return nil, err
	}
	if rootScalar {
		// the whole document is itself the target number
		return []byte(raw), nil
	}
	var optimistic, inplace bool
	if opts != nil {
		optimistic = opts.Optimistic
		inplace = opts.ReplaceInPlace
	}
	return set(jstr, path, raw, false, false, optimistic, inplace)
}

// incrementComplex applies an independent increment to every match of a
// query path (for example "friends.#.age"), rewriting from the end so that
// earlier offsets stay valid. A missing match is never created for query
// paths, matching Set's complex-path behavior.
func incrementComplex(jstr string, res gjson.Result, delta string,
	initial *string, maxLen int) ([]byte, error) {
	type match struct {
		index int
		raw   string
	}
	matches := make([]match, 0, len(res.Indexes))
	res.ForEach(func(_, v gjson.Result) bool {
		if v.Type != gjson.Number {
			return false
		}
		matches = append(matches, match{raw: v.Raw})
		return true
	})
	if len(matches) != len(res.Indexes) {
		return nil, &errorType{
			"cannot increment a non-number json value"}
	}
	if len(matches) == 0 {
		// a query path that matched nothing behaves like Set on a complex
		// path: the document is returned unchanged with no error
		return nil, errNoChange
	}
	for i := range matches {
		matches[i].index = res.Indexes[i]
	}
	type replacement struct {
		index int
		raw   string
	}
	repls := make([]replacement, 0, len(matches))
	for _, m := range matches {
		raw, err := incrementNumber(m.raw, delta, initial, maxLen)
		if err != nil {
			return nil, err
		}
		repls = append(repls, replacement{index: m.index, raw: raw})
	}
	for i := len(repls) - 1; i >= 0; i-- {
		r := repls[i]
		m := matches[i]
		njson := make([]byte, 0, len(jstr)+len(r.raw)-len(m.raw))
		njson = append(njson, jstr[:r.index]...)
		njson = append(njson, r.raw...)
		njson = append(njson, jstr[r.index+len(m.raw):]...)
		jstr = *(*string)(unsafe.Pointer(&njson))
	}
	return []byte(jstr), nil
}

func incrementOptions(opts *Options) (*string, int) {
	if opts == nil {
		return nil, DefaultIncrementMaxLen
	}
	return opts.IncrementInitial, opts.MaxResultLen
}

// deltaLiteral converts the supported Increment delta types to a json
// number literal. Floating point values are rejected because their
// conversion could lose precision; pass a json number string instead.
func deltaLiteral(value interface{}) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case []byte:
		return *(*string)(unsafe.Pointer(&v)), nil
	case int:
		return big.NewInt(int64(v)).String(), nil
	case int8:
		return big.NewInt(int64(v)).String(), nil
	case int16:
		return big.NewInt(int64(v)).String(), nil
	case int32:
		return big.NewInt(int64(v)).String(), nil
	case int64:
		return big.NewInt(v).String(), nil
	case uint:
		return new(big.Int).SetUint64(uint64(v)).String(), nil
	case uint8:
		return new(big.Int).SetUint64(uint64(v)).String(), nil
	case uint16:
		return new(big.Int).SetUint64(uint64(v)).String(), nil
	case uint32:
		return new(big.Int).SetUint64(uint64(v)).String(), nil
	case uint64:
		return new(big.Int).SetUint64(v).String(), nil
	case uintptr:
		return new(big.Int).SetUint64(uint64(v)).String(), nil
	default:
		return "", &errorType{
			"increment delta must be a json number string or integer"}
	}
}

func incrementValue(jstr string, path string, value interface{},
	opts *Options) (string, error) {
	if path == "" {
		return "", &errorType{"path cannot be empty"}
	}
	delta, err := deltaLiteral(value)
	if err != nil {
		return "", err
	}
	res, err := increment(jstr, path, delta, opts)
	if err == errNoChange {
		return jstr, nil
	}
	return string(res), err
}

func incrementBytesValue(json []byte, path string, value interface{},
	opts *Options) ([]byte, error) {
	if path == "" {
		return nil, &errorType{"path cannot be empty"}
	}
	delta, err := deltaLiteral(value)
	if err != nil {
		return nil, err
	}
	jstr := *(*string)(unsafe.Pointer(&json))
	res, err := increment(jstr, path, delta, opts)
	if err == errNoChange {
		return json, nil
	}
	return res, err
}

// Increment adds a json number delta to the number found at path using
// exact, arbitrary precision decimal arithmetic. Neither the target nor the
// delta is converted to float64, so integers beyond 2^53 and fractional
// amounts stay exact.
//
// The delta must be a json number literal (such as "1", "-0.25" or "1e3")
// or a Go integer. Floating point values are not accepted.
//
// Only an existing json number can be incremented: null, strings, booleans
// and objects are never coerced into numbers. A missing path is treated as
// 0, or as Options.IncrementInitial when it is provided.
//
// The surrounding document is rewritten through the same machinery as Set,
// so all unrelated formatting is preserved.
func Increment(json, path string, value interface{}) (string, error) {
	return IncrementOptions(json, path, value, nil)
}

// IncrementBytes is the byte-oriented form of Increment.
func IncrementBytes(json []byte, path string,
	value interface{}) ([]byte, error) {
	return IncrementBytesOptions(json, path, value, nil)
}

// IncrementOptions works like Increment but accepts additional options for
// the initial value of a missing target and the maximum result length.
func IncrementOptions(json, path string, value interface{},
	opts *Options) (string, error) {
	if opts != nil && opts.ReplaceInPlace {
		// strings cannot share storage with the caller's byte slice
		nopts := *opts
		opts = &nopts
		opts.ReplaceInPlace = false
	}
	return incrementValue(json, path, value, opts)
}

// IncrementBytesOptions works like IncrementOptions but operates on bytes.
func IncrementBytesOptions(json []byte, path string, value interface{},
	opts *Options) ([]byte, error) {
	return incrementBytesValue(json, path, value, opts)
}
