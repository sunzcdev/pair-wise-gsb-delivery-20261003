package mapstructure

import (
	"fmt"
	"reflect"
	"strings"
)

// replayOp performs one recorded operation against the live target.
func replayOp(root reflect.Value, op Op) error {
	switch op.Kind {
	case OpRetain, OpUserCode:
		// Retains do nothing; user-code ops are executed by a full decode in
		// Plan.Apply and never reach this function.
		return nil
	}
	return replayInto(root, op.Path, op, func(dst reflect.Value) error {
		return assignLeaf(dst, op)
	})
}

// replayInto walks to the node described by path and runs leaf on it. Map
// elements are not addressable, so descent through a map works on a copy that
// is written back via commit.
func replayInto(cur reflect.Value, path []PathSegment, op Op, leaf func(reflect.Value) error) error {
	if len(path) == 0 {
		return leaf(cur)
	}
	seg := path[0]
	rest := path[1:]

	switch seg.Kind {
	case FieldSegment:
		for _, name := range strings.Split(seg.Field, ">") {
			if cur.Kind() == reflect.Ptr {
				if cur.IsNil() {
					if !cur.CanSet() {
						return fmt.Errorf("cannot allocate embedded pointer at %s", op.Name)
					}
					cur.Set(reflect.New(cur.Type().Elem()))
				}
				cur = cur.Elem()
			}
			if cur.Kind() != reflect.Struct {
				return fmt.Errorf("expected struct for field %q, got %s", name, cur.Kind())
			}
			cur = cur.FieldByName(name)
			if !cur.IsValid() {
				return fmt.Errorf("field %q not found", name)
			}
		}
		return replayInto(cur, rest, op, leaf)

	case DerefSegment:
		if cur.Kind() == reflect.Interface {
			if cur.IsNil() {
				return fmt.Errorf("cannot dereference nil interface at %s", op.Name)
			}
			// Concrete values behind an interface are not addressable; an
			// interface node is never merged, so a deref here means the
			// pointer is itself inside an interface box.
			cur = cur.Elem()
			return replayInto(cur, rest, op, leaf)
		}
		if cur.Kind() != reflect.Ptr {
			return fmt.Errorf("expected pointer for dereference, got %s at %s", cur.Kind(), op.Name)
		}
		if cur.IsNil() {
			if !cur.CanSet() {
				return fmt.Errorf("cannot allocate nil pointer at %s", op.Name)
			}
			cur.Set(reflect.New(cur.Type().Elem()))
		}
		return replayInto(cur.Elem(), rest, op, leaf)

	case KeySegment:
		if cur.Kind() != reflect.Map {
			return fmt.Errorf("expected map for key %v, got %s", seg.Key, cur.Kind())
		}
		if cur.IsNil() {
			if !cur.CanSet() {
				return fmt.Errorf("cannot allocate nil map at %s", op.Name)
			}
			cur.Set(reflect.MakeMap(cur.Type()))
		}
		key, err := convertMapKey(cur.Type().Key(), seg.Key)
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			return assignMapEntry(cur, key, op)
		}

		elemType := cur.Type().Elem()
		existing := cur.MapIndex(key)
		copyVal := reflect.New(elemType).Elem()
		if existing.IsValid() {
			copyVal.Set(existing)
		}
		if err := replayInto(copyVal, rest, op, leaf); err != nil {
			return err
		}
		cur.SetMapIndex(key, copyVal)
		return nil

	case IndexSegment:
		if cur.Kind() != reflect.Slice && cur.Kind() != reflect.Array {
			return fmt.Errorf("expected slice for index %d, got %s", seg.Index, cur.Kind())
		}
		if seg.Index >= cur.Len() {
			return fmt.Errorf("index %d out of range at %s", seg.Index, op.Name)
		}
		return replayInto(cur.Index(seg.Index), rest, op, leaf)
	}

	return fmt.Errorf("unsupported path segment at %s", op.Name)
}

func assignLeaf(dst reflect.Value, op Op) error {
	if !dst.CanSet() {
		return fmt.Errorf("target %s is not settable", op.Name)
	}
	switch op.Kind {
	case OpZero:
		if dst.Kind() == reflect.Map {
			if dst.IsNil() {
				dst.Set(reflect.MakeMap(dst.Type()))
			} else {
				for _, k := range dst.MapKeys() {
					dst.SetMapIndex(k, reflect.Value{})
				}
			}
			return nil
		}
		dst.Set(reflect.Zero(dst.Type()))
	case OpSet:
		val := reflect.ValueOf(op.Value)
		if !val.IsValid() {
			dst.Set(reflect.Zero(dst.Type()))
			return nil
		}
		if !val.Type().AssignableTo(dst.Type()) {
			return fmt.Errorf("planned value %s is not assignable to %s at %s", val.Type(), dst.Type(), op.Name)
		}
		dst.Set(val)
	}
	return nil
}

func assignMapEntry(m, key reflect.Value, op Op) error {
	switch op.Kind {
	case OpZero:
		m.SetMapIndex(key, reflect.Value{})
	case OpSet:
		val := reflect.ValueOf(op.Value)
		if !val.IsValid() {
			m.SetMapIndex(key, reflect.New(m.Type().Elem()).Elem())
			return nil
		}
		if !val.Type().AssignableTo(m.Type().Elem()) {
			return fmt.Errorf("planned value %s is not assignable to %s at %s", val.Type(), m.Type().Elem(), op.Name)
		}
		m.SetMapIndex(key, val)
	}
	return nil
}

// convertMapKey adapts a boxed diff key to the live map's key type.
func convertMapKey(keyType reflect.Type, raw any) (reflect.Value, error) {
	v := reflect.ValueOf(raw)
	if !v.IsValid() {
		return reflect.Value{}, fmt.Errorf("nil map key")
	}
	if v.Type().AssignableTo(keyType) {
		return v, nil
	}
	if v.Type().ConvertibleTo(keyType) {
		return v.Convert(keyType), nil
	}
	return reflect.Value{}, fmt.Errorf("cannot use key %v (%s) as %s", raw, v.Type(), keyType)
}
