package incremental

import (
	"fmt"
	"reflect"
)

// setEnv assigns value to the location identified by p inside env. Maps,
// slices and arrays are mutated in place; structs are mutated through
// pointers, so a struct environment must be passed to NewSession as a
// pointer for Set to work.
func setEnv(env any, p Path, value any) error {
	if len(p) == 0 {
		return fmt.Errorf("cannot replace the environment root")
	}
	v := reflect.ValueOf(env)
	if !v.IsValid() {
		return fmt.Errorf("cannot set %v on nil environment", p)
	}
	return setRec(v, p, value, p)
}

func setRec(v reflect.Value, p Path, value any, full Path) error {
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return fmt.Errorf("cannot set %v: nil encountered at %v", full, p)
		}
		v = v.Elem()
	}
	seg := p[0]
	last := len(p) == 1

	switch v.Kind() {
	case reflect.Map:
		key, err := mapKey(v.Type().Key(), seg)
		if err != nil {
			return err
		}
		if last {
			v.SetMapIndex(key, mapValue(v.Type().Elem(), value))
			return nil
		}
		elem := v.MapIndex(key)
		if !elem.IsValid() {
			return fmt.Errorf("cannot set %v: missing key %v", full, seg)
		}
		// Map values are not addressable: copy, mutate, write back.
		// Reference kinds (maps, slices, pointers) keep sharing the
		// underlying data, so the write-back is only required for
		// value kinds such as structs.
		cp := reflect.New(v.Type().Elem()).Elem()
		cp.Set(elem)
		if err := setRec(cp, p[1:], value, full); err != nil {
			return err
		}
		v.SetMapIndex(key, cp)
		return nil

	case reflect.Struct:
		name, ok := seg.(string)
		if !ok {
			return fmt.Errorf("cannot set %v: struct field must be a string, got %v", full, seg)
		}
		f := v.FieldByName(name)
		if !f.IsValid() {
			return fmt.Errorf("cannot set %v: no field %q on %v", full, name, v.Type())
		}
		if !f.CanSet() {
			return fmt.Errorf("cannot set %v: field %q of %v is not settable (pass the environment by pointer)", full, name, v.Type())
		}
		if last {
			return assign(f, value, full)
		}
		return setRec(f, p[1:], value, full)

	case reflect.Slice, reflect.Array:
		idx, ok := seg.(int)
		if !ok {
			return fmt.Errorf("cannot set %v: index must be an int, got %v", full, seg)
		}
		if idx < 0 {
			idx += v.Len()
		}
		if idx < 0 || idx >= v.Len() {
			return fmt.Errorf("cannot set %v: index %d out of range (length %d)", full, idx, v.Len())
		}
		elem := v.Index(idx)
		if last {
			return assign(elem, value, full)
		}
		if elem.CanSet() {
			return setRec(elem, p[1:], value, full)
		}
		cp := reflect.New(elem.Type()).Elem()
		cp.Set(elem)
		if err := setRec(cp, p[1:], value, full); err != nil {
			return err
		}
		if !v.Index(idx).CanSet() {
			return fmt.Errorf("cannot set %v: element %d is not settable", full, idx)
		}
		v.Index(idx).Set(cp)
		return nil
	}
	return fmt.Errorf("cannot set %v: cannot index %v", full, v.Type())
}

func mapKey(t reflect.Type, seg any) (reflect.Value, error) {
	k := reflect.ValueOf(seg)
	if !k.IsValid() {
		return reflect.Zero(t), nil
	}
	if k.Type().AssignableTo(t) {
		return k, nil
	}
	if k.Type().ConvertibleTo(t) {
		return k.Convert(t), nil
	}
	return reflect.Value{}, fmt.Errorf("cannot use %v as map key of type %v", seg, t)
}

func mapValue(t reflect.Type, value any) reflect.Value {
	if value == nil {
		return reflect.Zero(t)
	}
	v := reflect.ValueOf(value)
	if v.Type().AssignableTo(t) {
		return v
	}
	if v.Type().ConvertibleTo(t) {
		return v.Convert(t)
	}
	return v
}

func assign(dst reflect.Value, value any, full Path) error {
	if !dst.CanSet() {
		return fmt.Errorf("cannot set %v: not settable", full)
	}
	if value == nil {
		dst.Set(reflect.Zero(dst.Type()))
		return nil
	}
	v := reflect.ValueOf(value)
	if v.Type().AssignableTo(dst.Type()) {
		dst.Set(v)
		return nil
	}
	if v.Type().ConvertibleTo(dst.Type()) {
		dst.Set(v.Convert(dst.Type()))
		return nil
	}
	return fmt.Errorf("cannot set %v: cannot assign %T to %v", full, value, dst.Type())
}
