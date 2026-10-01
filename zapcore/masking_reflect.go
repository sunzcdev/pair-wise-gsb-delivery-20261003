package zapcore

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type maskingVisit struct {
	kind reflect.Kind
	ptr  uintptr
}

func (o *maskingObject) AddReflected(key string, value interface{}) error {
	child := o.child(key)
	if child == nil || !child.hasRules() {
		return o.delegate.AddReflected(key, value)
	}
	if child.terminal {
		o.delegate.AddString(key, o.replacement)
		return nil
	}
	return addMaskedReflected(
		o.delegate, key, value, child, o.replacement, o.depth+1, nil,
	)
}

func addMaskedReflected(
	enc ObjectEncoder,
	key string,
	value interface{},
	rules *maskNode,
	replacement string,
	depth int,
	visiting map[maskingVisit]bool,
) error {
	rv := reflect.ValueOf(value)
	if value == nil || !rv.IsValid() {
		return enc.AddReflected(key, nil)
	}
	if depth > maxMaskingDepth {
		return errMaskingEncoding
	}
	if maskingValueIsNil(rv) {
		return enc.AddReflected(key, nil)
	}

	opaque, err := maskingOpaqueValue(rv, visiting)
	if err != nil {
		return err
	}
	if opaque {
		if rules.hasRules() {
			enc.AddString(key, replacement)
			return nil
		}
		return enc.AddReflected(key, value)
	}

	switch rv.Kind() {
	case reflect.Struct, reflect.Map:
		return enc.AddObject(key, ObjectMarshalerFunc(func(delegate ObjectEncoder) error {
			return withMaskingRecovery(func() error {
				return encodeMaskedReflected(
					delegate, rv, rules, replacement, depth+1, visiting,
				)
			})
		}))
	case reflect.Slice, reflect.Array:
		arrayRules := rules
		if rules.terminal {
			arrayRules = cloneMaskingArrayNode(rules)
		}
		return enc.AddArray(key, ArrayMarshalerFunc(func(delegate ArrayEncoder) error {
			return withMaskingRecovery(func() error {
				return encodeMaskedReflectedArray(
					delegate, rv, arrayRules, replacement, depth+1, visiting,
				)
			})
		}))
	default:
		enc.AddString(key, replacement)
		return nil
	}
}

func appendMaskedReflected(
	enc ArrayEncoder,
	value interface{},
	rules *maskNode,
	replacement string,
	depth int,
	visiting map[maskingVisit]bool,
) error {
	rv := reflect.ValueOf(value)
	if value == nil || !rv.IsValid() {
		return enc.AppendReflected(nil)
	}
	if depth > maxMaskingDepth {
		return errMaskingEncoding
	}
	if maskingValueIsNil(rv) {
		return enc.AppendReflected(nil)
	}

	opaque, err := maskingOpaqueValue(rv, visiting)
	if err != nil {
		return err
	}
	if opaque {
		if rules.hasRules() {
			enc.AppendString(replacement)
			return nil
		}
		return enc.AppendReflected(value)
	}

	switch rv.Kind() {
	case reflect.Struct, reflect.Map:
		return enc.AppendObject(ObjectMarshalerFunc(func(delegate ObjectEncoder) error {
			return withMaskingRecovery(func() error {
				return encodeMaskedReflected(
					delegate, rv, rules, replacement, depth+1, visiting,
				)
			})
		}))
	case reflect.Slice, reflect.Array:
		arrayRules := rules
		if rules.terminal {
			arrayRules = cloneMaskingArrayNode(rules)
		}
		return enc.AppendArray(ArrayMarshalerFunc(func(delegate ArrayEncoder) error {
			return withMaskingRecovery(func() error {
				return encodeMaskedReflectedArray(
					delegate, rv, arrayRules, replacement, depth+1, visiting,
				)
			})
		}))
	default:
		enc.AppendString(replacement)
		return nil
	}
}

func maskingOpaqueValue(
	rv reflect.Value,
	visiting map[maskingVisit]bool,
) (bool, error) {
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return true, nil
		}
		if rv.Kind() == reflect.Pointer {
			visit := maskingVisit{kind: rv.Kind(), ptr: rv.Pointer()}
			if visiting[visit] {
				return false, errMaskingEncoding
			}
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return true, nil
		}
	}

	if implementsMaskingOpaque(rv.Type()) ||
		rv.CanAddr() && implementsMaskingOpaque(rv.Addr().Type()) {
		return true, nil
	}
	switch rv.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice:
		return false, nil
	default:
		return true, nil
	}
}

func maskingValueIsNil(rv reflect.Value) bool {
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return true
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func withMaskingRecovery(fn func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = errMaskingEncoding
		}
	}()
	return fn()
}

func implementsMaskingOpaque(typ reflect.Type) bool {
	jsonMarshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshaler := reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	return typ.Implements(jsonMarshaler) || typ.Implements(textMarshaler)
}

func enterMaskingVisit(
	visiting map[maskingVisit]bool,
	rv reflect.Value,
) (map[maskingVisit]bool, maskingVisit, bool) {
	if rv.Kind() != reflect.Pointer && rv.Kind() != reflect.Map &&
		rv.Kind() != reflect.Slice {
		return visiting, maskingVisit{}, true
	}
	if rv.IsNil() {
		return visiting, maskingVisit{}, true
	}

	visit := maskingVisit{kind: rv.Kind(), ptr: rv.Pointer()}
	if visiting[visit] {
		return visiting, visit, false
	}
	if visiting == nil {
		visiting = make(map[maskingVisit]bool)
	}
	visiting[visit] = true
	return visiting, visit, true
}

func encodeMaskedReflected(
	enc ObjectEncoder,
	rv reflect.Value,
	rules *maskNode,
	replacement string,
	depth int,
	visiting map[maskingVisit]bool,
) error {
	if depth > maxMaskingDepth {
		return errMaskingEncoding
	}

	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return errMaskingEncoding
		}
		var ok bool
		var visit maskingVisit
		visiting, visit, ok = enterMaskingVisit(visiting, rv)
		if !ok {
			return errMaskingEncoding
		}
		defer delete(visiting, visit)
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Struct:
		return encodeMaskedReflectedStruct(enc, rv, rules, replacement, depth, visiting)
	case reflect.Map:
		if rv.IsNil() {
			return errMaskingEncoding
		}
		return encodeMaskedReflectedMap(enc, rv, rules, replacement, depth, visiting)
	default:
		return errMaskingEncoding
	}
}

func encodeMaskedReflectedStruct(
	enc ObjectEncoder,
	rv reflect.Value,
	rules *maskNode,
	replacement string,
	depth int,
	visiting map[maskingVisit]bool,
) error {
	if depth > maxMaskingDepth {
		return errMaskingEncoding
	}

	for _, field := range reflect.VisibleFields(rv.Type()) {
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}

		name, options, hasExplicitName := maskingJSONFieldName(field)
		if name == "-" {
			continue
		}

		value, ok := maskingVisibleField(rv, field.Index)
		if !ok {
			continue
		}

		if field.Anonymous && !hasExplicitName {
			inline := value
			for inline.Kind() == reflect.Pointer {
				if inline.IsNil() {
					return nil
				}
				inline = inline.Elem()
			}
			if inline.Kind() == reflect.Struct {
				if err := encodeMaskedReflectedStruct(
					enc, inline, rules, replacement, depth+1, visiting,
				); err != nil {
					return err
				}
				continue
			}
		}

		if maskingOmitEmpty(options) && value.IsZero() {
			continue
		}

		child := rules.child(name)
		if child != nil && child.terminal {
			enc.AddString(name, replacement)
			continue
		}
		if child == nil || !child.hasRules() {
			if err := enc.AddReflected(name, value.Interface()); err != nil {
				return errMaskingEncoding
			}
			continue
		}
		if err := addMaskedReflected(
			enc, name, value.Interface(), child, replacement, depth+1, visiting,
		); err != nil {
			return err
		}
	}
	return nil
}

func maskingVisibleField(rv reflect.Value, index []int) (reflect.Value, bool) {
	if len(index) == 0 {
		return reflect.Value{}, false
	}

	value := rv
	for _, i := range index {
		for value.Kind() == reflect.Pointer {
			if value.IsNil() {
				return reflect.Value{}, false
			}
			value = value.Elem()
		}
		if value.Kind() != reflect.Struct || i >= value.NumField() {
			return reflect.Value{}, false
		}
		value = value.Field(i)
	}
	return value, value.CanInterface()
}

func maskingJSONFieldName(field reflect.StructField) (name, options string, explicit bool) {
	first, options, _ := strings.Cut(field.Tag.Get("json"), ",")
	if first == "-" {
		return "-", "", false
	}
	if first != "" {
		return first, options, true
	}
	return field.Name, options, false
}

func maskingOmitEmpty(options string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == "omitempty" {
			return true
		}
	}
	return false
}

func encodeMaskedReflectedMap(
	enc ObjectEncoder,
	rv reflect.Value,
	rules *maskNode,
	replacement string,
	depth int,
	visiting map[maskingVisit]bool,
) error {
	if rv.IsNil() {
		return enc.AddReflected("", nil)
	}
	if depth > maxMaskingDepth {
		return errMaskingEncoding
	}

	var ok bool
	var visit maskingVisit
	visiting, visit, ok = enterMaskingVisit(visiting, rv)
	if !ok {
		return errMaskingEncoding
	}
	defer delete(visiting, visit)

	keys, err := sortedMapKeys(rv.MapKeys())
	if err != nil {
		return err
	}
	for _, item := range keys {
		value := rv.MapIndex(item.value)
		if !value.CanInterface() {
			continue
		}

		child := rules.child(item.name)
		if child != nil && child.terminal {
			enc.AddString(item.name, replacement)
			continue
		}
		if child == nil || !child.hasRules() {
			if err := enc.AddReflected(item.name, value.Interface()); err != nil {
				return errMaskingEncoding
			}
			continue
		}
		if err := addMaskedReflected(
			enc, item.name, value.Interface(), child, replacement, depth+1, visiting,
		); err != nil {
			return err
		}
	}
	return nil
}

type maskingMapKey struct {
	name  string
	value reflect.Value
}

func sortedMapKeys(keys []reflect.Value) ([]maskingMapKey, error) {
	items := make([]maskingMapKey, 0, len(keys))
	for _, key := range keys {
		name, err := maskingMapKeyName(key)
		if err != nil {
			return nil, err
		}
		items = append(items, maskingMapKey{name: name, value: key})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].name < items[j].name
	})
	return items, nil
}

func maskingMapKeyName(key reflect.Value) (string, error) {
	for key.Kind() == reflect.Interface {
		key = key.Elem()
	}
	switch key.Kind() {
	case reflect.String:
		return key.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(key.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
		reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(key.Uint(), 10), nil
	default:
		if text, ok := key.Interface().(encoding.TextMarshaler); ok {
			if bytes, err := text.MarshalText(); err == nil {
				return string(bytes), nil
			}
		}
		return fmt.Sprint(key.Interface()), nil
	}
}

func encodeMaskedReflectedArray(
	enc ArrayEncoder,
	rv reflect.Value,
	rules *maskNode,
	replacement string,
	depth int,
	visiting map[maskingVisit]bool,
) error {
	if depth > maxMaskingDepth {
		return errMaskingEncoding
	}
	if rv.Kind() == reflect.Slice {
		if rv.IsNil() {
			return enc.AppendReflected(nil)
		}
		var ok bool
		var visit maskingVisit
		visiting, visit, ok = enterMaskingVisit(visiting, rv)
		if !ok {
			return errMaskingEncoding
		}
		defer delete(visiting, visit)
	}

	for i := 0; i < rv.Len(); i++ {
		value := rv.Index(i)
		if !value.CanInterface() {
			return errMaskingEncoding
		}

		primitive := maskingArrayElementIsPrimitive(value)
		if primitive && rules.elementMasked || rules == maskAllNode {
			enc.AppendString(replacement)
			continue
		}
		if !rules.hasRules() {
			if err := enc.AppendReflected(value.Interface()); err != nil {
				return errMaskingEncoding
			}
			continue
		}
		if err := appendMaskedReflected(
			enc, value.Interface(), rules, replacement, depth+1, visiting,
		); err != nil {
			return err
		}
	}
	return nil
}

func maskingArrayElementIsPrimitive(rv reflect.Value) bool {
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return true
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		return false
	default:
		return true
	}
}
