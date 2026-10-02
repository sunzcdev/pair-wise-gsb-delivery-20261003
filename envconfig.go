// Copyright (c) 2013 Kelsey Hightower. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package envconfig

import (
	"encoding"
	"errors"
	"fmt"
	"log"
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
	Aliases        []string
	Deprecated     bool
	DeprecationMsg string
	DeprecatedSet  map[string]bool
	Conflict       string
	Field          reflect.Value
	Tags           reflect.StructTag
}

// ConflictPolicy determines how Process behaves when several known names
// (the canonical key and any declared aliases) for the same field are set in
// the environment with different values.
type ConflictPolicy int

const (
	// ConflictError makes Process return an error when multiple names are set
	// to different values. This is the default and safest choice.
	ConflictError ConflictPolicy = iota
	// ConflictPriority resolves the conflict by using the canonical key first,
	// then the aliases in the order they were declared.
	ConflictPriority
)

// String returns the string representation of a ConflictPolicy.
func (p ConflictPolicy) String() string {
	switch p {
	case ConflictPriority:
		return "priority"
	default:
		return "error"
	}
}

// Config customizes how environment variables are processed.
type Config struct {
	// ConflictPolicy selects the behavior when the canonical key and one or
	// more aliases are present with differing values. The field-level
	// "on_conflict" tag takes precedence over this setting.
	ConflictPolicy ConflictPolicy
}

// FieldLoad reports how a single struct field was populated.
type FieldLoad struct {
	// Field is the Go struct field name.
	Field string
	// Key is the canonical environment variable name, including any prefix.
	Key string
	// Aliases lists every alternate name, including any prefix.
	Aliases []string
	// Source is the environment variable name that actually supplied the
	// value. It is empty when the value came from the default tag or nothing
	// was configured.
	Source string
	// Value is the raw value that was applied.
	Value string
	// Default reports whether the value came from the "default" tag.
	Default bool
	// Deprecated reports whether the field declares deprecated aliases.
	Deprecated bool
	// Warnings contains the deprecation notices generated for this field.
	Warnings []string
}

// LoadReport describes the result of processing the environment. One entry is
// present for every field in the specification, in declaration order.
type LoadReport struct {
	// Fields holds the per-field loading information.
	Fields []FieldLoad
	// Warnings aggregates every deprecation notice, de-duplicated.
	Warnings []string
}

type candidate struct {
	name  string
	value string
	old   bool
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

		for _, alias := range strings.Split(ftype.Tag.Get("aliases"), ",") {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				continue
			}
			aliasKey := alias
			if prefix != "" {
				aliasKey = fmt.Sprintf("%s_%s", prefix, alias)
			}
			if strings.ToUpper(aliasKey) == info.Key {
				continue
			}
			duplicate := false
			for _, existing := range info.Aliases {
				if existing == strings.ToUpper(aliasKey) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				info.Aliases = append(info.Aliases, strings.ToUpper(aliasKey))
			}
		}

		if msg := ftype.Tag.Get("deprecated"); msg != "" {
			info.Deprecated = true
			if msg != "true" {
				info.DeprecationMsg = msg
			}
		}
		info.DeprecatedSet = make(map[string]bool)
		if info.Deprecated {
			for _, alias := range info.Aliases {
				info.DeprecatedSet[alias] = true
			}
		}
		for _, name := range strings.Split(ftype.Tag.Get("deprecated_aliases"), ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			nameKey := name
			if prefix != "" {
				nameKey = fmt.Sprintf("%s_%s", prefix, name)
			}
			info.DeprecatedSet[strings.ToUpper(nameKey)] = true
		}
		info.Conflict = strings.ToLower(strings.TrimSpace(ftype.Tag.Get("on_conflict")))

		if prefix != "" {
			info.Key = fmt.Sprintf("%s_%s", prefix, info.Key)
		}
		info.Key = strings.ToUpper(info.Key)

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
		for _, alias := range info.Aliases {
			vars[alias] = struct{}{}
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

// Process populates the specified struct based on environment variables
func Process(prefix string, spec interface{}) error {
	_, err := (Config{}).process(prefix, spec)
	return err
}

// ProcessWithReport is the same as Process but additionally returns a
// LoadReport describing which environment variable supplied each field and
// any deprecation warnings that were generated.
func ProcessWithReport(prefix string, spec interface{}) (*LoadReport, error) {
	return (Config{}).process(prefix, spec)
}

// ProcessConfig populates the specified struct using the supplied Config and
// returns a LoadReport.
func ProcessConfig(prefix string, spec interface{}, cfg Config) (*LoadReport, error) {
	return cfg.process(prefix, spec)
}

func (cfg Config) process(prefix string, spec interface{}) (*LoadReport, error) {
	infos, err := gatherInfo(prefix, spec)
	if err != nil {
		return nil, err
	}

	report := &LoadReport{}
	seenWarnings := make(map[string]struct{})
	addWarning := func(fl *FieldLoad, msg string) {
		fl.Warnings = append(fl.Warnings, msg)
		if _, seen := seenWarnings[msg]; !seen {
			seenWarnings[msg] = struct{}{}
			report.Warnings = append(report.Warnings, msg)
			log.Printf("envconfig: %s", msg)
		}
	}

	for _, info := range infos {
		fl := FieldLoad{
			Field:      info.Name,
			Key:        info.Key,
			Aliases:    append([]string(nil), info.Aliases...),
			Deprecated: len(info.DeprecatedSet) > 0,
		}

		candidates := make([]candidate, 0, len(info.Aliases)+2)
		if value, ok := lookupEnv(info.Key); ok {
			candidates = append(candidates, candidate{info.Key, value, false})
		}
		// Historical behavior: the value of the "envconfig" tag (Alt) was
		// also looked up without the prefix when the prefixed key was unset.
		if info.Alt != "" {
			altKey := strings.ToUpper(info.Alt)
			if _, present := lookupEnv(info.Key); !present {
				if value, ok := lookupEnv(altKey); ok {
					candidates = append(candidates, candidate{altKey, value, false})
				}
			}
		}
		for _, alias := range info.Aliases {
			if value, ok := lookupEnv(alias); ok {
				candidates = append(candidates, candidate{
					name:  alias,
					value: value,
					old:   info.DeprecatedSet[alias],
				})
			}
		}

		policy := cfg.ConflictPolicy
		switch info.Conflict {
		case "error":
			policy = ConflictError
		case "priority":
			policy = ConflictPriority
		}

		var (
			value   string
			ok      bool
			source  string
			usedOld []string
		)

		if len(candidates) > 0 {
			chosen := candidates[0]
			conflict := false
			for _, candidate := range candidates[1:] {
				if candidate.value != chosen.value {
					conflict = true
					break
				}
			}
			if conflict && policy == ConflictError {
				names := make([]string, len(candidates))
				for i, candidate := range candidates {
					names[i] = candidate.name
				}
				return nil, fmt.Errorf(
					"conflicting environment variables %s for field %s: values differ",
					strings.Join(names, ", "), info.Name)
			}
			value = chosen.value
			ok = true
			source = chosen.name
			if chosen.old {
				usedOld = append(usedOld, chosen.name)
			}

			if conflict {
				addWarning(&fl, fmt.Sprintf(
					"multiple environment variables set for field %s; using %s with priority %s",
					info.Name, chosen.name, precedenceString(candidates)))
			} else if len(candidates) > 1 {
				// Same value supplied under several names: report a single
				// deprecation notice covering every deprecated source.
				for _, candidate := range candidates[1:] {
					if candidate.old {
						usedOld = append(usedOld, candidate.name)
					}
				}
			}
		}

		def := info.Tags.Get("default")
		if def != "" && !ok {
			value = def
			fl.Default = true
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
			report.Fields = append(report.Fields, fl)
			continue
		}

		if ok {
			fl.Source = source
			fl.Value = value
		} else {
			fl.Value = value
		}

		if ok && len(usedOld) > 0 {
			msg := fmt.Sprintf(
				"%s is deprecated, use %s instead",
				strings.Join(usedOld, ", "), info.Key)
			if info.DeprecationMsg != "" {
				msg = fmt.Sprintf("%s is deprecated: %s; use %s instead",
					strings.Join(usedOld, ", "), info.DeprecationMsg, info.Key)
			}
			addWarning(&fl, msg)
		}

		if err := processField(value, info.Field); err != nil {
			return nil, &ParseError{
				KeyName:   info.Key,
				FieldName: info.Name,
				TypeName:  info.Field.Type().String(),
				Value:     value,
				Err:       err,
			}
		}

		report.Fields = append(report.Fields, fl)
	}

	return report, nil
}

func precedenceString(candidates []candidate) string {
	names := make([]string, len(candidates))
	for i, candidate := range candidates {
		names[i] = candidate.name
	}
	return strings.Join(names, " > ")
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
