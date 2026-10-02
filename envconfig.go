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
	Name           string
	Alt            string
	Key            string
	AltKeys        []string
	DeprecatedKeys []string
	deprecated     map[string]bool
	Field          reflect.Value
	Tags           reflect.StructTag
}

func splitTagList(tag string) []string {
	var out []string
	for _, part := range strings.Split(tag, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
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
	typeOfSpec := s.Type()

	// over allocate an info array, we will extend if needed later
	infos := make([]varInfo, 0, s.NumField())
	for i := 0; i < s.NumField(); i++ {
		f := s.Field(i)
		ftype := typeOfSpec.Field(i)
		if !f.CanSet() || isTrue(ftype.Tag.Get("ignored")) {
			continue
		}

		for f.Kind() == reflect.Ptr {
			if f.IsNil() {
				if f.Type().Elem().Kind() != reflect.Struct {
					// nil pointer to a non-struct: leave it alone
					break
				}
				// nil pointer to struct: create a zero instance
				f.Set(reflect.New(f.Type().Elem()))
			}
			f = f.Elem()
		}

		// Capture information about the config variable
		info := varInfo{
			Name:  ftype.Name,
			Field: f,
			Tags:  ftype.Tag,
			Alt:   strings.ToUpper(ftype.Tag.Get("envconfig")),
		}

		// Default to the field name as the env var name (will be upcased)
		info.Key = info.Name

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

				info.Key = strings.Join(name, "_")
			}
		}
		if info.Alt != "" {
			info.Key = info.Alt
		}
		if prefix != "" {
			info.Key = fmt.Sprintf("%s_%s", prefix, info.Key)
		}
		info.Key = strings.ToUpper(info.Key)

		// Field-level aliases and deprecated names share the same (possibly
		// nested) prefix as the canonical key, so they cannot collide with
		// keys declared by other components.
		deprecatedNames := splitTagList(ftype.Tag.Get("deprecated"))
		deprecatedNamesSet := make(map[string]bool, len(deprecatedNames))
		for _, name := range deprecatedNames {
			deprecatedNamesSet[strings.ToUpper(name)] = true
		}
		info.deprecated = make(map[string]bool, len(deprecatedNames))
		seen := map[string]bool{info.Key: true}
		for _, name := range append(splitTagList(ftype.Tag.Get("aliases")), deprecatedNames...) {
			key := strings.ToUpper(name)
			if prefix != "" {
				key = fmt.Sprintf("%s_%s", prefix, key)
			}
			key = strings.ToUpper(key)
			if seen[key] {
				continue
			}
			seen[key] = true
			info.AltKeys = append(info.AltKeys, key)
			if deprecatedNamesSet[strings.ToUpper(name)] {
				info.DeprecatedKeys = append(info.DeprecatedKeys, key)
				info.deprecated[key] = true
			}
		}

		infos = append(infos, info)

		if f.Kind() == reflect.Struct {
			// honor Decode if present
			if decoderFrom(f) == nil && setterFrom(f) == nil && textUnmarshaler(f) == nil && binaryUnmarshaler(f) == nil {
				innerPrefix := prefix
				if !ftype.Anonymous {
					innerPrefix = info.Key
				}

				embeddedPtr := f.Addr().Interface()
				embeddedInfos, err := gatherInfo(innerPrefix, embeddedPtr)
				if err != nil {
					return nil, err
				}
				infos = append(infos[:len(infos)-1], embeddedInfos...)

				continue
			}
		}
	}
	return infos, nil
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
		for _, altKey := range info.AltKeys {
			vars[altKey] = struct{}{}
		}
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

// ConflictMode determines how ProcessWithReport reacts when several
// environment variables declared for the same field are set to different
// values.
type ConflictMode int

const (
	// ConflictUsePriority resolves conflicts by priority: the canonical key
	// wins over aliases, and earlier declared aliases win over later ones.
	// This is the default behavior.
	ConflictUsePriority ConflictMode = iota
	// ConflictError makes processing fail when conflicting values are set
	// for the same field.
	ConflictError
)

// Option configures ProcessWithReport.
type Option func(*config)

type config struct {
	conflictMode ConflictMode
}

// WithConflictMode sets how conflicting values among a field's canonical
// key and its aliases are handled.
func WithConflictMode(mode ConflictMode) Option {
	return func(c *config) { c.conflictMode = mode }
}

// FieldReport describes how a single field was populated.
type FieldReport struct {
	// FieldName is the name of the Go struct field.
	FieldName string
	// Key is the canonical environment variable name for the field.
	Key string
	// UsedKey is the environment variable that actually supplied the value.
	// It is empty when the value came from a default or was not set.
	UsedKey string
	// Deprecated is true when the value was supplied by a deprecated name.
	Deprecated bool
}

// Report describes how a specification was populated by ProcessWithReport.
type Report struct {
	// Fields holds one entry per processed field, in declaration order.
	Fields []FieldReport
	// Warnings holds human-readable notices, such as deprecation and
	// conflict warnings. Each deprecated name produces at most one warning.
	Warnings []string
}

// Process populates the specified struct based on environment variables
func Process(prefix string, spec interface{}) error {
	_, err := ProcessWithReport(prefix, spec)
	return err
}

// ProcessWithReport populates the specified struct based on environment
// variables and returns a Report describing which environment variable
// supplied each field, along with any deprecation or conflict warnings.
func ProcessWithReport(prefix string, spec interface{}, opts ...Option) (*Report, error) {
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}

	infos, err := gatherInfo(prefix, spec)
	if err != nil {
		return nil, err
	}

	report := &Report{}
	deprecatedWarned := make(map[string]bool)

	for _, info := range infos {

		// `os.Getenv` cannot differentiate between an explicitly set empty value
		// and an unset value. `os.LookupEnv` is preferred to `syscall.Getenv`,
		// but it is only available in go1.5 or newer. We're using Go build tags
		// here to use os.LookupEnv for >=go1.5
		type candidate struct {
			key   string
			value string
		}
		var candidates []candidate
		if value, ok := lookupEnv(info.Key); ok {
			candidates = append(candidates, candidate{info.Key, value})
		}
		if info.Alt != "" {
			if value, ok := lookupEnv(info.Alt); ok {
				candidates = append(candidates, candidate{info.Alt, value})
			}
		}
		for _, altKey := range info.AltKeys {
			if value, ok := lookupEnv(altKey); ok {
				candidates = append(candidates, candidate{altKey, value})
			}
		}

		var value, usedKey string
		ok := len(candidates) > 0
		if ok {
			value = candidates[0].value
			usedKey = candidates[0].key

			conflict := false
			for _, c := range candidates[1:] {
				if c.value != value {
					conflict = true
					break
				}
			}
			if conflict {
				keys := make([]string, len(candidates))
				for i, c := range candidates {
					keys[i] = c.key
				}
				if cfg.conflictMode == ConflictError {
					return nil, fmt.Errorf("envconfig: field %s has conflicting values: %s are set to different values", info.Name, strings.Join(keys, ", "))
				}
				report.Warnings = append(report.Warnings, fmt.Sprintf("field %s: %s are set to different values, using %s", info.Name, strings.Join(keys, ", "), usedKey))
			}

			for _, c := range candidates {
				if info.deprecated[c.key] && !deprecatedWarned[c.key] {
					deprecatedWarned[c.key] = true
					report.Warnings = append(report.Warnings, fmt.Sprintf("environment variable %s is deprecated, use %s instead", c.key, info.Key))
				}
			}
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
				return nil, fmt.Errorf("required key %s missing value", key)
			}
			report.Fields = append(report.Fields, FieldReport{
				FieldName: info.Name,
				Key:       info.Key,
			})
			continue
		}

		err = processField(value, info.Field)
		if err != nil {
			keyName := usedKey
			if keyName == "" {
				keyName = info.Key
			}
			return nil, &ParseError{
				KeyName:   keyName,
				FieldName: info.Name,
				TypeName:  info.Field.Type().String(),
				Value:     value,
				Err:       err,
			}
		}

		report.Fields = append(report.Fields, FieldReport{
			FieldName:  info.Name,
			Key:        info.Key,
			UsedKey:    usedKey,
			Deprecated: info.deprecated[usedKey],
		})
	}

	return report, nil
}

// MustProcess is the same as Process but panics if an error occurs
func MustProcess(prefix string, spec interface{}) {
	if err := Process(prefix, spec); err != nil {
		panic(err)
	}
}

func processField(value string, field reflect.Value) error {
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
