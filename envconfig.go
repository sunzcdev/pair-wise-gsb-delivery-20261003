// Copyright (c) 2013 Kelsey Hightower. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package envconfig

import (
	"encoding"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidSpecification indicates that a specification is of the wrong type.
var ErrInvalidSpecification = errors.New("specification must be a struct pointer")

// ErrRecursiveSpecification indicates that a specification references itself
// through a nested pointer field. Such types cannot be expanded into a fixed
// set of environment variables.
var ErrRecursiveSpecification = errors.New("recursive specification: a nested pointer struct must not contain itself")

var gatherRegexp = regexp.MustCompile("([^A-Z]+|[A-Z]+[^A-Z]+|[A-Z]+)")
var acronymRegexp = regexp.MustCompile("([A-Z]+)([A-Z][^A-Z]+)")

// A ParseError occurs when an environment variable cannot be converted to
// the type required by a struct field during assignment.
type ParseError struct {
	KeyName   string
	FieldName string
	TypeName  string
	Value     string
	Err       error
}

// Decoder has the same semantics as Setter, but takes higher precedence.
// It is provided for historical compatibility.
type Decoder interface {
	Decode(value string) error
}

// Setter is implemented by types can self-deserialize values.
// Any type that implements flag.Value also implements Setter.
type Setter interface {
	Set(value string) error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("envconfig.Process: assigning %[1]s to %[2]s: converting '%[3]s' to type %[4]s. details: %[5]s", e.KeyName, e.FieldName, e.Value, e.TypeName, e.Err)
}

// varInfo maintains information about the configuration variable
type varInfo struct {
	Name   string
	Alt    string
	Key    string
	Tags   reflect.StructTag
	Index  int
	Owners []owner
	Typ    reflect.Type
}

// owner describes a single nested-struct hop that must be traversed to reach
// a field. When Ptr is greater than zero the hop goes through one or more
// pointers, which are only allocated lazily while processing.
type owner struct {
	Index int
	Ptr   int
}

// gatherInfo gathers information about the specified struct without mutating
// it. In particular it never allocates a nested pointer field; callers that
// need to populate a leaf do so through ensureField.
func gatherInfo(prefix string, spec interface{}) ([]varInfo, error) {
	s := reflect.ValueOf(spec)

	if s.Kind() != reflect.Ptr {
		return nil, ErrInvalidSpecification
	}
	s = s.Elem()
	if s.Kind() != reflect.Struct {
		return nil, ErrInvalidSpecification
	}
	typeOfSpec := s.Type()

	infos := make([]varInfo, 0, typeOfSpec.NumField())
	if err := gatherFields(prefix, typeOfSpec, nil, nil, &infos); err != nil {
		return nil, err
	}
	return infos, nil
}

// gatherFields walks the fields of a struct type, appending one varInfo per
// leaf. ptrStack tracks pointer-to-struct types currently on the traversal
// path so self-referential specifications are reported instead of looping
// forever.
func gatherFields(prefix string, t reflect.Type, owners []owner, ptrStack []reflect.Type, infos *[]varInfo) error {
	for i := 0; i < t.NumField(); i++ {
		ftype := t.Field(i)
		if ftype.PkgPath != "" || isTrue(ftype.Tag.Get("ignored")) {
			continue
		}

		alt := strings.ToUpper(ftype.Tag.Get("envconfig"))

		// Default to the field name as the env var name (will be upcased)
		key := ftype.Name

		// Best effort to un-pick camel casing as separate words
		if isTrue(ftype.Tag.Get("split_words")) {
			words := gatherRegexp.FindAllStringSubmatch(ftype.Name, -1)
			if len(words) > 0 {
				var name []string
				for _, words := range words {
					if m := acronymRegexp.FindStringSubmatch(words[0]); len(m) == 3 {
						name = append(name, m[1], m[2])
					} else {
						name = append(name, words[0])
					}
				}

				key = strings.Join(name, "_")
			}
		}
		if alt != "" {
			key = alt
		}
		if prefix != "" {
			key = fmt.Sprintf("%s_%s", prefix, key)
		}
		key = strings.ToUpper(key)

		fieldType := ftype.Type
		baseType := fieldType
		ptrDepth := 0
		for baseType.Kind() == reflect.Ptr {
			baseType = baseType.Elem()
			ptrDepth++
		}

		if baseType.Kind() == reflect.Struct && !implementsInterface(fieldType) {
			// A self-referential pointer struct would be expanded forever;
			// value-embedded structs cannot form a cycle in valid Go code.
			var childStack []reflect.Type
			if ptrDepth > 0 {
				for _, seen := range ptrStack {
					if seen == baseType {
						return fmt.Errorf("%w: %s", ErrRecursiveSpecification, baseType)
					}
				}
				childStack = make([]reflect.Type, len(ptrStack)+1)
				copy(childStack, ptrStack)
				childStack[len(ptrStack)] = baseType
			}

			innerPrefix := prefix
			if !ftype.Anonymous {
				innerPrefix = key
			}

			nextOwners := make([]owner, len(owners), len(owners)+1)
			copy(nextOwners, owners)
			nextOwners = append(nextOwners, owner{Index: i, Ptr: ptrDepth})

			if err := gatherFields(innerPrefix, baseType, nextOwners, childStack, infos); err != nil {
				return err
			}
			continue
		}

		fieldOwners := make([]owner, len(owners))
		copy(fieldOwners, owners)
		*infos = append(*infos, varInfo{
			Name:   ftype.Name,
			Alt:    alt,
			Key:    key,
			Tags:   ftype.Tag,
			Index:  i,
			Owners: fieldOwners,
			Typ:    fieldType,
		})
	}
	return nil
}

// ensureField resolves the addressable value backing a leaf, allocating each
// pointer-to-struct ancestor on the path only when a value actually needs to
// be stored. Sibling pointer subtrees that are not touched stay nil.
func ensureField(spec interface{}, info varInfo) reflect.Value {
	v := reflect.ValueOf(spec).Elem()
	for _, o := range info.Owners {
		f := v.Field(o.Index)
		for d := 0; d < o.Ptr; d++ {
			if f.IsNil() {
				f.Set(reflect.New(f.Type().Elem()))
			}
			f = f.Elem()
		}
		v = f
	}
	return v.Field(info.Index)
}

// CheckDisallowed checks that no environment variables with the prefix are set
// that we don't know how or want to parse. This is likely only meaningful with
// a non-empty prefix.
func CheckDisallowed(prefix string, spec interface{}) error {
	infos, err := gatherInfo(prefix, spec)
	if err != nil {
		return err
	}

	vars := make(map[string]struct{})
	for _, info := range infos {
		vars[info.Key] = struct{}{}
	}

	if prefix != "" {
		prefix = strings.ToUpper(prefix) + "_"
	}

	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, prefix) {
			continue
		}
		v := strings.SplitN(env, "=", 2)[0]
		if _, found := vars[v]; !found {
			return fmt.Errorf("unknown environment variable %s", v)
		}
	}

	return nil
}

// Config controls optional behavior of ProcessWith.
type Config struct {
	// LazyNestedPointers keeps nested pointer-to-struct fields nil unless a
	// value actually needs to be stored in their subtree. A subtree is
	// populated only when an environment variable is set for one of its
	// leaves, when a leaf has an applicable default, or when a required leaf
	// forces the ancestor objects into existence. Optional pointer subtrees
	// that receive no value remain nil, so setting one sibling configuration
	// does not allocate the others.
	LazyNestedPointers bool
}

// Process populates the specified struct based on environment variables. It
// behaves like ProcessWith with LazyNestedPointers enabled.
func Process(prefix string, spec interface{}) error {
	return ProcessWith(prefix, spec, Config{LazyNestedPointers: true})
}

// ProcessWith populates the specified struct based on environment variables,
// honoring the supplied configuration.
func ProcessWith(prefix string, spec interface{}, config Config) error {
	infos, err := gatherInfo(prefix, spec)
	if err != nil {
		return err
	}

	if !config.LazyNestedPointers {
		// Reproduce the historical behavior: eagerly instantiate every
		// nested pointer-to-struct before looking at the environment.
		allocateAll(infos, spec)
	}

	for _, info := range infos {

		// `os.Getenv` cannot differentiate between an explicitly set empty value
		// and an unset value. `os.LookupEnv` is preferred to `syscall.Getenv`,
		// but it is only available in go1.5 or newer. We're using Go build tags
		// here to use os.LookupEnv for >=go1.5
		value, ok := lookupEnv(info.Key)
		if !ok && info.Alt != "" {
			value, ok = lookupEnv(info.Alt)
		}

		def := info.Tags.Get("default")
		if def != "" && !ok {
			value = def
		}

		hasValue := ok || def != ""

		req := info.Tags.Get("required")
		if !hasValue {
			if isTrue(req) {
				key := info.Key
				if info.Alt != "" {
					key = info.Alt
				}
				return fmt.Errorf("required key %s missing value", key)
			}
			continue
		}

		field := ensureField(spec, info)
		err = processField(value, field)
		if err != nil {
			return &ParseError{
				KeyName:   info.Key,
				FieldName: info.Name,
				TypeName:  field.Type().String(),
				Value:     value,
				Err:       err,
			}
		}
	}

	return err
}

// allocateAll instantiates every pointer-to-struct ancestor across all
// leaves, so the whole optional pointer tree exists even when no environment
// variable targets it.
func allocateAll(infos []varInfo, spec interface{}) {
	v := reflect.ValueOf(spec).Elem()
	for _, info := range infos {
		cur := v
		for _, o := range info.Owners {
			f := cur.Field(o.Index)
			for d := 0; d < o.Ptr; d++ {
				if f.IsNil() {
					f.Set(reflect.New(f.Type().Elem()))
				}
				f = f.Elem()
			}
			cur = f
		}
	}

	// Historical gatherInfo also instantiated pointer leaves that point at a
	// struct; custom-decoder struct pointers were left untouched and are
	// handled by processField.
	for _, info := range infos {
		field := ensureField(spec, info)
		for field.Kind() == reflect.Ptr {
			if field.IsNil() {
				if field.Type().Elem().Kind() != reflect.Struct {
					break
				}
				field.Set(reflect.New(field.Type().Elem()))
			}
			field = field.Elem()
		}
	}
}

// MustProcess is the same as Process but panics if an error occurs
func MustProcess(prefix string, spec interface{}) {
	if err := Process(prefix, spec); err != nil {
		panic(err)
	}
}

func processField(value string, field reflect.Value) error {
	// A nil pointer whose method set provides a decoder must be allocated
	// first, otherwise invoking the pointer-receiver method dereferences nil.
	for field.Kind() == reflect.Ptr && field.IsNil() && implementsInterface(field.Type()) {
		field.Set(reflect.New(field.Type().Elem()))
		field = field.Elem()
	}

	typ := field.Type()

	decoder := decoderFrom(field)
	if decoder != nil {
		return decoder.Decode(value)
	}
	// look for Set method if Decode not defined
	setter := setterFrom(field)
	if setter != nil {
		return setter.Set(value)
	}

	if t := textUnmarshaler(field); t != nil {
		return t.UnmarshalText([]byte(value))
	}

	if b := binaryUnmarshaler(field); b != nil {
		return b.UnmarshalBinary([]byte(value))
	}

	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
		if field.IsNil() {
			field.Set(reflect.New(typ))
		}
		field = field.Elem()
	}

	switch typ.Kind() {
	case reflect.String:
		field.SetString(value)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var (
			val int64
			err error
		)
		if field.Kind() == reflect.Int64 && typ.PkgPath() == "time" && typ.Name() == "Duration" {
			var d time.Duration
			d, err = time.ParseDuration(value)
			val = int64(d)
		} else {
			val, err = strconv.ParseInt(value, 0, typ.Bits())
		}
		if err != nil {
			return err
		}

		field.SetInt(val)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		val, err := strconv.ParseUint(value, 0, typ.Bits())
		if err != nil {
			return err
		}
		field.SetUint(val)
	case reflect.Bool:
		val, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		field.SetBool(val)
	case reflect.Float32, reflect.Float64:
		val, err := strconv.ParseFloat(value, typ.Bits())
		if err != nil {
			return err
		}
		field.SetFloat(val)
	case reflect.Slice:
		sl := reflect.MakeSlice(typ, 0, 0)
		if typ.Elem().Kind() == reflect.Uint8 {
			sl = reflect.ValueOf([]byte(value))
		} else if strings.TrimSpace(value) != "" {
			vals := strings.Split(value, ",")
			sl = reflect.MakeSlice(typ, len(vals), len(vals))
			for i, val := range vals {
				err := processField(val, sl.Index(i))
				if err != nil {
					return err
				}
			}
		}
		field.Set(sl)
	case reflect.Map:
		mp := reflect.MakeMap(typ)
		if strings.TrimSpace(value) != "" {
			pairs := strings.Split(value, ",")
			for _, pair := range pairs {
				kvpair := strings.Split(pair, ":")
				if len(kvpair) != 2 {
					return fmt.Errorf("invalid map item: %q", pair)
				}
				k := reflect.New(typ.Key()).Elem()
				err := processField(kvpair[0], k)
				if err != nil {
					return err
				}
				v := reflect.New(typ.Elem()).Elem()
				err = processField(kvpair[1], v)
				if err != nil {
					return err
				}
				mp.SetMapIndex(k, v)
			}
		}
		field.Set(mp)
	}

	return nil
}

func interfaceFrom(field reflect.Value, fn func(interface{}, *bool)) {
	// it may be impossible for a struct field to fail this check
	if !field.CanInterface() {
		return
	}
	var ok bool
	fn(field.Interface(), &ok)
	if !ok && field.CanAddr() {
		fn(field.Addr().Interface(), &ok)
	}
}

func decoderFrom(field reflect.Value) (d Decoder) {
	interfaceFrom(field, func(v interface{}, ok *bool) { d, *ok = v.(Decoder) })
	return d
}

func setterFrom(field reflect.Value) (s Setter) {
	interfaceFrom(field, func(v interface{}, ok *bool) { s, *ok = v.(Setter) })
	return s
}

func textUnmarshaler(field reflect.Value) (t encoding.TextUnmarshaler) {
	interfaceFrom(field, func(v interface{}, ok *bool) { t, *ok = v.(encoding.TextUnmarshaler) })
	return t
}

func binaryUnmarshaler(field reflect.Value) (b encoding.BinaryUnmarshaler) {
	interfaceFrom(field, func(v interface{}, ok *bool) { b, *ok = v.(encoding.BinaryUnmarshaler) })
	return b
}

func isTrue(s string) bool {
	b, _ := strconv.ParseBool(s)
	return b
}
