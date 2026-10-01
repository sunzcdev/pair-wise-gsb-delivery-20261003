package zapcore

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap/buffer"
)

const (
	defaultRedaction = "[REDACTED]"
	defaultMaxDepth  = 1000
	maxConfigDepth   = 10000
	maxPathCount     = 1024
	maxPathSegments  = 128
	maxReplacement   = 1024
)

var (
	errRedactionDepth      = errors.New("zapcore: redaction depth limit exceeded")
	errRedactedMarshaler   = errors.New("zapcore: value uses a marshaler and cannot be inspected for redaction")
	errRedactedUnsupported = errors.New("zapcore: unsupported reflected value")
)

// RedactingEncoderConfig configures structural field redaction.
type RedactingEncoderConfig struct {
	// FieldPaths are exact dot-separated paths. [] matches every array
	// element, and [n] matches element n.
	FieldPaths []string
	// Replacement replaces a matched value. Empty means [REDACTED].
	Replacement string
	// MaxDepth bounds nested marshaler and reflected traversal.
	MaxDepth int
}

type redactionSegmentKind uint8

const (
	redactionObject redactionSegmentKind = iota
	redactionArrayAny
	redactionArrayIndex
)

type redactionSegment struct {
	kind  redactionSegmentKind
	name  string
	index int
}

type redactionNode struct {
	terminal bool
	objects  map[string]*redactionNode
	value    *redactionNode
	arrays   map[int]*redactionNode
	anyArray *redactionNode
}

// NewCredentialPasswordRedactingEncoder redacts the exact field path
// credentials.password while leaving other fields named password untouched.
func NewCredentialPasswordRedactingEncoder(enc Encoder) Encoder {
	return NewRedactingEncoder(enc, RedactingEncoderConfig{
		FieldPaths:  []string{"credentials.password"},
		Replacement: defaultRedaction,
		MaxDepth:    defaultMaxDepth,
	})
}

// NewRedactingEncoder wraps enc with structural redaction. With no configured
// field paths it returns enc itself, preserving the original encoder's path.
func NewRedactingEncoder(enc Encoder, cfg RedactingEncoderConfig) Encoder {
	if len(cfg.FieldPaths) == 0 {
		return enc
	}

	rules, err := buildRedactionRules(cfg)
	replacement := defaultRedaction
	if cfg.Replacement != "" {
		replacement = cfg.Replacement
	}
	maxDepth := cfg.MaxDepth
	if maxDepth < 1 || maxDepth > maxConfigDepth {
		maxDepth = defaultMaxDepth
	}

	return &redactingEncoder{
		redactingObjectEncoder: redactingObjectEncoder{
			ObjectEncoder: enc,
			rules:         rules,
			current:       rules,
			replacement:   replacement,
			maxDepth:      maxDepth,
			err:           err,
		},
	}
}

func buildRedactionRules(cfg RedactingEncoderConfig) (*redactionNode, error) {
	if len(cfg.FieldPaths) > maxPathCount {
		return nil, fmt.Errorf("zapcore: too many redaction paths (max %d)", maxPathCount)
	}
	if len(cfg.Replacement) > maxReplacement {
		return nil, fmt.Errorf("zapcore: redaction replacement too large (max %d bytes)", maxReplacement)
	}
	if cfg.MaxDepth < 1 || cfg.MaxDepth > maxConfigDepth {
		return nil, fmt.Errorf("zapcore: redaction max depth must be between 1 and %d", maxConfigDepth)
	}

	root := &redactionNode{}
	for _, path := range cfg.FieldPaths {
		segments, err := parseRedactionPath(path)
		if err != nil {
			return nil, err
		}

		node := root
		for i, segment := range segments {
			if i+1 < len(segments) && segment.kind == redactionObject {
				switch segments[i+1].kind {
				case redactionArrayAny, redactionArrayIndex:
					node = node.ruleChild(segment)
					if node.value == nil {
						node.value = &redactionNode{}
					}
					node = node.value
					continue
				}
			}
			node = node.ruleChild(segment)
		}
		node.terminal = true
	}
	return root, nil
}

func parseRedactionPath(path string) ([]redactionSegment, error) {
	if path == "" {
		return nil, errors.New("zapcore: redaction path cannot be empty")
	}

	var segments []redactionSegment
	for path != "" {
		dot := strings.IndexByte(path, '.')
		bracket := strings.IndexByte(path, '[')
		end := len(path)
		if dot >= 0 && dot < end {
			end = dot
		}
		if bracket >= 0 && bracket < end {
			end = bracket
		}
		if end == 0 {
			return nil, fmt.Errorf("zapcore: invalid redaction path")
		}

		name := path[:end]
		path = path[end:]
		segments = append(segments, redactionSegment{kind: redactionObject, name: name})

		for strings.HasPrefix(path, "[") {
			closeBracket := strings.IndexByte(path, ']')
			if closeBracket < 0 {
				return nil, fmt.Errorf("zapcore: invalid redaction path")
			}
			indexText := path[1:closeBracket]
			if indexText == "" || indexText == "*" {
				segments = append(segments, redactionSegment{kind: redactionArrayAny})
			} else {
				index, err := strconv.Atoi(indexText)
				if err != nil || index < 0 {
					return nil, fmt.Errorf("zapcore: invalid array index %q", indexText)
				}
				segments = append(segments, redactionSegment{kind: redactionArrayIndex, index: index})
			}
			path = path[closeBracket+1:]
		}

		if strings.HasPrefix(path, ".") {
			path = path[1:]
		} else if path != "" {
			return nil, fmt.Errorf("zapcore: invalid redaction path")
		}
		if len(segments) > maxPathSegments {
			return nil, fmt.Errorf("zapcore: redaction path too long (max %d segments)", maxPathSegments)
		}
	}
	return segments, nil
}
func (n *redactionNode) ruleChild(segment redactionSegment) *redactionNode {
	switch segment.kind {
	case redactionArrayAny:
		if n.anyArray == nil {
			n.anyArray = &redactionNode{}
		}
		return n.anyArray
	case redactionArrayIndex:
		if n.arrays == nil {
			n.arrays = make(map[int]*redactionNode)
		}
		child := n.arrays[segment.index]
		if child == nil {
			child = &redactionNode{}
			n.arrays[segment.index] = child
		}
		return child
	default:
		if n.objects == nil {
			n.objects = make(map[string]*redactionNode)
		}
		return n.ruleObjectChild(segment.name)
	}
}

func (n *redactionNode) ruleObjectChild(name string) *redactionNode {
	child := n.objects[name]
	if child == nil {
		child = &redactionNode{}
		n.objects[name] = child
	}
	return child
}

func (n *redactionNode) objectChild(key string) *redactionNode {
	if n == nil {
		return nil
	}
	return n.objects[key]
}

func (n *redactionNode) arrayObjectChild(key string) *redactionNode {
	child := n.objectChild(key)
	if child == nil {
		return nil
	}
	if child.value != nil {
		return child.value
	}
	return child
}

func (n *redactionNode) reflectChild(key string, value reflect.Value) *redactionNode {
	child := n.objectChild(key)
	if child == nil {
		return nil
	}
	if child.value != nil {
		value = indirectReflectValue(value)
		if value.IsValid() {
			switch value.Kind() {
			case reflect.Array, reflect.Slice:
				return child.value
			}
		}
		return child
	}
	return child
}

func (n *redactionNode) arrayChild(index int) *redactionNode {
	if n == nil {
		return nil
	}
	if child := n.arrays[index]; child != nil {
		return child
	}
	return n.anyArray
}

type redactingEncoder struct {
	redactingObjectEncoder
}

var _ Encoder = (*redactingEncoder)(nil)

func (e *redactingEncoder) Clone() Encoder {
	clone := *e
	clone.ObjectEncoder = e.ObjectEncoder.(Encoder).Clone()
	return &clone
}

func (e *redactingEncoder) EncodeEntry(ent Entry, fields []Field) (*buffer.Buffer, error) {
	if e.err != nil {
		return nil, e.err
	}

	clone := e.Clone().(*redactingEncoder)
	addFields(clone, fields)
	return clone.ObjectEncoder.(Encoder).EncodeEntry(ent, nil)
}

type redactingObjectEncoder struct {
	ObjectEncoder
	rules       *redactionNode
	current     *redactionNode
	depth       int
	replacement string
	maxDepth    int
	err         error
}

var _ ObjectEncoder = (*redactingObjectEncoder)(nil)

type redactingArrayEncoder struct {
	ArrayEncoder
	rules       *redactionNode
	current     *redactionNode
	index       int
	depth       int
	replacement string
	maxDepth    int
}

var _ ArrayEncoder = (*redactingArrayEncoder)(nil)

type redactedObject struct{}

func (redactedObject) MarshalLogObject(ObjectEncoder) error { return nil }

type redactedArray struct{}

func (redactedArray) MarshalLogArray(ArrayEncoder) error { return nil }

type objectRedactionMarshaler struct {
	marshaler ObjectMarshaler
	encoder   *redactingObjectEncoder
}

func (m objectRedactionMarshaler) MarshalLogObject(ObjectEncoder) (retErr error) {
	defer func() {
		if recover() != nil {
			retErr = errRedactedMarshaler
		}
	}()
	if err := m.marshaler.MarshalLogObject(m.encoder); err != nil {
		return errRedactedMarshaler
	}
	return nil
}

type arrayRedactionMarshaler struct {
	marshaler ArrayMarshaler
	encoder   *redactingArrayEncoder
}

func (m arrayRedactionMarshaler) MarshalLogArray(ArrayEncoder) (retErr error) {
	defer func() {
		if recover() != nil {
			retErr = errRedactedMarshaler
		}
	}()
	if err := m.marshaler.MarshalLogArray(m.encoder); err != nil {
		return errRedactedMarshaler
	}
	return nil
}

func (e *redactingObjectEncoder) addLogFields(fields []Field) {
	for i := range fields {
		e.processField(fields[i])
	}
}

func (e *redactingObjectEncoder) processField(field Field) {
	switch field.Type {
	case ArrayMarshalerType:
		if err := e.AddArray(field.Key, field.Interface.(ArrayMarshaler)); err != nil {
			e.AddString(field.Key+"Error", err.Error())
		}
	case ObjectMarshalerType:
		if err := e.AddObject(field.Key, field.Interface.(ObjectMarshaler)); err != nil {
			e.AddString(field.Key+"Error", err.Error())
		}
	case InlineMarshalerType:
		if err := field.Interface.(ObjectMarshaler).MarshalLogObject(e); err != nil {
			e.AddString("Error", err.Error())
		}
	case ReflectType:
		if err := e.AddReflected(field.Key, field.Interface); err != nil {
			e.AddString(field.Key+"Error", err.Error())
		}
	case StringerType:
		child := e.current.objectChild(field.Key)
		if child != nil && child.terminal {
			e.AddString(field.Key, e.replacement)
			return
		}
		if err := encodeStringer(field.Key, field.Interface, e); err != nil {
			e.AddString(field.Key+"Error", err.Error())
		}
	case ErrorType:
		child := e.current.objectChild(field.Key)
		if child != nil && child.terminal {
			e.AddString(field.Key, e.replacement)
			return
		}
		if err := encodeError(field.Key, field.Interface.(error), e); err != nil {
			e.AddString(field.Key+"Error", err.Error())
		}
	case NamespaceType:
		e.OpenNamespace(field.Key)
	case BinaryType:
		e.AddBinary(field.Key, field.Interface.([]byte))
	case BoolType:
		e.AddBool(field.Key, field.Integer == 1)
	case ByteStringType:
		e.AddByteString(field.Key, field.Interface.([]byte))
	case Complex128Type:
		e.AddComplex128(field.Key, field.Interface.(complex128))
	case Complex64Type:
		e.AddComplex64(field.Key, field.Interface.(complex64))
	case DurationType:
		e.AddDuration(field.Key, time.Duration(field.Integer))
	case Float64Type:
		e.AddFloat64(field.Key, math.Float64frombits(uint64(field.Integer)))
	case Float32Type:
		e.AddFloat32(field.Key, math.Float32frombits(uint32(field.Integer)))
	case Int64Type:
		e.AddInt64(field.Key, field.Integer)
	case Int32Type:
		e.AddInt32(field.Key, int32(field.Integer))
	case Int16Type:
		e.AddInt16(field.Key, int16(field.Integer))
	case Int8Type:
		e.AddInt8(field.Key, int8(field.Integer))
	case StringType:
		e.AddString(field.Key, field.String)
	case TimeType:
		if field.Interface != nil {
			e.AddTime(field.Key, time.Unix(0, field.Integer).In(field.Interface.(*time.Location)))
		} else {
			e.AddTime(field.Key, time.Unix(0, field.Integer))
		}
	case TimeFullType:
		e.AddTime(field.Key, field.Interface.(time.Time))
	case Uint64Type:
		e.AddUint64(field.Key, uint64(field.Integer))
	case Uint32Type:
		e.AddUint32(field.Key, uint32(field.Integer))
	case Uint16Type:
		e.AddUint16(field.Key, uint16(field.Integer))
	case Uint8Type:
		e.AddUint8(field.Key, uint8(field.Integer))
	case UintptrType:
		e.AddUintptr(field.Key, uintptr(field.Integer))
	case SkipType:
	default:
	}
}

func (e *redactingObjectEncoder) child(key string) *redactionNode {
	return e.current.objectChild(key)
}

func (e *redactingObjectEncoder) childEncoder(child *redactionNode) *redactingObjectEncoder {
	return &redactingObjectEncoder{
		ObjectEncoder: e.ObjectEncoder,
		rules:         e.rules,
		current:       child,
		depth:         e.depth + 1,
		replacement:   e.replacement,
		maxDepth:      e.maxDepth,
	}
}

func (e *redactingObjectEncoder) childArrayEncoder(child *redactionNode) (*redactingArrayEncoder, bool) {
	arrayEncoder, ok := e.ObjectEncoder.(ArrayEncoder)
	if !ok {
		return nil, false
	}
	return &redactingArrayEncoder{
		ArrayEncoder: arrayEncoder,
		rules:        e.rules,
		current:      child,
		depth:        e.depth + 1,
		replacement:  e.replacement,
		maxDepth:     e.maxDepth,
	}, true
}

func (e *redactingObjectEncoder) AddArray(key string, marshaler ArrayMarshaler) error {
	child := e.current.arrayObjectChild(key)
	if child == nil {
		return e.ObjectEncoder.AddArray(key, marshaler)
	}
	if child.terminal {
		return e.ObjectEncoder.AddArray(key, redactedArray{})
	}
	if e.depth >= e.maxDepth {
		return errRedactionDepth
	}
	childEncoder, ok := e.childArrayEncoder(child)
	if !ok {
		return errRedactedUnsupported
	}
	return e.ObjectEncoder.AddArray(key, arrayRedactionMarshaler{
		marshaler: marshaler,
		encoder:   childEncoder,
	})
}

func (e *redactingObjectEncoder) AddObject(key string, marshaler ObjectMarshaler) error {
	child := e.child(key)
	if child == nil {
		return e.ObjectEncoder.AddObject(key, marshaler)
	}
	if child.terminal {
		return e.ObjectEncoder.AddObject(key, redactedObject{})
	}
	if e.depth >= e.maxDepth {
		return errRedactionDepth
	}
	return e.ObjectEncoder.AddObject(key, objectRedactionMarshaler{
		marshaler: marshaler,
		encoder:   e.childEncoder(child),
	})
}

func (e *redactingObjectEncoder) AddReflected(key string, value interface{}) error {
	child := e.current.reflectChild(key, reflect.ValueOf(value))
	if child == nil {
		return e.ObjectEncoder.AddReflected(key, value)
	}
	return appendReflectedField(e.ObjectEncoder, key, reflect.ValueOf(value), child, e.depth, e.maxDepth, e.replacement)
}

func (e *redactingObjectEncoder) OpenNamespace(key string) {
	child := e.child(key)
	e.ObjectEncoder.OpenNamespace(key)
	if child == nil {
		e.current = nil
		return
	}
	e.current = child
	e.depth++
}

func (e *redactingObjectEncoder) AddBinary(key string, value []byte) {
	if e.matches(key) {
		e.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddBinary(key, value)
}

func (e *redactingObjectEncoder) AddByteString(key string, value []byte) {
	if e.matches(key) {
		e.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddByteString(key, value)
}

func (e *redactingObjectEncoder) AddBool(key string, value bool) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddBool(key, value)
}

func (e *redactingObjectEncoder) AddComplex128(key string, value complex128) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddComplex128(key, value)
}

func (e *redactingObjectEncoder) AddComplex64(key string, value complex64) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddComplex64(key, value)
}

func (e *redactingObjectEncoder) AddDuration(key string, value time.Duration) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddDuration(key, value)
}

func (e *redactingObjectEncoder) AddFloat64(key string, value float64) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddFloat64(key, value)
}

func (e *redactingObjectEncoder) AddFloat32(key string, value float32) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddFloat32(key, value)
}

func (e *redactingObjectEncoder) AddInt(key string, value int) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddInt(key, value)
}

func (e *redactingObjectEncoder) AddInt64(key string, value int64) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddInt64(key, value)
}

func (e *redactingObjectEncoder) AddInt32(key string, value int32) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddInt32(key, value)
}

func (e *redactingObjectEncoder) AddInt16(key string, value int16) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddInt16(key, value)
}

func (e *redactingObjectEncoder) AddInt8(key string, value int8) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddInt8(key, value)
}

func (e *redactingObjectEncoder) AddString(key, value string) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddString(key, value)
}

func (e *redactingObjectEncoder) AddTime(key string, value time.Time) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddTime(key, value)
}

func (e *redactingObjectEncoder) AddUint(key string, value uint) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddUint(key, value)
}

func (e *redactingObjectEncoder) AddUint64(key string, value uint64) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddUint64(key, value)
}

func (e *redactingObjectEncoder) AddUint32(key string, value uint32) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddUint32(key, value)
}

func (e *redactingObjectEncoder) AddUint16(key string, value uint16) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddUint16(key, value)
}

func (e *redactingObjectEncoder) AddUint8(key string, value uint8) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddUint8(key, value)
}

func (e *redactingObjectEncoder) AddUintptr(key string, value uintptr) {
	if e.matches(key) {
		e.ObjectEncoder.AddString(key, e.replacement)
		return
	}
	e.ObjectEncoder.AddUintptr(key, value)
}

func (e *redactingObjectEncoder) matches(key string) bool {
	child := e.child(key)
	return child != nil && child.terminal
}
func (a *redactingArrayEncoder) next() *redactionNode {
	child := a.current.arrayChild(a.index)
	a.index++
	return child
}

func (a *redactingArrayEncoder) objectEncoder(child *redactionNode) *redactingObjectEncoder {
	objectEncoder, ok := a.ArrayEncoder.(ObjectEncoder)
	if !ok {
		return nil
	}
	return &redactingObjectEncoder{
		ObjectEncoder: objectEncoder,
		rules:         a.rules,
		current:       child,
		depth:         a.depth + 1,
		replacement:   a.replacement,
		maxDepth:      a.maxDepth,
	}
}

func (a *redactingArrayEncoder) arrayEncoder(child *redactionNode) *redactingArrayEncoder {
	return &redactingArrayEncoder{
		ArrayEncoder: a.ArrayEncoder,
		rules:        a.rules,
		current:      child,
		depth:        a.depth + 1,
		replacement:  a.replacement,
		maxDepth:     a.maxDepth,
	}
}

func (a *redactingArrayEncoder) AppendArray(marshaler ArrayMarshaler) error {
	child := a.next()
	if child == nil {
		return a.ArrayEncoder.AppendArray(marshaler)
	}
	if child.terminal {
		return a.ArrayEncoder.AppendArray(redactedArray{})
	}
	if a.depth >= a.maxDepth {
		return errRedactionDepth
	}
	return a.ArrayEncoder.AppendArray(arrayRedactionMarshaler{
		marshaler: marshaler,
		encoder:   a.arrayEncoder(child),
	})
}

func (a *redactingArrayEncoder) AppendObject(marshaler ObjectMarshaler) error {
	child := a.next()
	if child == nil {
		return a.ArrayEncoder.AppendObject(marshaler)
	}
	if child.terminal {
		return a.ArrayEncoder.AppendObject(redactedObject{})
	}
	if a.depth >= a.maxDepth {
		return errRedactionDepth
	}
	childEncoder := a.objectEncoder(child)
	if childEncoder == nil {
		return errRedactedUnsupported
	}
	return a.ArrayEncoder.AppendObject(objectRedactionMarshaler{
		marshaler: marshaler,
		encoder:   childEncoder,
	})
}

func (a *redactingArrayEncoder) AppendReflected(value interface{}) error {
	child := a.next()
	if child == nil {
		return a.ArrayEncoder.AppendReflected(value)
	}
	return appendReflectedValue(a.ArrayEncoder, reflect.ValueOf(value), child, a.depth, a.maxDepth, a.replacement, nil)
}

func (a *redactingArrayEncoder) AppendBool(value bool) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendBool(value)
}

func (a *redactingArrayEncoder) AppendByteString(value []byte) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendByteString(value)
}

func (a *redactingArrayEncoder) AppendComplex128(value complex128) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendComplex128(value)
}

func (a *redactingArrayEncoder) AppendComplex64(value complex64) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendComplex64(value)
}

func (a *redactingArrayEncoder) AppendFloat64(value float64) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendFloat64(value)
}

func (a *redactingArrayEncoder) AppendFloat32(value float32) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendFloat32(value)
}

func (a *redactingArrayEncoder) AppendInt(value int) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendInt(value)
}

func (a *redactingArrayEncoder) AppendInt64(value int64) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendInt64(value)
}

func (a *redactingArrayEncoder) AppendInt32(value int32) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendInt32(value)
}

func (a *redactingArrayEncoder) AppendInt16(value int16) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendInt16(value)
}

func (a *redactingArrayEncoder) AppendInt8(value int8) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendInt8(value)
}

func (a *redactingArrayEncoder) AppendString(value string) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendString(value)
}

func (a *redactingArrayEncoder) AppendUint(value uint) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendUint(value)
}

func (a *redactingArrayEncoder) AppendUint64(value uint64) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendUint64(value)
}

func (a *redactingArrayEncoder) AppendUint32(value uint32) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendUint32(value)
}

func (a *redactingArrayEncoder) AppendUint16(value uint16) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendUint16(value)
}

func (a *redactingArrayEncoder) AppendUint8(value uint8) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendUint8(value)
}

func (a *redactingArrayEncoder) AppendUintptr(value uintptr) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendUintptr(value)
}

func (a *redactingArrayEncoder) AppendDuration(value time.Duration) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendDuration(value)
}

func (a *redactingArrayEncoder) AppendTime(value time.Time) {
	if a.appendRedactedScalar() {
		return
	}
	a.ArrayEncoder.AppendTime(value)
}

func (a *redactingArrayEncoder) appendRedactedScalar() bool {
	child := a.next()
	if child == nil || !child.terminal {
		return false
	}
	a.ArrayEncoder.AppendString(a.replacement)
	return true
}

type reflectVisit struct {
	kind reflect.Kind
	ptr  uintptr
}

type reflectFieldInfo struct {
	name      string
	index     []int
	omitempty bool
	inline    bool
}

type jsonMarshaler interface {
	MarshalJSON() ([]byte, error)
}

type textMarshaler interface {
	MarshalText() ([]byte, error)
}

func newReflectedObjectEncoder(enc ObjectEncoder, rules, current *redactionNode, depth, maxDepth int, replacement string) ObjectEncoder {
	return &redactingObjectEncoder{
		ObjectEncoder: enc,
		rules:         rules,
		current:       current,
		depth:         depth,
		replacement:   replacement,
		maxDepth:      maxDepth,
	}
}

func newReflectedArrayEncoder(enc ArrayEncoder, rules, current *redactionNode, depth, maxDepth int, replacement string) ArrayEncoder {
	return &redactingArrayEncoder{
		ArrayEncoder: enc,
		rules:        rules,
		current:      current,
		depth:        depth,
		replacement:  replacement,
		maxDepth:     maxDepth,
	}
}

type reflectObjectMarshaler struct {
	value       reflect.Value
	rules       *redactionNode
	target      ObjectEncoder
	depth       int
	maxDepth    int
	replacement string
	seen        map[reflectVisit]struct{}
}

func (m reflectObjectMarshaler) MarshalLogObject(enc ObjectEncoder) error {
	targetEnc := enc
	if m.target != nil {
		targetEnc = m.target
	}
	return writeReflectObject(targetEnc, m.value, m.rules, m.depth, m.maxDepth, m.replacement, m.seen)
}

type reflectArrayMarshaler struct {
	value       reflect.Value
	rules       *redactionNode
	target      ArrayEncoder
	depth       int
	maxDepth    int
	replacement string
	seen        map[reflectVisit]struct{}
}

func (m reflectArrayMarshaler) MarshalLogArray(enc ArrayEncoder) error {
	targetEnc := enc
	if m.target != nil {
		targetEnc = m.target
	}
	return writeReflectArray(targetEnc, m.value, m.rules, m.depth, m.maxDepth, m.replacement, m.seen)
}

func appendReflectedField(enc ObjectEncoder, key string, value reflect.Value, node *redactionNode, depth, maxDepth int, replacement string, parentSeen ...map[reflectVisit]struct{}) error {
	if node.terminal {
		return appendTerminalReflectedField(enc, key, value, replacement)
	}
	var seen map[reflectVisit]struct{}
	if len(parentSeen) > 0 {
		seen = parentSeen[0]
	}
	value, seen, leave, err := prepareReflectValue(value, depth, maxDepth, seen)
	if err != nil {
		return err
	}
	defer leave()
	if !value.IsValid() {
		return enc.AddReflected(key, nil)
	}
	if isTimeValue(value) {
		return enc.AddReflected(key, value.Interface())
	}

	switch value.Kind() {
	case reflect.Struct:
		return enc.AddObject(key, reflectObjectMarshaler{
			value:       value,
			rules:       node,
			target:      newReflectedObjectEncoder(enc, node, node, depth+1, maxDepth, replacement),
			depth:       depth + 1,
			maxDepth:    maxDepth,
			replacement: replacement,
			seen:        seen,
		})
	case reflect.Map:
		return enc.AddObject(key, reflectObjectMarshaler{
			value:       value,
			rules:       node,
			target:      newReflectedObjectEncoder(enc, node, node, depth+1, maxDepth, replacement),
			depth:       depth + 1,
			maxDepth:    maxDepth,
			replacement: replacement,
			seen:        seen,
		})
	case reflect.Array, reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 && !nodeHasArrayRules(node) {
			return enc.AddReflected(key, value.Interface())
		}
		arrayEncoder, ok := enc.(ArrayEncoder)
		if !ok {
			return errRedactedUnsupported
		}
		return enc.AddArray(key, reflectArrayMarshaler{
			value:       value,
			rules:       node,
			target:      newReflectedArrayEncoder(arrayEncoder, node, node, depth+1, maxDepth, replacement),
			depth:       depth + 1,
			maxDepth:    maxDepth,
			replacement: replacement,
			seen:        seen,
		})
	default:
		return enc.AddReflected(key, value.Interface())
	}
}

func baseArrayObjectEncoder(enc ArrayEncoder) ObjectEncoder {
	if objectEncoder, ok := enc.(ObjectEncoder); ok {
		return objectEncoder
	}
	if wrapper, ok := enc.(*redactingArrayEncoder); ok {
		if objectEncoder, ok := wrapper.ArrayEncoder.(ObjectEncoder); ok {
			return objectEncoder
		}
	}
	return nil
}

func appendReflectedValue(enc ArrayEncoder, value reflect.Value, node *redactionNode, depth, maxDepth int, replacement string, seen map[reflectVisit]struct{}) error {
	if node.terminal {
		return appendTerminalReflectedValue(enc, value, replacement)
	}
	value, seen, leave, err := prepareReflectValue(value, depth, maxDepth, seen)
	if err != nil {
		return err
	}
	defer leave()
	if !value.IsValid() {
		return enc.AppendReflected(nil)
	}
	if isTimeValue(value) {
		return enc.AppendReflected(value.Interface())
	}

	switch value.Kind() {
	case reflect.Struct:
		objectEncoder := baseArrayObjectEncoder(enc)
		if objectEncoder == nil {
			return errRedactedUnsupported
		}
		return enc.AppendObject(reflectObjectMarshaler{
			value:       value,
			rules:       node,
			target:      newReflectedObjectEncoder(objectEncoder, node, node, depth+1, maxDepth, replacement),
			depth:       depth + 1,
			maxDepth:    maxDepth,
			replacement: replacement,
			seen:        seen,
		})
	case reflect.Map:
		objectEncoder := baseArrayObjectEncoder(enc)
		if objectEncoder == nil {
			return errRedactedUnsupported
		}
		return enc.AppendObject(reflectObjectMarshaler{
			value:       value,
			rules:       node,
			target:      newReflectedObjectEncoder(objectEncoder, node, node, depth+1, maxDepth, replacement),
			depth:       depth + 1,
			maxDepth:    maxDepth,
			replacement: replacement,
			seen:        seen,
		})
	case reflect.Array, reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 && !nodeHasArrayRules(node) {
			return enc.AppendReflected(value.Interface())
		}
		return enc.AppendArray(reflectArrayMarshaler{
			value:       value,
			rules:       node,
			target:      newReflectedArrayEncoder(enc, node, node, depth+1, maxDepth, replacement),
			depth:       depth + 1,
			maxDepth:    maxDepth,
			replacement: replacement,
			seen:        seen,
		})
	default:
		return enc.AppendReflected(value.Interface())
	}
}

func appendTerminalReflectedField(enc ObjectEncoder, key string, value reflect.Value, replacement string) error {
	value = indirectReflectValue(value)
	if !value.IsValid() || (value.Kind() == reflect.Ptr && value.IsNil()) {
		return enc.AddReflected(key, nil)
	}
	switch value.Kind() {
	case reflect.Map:
		if value.IsNil() {
			return enc.AddReflected(key, nil)
		}
		return enc.AddObject(key, redactedObject{})
	case reflect.Array, reflect.Slice:
		if value.IsNil() {
			return enc.AddReflected(key, nil)
		}
		return enc.AddArray(key, redactedArray{})
	default:
		enc.AddString(key, replacement)
		return nil
	}
}

func appendTerminalReflectedValue(enc ArrayEncoder, value reflect.Value, replacement string) error {
	value = indirectReflectValue(value)
	if !value.IsValid() || (value.Kind() == reflect.Ptr && value.IsNil()) {
		return enc.AppendReflected(nil)
	}
	switch value.Kind() {
	case reflect.Map:
		if value.IsNil() {
			return enc.AppendReflected(nil)
		}
		return enc.AppendObject(redactedObject{})
	case reflect.Array, reflect.Slice:
		if value.IsNil() {
			return enc.AppendReflected(nil)
		}
		return enc.AppendArray(redactedArray{})
	default:
		enc.AppendString(replacement)
		return nil
	}
}

func prepareReflectValue(value reflect.Value, depth, maxDepth int, seen map[reflectVisit]struct{}) (reflect.Value, map[reflectVisit]struct{}, func(), error) {
	leave := func() {}
	if depth >= maxDepth {
		return value, nil, leave, errRedactionDepth
	}
	if !value.IsValid() {
		return value, nil, leave, nil
	}
	if seen == nil {
		seen = make(map[reflectVisit]struct{})
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return reflect.Value{}, seen, leave, nil
		}
		value = value.Elem()
	}
	if value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return reflect.Value{}, seen, leave, nil
		}
		if scalarCanMarshal(value) {
			return value, nil, leave, errRedactedMarshaler
		}
		visit := reflectVisit{kind: value.Kind(), ptr: value.Pointer()}
		if !enterReflectVisit(value, seen) {
			return value, nil, leave, errRedactedUnsupported
		}
		previousLeave := leave
		leave = func() {
			delete(seen, visit)
			previousLeave()
		}
		value = value.Elem()
	}
	if value.Kind() == reflect.Map || value.Kind() == reflect.Slice {
		if value.IsNil() {
			return reflect.Value{}, seen, leave, nil
		}
		visit := reflectVisit{kind: value.Kind(), ptr: value.Pointer()}
		if !enterReflectVisit(value, seen) {
			return value, nil, leave, errRedactedUnsupported
		}
		previousLeave := leave
		leave = func() {
			delete(seen, visit)
			previousLeave()
		}
	}
	if scalarCanMarshal(value) {
		return value, nil, leave, errRedactedMarshaler
	}
	return value, seen, leave, nil
}

func indirectReflectValue(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Ptr) {
		if value.IsNil() {
			return value
		}
		value = value.Elem()
	}
	return value
}

func enterReflectVisit(value reflect.Value, seen map[reflectVisit]struct{}) bool {
	visit := reflectVisit{kind: value.Kind(), ptr: value.Pointer()}
	if _, ok := seen[visit]; ok {
		return false
	}
	seen[visit] = struct{}{}
	return true
}

func isTimeValue(value reflect.Value) bool {
	return value.IsValid() && value.Type() == reflect.TypeOf(time.Time{})
}

func scalarCanMarshal(value reflect.Value) bool {
	if !value.IsValid() || !value.CanInterface() {
		return false
	}
	switch value.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		return false
	}
	if _, ok := value.Interface().(jsonMarshaler); ok {
		return true
	}
	if _, ok := value.Interface().(textMarshaler); ok {
		return true
	}
	if value.CanAddr() {
		if _, ok := value.Addr().Interface().(jsonMarshaler); ok {
			return true
		}
		if _, ok := value.Addr().Interface().(textMarshaler); ok {
			return true
		}
	}
	return false
}

func nodeHasArrayRules(node *redactionNode) bool {
	return node.anyArray != nil || len(node.arrays) > 0
}

func writeReflectObject(enc ObjectEncoder, value reflect.Value, node *redactionNode, depth, maxDepth int, replacement string, seen map[reflectVisit]struct{}) error {
	if value.Kind() == reflect.Struct {
		return writeReflectStruct(enc, value, node, depth, maxDepth, replacement, seen)
	}
	return writeReflectMap(enc, value, node, depth, maxDepth, replacement, seen)
}

func writeReflectStruct(enc ObjectEncoder, value reflect.Value, node *redactionNode, depth, maxDepth int, replacement string, seen map[reflectVisit]struct{}) error {
	for _, field := range reflectFields(value.Type()) {
		fieldValue := safeFieldByIndex(value, field.index)
		if !fieldValue.IsValid() || !fieldValue.CanInterface() {
			continue
		}

		if field.inline {
			inlineValue := indirectReflectValue(fieldValue)
			if !inlineValue.IsValid() || inlineValue.Kind() != reflect.Struct {
				continue
			}
			if err := writeReflectStruct(enc, inlineValue, node, depth, maxDepth, replacement, seen); err != nil {
				return err
			}
			continue
		}

		if field.omitempty && isEmptyReflectValue(fieldValue) {
			continue
		}

		child := node.reflectChild(field.name, fieldValue)
		if child == nil {
			if err := enc.AddReflected(field.name, fieldValue.Interface()); err != nil {
				return err
			}
			continue
		}

		if err := appendReflectedField(enc, field.name, fieldValue, child, depth, maxDepth, replacement, seen); err != nil {
			return err
		}
	}
	return nil
}

func reflectFields(typ reflect.Type) []reflectFieldInfo {
	fields := make([]reflectFieldInfo, 0, typ.NumField())
	for _, field := range reflect.VisibleFields(typ) {
		info := reflectFieldInfo{name: field.Name, index: field.Index}
		if tag := field.Tag.Get("json"); tag != "" {
			if tag == "-" {
				continue
			}
			parts := strings.Split(tag, ",")
			if parts[0] != "" {
				info.name = parts[0]
			}
			for _, option := range parts[1:] {
				if option == "omitempty" {
					info.omitempty = true
				}
			}
		} else if field.Anonymous {
			info.inline = true
		}
		fields = append(fields, info)
	}

	return fields
}

func safeFieldByIndex(value reflect.Value, index []int) reflect.Value {
	for _, i := range index {
		if value.Kind() == reflect.Ptr {
			if value.IsNil() {
				return reflect.Value{}
			}
			value = value.Elem()
		}
		if value.Kind() != reflect.Struct || i >= value.NumField() {
			return reflect.Value{}
		}
		value = value.Field(i)
	}
	return value
}

func writeReflectMap(enc ObjectEncoder, value reflect.Value, node *redactionNode, depth, maxDepth int, replacement string, seen map[reflectVisit]struct{}) error {
	keys := value.MapKeys()
	sort.Slice(keys, func(i, j int) bool {
		return reflectMapKey(keys[i]) < reflectMapKey(keys[j])
	})

	for _, key := range keys {
		name, err := reflectMapKeyValue(key)
		if err != nil {
			return err
		}
		mapValue := value.MapIndex(key)
		child := node.reflectChild(name, mapValue)
		if child == nil {
			if err := enc.AddReflected(name, mapValue.Interface()); err != nil {
				return err
			}
			continue
		}
		if err := appendReflectedField(enc, name, mapValue, child, depth, maxDepth, replacement, seen); err != nil {
			return err
		}
	}
	return nil
}

func reflectMapKey(value reflect.Value) string {
	name, _ := reflectMapKeyValue(value)
	return name
}

func reflectMapKeyValue(value reflect.Value) (string, error) {
	if value.Kind() == reflect.Interface {
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.String:
		return value.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(value.Uint(), 10), nil
	default:
		return "", errRedactedUnsupported
	}
}

func writeReflectArray(enc ArrayEncoder, value reflect.Value, node *redactionNode, depth, maxDepth int, replacement string, seen map[reflectVisit]struct{}) error {
	for i := 0; i < value.Len(); i++ {
		child := node.arrayChild(i)
		item := value.Index(i)
		if child == nil {
			if err := enc.AppendReflected(item.Interface()); err != nil {
				return err
			}
			continue
		}
		if err := appendReflectedValue(enc, item, child, depth, maxDepth, replacement, seen); err != nil {
			return err
		}
	}
	return nil
}

func isEmptyReflectValue(value reflect.Value) bool {
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return value.Len() == 0
	case reflect.Bool:
		return !value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return value.Float() == 0
	case reflect.Complex64, reflect.Complex128:
		return value.Complex() == 0
	case reflect.Interface, reflect.Ptr:
		return value.IsNil()
	default:
		return false
	}
}
