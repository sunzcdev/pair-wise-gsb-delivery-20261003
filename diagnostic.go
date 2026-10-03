// Copyright 2026 Unknwon
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package ini

import (
	"fmt"
	"reflect"
	"strings"
	"time"
)

// MapError describes a single field conversion failure discovered by
// MapToWithDiagnostics.
type MapError struct {
	// Path is the target field path built from Go field names,
	// e.g. "Server.Port". Slice elements are suffixed with their index,
	// e.g. "Servers[1].Host" or "Ports[2]".
	Path string
	// Section is the name of the section the value was read from.
	Section string
	// Key is the name of the key within the section. It is empty when the
	// failure concerns a whole section rather than a single key.
	Key string
	// Reason describes why the conversion failed.
	Reason string
}

// Error implements the error interface.
func (e MapError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("field %q (section %q): %s", e.Path, e.Section, e.Reason)
	}
	return fmt.Sprintf("field %q (section %q, key %q): %s", e.Path, e.Section, e.Key, e.Reason)
}

// MapReport is the result of a diagnostic mapping performed by
// MapToWithDiagnostics.
//
// The mapping is partially applied: fields that converted successfully were
// written to the target, while every field listed in Errors kept the value it
// had before the call.
type MapReport struct {
	// Errors lists every field conversion failure in the order the fields
	// are declared in the struct definition. The order is stable and does
	// not depend on the iteration order of the section or key maps.
	Errors []MapError
}

// HasErrors returns true if the report contains at least one error.
func (r *MapReport) HasErrors() bool {
	return r != nil && len(r.Errors) > 0
}

// String returns a human-readable summary of the report.
func (r *MapReport) String() string {
	if !r.HasErrors() {
		return "ini: mapping completed without errors"
	}
	var buf strings.Builder
	fmt.Fprintf(&buf, "ini: %d field(s) failed to map", len(r.Errors))
	buf.WriteString(" (successful fields were applied; failed fields kept their previous values):")
	for _, e := range r.Errors {
		fmt.Fprintf(&buf, "\n  - %s", e)
	}
	return buf.String()
}

// mapErrorCollector accumulates field conversion errors in discovery order.
type mapErrorCollector struct {
	errs []MapError
}

func (c *mapErrorCollector) add(path, section, key string, err error) {
	c.errs = append(c.errs, MapError{
		Path:    path,
		Section: section,
		Key:     key,
		Reason:  err.Error(),
	})
}

// collectMapErrors walks the given value in struct definition order and
// collects every field conversion error instead of stopping at the first one.
// Fields that convert successfully are written; fields that fail are left
// untouched. When a parent field fails, its children are not visited, so no
// synthetic child errors are reported for it.
func (s *Section) collectMapErrors(val reflect.Value, sectionIndex int, path string, c *mapErrorCollector) {
	if val.Kind() == reflect.Pointer {
		val = val.Elem()
	}
	typ := val.Type()

	for i := 0; i < typ.NumField(); i++ {
		field := val.Field(i)
		tpField := typ.Field(i)

		tag := tpField.Tag.Get("ini")
		if tag == "-" {
			continue
		}

		rawName, _, allowShadow, allowNonUnique, extends := parseTagOptions(tag)
		fieldName := s.parseFieldName(tpField.Name, rawName)
		if len(fieldName) == 0 || !field.CanSet() {
			continue
		}

		fieldPath := tpField.Name
		if len(path) > 0 {
			fieldPath = path + "." + tpField.Name
		}

		isStruct := tpField.Type.Kind() == reflect.Struct
		isStructPtr := tpField.Type.Kind() == reflect.Pointer && tpField.Type.Elem().Kind() == reflect.Struct
		isAnonymousPtr := tpField.Type.Kind() == reflect.Pointer && tpField.Anonymous
		if isAnonymousPtr {
			field.Set(reflect.New(tpField.Type.Elem()))
		}

		if extends && (isAnonymousPtr || (isStruct && tpField.Anonymous)) {
			if isStructPtr && field.IsNil() {
				field.Set(reflect.New(tpField.Type.Elem()))
			}
			fieldSection := s
			if rawName != "" {
				sectionName := s.name + s.f.options.ChildSectionDelimiter + rawName
				if secs, err := s.f.SectionsByName(sectionName); err == nil && sectionIndex < len(secs) {
					fieldSection = secs[sectionIndex]
				}
			}
			fieldSection.collectMapErrors(field, sectionIndex, fieldPath, c)
			continue
		}

		if isAnonymousPtr || isStruct || isStructPtr {
			if secs, err := s.f.SectionsByName(fieldName); err == nil {
				if len(secs) <= sectionIndex {
					c.add(fieldPath, fieldName, "", fmt.Errorf("there are not enough sections (%d <= %d) for the field", len(secs), sectionIndex))
					continue
				}
				// Only set the field to non-nil struct value if we have a section for it.
				// Otherwise, we end up with a non-nil struct ptr even though there is no data.
				if isStructPtr && field.IsNil() {
					field.Set(reflect.New(tpField.Type.Elem()))
				}
				secs[sectionIndex].collectMapErrors(field, sectionIndex, fieldPath, c)
				continue
			}
		}

		// Map non-unique sections
		if allowNonUnique && tpField.Type.Kind() == reflect.Slice {
			secs, err := s.f.SectionsByName(fieldName)
			if err != nil {
				c.add(fieldPath, fieldName, "", err)
				continue
			}

			elemTyp := field.Type().Elem()
			newField := field
			for i, sec := range secs {
				elem := reflect.New(elemTyp)
				sec.collectMapErrors(elem, i, fmt.Sprintf("%s[%d]", fieldPath, i), c)
				newField = reflect.Append(newField, elem.Elem())
			}
			field.Set(newField)
			continue
		}

		key, err := s.GetKey(fieldName)
		if err != nil {
			continue
		}
		delim := parseDelim(tpField.Tag.Get("delim"))
		if tpField.Type.Kind() == reflect.Slice {
			s.setSliceDiagnostic(key, field, delim, allowShadow, fieldPath, c)
			continue
		}
		if err = setWithProperType(tpField.Type, key, field, delim, allowShadow, true); err != nil {
			c.add(fieldPath, s.name, fieldName, err)
		}
	}
}

// setSliceDiagnostic converts a key value to a slice in diagnostic mode.
// Every element that fails to convert is reported with its index, and the
// field is only written when all elements convert successfully.
func (s *Section) setSliceDiagnostic(key *Key, field reflect.Value, delim string, allowShadow bool, path string, c *mapErrorCollector) {
	var strs []string
	if allowShadow {
		strs = key.StringsWithShadows(delim)
	} else {
		strs = key.Strings(delim)
	}
	if len(strs) == 0 {
		return
	}

	sliceOf := field.Type().Elem().Kind()
	switch sliceOf {
	case reflect.String, reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint64, reflect.Float64, reflect.Bool, reflectTime:
	default:
		c.add(path, s.name, key.name, fmt.Errorf("unsupported type '[]%s'", sliceOf))
		return
	}

	slice := reflect.MakeSlice(field.Type(), len(strs), len(strs))
	ok := true
	for i, str := range strs {
		elemKey := newKey(key.s, key.name, str)
		val, err := parseSliceElement(elemKey, sliceOf, str)
		if err != nil {
			c.add(fmt.Sprintf("%s[%d]", path, i), s.name, key.name, err)
			ok = false
			continue
		}
		slice.Index(i).Set(val)
	}
	if ok {
		field.Set(slice)
	}
}

// parseSliceElement parses a single slice element value and returns it as a
// reflect.Value ready to be assigned to a slice of the given element kind.
func parseSliceElement(key *Key, kind reflect.Kind, str string) (reflect.Value, error) {
	switch kind {
	case reflect.String:
		return reflect.ValueOf(str), nil
	case reflect.Int:
		vals, err := key.parseInts([]string{str}, false, true)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(vals[0]), nil
	case reflect.Int64:
		vals, err := key.parseInt64s([]string{str}, false, true)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(vals[0]), nil
	case reflect.Uint:
		vals, err := key.parseUints([]string{str}, false, true)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(vals[0]), nil
	case reflect.Uint64:
		vals, err := key.parseUint64s([]string{str}, false, true)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(vals[0]), nil
	case reflect.Float64:
		vals, err := key.parseFloat64s([]string{str}, false, true)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(vals[0]), nil
	case reflect.Bool:
		vals, err := key.parseBools([]string{str}, false, true)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(vals[0]), nil
	case reflectTime:
		vals, err := key.parseTimesFormat(time.RFC3339, []string{str}, false, true)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(vals[0]), nil
	}
	return reflect.Value{}, fmt.Errorf("unsupported type '[]%s'", kind)
}

// mapToDiagnostics maps a section to object v in diagnostic mode and returns
// a report of all field conversion errors.
func (s *Section) mapToDiagnostics(v interface{}) *MapReport {
	report := &MapReport{}
	c := &mapErrorCollector{}
	defer func() {
		report.Errors = c.errs
	}()

	typ := reflect.TypeOf(v)
	val := reflect.ValueOf(v)
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
		val = val.Elem()
	} else {
		c.add("", s.name, "", fmt.Errorf("not a pointer to a struct"))
		return report
	}

	if typ.Kind() == reflect.Slice {
		secs, err := s.f.SectionsByName(s.name)
		if err != nil {
			c.add("", s.name, "", err)
			return report
		}

		elemTyp := val.Type().Elem()
		newField := val
		for i, sec := range secs {
			elem := reflect.New(elemTyp)
			sec.collectMapErrors(elem, i, fmt.Sprintf("[%d]", i), c)
			newField = reflect.Append(newField, elem.Elem())
		}
		val.Set(newField)
		return report
	}

	s.collectMapErrors(val, 0, "", c)
	return report
}

// MapToWithDiagnostics maps section to given struct in diagnostic mode:
// instead of stopping at the first error, it collects every field conversion
// failure that can be discovered independently and reports them all at once.
//
// The mapping is partially applied: fields that converted successfully are
// written to v, while fields listed in the report keep the values they had
// before the call.
func (s *Section) MapToWithDiagnostics(v interface{}) *MapReport {
	return s.mapToDiagnostics(v)
}

// MapToWithDiagnostics maps file to given struct in diagnostic mode:
// instead of stopping at the first error, it collects every field conversion
// failure that can be discovered independently and reports them all at once.
//
// The mapping is partially applied: fields that converted successfully are
// written to v, while fields listed in the report keep the values they had
// before the call.
func (f *File) MapToWithDiagnostics(v interface{}) *MapReport {
	return f.Section("").MapToWithDiagnostics(v)
}
