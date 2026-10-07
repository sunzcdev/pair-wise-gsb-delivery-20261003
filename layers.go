package aconfig

import (
	"fmt"
	"reflect"
	"strings"
)

// Delete is a sentinel value that a Layer may use to explicitly revoke
// a value provided by a lower-priority source (defaults or a previous layer).
//
// It is a dedicated typed value, not a reserved string, so it can never
// collide with a legitimate configuration value (an empty string included).
var Delete = deleteMarker{}

type deleteMarker struct{}

func isDelete(value any) bool {
	_, ok := value.(deleteMarker)
	return ok
}

// Layer is a single source of configuration values for LoadLayers.
// Layers are applied in order: the first layer has the lowest priority.
//
// Values are keyed by the field names of the JSON format (the same names
// a JSON file would use), either flat ("database.host") or nested maps.
// A nil value is ignored. The Delete sentinel revokes the field (or the
// whole subtree when used on a parent key) from lower-priority sources.
type Layer struct {
	Name   string         // Name is used in error messages only.
	Values map[string]any // Values keyed by JSON field names.
}

// LayerPolicy controls how LoadLayers treats edge cases.
type LayerPolicy struct {
	// AllowDeleteDefault permits layers to revoke a value that currently
	// comes from a 'default' tag. When false such a delete is an error.
	// Deleting a value that was set by a previous layer is always allowed.
	AllowDeleteDefault bool
}

// LoadLayers merges the given layers on top of the defaults (unless
// SkipDefaults is set) and checks required fields after the merge.
//
// It is an optional alternative to Load: files, environment variables and
// flags are not consulted, and the behavior of Load is unchanged.
//
// Semantics per field kind:
//   - scalars (including pointers): a layer value replaces the current one;
//     an explicit zero value (0, false, "") is a real assignment and counts
//     as set; Delete resets the field to its zero value (nil for pointers)
//     and marks it as not set.
//   - slices: a layer value replaces the whole slice, no element-wise merge.
//   - maps: entries are merged key by key; Delete as an entry value removes
//     that key; Delete on the field itself resets the whole map.
//   - nested structs (including pointer structs): merged recursively field
//     by field, pointers are allocated on demand; Delete on a subtree resets
//     every leaf under it.
//
// The three states "deleted", "explicitly set to zero" and "never provided"
// are kept distinct: only an explicit assignment satisfies a required field.
//
// LoadLayers is atomic: on any error the destination struct is restored to
// the exact state it had before the call.
func (l *Loader) LoadLayers(layers []Layer, policy LayerPolicy) error {
	if l.errInit != nil {
		return fmt.Errorf("init loader: %w", l.errInit)
	}
	if l.config.NewParser {
		return fmt.Errorf("LoadLayers: NewParser is not supported")
	}
	if err := l.loadLayers(layers, policy); err != nil {
		return fmt.Errorf("load layers: %w", err)
	}
	return nil
}

func (l *Loader) loadLayers(layers []Layer, policy LayerPolicy) error {
	dst := reflect.ValueOf(l.dst).Elem()
	snapshot := copyValue(dst)
	isSet := make([]bool, len(l.fields))
	for i, f := range l.fields {
		isSet[i] = f.isSet
	}

	if err := l.mergeLayers(layers, policy); err != nil {
		dst.Set(snapshot)
		for i, f := range l.fields {
			f.isSet = isSet[i]
		}
		return err
	}
	return nil
}

func (l *Loader) mergeLayers(layers []Layer, policy LayerPolicy) error {
	if !l.config.SkipDefaults {
		if err := l.loadDefaults(); err != nil {
			return fmt.Errorf("load defaults: %w", err)
		}
	}

	// fromDefault tracks whether the current value of a field still
	// originates from a 'default' tag (i.e. no layer has touched it yet).
	fromDefault := make([]bool, len(l.fields))
	for i, f := range l.fields {
		fromDefault[i] = f.isSet
	}

	for _, layer := range layers {
		if err := l.applyLayer(layer, policy, fromDefault); err != nil {
			return err
		}
	}
	return l.checkRequired()
}

func (l *Loader) applyLayer(layer Layer, policy LayerPolicy, fromDefault []bool) error {
	values := make(map[string]any, len(layer.Values))
	for k, v := range layer.Values {
		values[k] = v
	}
	consumed := map[string]struct{}{}

	for i, field := range l.fields {
		name := l.fullTag("", field, "json")
		if name == "" {
			continue
		}

		value, ok := lookupLayerValue(values, name)
		if !ok {
			delKey, deleted := ancestorDeleteKey(values, name)
			if !deleted {
				continue
			}
			consumed[delKey] = struct{}{}
			if err := l.deleteLayerField(layer, field, i, policy, fromDefault); err != nil {
				return err
			}
			continue
		}
		consumed[name] = struct{}{}

		if value == nil {
			continue
		}
		if isDelete(value) {
			if err := l.deleteLayerField(layer, field, i, policy, fromDefault); err != nil {
				return err
			}
			continue
		}
		if err := l.setLayerField(field, value); err != nil {
			return fmt.Errorf("layer %q: field %q: %w", layer.Name, name, err)
		}
		field.isSet = true
		fromDefault[i] = false
	}

	if !l.config.AllowUnknownFields {
		for key := range values {
			if _, ok := consumed[key]; ok {
				continue
			}
			return fmt.Errorf("unknown field in layer %q: %s (see AllowUnknownFields config param)", layer.Name, key)
		}
	}
	return nil
}

// lookupLayerValue finds the value for a dotted field name, flattening
// nested maps in values on demand (same as file loading does).
func lookupLayerValue(values map[string]any, name string) (any, bool) {
	if value, ok := values[name]; ok {
		return value, true
	}
	find(values, name)
	value, ok := values[name]
	return value, ok
}

// ancestorDeleteKey reports whether any parent key of name holds a Delete
// marker, meaning the whole subtree is revoked.
func ancestorDeleteKey(values map[string]any, name string) (string, bool) {
	for {
		idx := strings.LastIndex(name, ".")
		if idx < 0 {
			return "", false
		}
		name = name[:idx]
		if value, ok := values[name]; ok && isDelete(value) {
			return name, true
		}
	}
}

func (l *Loader) deleteLayerField(layer Layer, field *fieldData, idx int, policy LayerPolicy, fromDefault []bool) error {
	if !l.config.SkipDefaults && !policy.AllowDeleteDefault && fromDefault[idx] {
		return fmt.Errorf("layer %q: cannot delete default value of field %q (see LayerPolicy.AllowDeleteDefault)", layer.Name, field.name)
	}
	value := l.resolveValue(field)
	value.Set(reflect.Zero(value.Type()))
	field.value = value
	field.isSet = false
	fromDefault[idx] = false
	return nil
}

func (l *Loader) setLayerField(field *fieldData, value any) error {
	field.value = l.resolveValue(field)

	// An empty string is a legal value: assign the zero value explicitly
	// instead of treating it as "no value" like file loading does.
	if str, ok := value.(string); ok && str == "" {
		fv := field.value
		for fv.Kind() == reflect.Ptr {
			if fv.IsNil() {
				fv.Set(reflect.New(fv.Type().Elem()))
			}
			fv = fv.Elem()
		}
		field.value = fv
		fv.Set(reflect.Zero(fv.Type()))
		return nil
	}

	if m, ok := value.(map[any]any); ok {
		value = mii(m)
	}

	// Maps merge key by key instead of being replaced wholesale.
	kind := field.value.Kind()
	if kind == reflect.Ptr {
		kind = field.value.Type().Elem().Kind()
	}
	if kind == reflect.Map {
		if entries, ok := value.(map[string]any); ok {
			return l.mergeLayerMap(field, entries)
		}
	}

	return l.setFieldData(field, value)
}

// mergeLayerMap merges entries into the current map value key by key.
// A Delete entry removes the key from the map.
func (l *Loader) mergeLayerMap(field *fieldData, entries map[string]any) error {
	fv := field.value
	for fv.Kind() == reflect.Ptr {
		if fv.IsNil() {
			fv.Set(reflect.New(fv.Type().Elem()))
		}
		fv = fv.Elem()
	}
	field.value = fv

	if fv.IsNil() {
		fv.Set(reflect.MakeMap(fv.Type()))
	}

	for key, val := range entries {
		fdk := l.newSimpleFieldData(reflect.New(fv.Type().Key()).Elem())
		if err := l.setFieldData(fdk, key); err != nil {
			return fmt.Errorf("incorrect map key %q: %w", key, err)
		}

		if isDelete(val) {
			fv.SetMapIndex(fdk.value, reflect.Value{})
			continue
		}

		fdv := l.newFieldData(reflect.StructField{}, reflect.New(fv.Type().Elem()).Elem(), field)
		fdv.field.Type = fv.Type().Elem()
		if err := l.setFieldData(fdv, val); err != nil {
			return fmt.Errorf("incorrect map value %q: %w", val, err)
		}
		fv.SetMapIndex(fdk.value, fdv.value)
	}
	return nil
}

// resolveValue returns a fresh, settable reflect.Value for the field,
// allocating nil struct pointers on the way down. This keeps field handles
// valid even after a delete reset a pointer to nil. If the path cannot be
// resolved (e.g. through embedded structs) the stored value is kept.
func (l *Loader) resolveValue(f *fieldData) reflect.Value {
	root := reflect.ValueOf(l.dst)
	for root.Kind() == reflect.Ptr {
		if root.IsNil() {
			return f.value
		}
		root = root.Elem()
	}

	var chain []*fieldData
	for p := f; p != nil; p = p.parent {
		chain = append(chain, p)
	}

	value := root
	for i := len(chain) - 1; i >= 0; i-- {
		fd := chain[i]
		idx := fd.field.Index
		if len(idx) != 1 || value.Kind() != reflect.Struct || idx[0] >= value.NumField() {
			return f.value
		}
		if sf := value.Type().Field(idx[0]); sf.Name != fd.field.Name {
			return f.value
		}
		value = value.Field(idx[0])
		if i == 0 {
			break
		}
		for value.Kind() == reflect.Ptr {
			if value.IsNil() {
				if !value.CanSet() {
					return f.value
				}
				value.Set(reflect.New(value.Type().Elem()))
			}
			value = value.Elem()
		}
	}
	if !value.CanSet() {
		return f.value
	}
	return value
}

// copyValue makes a deep copy of v so a failed merge can be rolled back
// without leaking partially applied values into the destination.
func copyValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.New(v.Type().Elem())
		c.Elem().Set(copyValue(v.Elem()))
		return c

	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		return copyValue(v.Elem())

	case reflect.Struct:
		c := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			if c.Field(i).CanSet() {
				c.Field(i).Set(copyValue(v.Field(i)))
			}
		}
		return c

	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			c.Index(i).Set(copyValue(v.Index(i)))
		}
		return c

	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			c.SetMapIndex(copyValue(iter.Key()), copyValue(iter.Value()))
		}
		return c

	default:
		return v
	}
}
