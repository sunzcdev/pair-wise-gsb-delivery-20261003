package mapstructure

import (
	"fmt"
	"reflect"
	"sort"
)

// buildOps compares the untouched target with the shadow value produced by
// the dry-run decode and enumerates the resulting assignments.
func buildOps(rootName string, old, neu, input reflect.Value, pl *planner) []Op {
	b := &opBuilder{planner: pl}
	b.diff(rootName, nil, old, neu, input)
	return b.ops
}

type opBuilder struct {
	planner *planner
	ops     []Op
}

func (b *opBuilder) emit(op Op) {
	b.ops = append(b.ops, op)
}

// diff enumerates assignments for one node. path is the structural location,
// name the decoder-style name, old the current value, neu the shadow value
// and in the corresponding (best effort) input node.
func (b *opBuilder) diff(name string, path []PathSegment, old, neu, in reflect.Value) {
	if info, _, ok := b.planner.underOpaque(name); ok {
		b.emit(Op{
			Kind:       OpUserCode,
			Path:       clonePath(path),
			Name:       name,
			TargetType: neu.Type(),
			Callback:   info.callback,
		})
		return
	}

	if !neu.IsValid() {
		return
	}

	switch neu.Kind() {
	case reflect.Interface:
		if neu.IsNil() {
			if old.IsValid() && old.Kind() == reflect.Interface && !old.IsNil() {
				b.emitZero(name, path, neu.Type())
			} else {
				b.emitRetain(name, path, neu.Type())
			}
			return
		}
		// Interface values are replaced wholesale; the concrete value is not
		// merged in place.
		if !old.IsValid() && old.Kind() == reflect.Interface && !old.IsNil() &&
			old.Elem().Type() == neu.Elem().Type() && valuesEqual(old.Elem(), neu.Elem()) {
			b.emitRetain(name, path, neu.Type())
			return
		}
		b.emitSet(name, path, old, neu, in)

	case reflect.Ptr:
		if neu.IsNil() {
			if old.IsValid() && (old.Kind() != reflect.Ptr || !old.IsNil()) {
				b.emitZero(name, path, neu.Type())
			} else {
				b.emitRetain(name, path, neu.Type())
			}
			return
		}
		switch {
		case !old.IsValid() || old.Kind() != reflect.Ptr:
			// Type change: replace wholesale.
			b.emitSet(name, path, old, neu, in)
			old = reflect.New(neu.Type().Elem())
		case old.IsNil():
			// The decoder allocates a new pointee; record the allocation.
			b.emitSet(name, path, old, neu, in)
			old = reflect.New(neu.Type().Elem())
		}
		childPath := append(clonePath(path), PathSegment{Kind: DerefSegment})
		b.diff(name, childPath, old.Elem(), neu.Elem(), indirectForInput(in))

	case reflect.Struct:
		if !old.IsValid() || old.Kind() != reflect.Struct {
			b.emitSet(name, path, old, neu, in)
			return
		}
		b.diffStruct(name, path, old, neu, in)

	case reflect.Map:
		if !old.IsValid() || old.Kind() != reflect.Map {
			b.emitSet(name, path, old, neu, in)
			return
		}
		b.diffMap(name, path, old, neu, in)

	case reflect.Slice:
		b.emitSet(name, path, old, neu, in)

	case reflect.Array:
		if !old.IsValid() || old.Kind() != reflect.Array {
			b.emitSet(name, path, old, neu, in)
			return
		}
		for i := 0; i < neu.Len(); i++ {
			childName := fmt.Sprintf("%s[%d]", name, i)
			childPath := append(clonePath(path), PathSegment{Kind: IndexSegment, Index: i})
			var childIn reflect.Value
			if in.IsValid() && (in.Kind() == reflect.Array || in.Kind() == reflect.Slice) && i < in.Len() {
				childIn = in.Index(i)
			}
			b.diff(childName, childPath, old.Index(i), neu.Index(i), childIn)
		}

	default:
		if valuesEqual(old, neu) {
			b.emitRetain(name, path, neu.Type())
		} else {
			b.emitSet(name, path, old, neu, in)
		}
	}
}

func (b *opBuilder) diffStruct(name string, path []PathSegment, old, neu, in reflect.Value) {
	typ := neu.Type()
	walkStructFields(typ, func(fieldChain []int, fieldName string, squashed bool) {
		newField := fieldByChain(neu, fieldChain)
		if !newField.IsValid() || !newField.CanSet() {
			return
		}
		childName := joinName(name, fieldName)
		oldField := fieldByChain(old, fieldChain)

		var childIn reflect.Value
		if in.IsValid() {
			childIn = findInputForField(in, fieldName)
		}

		childPath := append(clonePath(path), PathSegment{
			Kind:  FieldSegment,
			Field: goFieldChain(typ, fieldChain),
		})
		// Zeroed maps are rebuilt key by key inside diffMap so every deleted
		// and written entry is listed; other kinds are a plain zero.
		if b.planner.zeroed[childName] && newField.Kind() != reflect.Map {
			b.emitZero(childName, childPath, newField.Type())
			return
		}
		b.diff(childName, childPath, oldField, newField, childIn)
	})
}

// walkStructFields visits every decodable field, descending into squashed
// (anonymous) struct fields. fieldName is the tag/map key name used by the
// decoder; squashed fields contribute no name segment.
func walkStructFields(typ reflect.Type, visit func(chain []int, fieldName string, squashed bool)) {
	var walk func(t reflect.Type, prefix []int, squashed bool)
	walk = func(t reflect.Type, prefix []int, squashed bool) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			chain := append(append([]int(nil), prefix...), i)
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			// Only exported embedded structs are flattened here, mirroring
			// decodeStructFromMap. (The decoder additionally descends into
			// anonymous pointer-to-struct fields; fieldByChain handles those.)
			if f.Anonymous && f.PkgPath == "" && ft.Kind() == reflect.Struct {
				walk(ft, chain, true)
				continue
			}
			if f.PkgPath != "" {
				continue
			}
			name := f.Name
			if tag, _ := getTagValue(f, "mapstructure"); tag != "" {
				if key := splitTag(tag); key != "" && key != "-" {
					name = key
				}
			}
			visit(chain, name, squashed)
		}
	}
	walk(typ, nil, false)
}

func fieldByChain(v reflect.Value, chain []int) reflect.Value {
	for _, i := range chain {
		if v.Kind() == reflect.Ptr {
			if v.IsNil() {
				return reflect.Value{}
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}
		}
		v = v.Field(i)
	}
	return v
}

// goFieldChain renders an index chain as "Outer>Leaf" Go field names so apply
// can descend into embedded structs without storing reflect metadata.
func goFieldChain(typ reflect.Type, chain []int) string {
	names := make([]string, 0, len(chain))
	t := typ
	for _, i := range chain {
		f := t.Field(i)
		names = append(names, f.Name)
		t = f.Type
		if t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
	}
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ">"
		}
		out += n
	}
	return out
}

func (b *opBuilder) diffMap(name string, path []PathSegment, old, neu, in reflect.Value) {
	zeroed := b.planner.zeroed[name]

	keySet := map[any]bool{}
	addKey := func(k reflect.Value) {
		if k.IsValid() {
			keySet[boxKey(k)] = true
		}
	}
	for _, k := range old.MapKeys() {
		addKey(k)
	}
	for _, k := range neu.MapKeys() {
		addKey(k)
	}

	keys := make([]any, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return fmt.Sprintf("%v", keys[i]) < fmt.Sprintf("%v", keys[j])
	})

	for _, key := range keys {
		keyValOld := lookupMapKey(old, key)
		keyValNew := lookupMapKey(neu, key)
		childName := fmt.Sprintf("%s[%v]", name, key)
		keySeg := PathSegment{Kind: KeySegment, Key: key}
		childPath := append(clonePath(path), keySeg)

		var childIn reflect.Value
		if in.IsValid() && in.Kind() == reflect.Map {
			childIn = lookupMapKey(in, key)
		}

		switch {
		case !keyValNew.IsValid():
			if zeroed {
				b.emitZero(childName, childPath, mapValueType(neu, old, key))
			} else {
				b.emitRetain(childName, childPath, mapValueType(old, neu, key))
			}
		case !keyValOld.IsValid():
			b.diff(childName, childPath, reflect.Value{}, keyValNew, childIn)
		default:
			b.diff(childName, childPath, keyValOld, keyValNew, childIn)
		}
	}
}

func mapValueType(a, b reflect.Value, key any) reflect.Type {
	if a.IsValid() && a.Kind() == reflect.Map {
		if v := lookupMapKey(a, key); v.IsValid() {
			return v.Type()
		}
		return a.Type().Elem()
	}
	if b.IsValid() && b.Kind() == reflect.Map {
		return b.Type().Elem()
	}
	return nil
}

func lookupMapKey(m reflect.Value, key any) reflect.Value {
	if !m.IsValid() || m.Kind() != reflect.Map {
		return reflect.Value{}
	}
	kv := reflect.ValueOf(key)
	if kv.IsValid() && kv.Type().AssignableTo(m.Type().Key()) {
		if v := m.MapIndex(kv); v.IsValid() {
			return v
		}
	}
	for _, mk := range m.MapKeys() {
		if boxKey(mk) == key {
			return m.MapIndex(mk)
		}
	}
	return reflect.Value{}
}

func boxKey(k reflect.Value) any {
	if !k.IsValid() {
		return nil
	}
	for k.Kind() == reflect.Interface {
		if k.IsNil() {
			return nil
		}
		k = k.Elem()
	}
	return k.Interface()
}

// findInputForField resolves the input entry feeding a struct field using the
// same direct then case-insensitive lookup as decodeStructFromMap.
func findInputForField(in reflect.Value, fieldName string) reflect.Value {
	if in.Kind() == reflect.Map {
		direct := reflect.ValueOf(fieldName)
		if direct.Type().AssignableTo(in.Type().Key()) {
			if v := in.MapIndex(direct); v.IsValid() {
				return v
			}
		}
		for _, k := range in.MapKeys() {
			if ks, ok := boxKey(k).(string); ok && equalFold(ks, fieldName) {
				return in.MapIndex(k)
			}
		}
	}
	return reflect.Value{}
}

func indirectForInput(in reflect.Value) reflect.Value {
	for in.IsValid() && in.Kind() == reflect.Ptr {
		if in.IsNil() {
			return reflect.Value{}
		}
		in = in.Elem()
	}
	return in
}

func derefKind(t reflect.Type) reflect.Kind {
	if t.Kind() == reflect.Ptr {
		return t.Elem().Kind()
	}
	return t.Kind()
}

func splitTag(tag string) string {
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i]
		}
	}
	return tag
}

func joinName(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

func clonePath(p []PathSegment) []PathSegment {
	out := make([]PathSegment, len(p))
	copy(out, p)
	return out
}

func (b *opBuilder) emitZero(name string, path []PathSegment, typ reflect.Type) {
	b.emit(Op{Kind: OpZero, Path: clonePath(path), Name: name, TargetType: typ})
}

func (b *opBuilder) emitRetain(name string, path []PathSegment, typ reflect.Type) {
	b.emit(Op{Kind: OpRetain, Path: clonePath(path), Name: name, TargetType: typ})
}

func (b *opBuilder) emitSet(name string, path []PathSegment, old, neu, in reflect.Value) {
	var value any
	if neu.CanInterface() {
		value = neu.Interface()
	}
	b.emit(Op{
		Kind:       OpSet,
		Path:       clonePath(path),
		Name:       name,
		TargetType: neu.Type(),
		Value:      value,
		Conversion: b.describeConversion(name, neu.Type(), in),
	})
}

func (b *opBuilder) describeConversion(name string, target reflect.Type, in reflect.Value) string {
	src, ok := b.planner.inputTypes[name]
	if !ok || src == nil {
		if in.IsValid() {
			src = in.Type()
		} else {
			return fmt.Sprintf("assign %s", target)
		}
	}
	if src.AssignableTo(target) {
		return fmt.Sprintf("%s -> %s", src, target)
	}
	return fmt.Sprintf("convert %s to %s", src, target)
}

func valuesEqual(a, b reflect.Value) bool {
	if !a.IsValid() {
		return !b.IsValid() || b.IsZero()
	}
	if !b.IsValid() {
		return a.IsZero()
	}
	if a.Type() != b.Type() {
		return false
	}
	if !a.Type().Comparable() || !b.Type().Comparable() {
		return reflect.DeepEqual(safeInterface(a), safeInterface(b))
	}
	return a.Interface() == b.Interface()
}

// equalFold mirrors the default MatchName (strings.EqualFold).
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		c, d := a[i], b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if d >= 'A' && d <= 'Z' {
			d += 'a' - 'A'
		}
		if c != d {
			return false
		}
	}
	return true
}
