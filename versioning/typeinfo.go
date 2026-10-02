package versioning

import (
	"fmt"
	"reflect"
	"strconv"
	"sync"
)

// fieldTag is the struct tag that assigns a stable wire ID, for example
// `version:"1"`. The integer, not the Go field name, identifies the field on
// the wire and across versions.
const fieldTag = "version"

type fieldInfo struct {
	index []int
	id    uint64
}

type typeInfo struct {
	typ    reflect.Type
	ver    uint64
	fields map[uint64]fieldInfo
}

func buildTypeInfo(typ reflect.Type, ver uint64) (*typeInfo, error) {
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("versioning: registered version %d: %s is not a struct", ver, typ)
	}

	ti := &typeInfo{typ: typ, ver: ver, fields: make(map[uint64]fieldInfo)}
	if err := collectFields(ti, nil, typ); err != nil {
		return nil, err
	}
	return ti, nil
}

// collectFields walks exported fields recursively. Embedded (anonymous) fields
// are flattened so promoted fields can be versioned too.
func collectFields(ti *typeInfo, parent []int, typ reflect.Type) error {
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		index := append(append([]int(nil), parent...), i)

		if sf.Anonymous {
			ft := sf.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				if err := collectFields(ti, index, ft); err != nil {
					return err
				}
				continue
			}
		}

		if sf.PkgPath != "" {
			continue // unexported
		}

		tag, ok := sf.Tag.Lookup(fieldTag)
		if !ok {
			// An exported field without a stable ID has no defined wire
			// identity. Refuse it rather than silently encoding it under a
			// guessed name, so versioned output is always explicit.
			return fmt.Errorf(
				"versioning: field %s.%s has no %q tag",
				ti.typ, sf.Name, fieldTag)
		}

		id, err := strconv.ParseUint(tag, 10, 64)
		if err != nil {
			return fmt.Errorf(
				"versioning: field %s.%s has invalid %s tag %q: %w",
				ti.typ, sf.Name, fieldTag, tag, err)
		}
		if _, dup := ti.fields[id]; dup {
			return fmt.Errorf(
				"versioning: field %s.%s reuses %s id %d",
				ti.typ, sf.Name, fieldTag, id)
		}
		ti.fields[id] = fieldInfo{index: index, id: id}
	}
	return nil
}

// encodeFields reflects a struct value into the version-neutral field map.
func (ti *typeInfo) encodeFields(rv reflect.Value) (Fields, error) {
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	out := make(Fields, len(ti.fields))
	for id, fi := range ti.fields {
		fv := rv.FieldByIndex(fi.index)
		if err := out.Set(id, fv.Interface()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// decodeFields populates a struct value from the version-neutral field map.
// Unknown IDs are ignored so readers can tolerate fields added by newer
// versions once a conversion has brought the record up to date.
func (ti *typeInfo) decodeFields(rv reflect.Value, in Fields) error {
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	for id, fi := range ti.fields {
		raw, ok := in[id]
		if !ok || raw == nil {
			continue
		}
		target := rv.FieldByIndex(fi.index)
		if err := msgpackUnmarshal(raw, target.Addr().Interface()); err != nil {
			return fmt.Errorf("versioning: decode field id %d into %s: %w",
				id, ti.typ, err)
		}
	}
	return nil
}

// typeCache memoizes type inspection keyed by (type, version).
type typeCache struct {
	mu sync.Mutex
	m  map[reflect.Type]*typeInfo
}

func (c *typeCache) get(typ reflect.Type, ver uint64) (*typeInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[reflect.Type]*typeInfo)
	}
	if ti, ok := c.m[typ]; ok {
		if ti.ver != ver {
			return nil, fmt.Errorf("versioning: %s already registered at version %d",
				typ, ti.ver)
		}
		return ti, nil
	}
	ti, err := buildTypeInfo(typ, ver)
	if err != nil {
		return nil, err
	}
	c.m[typ] = ti
	return ti, nil
}
