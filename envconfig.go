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

// ErrRecursiveSpecification indicates that a specification contains a struct
// type that refers back to itself along its nested fields (for example a
// struct with a pointer field of its own type). Such types cannot be
// populated because they describe an infinite nesting.
var ErrRecursiveSpecification = errors.New("recursive specification")

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
	Name string
	Alt  string
	Key  string
	Tags reflect.StructTag
	Type reflect.Type
	Path []fieldStep
}

// fieldStep describes one traversal level from the specification root struct
// to a leaf field. Index is the struct field index at that level; Hops is the
// number of pointer dereferences performed on that field before reaching the
// next struct (or the leaf value).
type fieldStep struct {
	Index int
	Hops  int
}

// GatherInfo gathers information about the specified struct
func gatherInfo(prefix string, spec interface{}) ([]varInfo, error) {
	s := reflect.ValueOf(spec)

	if s.Kind() != reflect.Ptr {
		return nil, ErrInvalidSpecification
	}
	s = s.Elem()
	if s.Kind() != reflect.Struct {
		return nil, ErrInvalidSpecification
	}
	return gatherStructInfo(prefix, s.Type(), nil, map[reflect.Type]int{})
}

// gatherStructInfo walks a struct type without touching any values, so the
// caller's specification is never modified. The seen depths track the active
// type stack: a struct appearing twice along one ancestry is infinitely
// recursive, while the same type appearing in sibling subtrees (a diamond)
// is valid.
func gatherStructInfo(prefix string, t reflect.Type, path []fieldStep, seen map[reflect.Type]int) ([]varInfo, error) {
	if seen[t] > 0 {
		return nil, fmt.Errorf("%w: %s", ErrRecursiveSpecification, t.Name())
	}
	seen[t]++
	defer func() { seen[t]-- }()

	var infos []varInfo
	for i := 0; i < t.NumField(); i++ {
		ftype := t.Field(i)
		if !ftype.IsExported() || isTrue(ftype.Tag.Get("ignored")) {
			continue
		}

		inner, hops := derefType(ftype.Type)
		step := fieldStep{Index: i, Hops: hops}

		if inner.Kind() == reflect.Struct && !implementsInterface(inner) {
			innerPrefix := prefix
			if !ftype.Anonymous {
				innerPrefix = keyFor(ftype, prefix)
			}

			embeddedInfos, err := gatherStructInfo(innerPrefix, inner, appendPath(path, step), seen)
			if err != nil {
				return nil, err
			}
			infos = append(infos, embeddedInfos...)
			continue
		}

		info := varInfo{
			Name: ftype.Name,
			Tags: ftype.Tag,
			Alt:  strings.ToUpper(ftype.Tag.Get("envconfig")),
			Path: appendPath(path, step),
		}
		if hops > 0 && inner.Kind() != reflect.Struct {
			info.Type = ftype.Type
		} else {
			info.Type = inner
		}
		info.Key = keyFor(ftype, prefix)
		infos = append(infos, info)
	}
	return infos, nil
}

// derefType unwraps pointer layers, returning the pointed-to type and the
// number of pointer dereferences performed.
func derefType(t reflect.Type) (reflect.Type, int) {
	hops := 0
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
		hops++
	}
	return t, hops
}

// keyFor computes the environment variable key for a struct field.
func keyFor(ftype reflect.StructField, prefix string) string {
	key := ftype.Name
	if isTrue(ftype.Tag.Get("split_words")) {
		words := gatherRegexp.FindAllStringSubmatch(ftype.Name, -1)
		if len(words) > 0 {
			var parts []string
			for _, words := range words {
				if m := acronymRegexp.FindStringSubmatch(words[0]); len(m) == 3 {
					parts = append(parts, m[1], m[2])
				} else {
					parts = append(parts, words[0])
				}
			}
			key = strings.Join(parts, "_")
		}
	}
	if alt := strings.ToUpper(ftype.Tag.Get("envconfig")); alt != "" {
		key = alt
	}
	if prefix != "" {
		key = fmt.Sprintf("%s_%s", prefix, key)
	}
	return strings.ToUpper(key)
}

// appendPath appends a step to a copy of path, so sibling fields never share
// backing storage.
func appendPath(path []fieldStep, step fieldStep) []fieldStep {
	out := make([]fieldStep, 0, len(path)+1)
	out = append(out, path...)
	return append(out, step)
}

// field resolves the leaf field described by the path, lazily allocating
// pointer-to-struct ancestors. Pointer layers leading to a non-struct leaf
// are left untouched; processField materializes them when a value exists.
func (i varInfo) field(root reflect.Value) reflect.Value {
	f := root
	for n, step := range i.Path {
		f = f.Field(step.Index)
		if n == len(i.Path)-1 && step.Hops > 0 {
			t := f.Type()
			for t.Kind() == reflect.Ptr {
				t = t.Elem()
			}
			if t.Kind() != reflect.Struct {
				return f
			}
		}
		for h := 0; h < step.Hops; h++ {
			if f.IsNil() {
				f.Set(reflect.New(f.Type().Elem()))
			}
			f = f.Elem()
		}
	}
	return f
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

// Process populates the specified struct based on environment variables
func Process(prefix string, spec interface{}) error {
	infos, err := gatherInfo(prefix, spec)
	if err != nil {
		return err
	}
	root := reflect.ValueOf(spec).Elem()

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

		req := info.Tags.Get("required")
		if !ok && def == "" {
			if isTrue(req) {
				key := info.Key
				if info.Alt != "" {
					key = info.Alt
				}
				// A required descendant forces its pointer-to-struct
				// ancestors to exist even though no value is supplied.
				info.field(root)
				return fmt.Errorf("required key %s missing value", key)
			}
			continue
		}

		field := info.field(root)
		err = processField(value, field)
		if err != nil {
			return &ParseError{
				KeyName:   info.Key,
				FieldName: info.Name,
				TypeName:  info.Type.String(),
				Value:     value,
				Err:       err,
			}
		}
	}

	return nil
}

// MustProcess is the same as Process but panics if an error occurs
func MustProcess(prefix string, spec interface{}) {
	if err := Process(prefix, spec); err != nil {
		panic(err)
	}
}

func processField(value string, field reflect.Value) error {
	typ := field.Type()

	// Materialize pointer layers before honoring custom decoders: a type
	// implementing Decoder on its pointer receiver would otherwise be
	// unreachable through a nil pointer.
	for typ.Kind() == reflect.Ptr {
		if field.IsNil() {
			field.Set(reflect.New(typ.Elem()))
		}
		field = field.Elem()
		typ = field.Type()
	}

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
