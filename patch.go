package validator

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
)

// PatchFields describes which struct fields were included in a PATCH request
// body, independently of the values that were decoded.
//
// Because a field may be set to its zero value or explicitly sent as null,
// presence must never be inferred from the decoded value. Use
// PatchFieldsFromJSON to build one from the raw request body, or construct
// one manually from another protocol's field mask.
//
// Keys use the struct's Go field names (not json tags) and may be namespaced
// relative to the struct passed to StructPatch, eg. "End" or
// "Window.Start" or "Items[0].Name". Both this relative form and the form
// prefixed with the struct type name, as accepted by StructPartial, are
// accepted.
type PatchFields map[string]struct{}

// PatchLevel gives struct-level and field-level validation functions access
// to PATCH request information. It is an optional interface: a StructLevel
// or FieldLevel received outside StructPatch does not implement it, so
// existing custom functions keep compiling unchanged. Type-assert before
// use:
//
//	if pl, ok := sl.(validator.PatchLevel); ok {
//	    if pl.IsPresent("End") { ... }
//	}
type PatchLevel interface {
	// IsPresent reports whether field, a Go-field path relative to the
	// current struct (eg. "End" or "Window.Start"), was included in the
	// PATCH body. An explicit null counts as present; an omitted field does
	// not.
	IsPresent(field string) bool

	// OldTop returns the previous state passed to StructPatch, dereferenced
	// to the struct value. It is invalid when no previous state was given.
	OldTop() reflect.Value
}

// PatchFieldLevel is the field-level extension of PatchLevel. In addition to
// presence and the previous top-level state it exposes the previous value of
// the field currently being validated.
type PatchFieldLevel interface {
	PatchLevel

	// OldField returns the field's value in the previous state, or an invalid
	// Value when the field did not exist there (eg. new slice index, or no
	// previous state provided).
	OldField() reflect.Value
}

// NewPatchFields returns a PatchFields containing the given fields.
// Fields may be provided in a namespaced fashion relative to the struct,
// eg. "End", "Window.Start" or "Items[0].Name".
func NewPatchFields(fields ...string) PatchFields {
	pf := make(PatchFields, len(fields))
	for _, f := range fields {
		pf[f] = struct{}{}
	}
	return pf
}

// Add marks a field as present and returns the PatchFields for chaining.
func (pf PatchFields) Add(field string) PatchFields {
	pf[field] = struct{}{}
	return pf
}

// registerPatchPrefixes records every ancestor prefix of a full field path so
// that traversal reaches deep fields even when the mask lists only the leaf
// (eg. "Window.End" also gates "Window"). A prefix is recorded whenever some
// present key extends it through a field (".") or collection index ("[").
func registerPatchPrefixes(prefixes map[string]struct{}, path string) {
	// Walk the path and record each prefix that names a field or collection
	// element on the way to the leaf. Segments are separated by '.' and the
	// '[' ... ']' index notation, mirroring StructPartial key accumulation:
	// "patchOrder.Items[0].Name" yields
	// "patchOrder", "patchOrder.Items", "patchOrder.Items[0]".
	end := 0
	for end < len(path) {
		switch path[end] {
		case '.':
			prefixes[path[:end]] = struct{}{}
			end++
		case '[':
			// the collection field itself precedes the first index
			prefixes[path[:end]] = struct{}{}
			close := strings.IndexByte(path[end:], ']')
			if close < 0 {
				return
			}
			end += close + 1
			prefixes[path[:end]] = struct{}{}
		default:
			end++
		}
	}
}

// Has reports whether the given field path was present in the request.
func (pf PatchFields) Has(field string) bool {
	_, ok := pf[field]
	return ok
}

// PatchFieldsFromJSON extracts the set of fields present in a JSON object
// without unmarshalling into the struct itself. This is what lets validation
// distinguish "field omitted" from "field sent as null": null and zero
// values are still recorded as present.
//
// s is a value of the same struct type that the body decodes into; it is
// used only to map JSON keys (honouring `json` tags and encoding/json's case
// insensitivity) onto Go field names and to descend into nested types.
// The returned paths use Go field names, eg. {"window":{"end":null}} for a
// struct field named Window.End yields "Window.End".
//
// Fields inside nested objects, maps and arrays are flattened with the same
// notation used by StructPartial, eg. "Items[0].Name".
func PatchFieldsFromJSON(s interface{}, data []byte) (PatchFields, error) {
	val := reflect.ValueOf(s)
	for val.Kind() == reflect.Ptr {
		if val.IsNil() {
			return nil, &InvalidValidationError{Type: reflect.TypeOf(s)}
		}
		val = val.Elem()
	}

	if val.Kind() != reflect.Struct || val.Type().ConvertibleTo(timeType) {
		return nil, &InvalidValidationError{Type: reflect.TypeOf(s)}
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var raw interface{}
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}

	obj, ok := raw.(map[string]interface{})
	if !ok {
		// A JSON `null` body or a non-object body provides no present fields.
		return PatchFields{}, nil
	}

	pf := make(PatchFields)
	collectJSONFields(pf, val.Type(), nil, obj)
	return pf, nil
}

// collectJSONFields walks a decoded JSON object alongside the struct type and
// records every provided key as a present Go-field path. prefix holds the
// accumulated path segments.
func collectJSONFields(pf PatchFields, typ reflect.Type, prefix []byte, obj map[string]interface{}) {
	for key, value := range obj {
		field, embedPath, ok := jsonStructFieldWithEmbed(typ, key)
		if !ok {
			// Unknown field: record the wire name; it matches no struct tag.
			pf[string(appendField(prefix, key))] = struct{}{}
			continue
		}

		path := appendField(prefix, field.Name)
		pf[string(path)] = struct{}{}

		fieldType := derefType(field.Type)

		// Promoted field reached through one or more anonymous embedded
		// structs. JSON omits the embedding segment(s) but the validator's
		// namespace includes them (Type.Embed.Field), so record that variant.
		if len(embedPath) > 0 && len(prefix) == 0 {
			embedded := append([]byte(nil), embedPath...)
			embedded = appendField(embedded, field.Name)
			pf[string(embedded)] = struct{}{}
		}

		switch value := value.(type) {
		case map[string]interface{}:
			switch fieldType.Kind() {
			case reflect.Struct:
				if !fieldType.ConvertibleTo(timeType) {
					collectJSONFields(pf, fieldType, path, value)
				}
			case reflect.Map:
				if elemType := derefStruct(fieldType.Elem()); elemType != nil {
					collectJSONMapFields(pf, elemType, path, value)
				}
			}

		case []interface{}:
			if fieldType.Kind() == reflect.Slice || fieldType.Kind() == reflect.Array {
				collectJSONArrayFields(pf, derefType(fieldType.Elem()), path, value)
			}
		}
	}
}

func collectJSONMapFields(pf PatchFields, valType reflect.Type, prefix []byte, obj map[string]interface{}) {
	for key, value := range obj {
		path := appendIndex(prefix, key)
		pf[string(path)] = struct{}{}

		switch value := value.(type) {
		case map[string]interface{}:
			if valType != nil {
				collectJSONFields(pf, valType, path, value)
			}
		case []interface{}:
			collectJSONArrayFields(pf, derefType(valType), path, value)
		}
	}
}

func collectJSONArrayFields(pf PatchFields, elemType reflect.Type, prefix []byte, arr []interface{}) {
	structType := derefStruct(elemType)

	for i, value := range arr {
		path := appendIndex(prefix, strconv.Itoa(i))
		pf[string(path)] = struct{}{}

		switch value := value.(type) {
		case map[string]interface{}:
			if structType != nil {
				collectJSONFields(pf, structType, path, value)
			}
		case []interface{}:
			if elemType.Kind() == reflect.Slice || elemType.Kind() == reflect.Array {
				collectJSONArrayFields(pf, derefType(elemType.Elem()), path, value)
			}
		}
	}
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

func derefStruct(t reflect.Type) reflect.Type {
	t = derefType(t)
	if t.Kind() == reflect.Struct && !t.ConvertibleTo(timeType) {
		return t
	}
	return nil
}

// jsonStructFieldWithEmbed is like jsonStructField but also returns the path
// of anonymous embedded struct names traversed to reach a promoted field.
// Direct (non-promoted) fields return a nil embedPath.
func jsonStructFieldWithEmbed(typ reflect.Type, key string) (field reflect.StructField, embedPath []byte, found bool) {
	if f, p, ok := jsonFieldExactEmbed(typ, key, nil); ok {
		return f, p, true
	}
	if f, p, ok := jsonFieldFoldEmbed(typ, key, nil); ok {
		return f, p, true
	}
	return reflect.StructField{}, nil, false
}

func jsonFieldExactEmbed(typ reflect.Type, key string, embed []byte) (reflect.StructField, []byte, bool) {
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue
		}
		name, skip := jsonFieldName(f)
		if skip {
			continue
		}
		if name == key {
			return f, embed, true
		}
		if f.Anonymous {
			ft := derefType(f.Type)
			if ft.Kind() != reflect.Struct {
				continue
			}
			childEmbed := embed
			if name == "" {
				childEmbed = appendField(embed, f.Name)
			}
			if sf, p, ok := jsonFieldExactEmbed(ft, key, childEmbed); ok {
				return sf, p, true
			}
		}
	}
	return reflect.StructField{}, nil, false
}

func jsonFieldFoldEmbed(typ reflect.Type, key string, embed []byte) (reflect.StructField, []byte, bool) {
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue
		}
		name, skip := jsonFieldName(f)
		if skip {
			continue
		}
		if strings.EqualFold(name, key) {
			return f, embed, true
		}
		if f.Anonymous {
			ft := derefType(f.Type)
			if ft.Kind() != reflect.Struct {
				continue
			}
			childEmbed := embed
			if name == "" {
				childEmbed = appendField(embed, f.Name)
			}
			if sf, p, ok := jsonFieldFoldEmbed(ft, key, childEmbed); ok {
				return sf, p, true
			}
		}
	}
	return reflect.StructField{}, nil, false
}

// jsonFieldName returns the JSON name used for matching: the explicit tag
// name, the Go field name for a tagged field, and an empty name for an
// untagged anonymous field whose fields are promoted. skip mirrors json:"-".
func jsonFieldName(f reflect.StructField) (name string, skip bool) {
	tag := jsonExplicitTagName(f)
	if tag == "-" {
		return "", true
	}
	if tag != "" {
		return tag, false
	}
	if f.Anonymous {
		return "", false
	}
	return f.Name, false
}

// jsonExplicitTagName returns the name portion of a json tag, without
// falling back to the Go field name.
func jsonExplicitTagName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

func appendField(prefix []byte, name string) []byte {
	out := make([]byte, 0, len(prefix)+len(name)+1)
	out = append(out, prefix...)
	if len(out) > 0 {
		out = append(out, '.')
	}
	out = append(out, name...)
	return out
}

func appendIndex(prefix []byte, index string) []byte {
	out := make([]byte, 0, len(prefix)+len(index)+2)
	out = append(out, prefix...)
	out = append(out, '[')
	out = append(out, index...)
	out = append(out, ']')
	return out
}

// patchFieldPresent reports whether field (a single Go field name) or a
// descendant of it was included in the PATCH body. structNs is the enclosing
// struct's namespace, eg. "Booking." or "Booking.Window.".
func (v *validate) patchFieldPresent(structNs []byte, name string) bool {
	key := string(append(structNs, name...))
	if _, ok := v.patchFields[key]; ok {
		return true
	}

	// The field is also visited when a descendant of it was submitted
	// (eg. mask "Items[0].Name" must still traverse the Items collection).
	if _, ok := v.patchPrefixes[key]; ok {
		return true
	}

	return false
}

// patchElementPresent gates a dive element. elementName is the element's
// field name relative to the enclosing collection (eg. "Items[0]");
// structNs is the collection parent's namespace (eg. "patchOrder."), so the
// full key "patchOrder.Items[0]" is checked.
func (v *validate) patchElementPresent(structNs []byte, elementName string) bool {
	key := string(append(structNs, elementName...))
	if _, ok := v.patchFields[key]; ok {
		return true
	}
	if _, ok := v.patchPrefixes[key]; ok {
		return true
	}
	return false
}

// patchTargetPresent reports whether a field referenced by a relative
// namespace (the validator tag param, eg. "Start" or "Inner.Start") was
// submitted in the PATCH body. It uses the field-level parent's struct
// namespace; when no field is currently on the stack it falls back to the
// struct-level namespace.
func (v *validate) patchTargetPresent(namespace string) bool {
	prefix := v.patchPrefixNs
	if len(prefix) == 0 {
		prefix = v.actualNs
	}

	key := string(append(prefix, namespace...))
	if _, ok := v.patchFields[key]; ok {
		return true
	}

	// Top-level cross-struct tags (eqcsfield=...) are anchored at the root;
	// check the root namespace as well.
	root := v.rootNamespace()
	if len(root) > 0 && !strings.HasPrefix(key, string(root)) {
		rootKey := string(append(root, namespace...))
		if _, ok := v.patchFields[rootKey]; ok {
			return true
		}
	}

	return false
}

// patchOldBase maps a "new" struct value (the base of a cross-field lookup)
// onto its previous-state mirror. The immediate parent is tracked through
// traversal; arbitrary bases supplied via GetStructFieldOKAdvanced or
// cross-struct tags are located by walking the old root.
func (v *validate) patchOldBase(val reflect.Value) reflect.Value {
	if !val.IsValid() || !v.oldTop.IsValid() {
		return reflect.Value{}
	}

	if v.oldParent.IsValid() && valuesEqualStruct(val, v.slflParent) {
		return v.oldParent
	}

	if valuesEqualStruct(val, v.top) {
		return v.oldTop
	}

	return findMatchingValue(v.oldTop, val)
}

func (v *validate) rootNamespace() []byte {
	name := v.oldTop.Type().Name()
	if len(name) == 0 {
		return nil
	}
	return []byte(name + ".")
}

// IsPresent implements PatchLevel. The path is resolved against the current
// struct: the struct-level namespace when called from a StructLevel function,
// or the field-level parent namespace when called from a FieldLevel function.
func (v *validate) IsPresent(field string) bool {
	prefix := v.patchPrefixNs
	if len(prefix) == 0 {
		prefix = v.actualNs
	}

	key := string(append(prefix, field...))
	if _, ok := v.patchFields[key]; ok {
		return true
	}

	root := v.rootNamespace()
	if len(root) > 0 && !strings.HasPrefix(key, string(root)) {
		if _, ok := v.patchFields[string(append(root, field...))]; ok {
			return true
		}
	}

	return false
}

// OldTop implements PatchLevel.
func (v *validate) OldTop() reflect.Value {
	return v.oldTop
}

// OldField implements PatchFieldLevel.
func (v *validate) OldField() reflect.Value {
	return v.oldField
}

// valuesEqualStruct reports whether two reflect.Values reference the same
// underlying addressable object, falling back to interface equality for
// non-addressable values.
func valuesEqualStruct(a, b reflect.Value) bool {
	if !a.IsValid() || !b.IsValid() {
		return false
	}
	if a.Kind() == reflect.Ptr && b.Kind() == reflect.Ptr && !a.IsNil() && !b.IsNil() {
		return a.Pointer() == b.Pointer()
	}
	if a.CanAddr() && b.CanAddr() && a.Type() == b.Type() {
		return a.UnsafeAddr() == b.UnsafeAddr()
	}
	if a.CanInterface() && b.CanInterface() {
		ai, bi := a.Interface(), b.Interface()
		return reflect.DeepEqual(ai, bi)
	}
	return false
}

// findMatchingValue walks value looking for the same addressable object as
// target, descending into pointers, structs, slices, arrays and maps.
func findMatchingValue(value, target reflect.Value) reflect.Value {
	if !value.IsValid() {
		return reflect.Value{}
	}

	if valuesEqualStruct(value, target) {
		return value
	}

	switch value.Kind() {
	case reflect.Ptr, reflect.Interface:
		if value.IsNil() {
			return reflect.Value{}
		}
		return findMatchingValue(value.Elem(), target)

	case reflect.Struct:
		if value.Type().ConvertibleTo(timeType) {
			return reflect.Value{}
		}
		for i := 0; i < value.NumField(); i++ {
			if found := findMatchingValue(value.Field(i), target); found.IsValid() {
				return found
			}
		}

	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if found := findMatchingValue(value.Index(i), target); found.IsValid() {
				return found
			}
		}

	case reflect.Map:
		for _, key := range value.MapKeys() {
			if found := findMatchingValue(value.MapIndex(key), target); found.IsValid() {
				return found
			}
		}
	}

	return reflect.Value{}
}
