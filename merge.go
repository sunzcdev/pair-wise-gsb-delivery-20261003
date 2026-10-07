package aconfig

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// mergeTag is the struct tag that marks an identity field of an element.
// It is set on a slice of structs and names the Go field that uniquely
// identifies an element, e.g. `json:"pools" merge:"name"`.
const mergeTag = "merge"

// deleteMarker removes an element by its identity when present in an
// overlay layer, e.g. {"name": "a", "$delete": true}.
const deleteMarker = "$delete"

// fieldSchema describes the shape of a parsedField value.
// It is used while merging config levels to recognize slices of
// structs that must be merged element-by-element by an identity field.
type fieldSchema struct {
	// elem is set for slices/arrays and describes a single element.
	elem *fieldSchema

	// isSlice tells that this schema describes a slice/array value.
	// elem is non-nil only when the element is a struct (or a pointer to it).
	isSlice bool

	// fields describes struct fields keyed by every name they may have in a
	// decoded file (field name and each of the supported format tags).
	fields map[string]*fieldSchema

	// identityKeys lists the possible decoded names of the identity field.
	// Only set on schemas that describe an element of an identity slice.
	identityKeys []string
}

// fileFormats are the file decoder formats aconfig ships with.
var fileFormats = []string{"json", "yaml", "toml", "hcl"}

var (
	schemaMu    sync.Mutex
	schemaCache = map[reflect.Type]*fieldSchema{}
)

// schemaFor builds (and caches) the schema of a struct type.
func schemaFor(t reflect.Type) *fieldSchema {
	t = derefType(t)

	schemaMu.Lock()
	s, ok := schemaCache[t]
	schemaMu.Unlock()
	if ok {
		return s
	}

	// Reserve an entry to stop recursive types from looping forever.
	s = &fieldSchema{fields: map[string]*fieldSchema{}}
	schemaMu.Lock()
	schemaCache[t] = s
	schemaMu.Unlock()

	buildStructSchema(t, s)
	return s
}

func buildStructSchema(t reflect.Type, s *fieldSchema) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}

		fs := fieldSchemaFor(field)

		switch field.Type.Kind() {
		case reflect.Struct:
			if field.Anonymous {
				// Identity tags are not valid on embedded fields.
				if field.Tag.Get(mergeTag) != "" {
					panic(fmt.Sprintf("aconfig: %q tag is not allowed on embedded field %q", mergeTag, field.Name))
				}
				for k, v := range fs.fields {
					s.fields[k] = v
				}
				continue
			}
		case reflect.Pointer:
			if field.Anonymous && field.Type.Elem().Kind() == reflect.Struct {
				if field.Tag.Get(mergeTag) != "" {
					panic(fmt.Sprintf("aconfig: %q tag is not allowed on embedded field %q", mergeTag, field.Name))
				}
				for k, v := range fs.fields {
					s.fields[k] = v
				}
				continue
			}
		}

		for _, name := range fieldNames(field) {
			s.fields[name] = fs
		}
	}
}

func typeSchema(t reflect.Type) *fieldSchema {
	switch t.Kind() {
	case reflect.Struct:
		return schemaFor(t)

	case reflect.Pointer:
		// []*Struct keeps the struct element schema so identity merge
		// works the same as for value elements.
		if t.Elem().Kind() == reflect.Struct {
			return &fieldSchema{isSlice: false, elem: nil, fields: schemaFor(t.Elem()).fields}
		}
		return &fieldSchema{}

	case reflect.Slice, reflect.Array:
		fs := &fieldSchema{isSlice: true}
		if elem := derefType(t.Elem()); elem.Kind() == reflect.Struct {
			// Copy the element schema so an identity declared on one slice
			// field never leaks into another slice of the same type.
			es := *schemaFor(elem)
			es.identityKeys = nil
			fs.elem = &es
		}
		return fs

	default:
		return &fieldSchema{}
	}
}

// fieldSchemaFor builds the schema of a struct field and applies the
// optional identity tag declared on that field.
func fieldSchemaFor(field reflect.StructField) *fieldSchema {
	fs := typeSchema(field.Type)
	if id := field.Tag.Get(mergeTag); id != "" {
		setIdentity(fs, field, id)
	}
	return fs
}

func setIdentity(fs *fieldSchema, field reflect.StructField, identity string) {
	if !fs.isSlice {
		panic(fmt.Sprintf("aconfig: %q tag is allowed only on a slice of structs, field %q", mergeTag, field.Name))
	}

	elemType := derefType(field.Type.Elem())
	if field.Type.Elem().Kind() != reflect.Struct &&
		!(field.Type.Elem().Kind() == reflect.Pointer && elemType.Kind() == reflect.Struct) {
		panic(fmt.Sprintf("aconfig: %q tag requires a slice of structs, field %q", mergeTag, field.Name))
	}
	if fs.elem == nil {
		panic(fmt.Sprintf("aconfig: %q tag requires a slice of structs, field %q", mergeTag, field.Name))
	}

	idField, ok := findIdentityField(elemType, identity)
	if !ok {
		panic(fmt.Sprintf("aconfig: %q tag on field %q refers to missing field %q", mergeTag, field.Name, identity))
	}

	fs.elem.identityKeys = fieldNames(idField)
}

// findIdentityField resolves the merge tag against the element struct.
// The tag names the Go field (case-insensitive); a tag value matching a
// serialized key (json/yaml/toml/hcl) is accepted as well.
func findIdentityField(t reflect.Type, identity string) (reflect.StructField, bool) {
	if f, ok := t.FieldByName(identity); ok {
		return f, true
	}

	var byGoName, byFileKey *reflect.StructField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if strings.EqualFold(f.Name, identity) {
			byGoName = &f
		}
		for _, name := range fieldNames(f) {
			if name == identity {
				byFileKey = &f
			}
		}
	}
	switch {
	case byGoName != nil:
		return *byGoName, true
	case byFileKey != nil:
		return *byFileKey, true
	default:
		return reflect.StructField{}, false
	}
}

// fieldNames returns every name a struct field may have in a decoded file:
// the lower-cased Go name and each explicit format tag.
func fieldNames(field reflect.StructField) []string {
	names := []string{strings.ToLower(field.Name)}
	tags := append(append([]string{}, fileFormats...), "env", "flag")
	for _, tag := range tags {
		if name := tagName(field.Tag.Get(tag)); name != "" {
			names = append(names, name)
		}
	}
	return dedupeStrings(names)
}

func dedupeStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func tagName(tag string) string {
	if tag == "" || tag == "-" {
		return ""
	}
	name := tag
	if before, _, ok := strings.Cut(name, ","); ok {
		name = before
	}
	if name == "-" || name == "" {
		return ""
	}
	return name
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// mergeValues merges an overlay value into the current (base) value.
// schema describes the destination field; when it is nil plain replacement
// is used. Values returned never share mutable maps or slices with overlay,
// so the objects produced by the file decoders are never modified.
func mergeValues(base, overlay any, schema *fieldSchema, path string) (any, error) {
	switch ov := overlay.(type) {
	case map[string]any:
		bm, _ := base.(map[string]any)
		return mergeObject(bm, ov, schema, path)

	case map[any]any:
		bm, _ := base.(map[string]any)
		return mergeObject(bm, normalizeMap(ov), schema, path)

	case []any:
		if schema != nil && schema.isSlice && schema.elem != nil && len(schema.elem.identityKeys) != 0 {
			bs, _ := base.([]any)
			return mergeIdentitySlice(bs, ov, schema.elem, path)
		}
		// Plain slices (primitives, or struct slices without identity)
		// follow the original replacement rule.
		return deepCopy(ov), nil

	default:
		return overlay, nil
	}
}

func mergeObject(base, overlay map[string]any, schema *fieldSchema, path string) (map[string]any, error) {
	if base == nil {
		base = map[string]any{}
	}

	// Copy the base so decoded objects are never mutated.
	result := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		result[k] = v
	}

	for key, overlayVal := range overlay {
		childPath := path + "." + key

		var childSchema *fieldSchema
		if schema != nil && schema.fields != nil {
			childSchema = schema.fields[key]
		}

		// Do not let a property update change the identity of an element.
		var idKey string
		var isID bool
		if schema != nil {
			idKey, isID = schema.identityKeyName(key)
		}
		if isID {
			if cur, ok := result[key]; ok && !identityEqual(cur, overlayVal) {
				return nil, fmt.Errorf("identity field %q at %q cannot be changed: %v vs %v",
					idKey, path, cur, overlayVal)
			}
		}

		merged, err := mergeValues(result[key], overlayVal, childSchema, childPath)
		if err != nil {
			return nil, err
		}
		result[key] = merged
	}
	return result, nil
}

func mergeIdentitySlice(base, overlay []any, elemSchema *fieldSchema, path string) ([]any, error) {
	idKeys := elemSchema.identityKeys

	// Empty overlay means "no changes"; it differs from deleting everything.
	if len(overlay) == 0 {
		return base, nil
	}

	indexByID := map[string]int{}
	result := make([]any, 0, len(base)+len(overlay))

	for _, raw := range base {
		item, err := asItemMap(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}

		id, _, ok, err := itemIdentity(item, idKeys, path)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%s: element is missing identity field %q", path, idKeys)
		}
		if _, dup := indexByID[id]; dup {
			return nil, fmt.Errorf("%s: duplicate identity %q", path, id)
		}
		indexByID[id] = len(result)
		result = append(result, deepCopy(item))
	}

	seen := map[string]struct{}{}
	for i, raw := range overlay {
		item, err := asItemMap(raw)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", path, i, err)
		}

		id, idKey, ok, err := itemIdentity(item, idKeys, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%s[%d]: element is missing identity field %q", path, i, idKeys)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%s[%d]: duplicate identity %q", path, i, item[idKey])
		}
		seen[id] = struct{}{}

		if isDeleteMarker(item) {
			if idx, exists := indexByID[id]; exists {
				result = removeAt(result, idx)
				indexByID = buildIndex(result, idKeys, path)
			}
			continue
		}

		if idx, exists := indexByID[id]; exists {
			merged, err := mergeObject(asMap(result[idx]), item, elemSchema, path)
			if err != nil {
				return nil, err
			}
			result[idx] = merged
			continue
		}

		// New element: append preserving layer and input order.
		indexByID[id] = len(result)
		result = append(result, deepCopy(item))
	}

	return result, nil
}

func removeAt(s []any, idx int) []any {
	out := make([]any, 0, len(s)-1)
	out = append(out, s[:idx]...)
	out = append(out, s[idx+1:]...)
	return out
}

func buildIndex(items []any, idKeys []string, path string) map[string]int {
	index := make(map[string]int, len(items))
	for i, raw := range items {
		item, err := asItemMap(raw)
		if err != nil {
			return index
		}
		id, _, ok, _ := itemIdentity(item, idKeys, path)
		if ok {
			index[id] = i
		}
	}
	return index
}

// identityKeyName reports whether key is one of the element identity names.
func (s *fieldSchema) identityKeyName(key string) (string, bool) {
	if s == nil {
		return "", false
	}
	for _, name := range s.identityKeys {
		if name == key {
			return name, true
		}
	}
	return "", false
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func asItemMap(raw any) (map[string]any, error) {
	switch v := raw.(type) {
	case map[string]any:
		return v, nil
	case map[any]any:
		return normalizeMap(v), nil
	default:
		return nil, fmt.Errorf("identity slice element must be an object, got %T", raw)
	}
}

func itemIdentity(item map[string]any, keys []string, path string) (string, string, bool, error) {
	var key string
	var val any
	var ok bool
	for _, candidate := range keys {
		if v, found := item[candidate]; found {
			key, val, ok = candidate, v, true
			break
		}
	}
	if !ok {
		return "", "", false, nil
	}
	id := fmt.Sprintf("%T:%v", val, fmt.Sprint(val))
	if id == "" {
		return "", "", false, fmt.Errorf("%s: empty identity field %q", path, key)
	}
	return id, key, true, nil
}

func identityEqual(a, b any) bool {
	return fmt.Sprintf("%T:%v", a, a) == fmt.Sprintf("%T:%v", b, b)
}

func isDeleteMarker(item map[string]any) bool {
	switch v := item[deleteMarker].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

func normalizeMap(m map[any]any) map[string]any {
	res := make(map[string]any, len(m))
	for k, v := range m {
		res[fmt.Sprint(k)] = normalizeValue(v)
	}
	return res
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case map[any]any:
		return normalizeMap(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalizeValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalizeValue(e)
		}
		return out
	default:
		return v
	}
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case map[any]any:
		return normalizeMap(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}
