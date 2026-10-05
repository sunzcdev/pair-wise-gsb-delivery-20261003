package validator

// JSON Schema export.
//
// This file translates a subset of validator struct tags into a JSON Schema
// (draft 2020-12) document. Only constraints that can be expressed
// equivalently are translated; every constraint that cannot be represented
// (or can only be represented approximately) is reported through the returned
// diagnostics instead of being silently dropped. See README.md
// ("JSON Schema export") for the supported subset and known gaps.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const jsonSchemaDialect = "https://json-schema.org/draft/2020-12/schema"

// DiagnosticKind classifies why a validator constraint could not be exported
// to JSON Schema.
type DiagnosticKind string

const (
	// DiagnosticUnsupported means the tag cannot be expressed in JSON Schema
	// at all and was therefore not translated. Consumers must keep relying on
	// Go-side validation for this constraint.
	DiagnosticUnsupported DiagnosticKind = "unsupported"

	// DiagnosticApproximate means a related JSON Schema keyword was emitted,
	// but its semantics do not fully match the Go tag. The emitted keyword is
	// a best-effort approximation, not an equivalent.
	DiagnosticApproximate DiagnosticKind = "approximate"
)

// SchemaDiagnostic reports a single constraint that could not be translated
// into JSON Schema equivalently.
type SchemaDiagnostic struct {
	// Kind is either DiagnosticUnsupported or DiagnosticApproximate.
	Kind DiagnosticKind `json:"kind"`

	// JSONPointer is a RFC 6901 JSON Pointer identifying the schema node the
	// constraint applies to (e.g. "/properties/Name/properties/Tags/items").
	JSONPointer string `json:"jsonPointer"`

	// Tag is the validator tag that could not be translated
	// (e.g. "email", "gtfield").
	Tag string `json:"tag"`

	// Param is the tag's parameter, or "" when it has none.
	Param string `json:"param,omitempty"`

	// Reason explains why the tag is unsupported or only approximate.
	Reason string `json:"reason"`
}

// Schema is a subset of JSON Schema draft 2020-12 keywords. Numeric bounds use
// json.Number so that integer limits (including values larger than float64
// can represent safely) are rendered without loss.
type Schema struct {
	Ref                  string             `json:"$ref,omitempty"`
	Schema               string             `json:"$schema,omitempty"`
	Type                 []string           `json:"type,omitempty"`
	AnyOf                []*Schema          `json:"anyOf,omitempty"`
	Not                  *Schema            `json:"not,omitempty"`
	Enum                 []any              `json:"enum,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	PatternProperties    map[string]*Schema `json:"patternProperties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	AdditionalProperties any                `json:"additionalProperties,omitempty"`
	PropertyNames        *Schema            `json:"propertyNames,omitempty"`
	MinProperties        *int               `json:"minProperties,omitempty"`
	MaxProperties        *int               `json:"maxProperties,omitempty"`
	MinItems             *int               `json:"minItems,omitempty"`
	MaxItems             *int               `json:"maxItems,omitempty"`
	UniqueItems          bool               `json:"uniqueItems,omitempty"`
	MinLength            *int               `json:"minLength,omitempty"`
	MaxLength            *int               `json:"maxLength,omitempty"`
	Pattern              string             `json:"pattern,omitempty"`
	Minimum              *json.Number       `json:"minimum,omitempty"`
	Maximum              *json.Number       `json:"maximum,omitempty"`
	ExclusiveMinimum     *json.Number       `json:"exclusiveMinimum,omitempty"`
	ExclusiveMaximum     *json.Number       `json:"exclusiveMaximum,omitempty"`
	Defs                 map[string]*Schema `json:"$defs,omitempty"`
}

// JSONSchemaExport is the result of exporting a Go type to JSON Schema.
type JSONSchemaExport struct {
	// Schema is the exported document. Root-level $ref/$defs model nested
	// named struct types.
	Schema *Schema

	// Diagnostics lists every validator constraint that could not be
	// translated equivalently. The export must never be treated as complete
	// when this slice is non-empty.
	Diagnostics []SchemaDiagnostic
}

// ExportJSONSchema generates a JSON Schema draft 2020-12 document for the
// struct type represented by s (a value or pointer to the struct).
//
// The returned schema only encodes the validator tag subset that can be
// represented equivalently. Every dropped or approximated constraint is
// returned as a SchemaDiagnostic; callers must inspect the diagnostics before
// assuming the export is complete.
//
// Field names follow encoding/json: a `json` tag name is used when present,
// the Go field name otherwise, and fields tagged `json:"-"` are skipped.
func (v *Validate) ExportJSONSchema(s interface{}) (*JSONSchemaExport, error) {
	rv := reflect.ValueOf(s)
	if !rv.IsValid() {
		return nil, &InvalidValidationError{Type: nil}
	}
	typ := rv.Type()
	for typ != nil && typ.Kind() == reflect.Ptr {
		if rv.Kind() == reflect.Ptr && rv.IsNil() {
			return nil, &InvalidValidationError{Type: typ}
		}
		if rv.Kind() == reflect.Ptr {
			rv = rv.Elem()
		}
		typ = typ.Elem()
	}
	if typ == nil || typ.Kind() != reflect.Struct || typ.ConvertibleTo(timeType) {
		return nil, &InvalidValidationError{Type: reflect.TypeOf(s)}
	}

	e := &schemaExporter{
		v:       v,
		defs:    make(map[string]*Schema),
		typeDef: make(map[reflect.Type]string),
	}

	root := e.schemaForStruct(typ, "/$defs/root")
	root.Schema = jsonSchemaDialect
	if len(e.defs) > 0 {
		root.Defs = e.defs
	}

	return &JSONSchemaExport{Schema: root, Diagnostics: e.diags}, nil
}

type schemaExporter struct {
	v       *Validate
	defs    map[string]*Schema
	typeDef map[reflect.Type]string // type -> def key (named and anonymous)
	diags   []SchemaDiagnostic
	anonSeq int
}

func (e *schemaExporter) diag(ptr string, kind DiagnosticKind, tag, param, reason string) {
	e.diags = append(e.diags, SchemaDiagnostic{
		Kind:        kind,
		JSONPointer: ptr,
		Tag:         tag,
		Param:       param,
		Reason:      reason,
	})
}

// schemaForStruct builds (or reuses via $ref) the object schema for typ.
// Every named struct, including the root, lives under $defs so cycles and
// repeated references share one definition.
func (e *schemaExporter) schemaForStruct(typ reflect.Type, ptr string) *Schema {
	if name, ok := e.typeDef[typ]; ok {
		return &Schema{Ref: "#/$defs/" + name}
	}

	var name string
	if typ.Name() != "" {
		name = e.uniqueDefName(typ)
	} else {
		e.anonSeq++
		name = fmt.Sprintf("anon%d", e.anonSeq)
		for {
			if _, taken := e.defs[name]; !taken {
				break
			}
			e.anonSeq++
			name = fmt.Sprintf("anon%d", e.anonSeq)
		}
	}

	// Reserve before descending so recursive references resolve to this $ref.
	e.typeDef[typ] = name
	schema := &Schema{Type: []string{"object"}}
	e.defs[name] = schema

	if _, ok := e.v.structLevelFuncs[typ]; ok {
		e.diag("/$defs/"+name, DiagnosticUnsupported, "struct-level", "",
			"a RegisterStructValidation function is attached to this type; JSON Schema cannot invoke Go code")
	}

	e.buildStructFields(schema, typ, "/$defs/"+name)
	return &Schema{Ref: "#/$defs/" + name}
}

func (e *schemaExporter) buildStructFields(schema *Schema, typ reflect.Type, ptr string) {
	fields := collectJSONFields(typ)
	if len(fields) == 0 {
		return
	}
	schema.Properties = make(map[string]*Schema, len(fields))

	for _, jf := range fields {
		fld := typ.FieldByIndex(jf.index)
		fptr := ptr + "/properties/" + escapeJSONPointerToken(jf.name)
		e.buildField(schema, jf.name, fld, fptr)
	}
}

// buildField translates one struct field into a property plus, when needed, a
// "required" entry.
func (e *schemaExporter) buildField(parent *Schema, name string, fld reflect.StructField, ptr string) {
	tag := fld.Tag.Get(e.v.tagName)

	var root *cTag
	if len(tag) > 0 && tag != skipValidationTag {
		// Report unregistered tags individually before parsing so the
		// diagnostic names the exact tag rather than a parser failure.
		e.reportUnknownTags(tag, ptr)

		var recovered any
		root, recovered = e.safeParseTags(tag, fld.Name, ptr)
		if recovered != nil {
			// Continue without tags: structural typing is still exported, but
			// the failure is diagnosed so nothing is silently lost.
			root = nil
		}
	}

	ctx := &fieldContext{
		required:  rootHasTag(root, requiredTag),
		omitEmpty: rootHasType(root, typeOmitEmpty) || rootHasType(root, typeOmitZero) || rootHasType(root, typeIsDefault),
		omitNil:   rootHasType(root, typeOmitNil),
	}

	// omitempty only creates a divergence when later tags actually translate
	// to keywords that still run against a present zero value.
	if ctx.omitEmpty && chainHasTranslatedConstraint(root) {
		e.diag(ptr, DiagnosticApproximate, omitempty, "",
			"omitempty skips later tags when the value is the Go zero value, but JSON Schema "+
				"keywords still apply to a present zero value; the schema may reject zero values that Go accepts")
	}

	fieldSchema := e.schemaForType(fld.Type, ptr, root, ctx)

	// Go's required rejects the type's zero value for non-pointer scalars and
	// empty collections. "required" guarantees key presence; the non-zero
	// condition is added here. Pointers are handled by disallowing null.
	if ctx.required {
		e.addNonZeroRestriction(fieldSchema, fld.Type, ptr)
	}

	parent.Properties[name] = fieldSchema

	if ctx.required {
		parent.Required = append(parent.Required, name)
		sort.Strings(parent.Required)
	}
}

// fieldContext carries presence semantics discovered from the head of the tag
// chain (required / omitempty / omitnil), which decide whether a pointer is
// nullable and whether the property is mandatory.
type fieldContext struct {
	required  bool
	omitEmpty bool
	omitNil   bool
}

// addNonZeroRestriction mirrors hasValue for the common scalar and
// collection kinds: required fails on "", 0, false, or an empty
// array/object. Pointers are excluded (null is disallowed instead).
func (e *schemaExporter) addNonZeroRestriction(schema *Schema, typ reflect.Type, ptr string) {
	if typ.Kind() == reflect.Ptr {
		// Pointer presence is encoded by disallowing null in schemaForPointer;
		// nothing extra is needed here.
		return
	}

	switch typ.Kind() {
	case reflect.Struct:
		if typ.ConvertibleTo(timeType) {
			return
		}
		e.diag(ptr, DiagnosticApproximate, requiredTag, "",
			"required on a non-pointer struct only checks that the struct value is present, "+
				"but Go has no general zero-struct test in JSON Schema")
	case reflect.String:
		addNotEnum(schema, "")
	case reflect.Bool:
		addNotEnum(schema, false)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		addNotEnum(schema, 0)
	case reflect.Slice, reflect.Array, reflect.Map:
		// A JSON []/{} unmarshals to a non-nil Go value, which satisfies
		// hasValue; key presence plus the type keyword is sufficient.
	}
}

// addNotEnum adds a "not: {enum:[v]}" clause to a bare schema node.
func addNotEnum(schema *Schema, v any) {
	if schema.Not == nil {
		schema.Not = &Schema{}
	}
	schema.Not.Enum = []any{v}
}

func rootHasTag(root *cTag, tag string) bool {
	for ct := root; ct != nil; ct = ct.next {
		if ct.tag == tag && ct.typeof != typeOr {
			return true
		}
	}
	return false
}

func rootHasType(root *cTag, want tagType) bool {
	for ct := root; ct != nil; ct = ct.next {
		if ct.typeof == want {
			return true
		}
	}
	return false
}

// chainHasTranslatedConstraint reports whether the tag chain (ignoring the
// omitempty-family markers and required) contains at least one tag this
// exporter turns into a JSON Schema keyword.
func chainHasTranslatedConstraint(root *cTag) bool {
	for ct := root; ct != nil; ct = ct.next {
		switch ct.typeof {
		case typeDefault, typeOr:
			if ct.tag != "" && ct.tag != requiredTag {
				return true
			}
		}
	}
	return false
}

// safeParseTags parses a field tag chain, recovering from panics that the
// validator parser raises for an undefined tag.
func (e *schemaExporter) safeParseTags(tag, fieldName, ptr string) (root *cTag, recovered any) {
	defer func() {
		if r := recover(); r != nil {
			recovered = r
			e.diag(ptr, DiagnosticUnsupported, "<parse>", tag,
				fmt.Sprintf("tag chain could not be compiled and was skipped: %v", r))
		}
	}()
	root, _ = e.v.parseFieldTagsRecursive(tag, fieldName, "", false)
	return root, nil
}

// structuralTags are parser directives rather than registered validations;
// they must not be flagged as unknown.
var structuralTags = map[string]bool{
	"dive": true, "keys": true, "endkeys": true,
	omitempty: true, omitnil: true, omitzero: true, isdefault: true,
	structOnlyTag: true, noStructLevelTag: true,
}

// reportUnknownTags walks the comma/pipe-separated tag expression (including
// the region inside keys...endkeys) and emits a diagnostic for each tag name
// that is neither built-in, aliased, nor user-registered.
func (e *schemaExporter) reportUnknownTags(tag, ptr string) {
	parts := strings.Split(tag, tagSeparator)
	for i := 0; i < len(parts); i++ {
		part := parts[i]

		// Skip the region delimited by keys ... endkeys; the tags in between
		// are validated separately as map-key constraints.
		if part == keysTag {
			for i++; i < len(parts); i++ {
				if parts[i] == endKeysTag {
					break
				}
			}
			continue
		}

		for _, orExpr := range strings.Split(part, orSeparator) {
			name := orExpr
			if j := strings.Index(name, tagKeySeparator); j >= 0 {
				name = name[:j]
			}
			name = strings.TrimSpace(name)
			if name == "" || structuralTags[name] {
				continue
			}
			if _, ok := e.v.validations[name]; ok {
				continue
			}
			if _, ok := e.v.aliases[name]; ok {
				continue
			}
			e.diag(ptr, DiagnosticUnsupported, name, "",
				"custom or unknown validator; JSON Schema cannot invoke Go code")
		}
	}
}

// schemaForType converts a Go type plus its (possibly partially consumed) tag
// chain into a schema. Tags preceding a "dive" apply to the container; tags
// following it apply to elements/map values.
func (e *schemaExporter) schemaForType(typ reflect.Type, ptr string, root *cTag, fctx *fieldContext) *Schema {
	// Resolve named types to their underlying kind for keyword selection.
	underlying := typ
	for underlying.Kind() == reflect.Ptr {
		underlying = underlying.Elem()
	}

	switch {
	case typ.Kind() == reflect.Ptr:
		return e.schemaForPointer(typ, ptr, root, fctx)

	case underlying.ConvertibleTo(timeType) && underlying.Kind() == reflect.Struct:
		schema := &Schema{Type: []string{"string"}}
		e.applyAtomicTags(schema, typ, root, ptr)
		return schema

	case underlying.Kind() == reflect.Struct:
		// Constraints on the struct itself (as opposed to its fields) have no
		// structural JSON Schema translation; surface them as diagnostics.
		e.diagnoseTagsOnly(root, ptr)
		return e.schemaForStruct(underlying, ptr)

	case underlying.Kind() == reflect.Slice || underlying.Kind() == reflect.Array:
		return e.schemaForSlice(underlying, ptr, root)

	case underlying.Kind() == reflect.Map:
		return e.schemaForMap(underlying, ptr, root)

	case underlying.Kind() == reflect.Interface:
		// interface{} accepts any JSON value; tags can't constrain it.
		e.diagnoseTagsOnly(root, ptr)
		return &Schema{}

	default:
		schema := e.scalarSchema(underlying)
		e.applyAtomicTags(schema, typ, root, ptr)
		return schema
	}
}

func (e *schemaExporter) schemaForPointer(typ reflect.Type, ptr string, root *cTag, fctx *fieldContext) *Schema {
	elem := typ.Elem()

	// Presence semantics:
	//  - required            => key must be present and value non-null
	//  - omitnil/omitempty   => may be null (validator skips when nil)
	//  - plain (no presence) => validator runs tags even on nil, so a null
	//                           value with e.g. min= fails; null is disallowed.
	allowNull := !fctx.required && (fctx.omitNil || fctx.omitEmpty || !rootChainHasConstraints(root))

	elemSchema := e.schemaForType(elem, ptr, root, &fieldContext{})

	// A pointer introduces no array/object wrapping; merge null into types.
	if allowNull {
		if elemSchema.Ref != "" {
			return &Schema{
				AnyOf: []*Schema{{Ref: elemSchema.Ref}, {Type: []string{"null"}}},
			}
		}
		if len(elemSchema.Type) > 0 {
			elemSchema.Type = append(append([]string{}, elemSchema.Type...), "null")
		}
		return elemSchema
	}

	if elemSchema.Ref != "" {
		return &Schema{AnyOf: []*Schema{{Ref: elemSchema.Ref}}}
	}
	return elemSchema
}

func rootChainHasConstraints(root *cTag) bool {
	for ct := root; ct != nil; ct = ct.next {
		switch ct.typeof {
		case typeDefault:
			if isConstraintTag(ct.tag) {
				return true
			}
		case typeOr:
			return true
		}
	}
	return false
}

func (e *schemaExporter) schemaForSlice(typ reflect.Type, ptr string, root *cTag) *Schema {
	schema := &Schema{Type: []string{"array"}}

	// Split chain at the first dive: everything before constrains the array,
	// everything after constrains each element.
	before, after := splitAtDive(root)
	e.applyContainerTags(schema, typ, before, ptr)

	if after != nil {
		elemPtr := ptr + "/items"
		// Multiple dives: element is itself a container.
		schema.Items = e.schemaForType(typ.Elem(), elemPtr, after, &fieldContext{})
	} else if typ.Elem().Kind() == reflect.Struct {
		// encoding/json still decodes struct elements; emit their shape.
		schema.Items = e.schemaForType(typ.Elem(), ptr+"/items", nil, &fieldContext{})
	} else if typ.Elem().Kind() == reflect.Ptr {
		schema.Items = e.schemaForType(typ.Elem(), ptr+"/items", nil, &fieldContext{})
	}

	return schema
}

func (e *schemaExporter) schemaForMap(typ reflect.Type, ptr string, root *cTag) *Schema {
	schema := &Schema{Type: []string{"object"}}

	keyKind := typ.Key().Kind()
	if keyKind != reflect.String {
		e.diag(ptr, DiagnosticApproximate, "map-key-type", typ.Key().String(),
			"JSON object keys are always strings; non-string Go map keys cannot be represented faithfully")
	}

	before, after := splitAtDive(root)
	e.applyContainerTags(schema, typ, before, ptr)

	if after != nil {
		// A dive may optionally be preceded by keys...endkeys constraining map
		// keys, which in JSON Schema maps to "propertyNames".
		if after.typeof == typeKeys && after.keys != nil {
			keySchema := &Schema{Type: []string{"string"}}
			e.applyAtomicTags(keySchema, typ.Key(), after.keys, ptr+"/propertyNames")
			schema.PropertyNames = keySchema
			after = after.next
			// skip the matching endkeys marker if present
			if after != nil && after.typeof == typeEndKeys {
				after = after.next
			}
		}

		if after != nil {
			elem := typ.Elem()
			switch elem.Kind() {
			case reflect.Struct:
				schema.AdditionalProperties = e.schemaForType(elem, ptr+"/additionalProperties", after, &fieldContext{})
			default:
				schema.AdditionalProperties = e.schemaForType(elem, ptr+"/additionalProperties", after, &fieldContext{})
			}
		}
	}

	return schema
}

// splitAtDive divides a tag chain into the portion before the first dive and
// the portion that applies to elements/values after it.
func splitAtDive(root *cTag) (before, after *cTag) {
	if root == nil {
		return nil, nil
	}
	if root.typeof == typeDive {
		return nil, root.next
	}

	// Detach a shallow copy so the cached cTag linked list is never mutated.
	head := &cTag{tag: root.tag, param: root.param, typeof: root.typeof, aliasTag: root.aliasTag, hasParam: root.hasParam}
	tail := head
	for ct := root.next; ct != nil; ct = ct.next {
		if ct.typeof == typeDive {
			return head, ct.next
		}
		node := &cTag{
			tag:      ct.tag,
			param:    ct.param,
			typeof:   ct.typeof,
			aliasTag: ct.aliasTag,
			hasParam: ct.hasParam,
			keys:     ct.keys,
		}
		tail.next = node
		tail = node
	}
	return head, nil
}

func (e *schemaExporter) scalarSchema(typ reflect.Type) *Schema {
	switch typ.Kind() {
	case reflect.String:
		return &Schema{Type: []string{"string"}}
	case reflect.Bool:
		return &Schema{Type: []string{"boolean"}}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return &Schema{Type: []string{"integer"}}
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: []string{"number"}}
	default:
		// Channels, funcs, complex numbers, etc. do not survive encoding/json.
		return &Schema{}
	}
}

// applyContainerTags translates len/min/max/unique that validator applies to
// slice/map length, plus reporting tags it cannot represent.
func (e *schemaExporter) applyContainerTags(schema *Schema, typ reflect.Type, root *cTag, ptr string) {
	for ct := root; ct != nil; ct = ct.next {
		switch ct.typeof {
		case typeOr:
			e.diag(ptr, DiagnosticUnsupported, orTagLabel(ct), ct.param,
				"'or' alternatives on containers are not translated")
			ct = skipOrGroup(ct)
			continue
		case typeDive, typeKeys, typeEndKeys, typeStructOnly, typeNoStructLevel,
			typeOmitEmpty, typeOmitNil, typeOmitZero, typeIsDefault:
			continue
		}

		switch ct.tag {
		case "len", "eq":
			n, ok := parseBoundInt(ct.param)
			if !ok {
				e.diagUnparseable(ptr, ct)
				continue
			}
			if typ.Kind() == reflect.Map {
				schema.MinProperties, schema.MaxProperties = &n, &n
			} else {
				schema.MinItems, schema.MaxItems = &n, &n
			}
		case "min":
			n, ok := parseBoundInt(ct.param)
			if ok {
				if typ.Kind() == reflect.Map {
					schema.MinProperties = &n
				} else {
					schema.MinItems = &n
				}
			} else {
				e.diagUnparseable(ptr, ct)
			}
		case "max":
			n, ok := parseBoundInt(ct.param)
			if ok {
				if typ.Kind() == reflect.Map {
					schema.MaxProperties = &n
				} else {
					schema.MaxItems = &n
				}
			} else {
				e.diagUnparseable(ptr, ct)
			}
		case "unique":
			if ct.param != "" {
				e.diag(ptr, DiagnosticUnsupported, "unique", ct.param,
					"'unique=Field' dedupes on a struct field and has no JSON Schema equivalent")
			} else {
				schema.UniqueItems = true
			}
		default:
			e.classifyTag(schema, typ, ct, ptr)
		}
	}
}

// applyAtomicTags translates constraints on scalar values (strings, numbers,
// booleans, times) and reports everything not representable.
func (e *schemaExporter) applyAtomicTags(schema *Schema, typ reflect.Type, root *cTag, ptr string) {
	for ct := root; ct != nil; ct = ct.next {
		switch ct.typeof {
		case typeOr:
			e.diag(ptr, DiagnosticUnsupported, orTagLabel(ct), ct.param,
				"'or' alternatives are not translated")
			ct = skipOrGroup(ct)
			continue
		case typeOmitEmpty, typeOmitNil, typeOmitZero, typeIsDefault, typeStructOnly, typeNoStructLevel, typeDive, typeKeys, typeEndKeys:
			continue
		}
		e.classifyTag(schema, typ, ct, ptr)
	}
}

// diagnoseTagsOnly reports every constraint-bearing tag in a chain without
// attempting a translation, used where the Go type (struct, interface) has no
// schema node that could carry the keyword.
func (e *schemaExporter) diagnoseTagsOnly(root *cTag, ptr string) {
	for ct := root; ct != nil; ct = ct.next {
		switch ct.typeof {
		case typeOr:
			e.diag(ptr, DiagnosticUnsupported, orTagLabel(ct), ct.param,
				"'or' alternatives on this type are not translated")
			ct = skipOrGroup(ct)
			continue
		case typeOmitEmpty, typeOmitNil, typeOmitZero, typeIsDefault, typeStructOnly, typeNoStructLevel, typeDive, typeKeys, typeEndKeys:
			continue
		case typeDefault:
			if ct.tag == requiredTag {
				continue
			}
			e.diagUnknownTag(ct, ptr)
		}
	}
}

// classifyTag handles tags that are shared across scalar and container
// contexts; anything it does not recognize becomes a diagnostic.
func (e *schemaExporter) classifyTag(schema *Schema, typ reflect.Type, ct *cTag, ptr string) {
	switch ct.tag {
	case requiredTag:
		// handled at field presence level
		return

	case "len", "min", "max":
		e.applyLengthTag(schema, typ, ct, ptr)

	case "gt", "gte", "lt", "lte":
		e.applyRangeTag(schema, typ, ct, ptr)

	case "eq":
		e.applyEqTag(schema, typ, ct, ptr)

	case "ne":
		e.applyNeTag(schema, typ, ct, ptr)

	case "oneof":
		e.applyOneOfTag(schema, typ, ct, ptr, false)

	case "noneof":
		e.diag(ptr, DiagnosticUnsupported, "noneof", ct.param,
			"JSON Schema has no 'not in set' keyword; the allowed-set tag is oneof")

	default:
		e.diagUnknownTag(ct, ptr)
	}
}

func (e *schemaExporter) applyLengthTag(schema *Schema, typ reflect.Type, ct *cTag, ptr string) {
	n, ok := parseBoundInt(ct.param)
	if !ok {
		e.diagUnparseable(ptr, ct)
		return
	}

	switch typ.Kind() {
	case reflect.String:
		// validator counts UTF-8 runes; JSON Schema minLength/maxLength count
		// Unicode code points, which are the same for valid UTF-8.
		switch ct.tag {
		case "len":
			schema.MinLength, schema.MaxLength = &n, &n
		case "min":
			schema.MinLength = &n
		case "max":
			schema.MaxLength = &n
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		// For numbers validator treats min==gte and max==lte; len means the
		// value equals n.
		if !validJSONNumber(ct.param) {
			e.diagUnparseable(ptr, ct)
			return
		}
		num := json.Number(ct.param)
		switch ct.tag {
		case "len":
			setConst(schema, jsonNumeric(num))
		case "min":
			schema.Minimum = &num
		case "max":
			schema.Maximum = &num
		}
	default:
		if typ.ConvertibleTo(timeType) {
			e.diag(ptr, DiagnosticUnsupported, ct.tag, ct.param,
				"time.Time length comparisons are relative to now or durations and cannot be exported")
			return
		}
		e.diagUnknownTag(ct, ptr)
	}
}

func (e *schemaExporter) applyRangeTag(schema *Schema, typ reflect.Type, ct *cTag, ptr string) {
	switch typ.Kind() {
	case reflect.String:
		n, ok := parseBoundInt(ct.param)
		if !ok {
			e.diagUnparseable(ptr, ct)
			return
		}
		switch ct.tag {
		case "gt":
			// rune count strictly greater => minimum n+1
			n++
			schema.MinLength = &n
		case "gte":
			schema.MinLength = &n
		case "lt":
			// rune count strictly less => maximum n-1
			n--
			schema.MaxLength = &n
		case "lte":
			schema.MaxLength = &n
		}

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		num := json.Number(ct.param)
		if !validJSONNumber(ct.param) {
			e.diagUnparseable(ptr, ct)
			return
		}
		switch ct.tag {
		case "gt":
			schema.ExclusiveMinimum = &num
		case "gte":
			schema.Minimum = &num
		case "lt":
			schema.ExclusiveMaximum = &num
		case "lte":
			schema.Maximum = &num
		}

	default:
		if typ.ConvertibleTo(timeType) {
			e.diag(ptr, DiagnosticUnsupported, ct.tag, ct.param,
				"time.Time comparisons are relative to the current time and cannot be exported")
			return
		}
		e.diagUnknownTag(ct, ptr)
	}
}

func (e *schemaExporter) applyEqTag(schema *Schema, typ reflect.Type, ct *cTag, ptr string) {
	switch typ.Kind() {
	case reflect.String:
		setConst(schema, ct.param)
	case reflect.Bool:
		b, err := strconv.ParseBool(ct.param)
		if err != nil {
			e.diagUnparseable(ptr, ct)
			return
		}
		setConst(schema, b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		if !validJSONNumber(ct.param) {
			e.diagUnparseable(ptr, ct)
			return
		}
		setConst(schema, jsonNumeric(json.Number(ct.param)))
	default:
		e.diagUnknownTag(ct, ptr)
	}
}

func (e *schemaExporter) applyNeTag(schema *Schema, typ reflect.Type, ct *cTag, ptr string) {
	// "ne" is inequality against a scalar, expressed as "not": {enum:[v]}.
	var forbidden any
	switch typ.Kind() {
	case reflect.String:
		forbidden = ct.param
	case reflect.Bool:
		b, err := strconv.ParseBool(ct.param)
		if err != nil {
			e.diagUnparseable(ptr, ct)
			return
		}
		forbidden = b
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		if !validJSONNumber(ct.param) {
			e.diagUnparseable(ptr, ct)
			return
		}
		forbidden = jsonNumeric(json.Number(ct.param))
	default:
		e.diagUnknownTag(ct, ptr)
		return
	}
	schema.Not = &Schema{Enum: []any{forbidden}}
}

func (e *schemaExporter) applyOneOfTag(schema *Schema, typ reflect.Type, ct *cTag, ptr string, _ bool) {
	params := parseOneOfParam2(ct.param)
	enums := make([]any, 0, len(params))

	for _, p := range params {
		switch typ.Kind() {
		case reflect.String:
			enums = append(enums, p)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			if !validJSONNumber(p) {
				e.diagUnparseable(ptr, ct)
				return
			}
			enums = append(enums, jsonNumeric(json.Number(p)))
		default:
			e.diag(ptr, DiagnosticUnsupported, "oneof", ct.param,
				fmt.Sprintf("'oneof' is only exported for string and integer types, not %s", typ.Kind()))
			return
		}
	}
	schema.Enum = enums
}

func jsonNumeric(n json.Number) any {
	if i, err := n.Int64(); err == nil {
		return i
	}
	if f, err := n.Float64(); err == nil {
		return f
	}
	return n.String()
}

// setConst records an exact-equality constraint. JSON Schema would use
// "const", but a single-value "enum" validates instances identically while
// round-tripping zero values (0, false, "") through Go marshaling.
func setConst(schema *Schema, value any) {
	schema.Enum = []any{value}
}

func validJSONNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func parseBoundInt(s string) (int, bool) {
	n, err := strconv.ParseInt(s, 0, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return int(n), true
}

func (e *schemaExporter) diagUnparseable(ptr string, ct *cTag) {
	e.diag(ptr, DiagnosticUnsupported, ct.tag, ct.param,
		"tag parameter could not be parsed as the value the tag requires")
}

func (e *schemaExporter) diagUnknownTag(ct *cTag, ptr string) {
	if reason, known := unsupportedTagReasons[ct.tag]; known {
		e.diag(ptr, DiagnosticUnsupported, ct.tag, ct.param, reason)
		return
	}
	if _, baked := bakedInValidators[ct.tag]; baked {
		e.diag(ptr, DiagnosticUnsupported, ct.tag, ct.param,
			"built-in tag has no equivalent JSON Schema keyword")
		return
	}
	e.diag(ptr, DiagnosticUnsupported, ct.tag, ct.param,
		"custom or unknown validator; JSON Schema cannot invoke Go code")
}

func isConstraintTag(tag string) bool {
	switch tag {
	case "len", "min", "max", "gt", "gte", "lt", "lte", "eq", "ne",
		"oneof", "noneof", "unique":
		return true
	}
	_, baked := bakedInValidators[tag]
	return baked
}

func orTagLabel(ct *cTag) string {
	if ct.aliasTag != "" {
		return ct.aliasTag
	}
	return ct.tag
}

// skipOrGroup advances past a run of typeOr tags.
func skipOrGroup(ct *cTag) *cTag {
	for ct != nil && ct.typeof == typeOr {
		ct = ct.next
	}
	return ct
}

// unsupportedTagReasons documents categories of built-in tags that are
// fundamentally outside JSON Schema's data-instance model.
var unsupportedTagReasons = map[string]string{
	// Cross-field comparisons depend on sibling Go field values.
	"eqfield":       "references another struct field; JSON Schema cannot compare sibling fields",
	"nefield":       "references another struct field; JSON Schema cannot compare sibling fields",
	"gtfield":       "references another struct field; JSON Schema cannot compare sibling fields",
	"gtefield":      "references another struct field; JSON Schema cannot compare sibling fields",
	"ltfield":       "references another struct field; JSON Schema cannot compare sibling fields",
	"ltefield":      "references another struct field; JSON Schema cannot compare sibling fields",
	"eqcsfield":     "references a field in a parent struct; JSON Schema cannot compare sibling fields",
	"necsfield":     "references a field in a parent struct; JSON Schema cannot compare sibling fields",
	"gtcsfield":     "references a field in a parent struct; JSON Schema cannot compare sibling fields",
	"gtecsfield":    "references a field in a parent struct; JSON Schema cannot compare sibling fields",
	"ltcsfield":     "references a field in a parent struct; JSON Schema cannot compare sibling fields",
	"ltecsfield":    "references a field in a parent struct; JSON Schema cannot compare sibling fields",
	"fieldcontains": "references another struct field; JSON Schema cannot compare sibling fields",
	"fieldexcludes": "references another struct field; JSON Schema cannot compare sibling fields",

	// Conditional presence/exclusion keys off sibling field values.
	"required_if":          "presence is conditional on another field; not expressible as static JSON Schema",
	"required_unless":      "presence is conditional on another field; not expressible as static JSON Schema",
	"required_with":        "presence is conditional on another field; not expressible as static JSON Schema",
	"required_with_all":    "presence is conditional on another field; not expressible as static JSON Schema",
	"required_without":     "presence is conditional on another field; not expressible as static JSON Schema",
	"required_without_all": "presence is conditional on another field; not expressible as static JSON Schema",
	"excluded_if":          "exclusion is conditional on another field; not expressible as static JSON Schema",
	"excluded_unless":      "exclusion is conditional on another field; not expressible as static JSON Schema",
	"excluded_with":        "exclusion is conditional on another field; not expressible as static JSON Schema",
	"excluded_with_all":    "exclusion is conditional on another field; not expressible as static JSON Schema",
	"excluded_without":     "exclusion is conditional on another field; not expressible as static JSON Schema",
	"excluded_without_all": "exclusion is conditional on another field; not expressible as static JSON Schema",
	"skip_unless":          "evaluation is conditional on another field; not expressible as static JSON Schema",

	// Formats are assertions (regex/network/filesystem), not structural types.
	"email":         "format is not enforced by JSON Schema assertions; kept server-side",
	"url":           "format is not enforced by JSON Schema assertions; kept server-side",
	"http_url":      "format is not enforced by JSON Schema assertions; kept server-side",
	"https_url":     "format is not enforced by JSON Schema assertions; kept server-side",
	"uri":           "format is not enforced by JSON Schema assertions; kept server-side",
	"uuid":          "format is not enforced by JSON Schema assertions; kept server-side",
	"uuid_rfc4122":  "format is not enforced by JSON Schema assertions; kept server-side",
	"uuid3":         "format is not enforced by JSON Schema assertions; kept server-side",
	"uuid4":         "format is not enforced by JSON Schema assertions; kept server-side",
	"uuid5":         "format is not enforced by JSON Schema assertions; kept server-side",
	"uuid3_rfc4122": "format is not enforced by JSON Schema assertions; kept server-side",
	"uuid4_rfc4122": "format is not enforced by JSON Schema assertions; kept server-side",
	"uuid5_rfc4122": "format is not enforced by JSON Schema assertions; kept server-side",
	"ipv4":          "format is not enforced by JSON Schema assertions; kept server-side",
	"ipv6":          "format is not enforced by JSON Schema assertions; kept server-side",
	"ip":            "format is not enforced by JSON Schema assertions; kept server-side",
	"datetime":      "format is not enforced by JSON Schema assertions; kept server-side",
	"json":          "checks that a string is valid JSON; not a JSON Schema type constraint",
	"numeric":       "checks that a string is numeric; mixes string syntax with numeric type",
	"number":        "checks that a string is numeric; mixes string syntax with numeric type",
	"boolean":       "checks that a string is a boolean literal; mixes string syntax with boolean type",
}

func escapeJSONPointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")
	return token
}

// defName renders a stable $defs key for a named struct type.
func defName(t reflect.Type) string {
	name := t.Name()
	pkg := t.PkgPath()
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	pkg = sanitizeDefSegment(pkg)
	name = sanitizeDefSegment(name)
	if pkg == "" {
		return name
	}
	return pkg + "_" + name
}

var defSegmentInvalid = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func sanitizeDefSegment(s string) string {
	return defSegmentInvalid.ReplaceAllString(s, "_")
}

func (e *schemaExporter) uniqueDefName(typ reflect.Type) string {
	base := defName(typ)
	name := base
	for n := 2; ; n++ {
		if _, taken := e.defs[name]; !taken {
			return name
		}
		name = fmt.Sprintf("%s%d", base, n)
	}
}

type jsonField struct {
	field reflect.StructField
	index []int
	name  string // JSON key
}

// collectJSONFields mirrors encoding/json's field selection: anonymous
// embedded structs are flattened one level deep, fields conflict at equal
// shallowest depth and are then suppressed, and an explicit `json:"-"` skips
// the field.
func collectJSONFields(typ reflect.Type) []jsonField {
	type candidate struct {
		jsonField
		depth  int
		tagged bool
	}

	// BFS queue entries are embedded structs to expand. Start with the root.
	type embed struct {
		index []int
		depth int
	}

	var fields []candidate
	queue := []embed{{depth: 0}}

	for len(queue) > 0 {
		em := queue[0]
		queue = queue[1:]

		var st reflect.Type
		if len(em.index) == 0 {
			st = typ
		} else {
			f := typ.FieldByIndex(em.index)
			st = f.Type
			if st.Kind() == reflect.Ptr {
				st = st.Elem()
			}
		}

		for i := 0; i < st.NumField(); i++ {
			f := st.Field(i)
			idx := append(append([]int(nil), em.index...), i)

			// Anonymous embedded structs are flattened. encoding/json only
			// descends when the embed has no usable json name of its own.
			if f.Anonymous {
				name, _, _, skip := jsonName(f)
				ft := f.Type
				if ft.Kind() == reflect.Ptr {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct && !ft.ConvertibleTo(timeType) && (skip || name == "") {
					queue = append(queue, embed{index: idx, depth: em.depth + 1})
					continue
				}
			}

			if !f.Anonymous && f.PkgPath != "" {
				continue
			}

			name, _, named, skip := jsonName(f)
			if skip {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fields = append(fields, candidate{
				jsonField: jsonField{field: f, index: idx, name: name},
				depth:     em.depth + 1,
				tagged:    named,
			})
		}
	}

	// encoding/json conflict rule: shallowest depth wins; at that depth a
	// single json-tagged field beats untagged ones, otherwise equal-level
	// ambiguity suppresses every candidate.
	byName := make(map[string][]candidate)
	minDepth := make(map[string]int)
	for _, c := range fields {
		if d, ok := minDepth[c.name]; !ok || c.depth < d {
			minDepth[c.name] = c.depth
			byName[c.name] = []candidate{c}
		} else if c.depth == d {
			byName[c.name] = append(byName[c.name], c)
		}
	}

	out := make([]jsonField, 0, len(byName))
	for name, cands := range byName {
		var chosen *candidate
		var taggedCount int
		for i := range cands {
			if cands[i].tagged {
				taggedCount++
				chosen = &cands[i]
			}
		}
		if taggedCount == 1 {
			out = append(out, chosen.jsonField)
			continue
		}
		if taggedCount == 0 && len(cands) == 1 {
			out = append(out, cands[0].jsonField)
		}
		_ = name
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].index, out[j].index
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
	return out
}

// jsonName extracts the name from a `json` tag, reporting skip for `json:"-"`.
func jsonName(f reflect.StructField) (name string, omitempty, named, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, false, true
	}
	if tag == "" {
		return "", false, false, false
	}
	parts := strings.Split(tag, ",")
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			omitempty = true
		}
	}
	return parts[0], omitempty, parts[0] != "", false
}
