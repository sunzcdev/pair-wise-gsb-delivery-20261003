package aconfig

import (
	"fmt"
	"reflect"
	"strings"
)

// Delete is a marker value for Layer.Values to revoke a field that was set
// by a lower-priority source (defaults or previous layers).
// Higher-priority layers can still set the field again.
//
// It is a dedicated type (and not a reserved string) so that any string,
// including an empty one, remains a legal configuration value.
type Delete struct{}

// Layer is a single source of configuration values merged by LoadLayers.
// Layers are applied in order: every next layer has a higher priority.
type Layer struct {
	// Tag selects which field tag is used to resolve names in Values.
	// If empty, "json" is used.
	Tag string

	// Values maps field names to their values.
	// A Delete value revokes the field (or the whole subtree) set by
	// lower-priority sources. A nil value sets the field to its zero value.
	Values map[string]any
}

func isDeleteValue(v any) bool {
	_, ok := v.(Delete)
	return ok
}

// LoadLayers merges the given layers on top of the defaults and stores
// the result in the destination. Layers are applied in order of increasing
// priority: a value in a later layer overrides earlier ones, and a Delete
// marker revokes values from all lower-priority sources.
//
// Unlike Load, no files, environment variables or flags are read.
// The destination is not modified when an error is returned.
func (l *Loader) LoadLayers(layers ...Layer) error {
	if l.errInit != nil {
		return fmt.Errorf("init loader: %w", l.errInit)
	}
	if l.config.NewParser {
		return fmt.Errorf("LoadLayers is not supported with NewParser")
	}
	if err := l.loadLayers(layers); err != nil {
		return fmt.Errorf("load layers: %w", err)
	}
	return nil
}

func (l *Loader) loadLayers(layers []Layer) error {
	dstValue := reflect.ValueOf(l.dst).Elem()
	rollback := l.snapshot(dstValue)

	if err := l.mergeLayers(layers); err != nil {
		rollback()
		return err
	}
	return nil
}

func (l *Loader) mergeLayers(layers []Layer) error {
	fields := l.getFields(l.dst)
	orig := make(map[*fieldData]reflect.Value, len(fields))
	for _, field := range fields {
		orig[field] = field.value
	}

	if !l.config.SkipDefaults {
		for _, field := range fields {
			defaultValue := field.Tag("default")
			if err := l.setFieldData(field, defaultValue); err != nil {
				return fmt.Errorf("load defaults: %w", err)
			}
			field.isSet = defaultValue != ""
		}
	}

	for i, layer := range layers {
		if err := l.applyLayer(fields, orig, layer); err != nil {
			return fmt.Errorf("apply layer %d: %w", i, err)
		}
	}

	missedFields := []string{}
	for _, field := range fields {
		if field.isSet {
			continue
		}
		if field.isRequired || l.config.AllFieldRequired {
			missedFields = append(missedFields, field.name)
		}
	}
	if len(missedFields) > 0 {
		return fmt.Errorf("fields required but not set: %s", strings.Join(missedFields, ","))
	}
	return nil
}

func (l *Loader) applyLayer(fields []*fieldData, orig map[*fieldData]reflect.Value, layer Layer) error {
	tag := layer.Tag
	if tag == "" {
		tag = "json"
	}

	values := make(map[string]any, len(layer.Values))
	for k, v := range layer.Values {
		values[k] = v
	}

	for _, field := range fields {
		name := l.fullTag("", field, tag)
		if name == "" {
			continue
		}
		value, ok := values[name]
		if !ok {
			values = find(values, name)
			value, ok = values[name]
			if !ok {
				continue
			}
		}
		delete(values, name)

		if isDeleteValue(value) {
			if err := l.deleteLayerField(field, orig[field]); err != nil {
				return err
			}
			continue
		}
		if err := l.setLayerField(field, orig[field], value); err != nil {
			return fmt.Errorf("field %q: %w", name, err)
		}
		field.isSet = true
	}

	// remaining Delete entries may revoke a whole subtree
	for name, value := range values {
		if !isDeleteValue(value) {
			continue
		}
		deleted, err := l.deleteLayerSubtree(fields, orig, tag, name)
		if err != nil {
			return err
		}
		if deleted {
			delete(values, name)
		}
	}

	if !l.config.AllowUnknownFields {
		for name := range values {
			return fmt.Errorf("unknown field in layer: %s (see AllowUnknownFields config param)", name)
		}
	}
	return nil
}

func (l *Loader) deleteLayerField(field *fieldData, orig reflect.Value) error {
	if !l.config.SkipDefaults && !l.config.AllowDeleteDefaults && field.Tag("default") != "" {
		return fmt.Errorf("field %q: deleting a default value is not allowed (see AllowDeleteDefaults config param)", field.name)
	}
	orig.Set(reflect.Zero(orig.Type()))
	field.isSet = false
	return nil
}

func (l *Loader) deleteLayerSubtree(fields []*fieldData, orig map[*fieldData]reflect.Value, tag, prefix string) (bool, error) {
	deleted := false
	for _, field := range fields {
		name := l.fullTag("", field, tag)
		if name == "" || (name != prefix && !strings.HasPrefix(name, prefix+".")) {
			continue
		}
		if err := l.deleteLayerField(field, orig[field]); err != nil {
			return deleted, err
		}
		deleted = true
	}
	return deleted, nil
}

func (l *Loader) setLayerField(field *fieldData, orig reflect.Value, value any) error {
	if value == nil {
		orig.Set(reflect.Zero(orig.Type()))
		return nil
	}

	// an explicit empty string is a legal value, not an absent one
	if s, ok := value.(string); ok && s == "" {
		v := orig
		for v.Type().Kind() == reflect.Ptr {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v.Set(reflect.Zero(v.Type()))
		return nil
	}

	// maps are merged key-wise, slices and other types are replaced
	if m, ok := value.(map[string]any); ok {
		if orig.Type().Kind() == reflect.Map && !orig.IsNil() {
			return l.mergeLayerMap(orig, m)
		}
	}

	return l.setFieldData(field, value)
}

func (l *Loader) mergeLayerMap(dst reflect.Value, m map[string]any) error {
	for key, val := range m {
		fdk := l.newSimpleFieldData(reflect.New(dst.Type().Key()).Elem())
		if err := l.setFieldData(fdk, key); err != nil {
			return fmt.Errorf("incorrect map key %q: %w", key, err)
		}

		if isDeleteValue(val) {
			dst.SetMapIndex(fdk.value, reflect.Value{})
			continue
		}

		fdv := l.newFieldData(reflect.StructField{}, reflect.New(dst.Type().Elem()).Elem(), nil)
		fdv.field.Type = dst.Type().Elem()
		if err := l.setFieldData(fdv, val); err != nil {
			return fmt.Errorf("incorrect map value %q: %w", val, err)
		}
		dst.SetMapIndex(fdk.value, fdv.value)
	}
	return nil
}

func (l *Loader) snapshot(dstValue reflect.Value) func() {
	type entry struct {
		field reflect.Value
		value reflect.Value
	}
	entries := make([]entry, 0, dstValue.NumField())
	for i := 0; i < dstValue.NumField(); i++ {
		field := dstValue.Field(i)
		if !field.CanSet() {
			continue
		}
		entries = append(entries, entry{field: field, value: deepCopyValue(field)})
	}
	return func() {
		for _, e := range entries {
			e.field.Set(e.value)
		}
	}
}

func deepCopyValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.New(v.Type().Elem())
		c.Elem().Set(deepCopyValue(v.Elem()))
		return c

	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		return deepCopyValue(v.Elem())

	case reflect.Struct:
		c := reflect.New(v.Type()).Elem()
		settable := false
		for i := 0; i < v.NumField(); i++ {
			if !c.Field(i).CanSet() {
				continue
			}
			c.Field(i).Set(deepCopyValue(v.Field(i)))
			settable = true
		}
		if !settable {
			return v
		}
		return c

	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			c.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return c

	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			c.SetMapIndex(deepCopyValue(iter.Key()), deepCopyValue(iter.Value()))
		}
		return c

	default:
		c := reflect.New(v.Type()).Elem()
		c.Set(v)
		return c
	}
}
