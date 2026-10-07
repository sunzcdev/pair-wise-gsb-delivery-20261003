package msgpack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/vmihailenco/msgpack/v5/msgpcode"
)

// QueryPredicate compares a scalar field in each queried record.
type QueryPredicate struct {
	Path string
	Eq   interface{}
}

// QuerySpec is a reusable projection over a MessagePack stream.
//
// Path locates the record array with map keys and array indexes separated by
// dots. An empty path selects the root array and "*" may end a parent path.
// Start and End form a half-open array slice; zero End means the end of the
// array. Offset skips matching records; Limit stops after that many matches.
//
// When a predicate field appears after an output field, the output is retained
// as one raw value and is decoded only for that record if the predicate is
// true. Earlier fields therefore use a per-record value boundary, not a
// whole-record buffer; unselected subtrees are only scanned and skipped.
type QuerySpec struct {
	Path      string
	Start     int
	End       int
	Offset    int
	Limit     int
	Fields    []string
	Predicate QueryPredicate
}

// CompiledQuery is a compiled, reusable QuerySpec. It is safe for concurrent
// use by multiple decoders.
type CompiledQuery struct {
	path         []queryStep
	start        int
	end          int
	offset       int
	limit        int
	fields       []string
	filterValue  interface{}
	root         *queryNode
	hasPredicate bool
}

type queryStep struct {
	key   string
	index int
}

type queryNode struct {
	output        int
	filter        bool
	mapChildren   map[string]*queryNode
	arrayChildren map[int]*queryNode
}

type queryFrame struct {
	key   string
	index int
	array bool
}

type queryRunState struct {
	values       []interface{}
	valid        []bool
	hasPredicate bool
	matched      bool
	expected     interface{}
	rawValues    []RawMessage
	rawFrames    [][]queryFrame
}

func (s *queryRunState) hasPredicateBuffering() bool {
	return s.rawValues != nil
}

// QueryError wraps a stream error with the logical path being processed.
type QueryError struct {
	Path string
	Err  error
}

func (e *QueryError) Error() string {
	if e.Path == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("msgpack: query error at %q: %v", e.Path, e.Err)
}

func (e *QueryError) Unwrap() error {
	return e.Err
}

// CompileQuery validates and compiles a streaming record query.
func CompileQuery(spec QuerySpec) (*CompiledQuery, error) {
	if spec.Start < 0 || spec.Offset < 0 || spec.Limit < 0 {
		return nil, errors.New("msgpack: query start, offset and limit must not be negative")
	}
	if spec.End < 0 {
		return nil, errors.New("msgpack: query end must not be negative")
	}
	if spec.End != 0 && spec.End < spec.Start {
		return nil, errors.New("msgpack: query end is before start")
	}

	path, _, err := compileQueryPath(spec.Path)
	if err != nil {
		return nil, err
	}

	q := &CompiledQuery{
		path:   path,
		start:  spec.Start,
		end:    spec.End,
		offset: spec.Offset,
		limit:  spec.Limit,
		fields: append([]string(nil), spec.Fields...),
		root:   &queryNode{output: -1},
	}
	if len(q.fields) == 0 {
		return nil, errors.New("msgpack: query requires at least one field")
	}
	seenFields := make(map[string]bool, len(q.fields))
	for fieldIndex, field := range q.fields {
		if field == "" {
			return nil, errors.New("msgpack: query field path must not be empty")
		}
		if seenFields[field] {
			return nil, fmt.Errorf("msgpack: duplicate query field %q", field)
		}
		seenFields[field] = true
		steps, err := compileFieldPath(field)
		if err != nil {
			return nil, err
		}
		node, err := addQueryProjection(q.root, steps)
		if err != nil {
			return nil, err
		}
		node.output = fieldIndex
	}

	if spec.Predicate.Path != "" || spec.Predicate.Eq != nil {
		if spec.Predicate.Path == "" {
			return nil, errors.New("msgpack: predicate path must not be empty")
		}
		if err := validateQueryValue(spec.Predicate.Eq); err != nil {
			return nil, err
		}
		filterPath, err := compileFieldPath(spec.Predicate.Path)
		if err != nil {
			return nil, err
		}
		q.filterValue = normalizeQueryValue(spec.Predicate.Eq)
		filterNode, err := addQueryProjection(q.root, filterPath)
		if err != nil {
			return nil, err
		}
		if filterNode.output != -1 {
			return nil, fmt.Errorf("msgpack: predicate path %q must not contain an output path", spec.Predicate.Path)
		}
		filterNode.filter = true
		q.hasPredicate = true
	}

	return q, nil
}

func compileQueryPath(path string) ([]queryStep, bool, error) {
	if path == "" || path == "*" {
		return nil, true, nil
	}
	parts := strings.Split(path, ".")
	steps := make([]queryStep, 0, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil, false, fmt.Errorf("msgpack: invalid empty key in query path %q", path)
		}
		if part == "*" {
			if i != len(parts)-1 {
				return nil, false, fmt.Errorf("msgpack: wildcard in query path %q must be last", path)
			}
			return steps, true, nil
		}
		step, err := compileQueryStep(part)
		if err != nil {
			return nil, false, err
		}
		steps = append(steps, step)
	}
	return steps, false, nil
}

func compileFieldPath(path string) ([]queryStep, error) {
	parts := strings.Split(path, ".")
	steps := make([]queryStep, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("msgpack: invalid empty key in field path %q", path)
		}
		if part == "*" {
			return nil, fmt.Errorf("msgpack: wildcards are not supported in field path %q", path)
		}
		step, err := compileQueryStep(part)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func compileQueryStep(part string) (queryStep, error) {
	if index, err := strconv.Atoi(part); err == nil {
		if index < 0 {
			return queryStep{}, fmt.Errorf("msgpack: query array index %q must not be negative", part)
		}
		return queryStep{key: part, index: index}, nil
	}
	return queryStep{key: part, index: -1}, nil
}

func addQueryProjection(root *queryNode, steps []queryStep) (*queryNode, error) {
	node := root
	for _, step := range steps {
		if node.filter {
			return nil, fmt.Errorf("msgpack: query path %q contains predicate path", step.key)
		}
		if node.output != -1 {
			return nil, errors.New("msgpack: query paths must not contain an output path")
		}
		if node.mapChildren == nil {
			node.mapChildren = make(map[string]*queryNode)
		}
		child := node.mapChildren[step.key]
		if child == nil {
			child = &queryNode{output: -1}
			node.mapChildren[step.key] = child
		}
		if step.index >= 0 {
			if node.arrayChildren == nil {
				node.arrayChildren = make(map[int]*queryNode)
			}
			if existing := node.arrayChildren[step.index]; existing != nil && existing != child {
				return nil, fmt.Errorf("msgpack: ambiguous query path segment %q", step.key)
			}
			node.arrayChildren[step.index] = child
		}
		node = child
	}
	return node, nil
}

func validateQueryValue(value interface{}) error {
	switch value.(type) {
	case nil, string, []byte, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return nil
	default:
		return fmt.Errorf("msgpack: unsupported predicate value %T", value)
	}
}

func normalizeQueryValue(value interface{}) interface{} {
	switch value := value.(type) {
	case int:
		return int64(value)
	case int8:
		return int64(value)
	case int16:
		return int64(value)
	case int32:
		return int64(value)
	case int64:
		return value
	case uint:
		return uint64(value)
	case uint8:
		return uint64(value)
	case uint16:
		return uint64(value)
	case uint32:
		return uint64(value)
	case uint64:
		return value
	case float32:
		return float64(value)
	default:
		return value
	}
}

// Exec runs the compiled query against d.
func (q *CompiledQuery) Exec(d *Decoder) ([]map[string]interface{}, error) {
	return d.QueryCompiled(q)
}

// QueryCompiled runs a compiled query.
func (d *Decoder) QueryCompiled(q *CompiledQuery) ([]map[string]interface{}, error) {
	oldUseScannerRead := d.useScannerRead
	d.useScannerRead = true
	defer func() {
		d.useScannerRead = oldUseScannerRead
	}()
	frames := make([]queryFrame, 0, len(q.path)+len(q.fields)+4)
	code, err := d.PeekCode()
	if err != nil {
		return nil, d.queryError(frames, err)
	}
	if isMapCode(code) {
		return d.queryCompiledMapRoot(q, frames)
	}
	if isArrayCode(code) {
		return d.queryCompiledArrayRoot(q, frames, true)
	}
	return nil, fmt.Errorf("msgpack: unsupported code=%x decoding query root", code)
}

func (d *Decoder) queryCompiledMapRoot(q *CompiledQuery, frames []queryFrame) ([]map[string]interface{}, error) {
	values, err := d.navigateCompiledPath(q, 0, frames)
	if err != nil {
		return nil, d.queryError(frames, err)
	}
	return values, nil
}

func (d *Decoder) navigateCompiledPath(q *CompiledQuery, pos int, frames []queryFrame) ([]map[string]interface{}, error) {
	if pos == len(q.path) {
		return d.queryCompiledTarget(q, frames)
	}

	code, err := d.readCode()
	if err != nil {
		return nil, err
	}
	step := q.path[pos]

	if isMapCode(code) {
		n, err := d.mapLen(code)
		if err != nil {
			return nil, err
		}
		if n == -1 {
			return nil, nil
		}
		for i := 0; i < n; i++ {
			key, err := d.decodeStringTemp()
			if err != nil {
				return nil, d.queryError(frames, err)
			}
			if key == step.key {
				frames = pushMapFrame(frames, step.key)
				return d.navigateCompiledPath(q, pos+1, frames)
			}
			if err := d.skipPath(frames, key); err != nil {
				return nil, d.queryError(pushMapFrame(frames, key), err)
			}
		}
		return nil, nil
	}

	if isArrayCode(code) {
		n, err := d.arrayLen(code)
		if err != nil {
			return nil, err
		}
		if n == -1 {
			return nil, nil
		}
		for i := 0; i < n; i++ {
			if i == step.index {
				frames = pushArrayFrame(frames, i)
				return d.navigateCompiledPath(q, pos+1, frames)
			}
			frames = pushArrayFrame(frames, i)
			if err := d.skipPath(frames, ""); err != nil {
				return nil, d.queryError(frames, err)
			}
			frames = frames[:len(frames)-1]
		}
		return nil, nil
	}

	return nil, fmt.Errorf("msgpack: unsupported code=%x decoding query path", code)
}

func (d *Decoder) queryCompiledTarget(q *CompiledQuery, frames []queryFrame) ([]map[string]interface{}, error) {
	code, err := d.PeekCode()
	if err != nil {
		return nil, err
	}
	if isArrayCode(code) {
		return d.queryCompiledArrayRoot(q, frames, false)
	}

	state := queryRunState{
		values:   make([]interface{}, len(q.fields)),
		valid:    make([]bool, len(q.fields)),
		expected: q.filterValue,
	}
	if q.hasPredicate {
		state.rawValues = make([]RawMessage, len(q.fields))
		state.rawFrames = make([][]queryFrame, len(q.fields))
	}
	if err := d.scanQueryNode(q.root, &state, frames); err != nil {
		return nil, d.queryError(frames, err)
	}
	if q.hasPredicate && (!state.hasPredicate || !state.matched) {
		return nil, nil
	}
	if err := d.decodePendingQueryValues(q, &state, frames); err != nil {
		return nil, err
	}

	row := make(map[string]interface{}, len(q.fields))
	for i, field := range q.fields {
		if state.valid[i] {
			row[field] = state.values[i]
		}
	}
	return []map[string]interface{}{row}, nil
}

func (d *Decoder) queryCompiledArrayRoot(q *CompiledQuery, frames []queryFrame, root bool) ([]map[string]interface{}, error) {
	code, err := d.readCode()
	if err != nil {
		if root {
			return nil, d.queryError(frames, err)
		}
		return nil, err
	}
	if code == msgpcode.Nil {
		return nil, nil
	}
	if !isArrayCode(code) {
		err := fmt.Errorf("msgpack: unsupported code=%x decoding query array", code)
		if root {
			return nil, d.queryError(frames, err)
		}
		return nil, err
	}

	n, err := d.arrayLen(code)
	if err != nil {
		return nil, d.queryError(frames, err)
	}
	if n == -1 {
		return nil, nil
	}

	start := q.start
	if start > n {
		start = n
	}
	end := n
	if q.end > 0 && q.end < end {
		end = q.end
	}
	if end < start {
		end = start
	}

	for i := 0; i < start; i++ {
		frames = pushArrayFrame(frames, i)
		if err := d.skipPath(frames, ""); err != nil {
			return nil, d.queryError(frames, err)
		}
		frames = frames[:len(frames)-1]
	}

	capacity := q.limit
	if capacity == 0 {
		capacity = 0
	}
	rows := make([]map[string]interface{}, 0, capacity)
	state := queryRunState{
		values:   make([]interface{}, len(q.fields)),
		valid:    make([]bool, len(q.fields)),
		expected: q.filterValue,
	}
	if q.hasPredicate {
		state.rawValues = make([]RawMessage, len(q.fields))
		state.rawFrames = make([][]queryFrame, len(q.fields))
	}

	matches := 0
	i := start
	for i < end {
		state.reset(len(q.fields))
		frames = pushArrayFrame(frames, i)
		if err := d.scanQueryNode(q.root, &state, frames); err != nil {
			return nil, d.queryError(frames, err)
		}

		if q.hasPredicate && (!state.hasPredicate || !state.matched) {
			frames = frames[:len(frames)-1]
			i++
			continue
		}
		if q.hasPredicate {
			if err := d.decodePendingQueryValues(q, &state, frames); err != nil {
				return nil, err
			}
		}
		frames = frames[:len(frames)-1]

		if matches < q.offset {
			matches++
			i++
			continue
		}

		row := make(map[string]interface{}, len(q.fields))
		for i, field := range q.fields {
			if state.valid[i] {
				row[field] = state.values[i]
			}
		}
		rows = append(rows, row)
		matches++
		if q.limit > 0 && len(rows) == q.limit {
			i++
			for i < end {
				frames = pushArrayFrame(frames, i)
				if err := d.skipPath(frames, ""); err != nil {
					return nil, d.queryError(frames, err)
				}
				frames = frames[:len(frames)-1]
				i++
			}
			break
		}
		i++
	}

	return rows, nil
}

func (d *Decoder) decodePendingQueryValues(q *CompiledQuery, state *queryRunState, frames []queryFrame) error {
	for i, raw := range state.rawValues {
		if raw == nil {
			continue
		}
		decoder := NewDecoder(bytes.NewReader(raw))
		decoder.mapDecoder = d.mapDecoder
		decoder.structTag = d.structTag
		decoder.dict = d.dict
		decoder.flags = d.flags
		value, err := decoder.decodeInterfaceCond()
		if err != nil {
			return d.queryError(state.rawFrames[i], err)
		}
		state.values[i] = value
	}
	return nil
}

func (s *queryRunState) reset(n int) {
	s.hasPredicate = false
	s.matched = false
	for i := 0; i < n; i++ {
		s.values[i] = nil
		s.valid[i] = false
	}
	if s.rawValues != nil {
		for i := 0; i < n; i++ {
			s.rawValues[i] = nil
			s.rawFrames[i] = nil
		}
	}
}

func (d *Decoder) scanQueryNode(node *queryNode, state *queryRunState, frames []queryFrame) error {
	code, err := d.readCode()
	if err != nil {
		return d.queryError(frames, err)
	}
	terminal := node.output != -1 || node.filter
	if !terminal && code == msgpcode.Nil {
		return nil
	}
	if terminal {
		if node.output != -1 && !node.filter {
			if state.hasPredicateBuffering() {
				value, err := d.decodeRawFromCode(code)
				if err != nil {
					return d.queryError(frames, err)
				}
				state.rawValues[node.output] = value
				state.rawFrames[node.output] = append([]queryFrame(nil), frames...)
				state.valid[node.output] = true
				return nil
			}
			value, err := d.decodeInterfaceFromCode(code)
			if err != nil {
				return d.queryError(frames, err)
			}
			state.values[node.output] = value
			state.valid[node.output] = true
			return nil
		}

		if node.output != -1 && node.filter {
			value, matched, err := d.decodeAndCompareQueryValue(code, state.expected)
			if err != nil {
				return d.queryError(frames, err)
			}
			state.values[node.output] = value
			state.valid[node.output] = true
			state.hasPredicate = true
			state.matched = matched
			return nil
		}

		matched, err := d.compareQueryValue(code, state.expected)
		if err != nil {
			return d.queryError(frames, err)
		}
		state.hasPredicate = true
		state.matched = matched
		return nil
	}

	if isMapCode(code) {
		n, err := d.mapLen(code)
		if err != nil {
			return d.queryError(frames, err)
		}
		if n == -1 {
			return nil
		}
		for i := 0; i < n; i++ {
			key, err := d.decodeStringTemp()
			if err != nil {
				return d.queryError(frames, err)
			}
			child := node.mapChildren[key]
			if child != nil {
				frames = pushMapFrame(frames, key)
				if err := d.scanQueryNode(child, state, frames); err != nil {
					return d.queryError(frames, err)
				}
				frames = frames[:len(frames)-1]
				continue
			}
			valueCode, err := d.readCode()
			if err != nil {
				return d.queryError(pushMapFrame(frames, key), err)
			}
			if err := d.skipPathCode(valueCode, pushMapFrame(frames, key)); err != nil {
				return d.queryError(pushMapFrame(frames, key), err)
			}
		}
		return nil
	}

	if isArrayCode(code) {
		n, err := d.arrayLen(code)
		if err != nil {
			return d.queryError(frames, err)
		}
		if n == -1 {
			return nil
		}
		for i := 0; i < n; i++ {
			child := node.arrayChildren[i]
			if child != nil {
				frames = pushArrayFrame(frames, i)
				if err := d.scanQueryNode(child, state, frames); err != nil {
					return d.queryError(frames, err)
				}
				frames = frames[:len(frames)-1]
				continue
			}
			frames = pushArrayFrame(frames, i)
			if err := d.skipPath(frames, ""); err != nil {
				return d.queryError(frames, err)
			}
			frames = frames[:len(frames)-1]
		}
		return nil
	}
	return d.queryError(frames, fmt.Errorf("msgpack: unsupported code=%x decoding query field", code))
}

func (d *Decoder) decodeAndCompareQueryValue(code byte, expected interface{}) (interface{}, bool, error) {
	value, err := d.decodeInterfaceFromCode(code)
	if err != nil {
		return nil, false, err
	}
	matched, err := compareQueryValues(value, expected)
	return value, matched, err
}

func compareQueryValues(value, expected interface{}) (bool, error) {
	if expected == nil {
		return value == nil, nil
	}
	if value == nil {
		return false, nil
	}

	switch expected := expected.(type) {
	case string:
		value, ok := value.(string)
		return ok && value == expected, nil
	case []byte:
		value, ok := value.([]byte)
		return ok && bytes.Equal(value, expected), nil
	case bool:
		value, ok := value.(bool)
		return ok && value == expected, nil
	case int64:
		return compareQueryIntValue(value, expected)
	case uint64:
		return compareQueryUintValue(value, expected)
	case float64:
		return compareQueryFloatValue(value, expected)
	default:
		return false, nil
	}
}

func compareQueryIntValue(value interface{}, expected int64) (bool, error) {
	switch value := value.(type) {
	case int:
		return int64(value) == expected, nil
	case int8:
		return int64(value) == expected, nil
	case int16:
		return int64(value) == expected, nil
	case int32:
		return int64(value) == expected, nil
	case int64:
		return value == expected, nil
	case uint:
		return uint64(value) <= math.MaxInt64 && int64(value) == expected, nil
	case uint8:
		return uint64(value) <= math.MaxInt64 && int64(value) == expected, nil
	case uint16:
		return uint64(value) <= math.MaxInt64 && int64(value) == expected, nil
	case uint32:
		return uint64(value) <= math.MaxInt64 && int64(value) == expected, nil
	case uint64:
		return value <= math.MaxInt64 && int64(value) == expected, nil
	case float32:
		return float64(value) == float64(expected), nil
	case float64:
		return value == float64(expected), nil
	default:
		return false, nil
	}
}

func compareQueryUintValue(value interface{}, expected uint64) (bool, error) {
	switch value := value.(type) {
	case int:
		return value >= 0 && uint64(value) == expected, nil
	case int8:
		return value >= 0 && uint64(value) == expected, nil
	case int16:
		return value >= 0 && uint64(value) == expected, nil
	case int32:
		return value >= 0 && uint64(value) == expected, nil
	case int64:
		return value >= 0 && uint64(value) == expected, nil
	case uint:
		return uint64(value) == expected, nil
	case uint8:
		return uint64(value) == expected, nil
	case uint16:
		return uint64(value) == expected, nil
	case uint32:
		return uint64(value) == expected, nil
	case uint64:
		return value == expected, nil
	case float32:
		return float64(value) == float64(expected), nil
	case float64:
		return value == float64(expected), nil
	default:
		return false, nil
	}
}

func compareQueryFloatValue(value interface{}, expected float64) (bool, error) {
	switch value := value.(type) {
	case int:
		return float64(value) == expected, nil
	case int8:
		return float64(value) == expected, nil
	case int16:
		return float64(value) == expected, nil
	case int32:
		return float64(value) == expected, nil
	case int64:
		return float64(value) == expected, nil
	case uint:
		return float64(value) == expected, nil
	case uint8:
		return float64(value) == expected, nil
	case uint16:
		return float64(value) == expected, nil
	case uint32:
		return float64(value) == expected, nil
	case uint64:
		return float64(value) == expected, nil
	case float32:
		return float64(value) == expected, nil
	case float64:
		return value == expected, nil
	default:
		return false, nil
	}
}

func (d *Decoder) compareQueryValue(code byte, expected interface{}) (bool, error) {
	if code == msgpcode.Nil {
		return expected == nil, nil
	}
	switch expected := expected.(type) {
	case nil:
		return false, d.skipCode(code)
	case string:
		return d.compareQueryString(code, expected)
	case []byte:
		return d.compareQueryBytes(code, expected)
	case bool:
		if code == msgpcode.Nil {
			return false, nil
		}
		value, err := d.bool(code)
		return value == expected, err
	case int64:
		return d.compareQueryInt(code, expected)
	case uint64:
		return d.compareQueryUint(code, expected)
	case float64:
		value, err := d.float64(code)
		return value == expected, err
	default:
		return false, nil
	}
}

func (d *Decoder) compareQueryString(code byte, expected string) (bool, error) {
	if !isStringCode(code) {
		return false, d.skipCode(code)
	}
	n, err := d.bytesLen(code)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return expected == "", nil
	}
	if len(expected) != n {
		return false, d.skipN(n)
	}
	b, err := d.readN(n)
	if err != nil {
		return false, err
	}
	return bytes.Equal(b, stringToBytes(expected)), nil
}

func (d *Decoder) compareQueryBytes(code byte, expected []byte) (bool, error) {
	if !isBinaryCode(code) {
		return false, d.skipCode(code)
	}
	n, err := d.bytesLen(code)
	if err != nil {
		return false, err
	}
	if n != len(expected) {
		if n > 0 {
			if err := d.skipN(n); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	b, err := d.readN(n)
	if err != nil {
		return false, err
	}
	return bytes.Equal(b, expected), nil
}

func (d *Decoder) compareQueryInt(code byte, expected int64) (bool, error) {
	switch {
	case isStringCode(code) || isBinaryCode(code) || isContainerCode(code):
		return false, d.skipCode(code)
	case code == msgpcode.Uint64:
		value, err := d.uint(code)
		if err != nil || value > math.MaxInt64 {
			return false, err
		}
		return int64(value) == expected, nil
	case isUnsignedIntCode(code):
		value, err := d.uint(code)
		if err != nil {
			return false, err
		}
		return value <= math.MaxInt64 && int64(value) == expected, nil
	case isSignedIntCode(code) || msgpcode.IsFixedNum(code):
		value, err := d.int(code)
		return value == expected, err
	case code == msgpcode.Float || code == msgpcode.Double:
		value, err := d.float64(code)
		return value == float64(expected), err
	default:
		return false, d.skipCode(code)
	}
}

func (d *Decoder) compareQueryUint(code byte, expected uint64) (bool, error) {
	switch {
	case isStringCode(code) || isBinaryCode(code) || isContainerCode(code):
		return false, d.skipCode(code)
	case isSignedIntCode(code):
		value, err := d.int(code)
		if err != nil || value < 0 {
			return false, err
		}
		return uint64(value) == expected, nil
	case msgpcode.IsFixedNum(code) && code >= msgpcode.NegFixedNumLow:
		return false, nil
	case isUnsignedIntCode(code) || msgpcode.IsFixedNum(code):
		value, err := d.uint(code)
		return value == expected, err
	case code == msgpcode.Float || code == msgpcode.Double:
		value, err := d.float64(code)
		return value == float64(expected), err
	default:
		return false, d.skipCode(code)
	}
}

func isStringCode(code byte) bool {
	return msgpcode.IsFixedString(code) || code == msgpcode.Str8 || code == msgpcode.Str16 || code == msgpcode.Str32
}

func isBinaryCode(code byte) bool {
	return code == msgpcode.Bin8 || code == msgpcode.Bin16 || code == msgpcode.Bin32
}

func isMapCode(code byte) bool {
	return msgpcode.IsFixedMap(code) || code == msgpcode.Map16 || code == msgpcode.Map32
}

func isArrayCode(code byte) bool {
	return msgpcode.IsFixedArray(code) || code == msgpcode.Array16 || code == msgpcode.Array32
}

func isContainerCode(code byte) bool {
	return isMapCode(code) || isArrayCode(code)
}

func isUnsignedIntCode(code byte) bool {
	return code <= 0x7f ||
		code == msgpcode.Uint8 || code == msgpcode.Uint16 ||
		code == msgpcode.Uint32 || code == msgpcode.Uint64
}

func isSignedIntCode(code byte) bool {
	return code == msgpcode.Int8 || code == msgpcode.Int16 ||
		code == msgpcode.Int32 || code == msgpcode.Int64
}

func pushMapFrame(frames []queryFrame, key string) []queryFrame {
	return append(frames, queryFrame{key: key})
}

func pushArrayFrame(frames []queryFrame, index int) []queryFrame {
	return append(frames, queryFrame{index: index, array: true})
}

func (d *Decoder) skipPath(frames []queryFrame, _ string) error {
	code, err := d.readCode()
	if err != nil {
		return err
	}
	return d.skipPathCode(code, frames)
}

func (d *Decoder) skipPathCode(code byte, frames []queryFrame) error {
	switch {
	case isMapCode(code):
		n, err := d.mapLen(code)
		if err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			key, err := d.decodeStringTemp()
			if err != nil {
				return err
			}
			valueCode, err := d.readCode()
			if err != nil {
				return err
			}
			if err := d.skipPathCode(valueCode, pushMapFrame(frames, key)); err != nil {
				return err
			}
		}
		return nil
	case isArrayCode(code):
		n, err := d.arrayLen(code)
		if err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			frames = pushArrayFrame(frames, i)
			if err := d.skipPath(frames, ""); err != nil {
				return err
			}
			frames = frames[:len(frames)-1]
		}
		return nil
	default:
		return d.skipCode(code)
	}
}

func (d *Decoder) queryError(frames []queryFrame, err error) error {
	if err == nil {
		return nil
	}
	var queryErr *QueryError
	if errors.As(err, &queryErr) {
		return err
	}
	if errors.Is(err, io.EOF) && len(frames) > 0 {
		err = io.ErrUnexpectedEOF
	}
	return &QueryError{Path: queryPathString(frames), Err: err}
}

func queryPathString(frames []queryFrame) string {
	var builder strings.Builder
	for _, frame := range frames {
		if frame.array {
			builder.WriteByte('[')
			builder.WriteString(strconv.Itoa(frame.index))
			builder.WriteByte(']')
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('.')
		}
		builder.WriteString(frame.key)
	}
	return builder.String()
}
