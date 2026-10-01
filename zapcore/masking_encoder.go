package zapcore

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap/buffer"
)

const (
	defaultMaskingReplacement = "[REDACTED]"
	maxMaskingDepth           = 10000
	maxMaskingPathSegments    = 64
	maxMaskingPathSegmentSize = 1024
)

var (
	errMaskingEncoding = errors.New("value could not be redacted")
	maskAllNode        = &maskNode{terminal: true, maskElements: true, elementMasked: true}
)

type maskNode struct {
	terminal      bool
	maskElements  bool
	elementMasked bool
	children      map[string]*maskNode
	containsRules bool
}

func (n *maskNode) child(key string) *maskNode {
	if n == maskAllNode {
		return maskAllNode
	}
	if n == nil {
		return nil
	}
	return n.children[key]
}

func (n *maskNode) hasRules() bool {
	return n == maskAllNode || n != nil && n.containsRules
}

// NewMaskingEncoder wraps enc so values at complete field paths are replaced
// with replacement. Array boundaries do not add a named path segment. A
// trailing "[]" replaces every primitive element in an array. With no paths,
// enc is returned unchanged so callers retain the original encoder's fast
// path.
func NewMaskingEncoder(enc Encoder, replacement string, paths ...string) (Encoder, error) {
	if enc == nil {
		return nil, errors.New("cannot mask a nil encoder")
	}
	if len(paths) == 0 {
		return enc, nil
	}
	if replacement == "" {
		replacement = defaultMaskingReplacement
	}

	rules := &maskNode{}
	for _, path := range paths {
		if err := addMaskingPath(rules, path); err != nil {
			return nil, fmt.Errorf("invalid masking path %q: %w", path, err)
		}
	}
	finishMaskNode(rules)

	object := &maskingObject{
		delegate:    enc,
		rules:       rules,
		replacement: replacement,
	}
	return &maskingEncoder{delegate: enc, maskingObject: object}, nil
}

func addMaskingPath(root *maskNode, path string) error {
	if path == "" {
		return errors.New("path is empty")
	}

	node := root
	segments := 0
	for len(path) > 0 {
		if path[0] == '.' {
			return errors.New("path contains an empty field")
		}
		segment, remaining, crossedArray := nextMaskingPathSegment(path)
		if segment == "" {
			return errors.New("path contains an invalid or empty field")
		}
		if path != segment && remaining == "" && strings.HasSuffix(path, ".") {
			return errors.New("path contains an empty field")
		}
		segments++
		if segments > maxMaskingPathSegments {
			return errors.New("path has too many segments")
		}

		next := node.child(segment)
		if next == nil {
			if node.children == nil {
				node.children = make(map[string]*maskNode)
			}
			next = &maskNode{}
			node.children[segment] = next
		}

		if crossedArray {
			next.maskElements = true
			next.elementMasked = true
			if remaining == "" {
				next.terminal = true
			}
		}
		if len(segment) > maxMaskingPathSegmentSize {
			return errors.New("path segment is too long")
		}

		path = remaining
		node = next
	}
	node.terminal = true
	return nil
}

func nextMaskingPathSegment(path string) (string, string, bool) {
	end := strings.IndexAny(path, ".[")
	if end < 0 {
		return path, "", false
	}

	segment := path[:end]
	rest := path[end:]
	crossedArray := false
	for strings.HasPrefix(rest, "[]") {
		crossedArray = true
		rest = rest[2:]
	}
	if strings.HasPrefix(rest, "[") || rest != "" && !strings.HasPrefix(rest, ".") {
		return "", path, false
	}
	if strings.HasPrefix(rest, ".") {
		rest = rest[1:]
	}
	return segment, rest, crossedArray
}

func finishMaskNode(node *maskNode) bool {
	node.containsRules = node.terminal || node.maskElements
	for _, child := range node.children {
		if finishMaskNode(child) {
			node.containsRules = true
		}
	}
	return node.containsRules
}

type maskingEncoder struct {
	delegate Encoder
	*maskingObject
}

func (e *maskingEncoder) Clone() Encoder {
	delegate := e.delegate.Clone()
	return &maskingEncoder{
		delegate:      delegate,
		maskingObject: e.maskingObject.clone(delegate),
	}
}

func (e *maskingEncoder) EncodeEntry(entry Entry, fields []Field) (*buffer.Buffer, error) {
	cloned := e.Clone().(*maskingEncoder)
	addFields(cloned, fields)
	return cloned.delegate.EncodeEntry(entry, nil)
}

type maskingObject struct {
	delegate    ObjectEncoder
	rules       *maskNode
	replacement string
	depth       int
}

func (o *maskingObject) clone(delegate ObjectEncoder) *maskingObject {
	return &maskingObject{
		delegate:    delegate,
		rules:       o.rules,
		replacement: o.replacement,
		depth:       o.depth,
	}
}

func (o *maskingObject) child(key string) *maskNode {
	if o.rules == maskAllNode || o.depth >= maxMaskingDepth {
		return maskAllNode
	}
	return o.rules.child(key)
}

func (o *maskingObject) masks(key string) bool {
	child := o.child(key)
	return child != nil && child.terminal
}

func (o *maskingObject) prepareField(field Field) Field {
	if !o.masks(field.Key) {
		return field
	}
	switch field.Type {
	case StringerType, ErrorType:
		return Field{Key: field.Key, Type: StringType, String: o.replacement}
	default:
		return field
	}
}

func (o *maskingObject) AddArray(key string, marshaler ArrayMarshaler) error {
	child := o.child(key)
	if child == nil || !child.hasRules() {
		return o.delegate.AddArray(key, marshaler)
	}
	if child.terminal {
		if !child.maskElements {
			o.delegate.AddString(key, o.replacement)
			return nil
		}
	}

	return o.delegate.AddArray(key, ArrayMarshalerFunc(func(arr ArrayEncoder) error {
		arrayRules := child
		if child.terminal {
			arrayRules = cloneMaskingArrayNode(child)
		}
		return runMaskedArray(marshaler, newMaskingArrayEncoder(
			arr, arrayRules, o.replacement, o.depth+1,
		))
	}))
}

func (o *maskingObject) AddObject(key string, marshaler ObjectMarshaler) error {
	child := o.child(key)
	if child == nil || !child.hasRules() {
		return o.delegate.AddObject(key, marshaler)
	}
	if child.terminal {
		o.delegate.AddString(key, o.replacement)
		return nil
	}

	return o.delegate.AddObject(key, ObjectMarshalerFunc(func(delegate ObjectEncoder) error {
		return runMaskedObject(marshaler, &maskingObject{
			delegate:    delegate,
			rules:       child,
			replacement: o.replacement,
			depth:       o.depth + 1,
		})
	}))
}

func (o *maskingObject) AddBinary(key string, value []byte) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddBinary(key, value)
}

func (o *maskingObject) AddByteString(key string, value []byte) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddByteString(key, value)
}

func (o *maskingObject) AddBool(key string, value bool) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddBool(key, value)
}

func (o *maskingObject) AddComplex128(key string, value complex128) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddComplex128(key, value)
}

func (o *maskingObject) AddComplex64(key string, value complex64) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddComplex64(key, value)
}

func (o *maskingObject) AddDuration(key string, value time.Duration) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddDuration(key, value)
}

func (o *maskingObject) AddFloat64(key string, value float64) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddFloat64(key, value)
}

func (o *maskingObject) AddFloat32(key string, value float32) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddFloat32(key, value)
}

func (o *maskingObject) AddInt(key string, value int) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddInt(key, value)
}

func (o *maskingObject) AddInt64(key string, value int64) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddInt64(key, value)
}

func (o *maskingObject) AddInt32(key string, value int32) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddInt32(key, value)
}

func (o *maskingObject) AddInt16(key string, value int16) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddInt16(key, value)
}

func (o *maskingObject) AddInt8(key string, value int8) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddInt8(key, value)
}

func (o *maskingObject) AddString(key, value string) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddString(key, value)
}

func (o *maskingObject) AddTime(key string, value time.Time) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddTime(key, value)
}

func (o *maskingObject) AddUint(key string, value uint) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddUint(key, value)
}

func (o *maskingObject) AddUint64(key string, value uint64) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddUint64(key, value)
}

func (o *maskingObject) AddUint32(key string, value uint32) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddUint32(key, value)
}

func (o *maskingObject) AddUint16(key string, value uint16) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddUint16(key, value)
}

func (o *maskingObject) AddUint8(key string, value uint8) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddUint8(key, value)
}

func (o *maskingObject) AddUintptr(key string, value uintptr) {
	if o.masks(key) {
		o.delegate.AddString(key, o.replacement)
		return
	}
	o.delegate.AddUintptr(key, value)
}

func (o *maskingObject) OpenNamespace(key string) {
	o.delegate.OpenNamespace(key)
	o.rules = o.child(key)
	o.depth++
}

func cloneMaskingArrayNode(node *maskNode) *maskNode {
	cloned := *node
	cloned.elementMasked = true
	return &cloned
}

func runMaskedObject(marshaler ObjectMarshaler, enc *maskingObject) (err error) {
	defer func() {
		if recover() != nil {
			err = errMaskingEncoding
		}
	}()
	if err = marshaler.MarshalLogObject(enc); err != nil {
		return errMaskingEncoding
	}
	return nil
}

func runMaskedArray(marshaler ArrayMarshaler, enc *maskingArrayEncoder) (err error) {
	defer func() {
		if recover() != nil {
			err = errMaskingEncoding
		}
	}()
	if err = marshaler.MarshalLogArray(enc); err != nil {
		return errMaskingEncoding
	}
	return nil
}
