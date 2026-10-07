package aconfig

import (
	"fmt"
	"reflect"
	"strings"
)

// identityTag marks a struct field that identifies an element inside a slice
// of structs across configuration layers.
//
// When a slice of structs has a field tagged `identity:"true"` the loader does
// not replace the whole slice on every later layer (the default behaviour for
// regular slices). Instead it merges the slices by the value of this field:
//
//   - elements mentioned only by earlier layers are kept untouched,
//   - elements with the same identity are updated field by field,
//   - elements with a new identity are appended (in layer and input order),
//   - {"$delete": true} removes the element with the matching identity.
//
// An empty slice in a layer changes nothing and therefore differs from
// deleting every element explicitly.
const identityTag = "identity"

// deleteKey is the marker used inside an element to request its removal.
const deleteKey = "$delete"

// identityField describes the declared identity field of a slice element type.
type identityField struct {
	index int
	// keys are the accepted map keys for the identity value in decoded input,
	// in priority order: explicit format tags, then the Go field name.
	keys []string
}

// identityFieldOf returns the declared identity field of t, if exactly one is
// present. Multiple identity fields are a declaration error.
// t is expected to be a struct kind (pointer element types are dereferenced).
func identityFieldOf(t reflect.Type) (*identityField, bool, error) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, false, nil
	}

	var found *identityField
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.Tag.Get(identityTag) != "true" {
			continue
		}
		if found != nil {
			return nil, false, fmt.Errorf("type %s declares more than one %q field", t, identityTag)
		}
		found = &identityField{index: i, keys: identityKeys(sf)}
	}
	return found, found != nil, nil
}

// identityKeys enumerates the input keys that may carry an element identity:
// the JSON tag (used by the built-in decoder), other file-format tags and
// finally the Go field name itself.
func identityKeys(sf reflect.StructField) []string {
	seen := map[string]struct{}{}
	keys := make([]string, 0, 4)
	add := func(k string) {
		if k == "" || k == "-" {
			return
		}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	addTag := func(tag string) {
		name := strings.TrimSpace(sf.Tag.Get(tag))
		if name == "" {
			return
		}
		if before, _, ok := strings.Cut(name, ","); ok {
			add(before)
		} else {
			add(name)
		}
	}

	addTag("json")
	for _, format := range []string{"yaml", "toml", "hcl", "env"} {
		addTag(format)
	}
	add(sf.Name)
	return keys
}

// canonicalIdentity renders an identity value in a stable, comparable form.
func canonicalIdentity(v reflect.Value) string {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	return fmt.Sprintf("%v", v.Interface())
}

// identityFromValue extracts the identity of an already decoded slice element.
func (id *identityField) identityFromValue(v reflect.Value) (string, error) {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return "", fmt.Errorf("slice element without an identity (nil element)")
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return "", fmt.Errorf("identity field requires a struct element, got %s", v.Kind())
	}
	return canonicalIdentity(v.Field(id.index)), nil
}

// identityFromMap extracts the identity of a decoded element and also returns
// the raw value so callers can verify it.
func (id *identityField) identityFromMap(m map[string]any) (string, any, bool) {
	for _, key := range id.keys {
		if raw, ok := m[key]; ok {
			return rawIdentity(raw), raw, true
		}
	}
	for _, key := range id.keys {
		for k, v := range m {
			if strings.EqualFold(k, key) {
				return rawIdentity(v), v, true
			}
		}
	}
	return "", nil, false
}

// rawIdentity normalizes the identity value produced by a file decoder.
func rawIdentity(raw any) string {
	switch v := raw.(type) {
	case nil:
		return ""
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// isDelete reports whether an element requests deletion.
func isDelete(m map[string]any) bool {
	v, ok := m[deleteKey]
	if !ok {
		return false
	}
	switch d := v.(type) {
	case bool:
		return d
	case string:
		return d == "true"
	default:
		return false
	}
}

// mergeIdentifiedSlice merges the decoded incoming slice into the current
// value of a slice whose element type declares an identity field.
//
// The returned slice keeps the order of the base layer; appended elements keep
// their input order. Neither the incoming decoded maps nor the base layer are
// mutated while building the result.
func (l *Loader) mergeIdentifiedSlice(field *fieldData, incoming any, id *identityField) error {
	in, err := toAnySlice(incoming)
	if err != nil {
		return err
	}

	elemType := field.field.Type.Elem()
	out := reflect.MakeSlice(field.field.Type, 0, field.value.Len()+len(in))
	pos := map[string]int{}

	// Copy the base layer, rejecting duplicated or missing identities.
	for i := 0; i < field.value.Len(); i++ {
		elem := field.value.Index(i)
		ident, err := id.identityFromValue(elem)
		if err != nil {
			return err
		}
		if ident == "" {
			return fmt.Errorf("slice element at index %d has an empty identity", i)
		}
		if _, dup := pos[ident]; dup {
			return fmt.Errorf("duplicated identity %q in slice", ident)
		}
		pos[ident] = out.Len()
		out = reflect.Append(out, deepCopyValue(elem))
	}

	seenInLayer := map[string]struct{}{}

	// An empty incoming slice simply iterates zero times: the base layer is
	// kept untouched, which differs from deleting every element.
	for _, rawElem := range in {
		m, err := asMap(rawElem)
		if err != nil {
			return err
		}

		// Copy the map so markers can be removed without touching the object
		// produced by the source decoder.
		element := make(map[string]any, len(m))
		for k, v := range m {
			element[k] = v
		}

		ident, _, found := id.identityFromMap(element)
		if !found || ident == "" {
			return fmt.Errorf("slice element without an identity: %v", rawElem)
		}
		if _, dup := seenInLayer[ident]; dup {
			return fmt.Errorf("duplicated identity %q in slice", ident)
		}
		seenInLayer[ident] = struct{}{}

		idx, exists := pos[ident]
		deleting := isDelete(m)
		delete(element, deleteKey)

		switch {
		case exists && deleting:
			out = removeSliceIndex(out, idx)
			reindexPositions(out, id, pos)
			// The identity is released; a later element in the same layer may
			// re-add it (which then appends as a brand new element).
			delete(seenInLayer, ident)
			continue
		case !exists && deleting:
			return fmt.Errorf("cannot delete element with unknown identity %q", ident)
		case !exists:
			pos[ident] = out.Len()
			newElem := reflect.New(elemType).Elem()
			if err := l.decodeMapInto(newElem, element); err != nil {
				return err
			}
			got, err := id.identityFromValue(newElem)
			if err != nil {
				return err
			}
			if got != ident {
				return fmt.Errorf("identity of new element %q is inconsistent (got %q)", ident, got)
			}
			out = reflect.Append(out, newElem)
		default:
			// Update the existing element; nested values go through the
			// regular setter. The resulting identity must remain the one by
			// which the element was matched, never a value carried by update.
			target := out.Index(idx)
			if err := l.decodeMapInto(target, element); err != nil {
				return err
			}
			got, err := id.identityFromValue(target)
			if err != nil {
				return err
			}
			if got != ident {
				return fmt.Errorf("identity of element %q cannot be changed (got %q)", ident, got)
			}
		}
	}

	field.value.Set(out)
	return nil
}

// decodeMapInto decodes a decoded map[string]any into the addressable value.
func (l *Loader) decodeMapInto(dst reflect.Value, m map[string]any) error {
	if dst.Kind() == reflect.Ptr {
		if dst.IsNil() {
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		dst = dst.Elem()
	}
	if dst.Kind() == reflect.Struct {
		// m2s resolves the real reflect.StructField for every key, which keeps
		// field tags (and nested identified slices) working as expected.
		return l.m2s(m, dst)
	}
	fd := l.newFieldData(reflect.StructField{}, dst, nil)
	return l.setFieldData(fd, m)
}

// toAnySlice normalizes the slice shapes produced by the various decoders.
func toAnySlice(v any) ([]any, error) {
	switch s := v.(type) {
	case []any:
		return s, nil
	case []map[string]any:
		out := make([]any, len(s))
		for i := range s {
			out[i] = s[i]
		}
		return out, nil
	case []map[any]any:
		out := make([]any, len(s))
		for i := range s {
			out[i] = mii(s[i])
		}
		return out, nil
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice {
			return nil, fmt.Errorf("expected a slice, got %T", v)
		}
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = rv.Index(i).Interface()
		}
		return out, nil
	}
}

func asMap(v any) (map[string]any, error) {
	switch m := v.(type) {
	case map[string]any:
		return m, nil
	case map[any]any:
		return mii(m), nil
	default:
		return nil, fmt.Errorf("expected an object element, got %T", v)
	}
}

func removeSliceIndex(s reflect.Value, idx int) reflect.Value {
	out := reflect.MakeSlice(s.Type(), s.Len()-1, s.Len()-1)
	reflect.Copy(out, s.Slice(0, idx))
	reflect.Copy(out.Slice(idx, out.Len()), s.Slice(idx+1, s.Len()))
	return out
}

func reindexPositions(s reflect.Value, id *identityField, pos map[string]int) {
	for k := range pos {
		delete(pos, k)
	}
	for i := 0; i < s.Len(); i++ {
		ident, err := id.identityFromValue(s.Index(i))
		if err != nil {
			continue
		}
		pos[ident] = i
	}
}

// deepCopyValue returns a value that shares no pointer/map/slice/interface
// storage with v, so later layers never mutate an earlier layer's data.
func deepCopyValue(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return v
		}
		cp := reflect.New(v.Type().Elem())
		cp.Elem().Set(deepCopyValue(v.Elem()))
		return cp
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		return deepCopyValue(reflect.ValueOf(v.Interface()))
	case reflect.Struct:
		cp := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			if cp.Field(i).CanSet() {
				cp.Field(i).Set(deepCopyValue(v.Field(i)))
			}
		}
		return cp
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		cp := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			cp.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return cp
	case reflect.Array:
		cp := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			cp.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return cp
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		cp := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			cp.SetMapIndex(deepCopyValue(iter.Key()), deepCopyValue(iter.Value()))
		}
		return cp
	default:
		return v
	}
}
