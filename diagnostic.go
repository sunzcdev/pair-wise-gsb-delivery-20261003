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
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// FieldError describes a single field conversion failure discovered by
// MapToDiagnostics.
type FieldError struct {
	// Path is the dot-separated Go struct field path of the failed field,
	// e.g. "Server.Addr". Slice elements carry their index, e.g. "Ports[2]"
	// or "Servers[1].Port".
	Path string
	// Section is the name of the INI section the value was read from.
	Section string
	// Key is the name of the INI key that failed to convert. It is empty
	// when the failure is not specific to a key, e.g. a missing section.
	Key string
	// Reason is the human-readable cause of the failure.
	Reason string
	// Err is the underlying error.
	Err error
}

// Error returns a human-readable description of the field error.
func (e FieldError) Error() string {
	if e.Key != "" {
		return fmt.Sprintf("field %q (section %q, key %q): %s", e.Path, e.Section, e.Key, e.Reason)
	}
	return fmt.Sprintf("field %q (section %q): %s", e.Path, e.Section, e.Reason)
}

// MappingReport is the result of a diagnostic mapping. It collects every
// field conversion error that could be discovered independently in a single
// pass, in struct field definition order.
//
// Mapping is applied partially: fields that converted successfully have been
// written to the target, while fields reported in Errors kept the value they
// had before the call.
type MappingReport struct {
	Errors []FieldError
}

// Error returns a human-readable summary of all field errors.
func (r *MappingReport) Error() string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "%d field mapping error(s):", len(r.Errors))
	for _, e := range r.Errors {
		buf.WriteString("\n  ")
		buf.WriteString(e.Error())
	}
	return buf.String()
}

// diagnosticMapper walks a struct in field definition order and collects all
// conversion errors instead of stopping at the first one.
type diagnosticMapper struct {
	errors []FieldError
}

func (dm *diagnosticMapper) add(path, section, key string, err error) {
	dm.errors = append(dm.errors, FieldError{
		Path:    path,
		Section: section,
		Key:     key,
		Reason:  err.Error(),
		Err:     err,
	})
}

// setSliceElement parses str into a single slice element.
func setSliceElement(elem reflect.Value, str string) error {
	switch elem.Kind() {
	case reflect.String:
		elem.SetString(str)
	case reflect.Int, reflect.Int64:
		val, err := strconv.ParseInt(str, 0, 64)
		if err != nil {
			return err
		}
		elem.SetInt(val)
	case reflect.Uint, reflect.Uint64:
		val, err := strconv.ParseUint(str, 0, 64)
		if err != nil {
			return err
		}
		elem.SetUint(val)
	case reflect.Float64:
		val, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return err
		}
		elem.SetFloat(val)
	case reflect.Bool:
		val, err := parseBool(str)
		if err != nil {
			return err
		}
		elem.SetBool(val)
	case reflectTime:
		val, err := time.Parse(time.RFC3339, str)
		if err != nil {
			return err
		}
		elem.Set(reflect.ValueOf(val))
	default:
		return fmt.Errorf("unsupported type '[]%s'", elem.Kind())
	}
	return nil
}

// setSlice converts the key value into a slice, reporting one error per
// failing element. If any element fails, the field is left unchanged.
func (dm *diagnosticMapper) setSlice(key *Key, field reflect.Value, delim string, allowShadow bool, path, keyName string) {
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
		// The whole field is unsupported: report the parent field only,
		// without fabricating one error per element.
		dm.add(path, key.s.name, keyName, fmt.Errorf("unsupported type '[]%s'", sliceOf))
		return
	}

	slice := reflect.MakeSlice(field.Type(), len(strs), len(strs))
	failed := false
	for i, str := range strs {
		if err := setSliceElement(slice.Index(i), str); err != nil {
			dm.add(fmt.Sprintf("%s[%d]", path, i), key.s.name, keyName, err)
			failed = true
		}
	}
	if failed {
		// Keep the value the field had before the call.
		return
	}
	field.Set(slice)
}

// mapStruct maps the section to the struct value, collecting all field errors.
// The pathPrefix is the dot-separated field path of the struct itself.
func (dm *diagnosticMapper) mapStruct(s *Section, val reflect.Value, sectionIndex int, sectionName, pathPrefix string) {
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
		if pathPrefix != "" {
			fieldPath = pathPrefix + "." + tpField.Name
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
				childName := s.name + s.f.options.ChildSectionDelimiter + rawName
				if secs, err := s.f.SectionsByName(childName); err == nil && sectionIndex < len(secs) {
					fieldSection = secs[sectionIndex]
				}
			}
			// Embedded fields are promoted, keep the path prefix unchanged.
			dm.mapStruct(fieldSection, field, sectionIndex, sectionName, pathPrefix)
		} else if isAnonymousPtr || isStruct || isStructPtr {
			if secs, err := s.f.SectionsByName(fieldName); err == nil {
				if len(secs) <= sectionIndex {
					// The parent field failed: report it and do not
					// fabricate errors for its child fields.
					dm.add(fieldPath, s.name, "", fmt.Errorf("there are not enough sections (%d <= %d) for the field %q", len(secs), sectionIndex, fieldName))
					continue
				}
				// Only set the field to non-nil struct value if we have a section for it.
				// Otherwise, we end up with a non-nil struct ptr even though there is no data.
				if isStructPtr && field.IsNil() {
					field.Set(reflect.New(tpField.Type.Elem()))
				}
				dm.mapStruct(secs[sectionIndex], field, sectionIndex, fieldName, fieldPath)
				continue
			}
		}

		// Map non-unique sections
		if allowNonUnique && tpField.Type.Kind() == reflect.Slice {
			dm.mapNonUniqueSlice(s, fieldName, field, fieldPath)
			continue
		}

		key, err := s.GetKey(fieldName)
		if err != nil {
			continue
		}
		delim := parseDelim(tpField.Tag.Get("delim"))
		if tpField.Type.Kind() == reflect.Slice {
			dm.setSlice(key, field, delim, allowShadow, fieldPath, fieldName)
			continue
		}
		if err := setWithProperType(tpField.Type, key, field, delim, allowShadow, true); err != nil {
			dm.add(fieldPath, s.name, fieldName, err)
		}
	}
}

// mapNonUniqueSlice maps all sections with the same name onto a slice of
// structs, reporting errors with the index of the element they belong to.
// If any element fails, the field is left unchanged.
func (dm *diagnosticMapper) mapNonUniqueSlice(s *Section, fieldName string, field reflect.Value, fieldPath string) {
	secs, err := s.f.SectionsByName(fieldName)
	if err != nil {
		// The parent field failed: report it and do not fabricate errors
		// for its elements.
		dm.add(fieldPath, s.name, "", err)
		return
	}

	typ := field.Type().Elem()
	newField := field
	failed := false
	for i, sec := range secs {
		elem := reflect.New(typ)
		before := len(dm.errors)
		dm.mapStruct(sec, elem, i, sec.name, fmt.Sprintf("%s[%d]", fieldPath, i))
		if len(dm.errors) > before {
			failed = true
			continue
		}
		newField = reflect.Append(newField, elem.Elem())
	}
	if failed {
		// Keep the value the field had before the call.
		return
	}
	field.Set(newField)
}

// mapToDiagnostics maps a section to object v and collects all field errors.
func (s *Section) mapToDiagnostics(v interface{}) *MappingReport {
	typ := reflect.TypeOf(v)
	val := reflect.ValueOf(v)
	if typ.Kind() != reflect.Pointer {
		err := errors.New("not a pointer to a struct")
		return &MappingReport{Errors: []FieldError{{Reason: err.Error(), Err: err}}}
	}
	typ = typ.Elem()
	val = val.Elem()

	dm := &diagnosticMapper{}
	if typ.Kind() == reflect.Slice {
		secs, err := s.f.SectionsByName(s.name)
		if err != nil {
			dm.add("", s.name, "", err)
		} else {
			elemTyp := typ.Elem()
			newField := val
			failed := false
			for i, sec := range secs {
				elem := reflect.New(elemTyp)
				before := len(dm.errors)
				dm.mapStruct(sec, elem, i, sec.name, fmt.Sprintf("[%d]", i))
				if len(dm.errors) > before {
					failed = true
					continue
				}
				newField = reflect.Append(newField, elem.Elem())
			}
			if !failed {
				val.Set(newField)
			}
		}
	} else {
		dm.mapStruct(s, val, 0, s.name, "")
	}

	if len(dm.errors) == 0 {
		return nil
	}
	return &MappingReport{Errors: dm.errors}
}

// MapToDiagnostics maps the section to the given struct in diagnostic mode.
//
// Unlike MapTo and StrictMapTo, which stop at the first error, diagnostic
// mode walks the whole struct and reports every field conversion error that
// can be discovered independently in a single call. Each reported error
// carries the target field path, the section, the key and the reason.
//
// Mapping is applied partially: fields that convert successfully are written
// to v, while fields that fail keep the value they had before the call. If
// any element of a slice field fails, the whole slice field is left unchanged
// and each failing element is reported with its index, e.g. "Ports[2]". When
// a parent field fails (e.g. its section is missing), only the parent error
// is reported and no errors are fabricated for its child fields.
//
// Errors are reported in struct field definition order (depth-first), so the
// report is stable and does not depend on section map iteration order.
//
// The returned report is nil when no error was found.
func (s *Section) MapToDiagnostics(v interface{}) *MappingReport {
	return s.mapToDiagnostics(v)
}

// MapToDiagnostics maps the file to the given struct in diagnostic mode.
// See Section.MapToDiagnostics for the partial application semantics.
func (f *File) MapToDiagnostics(v interface{}) *MappingReport {
	return f.Section("").MapToDiagnostics(v)
}
