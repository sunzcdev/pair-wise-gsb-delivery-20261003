package validator

// This file implements JSON Schema (draft 2020-12) export. It translates the
// subset of validation tags that can be expressed equivalently and reports, as
// diagnostics, every constraint it cannot translate. It never silently drops a
// rule: a tag is either represented in the returned schema or listed in
// JSONSchemaExport.Diagnostics.
//
// See JSONSchemaExport for the result shape and JSONSchemaDiagnostic for the
// individual report.

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// jsonSchemaDraft is the JSON Schema dialect produced by this exporter.
const jsonSchemaDraft = "https://json-schema.org/draft/2020-12/schema"

// Diagnostic severity levels reported during JSON Schema export.
const (
	// DiagnosticError marks a constraint that cannot be represented in JSON
	// Schema and is therefore absent from the exported schema. The schema is
	// still returned; it is not a faithful translation while such diagnostics
	// exist.
	DiagnosticError = "error"

	// DiagnosticInfo documents a semantic difference that callers should be
	// aware of even though the translation is otherwise sound (for example the
	// fact that JSON null unmarshals into the Go zero value).
	DiagnosticInfo = "info"
)

// Reasons reported in JSONSchemaDiagnostic.Reason. They are stable identifiers
// that frontends and documentation can key on.
const (
	// ReasonUnsupportedTag is reported for a baked-in tag that has no
	// equivalent JSON Schema assertion (format validators, cross-field
	// validators, conditional rules, filesystem/network checks, etc.).
	ReasonUnsupportedTag = "unsupported_tag"

	// ReasonCustomValidator is reported for a tag backed by a user
	// registered validation function; its semantics are opaque.
	ReasonCustomValidator = "custom_validator"

	// ReasonStructLevel is reported for a type with a registered
	// struct-level validation; cross-field rules are not expressible.
	ReasonStructLevel = "struct_level_validation"

	// ReasonCustomTypeFunc is reported for a type handled through
	// RegisterCustomTypeFunc; the validated runtime value differs from the
	// JSON shape.
	ReasonCustomTypeFunc = "custom_type_func"

	// ReasonOrBranch is reported when one branch of an 'or' (|) expression
	// cannot be translated; the whole 'or' is then left untranslated.
	ReasonOrBranch = "or_branch_untranslatable"

	// ReasonOmitZero is reported for the omitzero tag, whose zero-value
	// skipping rule has no JSON Schema equivalent for non-nilable kinds.
	ReasonOmitZero = "omitzero_untranslatable"

	// ReasonImplicitZero is reported when a field carries at least one
	// translated constraint but also an untranslated one, so whether the Go
	// zero value would pass cannot be determined; the field is therefore not
	// marked required and the absent-field vs zero-value gap is flagged.
	ReasonImplicitZero = "implicit_zero_presence_undecidable"

	// ReasonStringOption is reported for fields using the encoding/json
	// `,string` option: the JSON value is a string encoding of the Go scalar,
	// so numeric/length tags cannot be applied to the raw JSON value.
	ReasonStringOption = "json_string_option"

	// ReasonMapKeys is reported for non-string map keys with a dive keys
	// block: JSON object keys are always strings.
	ReasonMapKeys = "non_string_map_keys"

	// ReasonByteSlice is reported when validation tags are placed on a
	// []byte field, which encoding/json renders as a base64 string.
	ReasonByteSlice = "byte_slice_base64"

	// ReasonUnsupportedKind is reported for kinds with no JSON
	// representation (channels, functions, complex numbers).
	ReasonUnsupportedKind = "unsupported_kind"

	// ReasonEmbedded is reported for embedded fields whose promotion cannot
	// be modeled exactly (embedded non-struct types, conflicting promotion).
	ReasonEmbedded = "embedded_field_untranslatable"

	// ReasonNullSemantics is the global note that explicit JSON null on a
	// non-pointer field unmarshals to the Go zero value in Go but is rejected
	// by the type-typed JSON Schema.
	ReasonNullSemantics = "null_vs_go_zero_value"
)

// JSONSchemaDiagnostic describes a single validation constraint that the
// exporter could not represent in JSON Schema (Severity == DiagnosticError) or
// a semantic difference that callers must account for (Severity ==
// DiagnosticInfo).
type JSONSchemaDiagnostic struct {
	// Severity is one of DiagnosticError or DiagnosticInfo.
	Severity string

	// Reason is a stable identifier; see the Reason* constants.
	Reason string

	// Pointer is a JSON Pointer (RFC 6901) locating the schema node the
	// diagnostic applies to, relative to JSONSchemaExport.Schema (the root
	// object). Definitions are reachable under "/$defs/<name>". It is empty
	// for whole-document diagnostics.
	Pointer string

	// GoNamespace is the Go-side dotted field path, using Go field names
	// (e.g. "Address.ZipCode"), which is useful when the JSON name differs.
	GoNamespace string

	// Tag is the original tag expression that could not be translated, e.g.
	// "email" or "gtefield=Start". Empty for diagnostics that are not about a
	// specific tag.
	Tag string

	// Message is a human readable explanation.
	Message string
}

// JSONSchemaExport is the result of exporting a Go type to JSON Schema.
type JSONSchemaExport struct {
	// Schema is the JSON Schema (draft 2020-12) document. It is a generic
	// representation suitable for json.Marshal.
	Schema map[string]interface{}

	// Diagnostics lists, without omission, every constraint that could not be
	// translated and every semantic difference worth flagging. The export is
	// a faithful, equivalent translation of the Go validation only when no
	// diagnostic has Severity == DiagnosticError.
	Diagnostics []JSONSchemaDiagnostic
}

// Complete reports whether the exported schema is a complete and equivalent
// representation of the Go validation tags. It returns false if any
// DiagnosticError is present; info-level diagnostics do not affect
// completeness.
func (e JSONSchemaExport) Complete() bool {
	for _, d := range e.Diagnostics {
		if d.Severity == DiagnosticError {
			return false
		}
	}
	return true
}

// JSONSchemaFor exports the validation tags defined on the given type (or a
// pointer to it) to a JSON Schema document. The returned schema covers the
// subset of tags expressible in JSON Schema draft 2020-12; every other
// constraint is returned in JSONSchemaExport.Diagnostics and is never silently
// dropped.
//
// JSON property names are taken from the `json` struct tag, falling back to
// the Go field name, matching encoding/json field naming. Fields named "-" are
// excluded.
func (v *Validate) JSONSchemaFor(i interface{}) (*JSONSchemaExport, error) {
	typ := reflect.TypeOf(i)
	if typ == nil {
		return nil, errors.New("JSONSchemaFor: nil type")
	}
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || typ.ConvertibleTo(timeType) {
		return nil, fmt.Errorf("JSONSchemaFor: expected struct, got %s", typ.Kind())
	}

	e := &schemaExporter{
		v:           v,
		defs:        make(map[string]*schemaNode),
		active:      make(map[reflect.Type]bool),
		diagnostics: make([]JSONSchemaDiagnostic, 0),
	}

	root := e.structNode(typ, "", "")

	doc := map[string]interface{}{"$schema": jsonSchemaDraft}

	if len(e.defs) > 0 {
		// If the root itself is a named type, define it and reference it so the
		// shape lives next to all other definitions.
		if name := defName(typ); name != "" {
			doc["$ref"] = "#/$defs/" + name
		} else {
			for k, v := range nodeMap(root) {
				doc[k] = v
			}
		}
		defsMap := make(map[string]interface{}, len(e.defs))
		keys := make([]string, 0, len(e.defs))
		for k := range e.defs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			defsMap[k] = nodeMap(e.defs[k])
		}
		doc["$defs"] = defsMap
	} else {
		for k, v := range nodeMap(root) {
			doc[k] = v
		}
	}

	if e.sawTaggedNonPointerValue {
		e.add(DiagnosticInfo, "", "", "", ReasonNullSemantics,
			"encoding/json unmarshals an explicit JSON null into the Go zero value of a non-pointer field, "+
				"which the Go validators then run against; JSON Schema instead rejects null for the field's type. "+
				"Avoid sending null for non-pointer fields to keep both sides equivalent.")
	}

	return &JSONSchemaExport{Schema: doc, Diagnostics: e.diagnostics}, nil
}

// schemaNode is the working representation of a JSON Schema subtree while it is
// being built. Only the assertions the exporter emits are modeled.
type schemaNode struct {
	types       []string
	format      string
	contentEnc  string
	enum        []interface{}
	constant    interface{}
	hasConstant bool
	minimum     *float64
	maximum     *float64
	minLength   *int64
	maxLength   *int64
	minItems    *int64
	maxItems    *int64
	minProps    *int64
	maxProps    *int64
	uniqueItems bool
	items       *schemaNode
	props       map[string]*schemaNode
	required    []string
	addProps    *schemaNode
	propNames   *schemaNode
	ref         string
	anyOf       []*schemaNode
	allOf       []*schemaNode
	not         *schemaNode

	// nonSchema marks a node produced for a kind that has no JSON
	// representation; it renders as {} and is always accompanied by a
	// diagnostic.
	nonSchema bool
}

// schemaExporter walks Go types once per export, collecting schema nodes and
// diagnostics.
type schemaExporter struct {
	v           *Validate
	defs        map[string]*schemaNode
	active      map[reflect.Type]bool
	diagnostics []JSONSchemaDiagnostic

	// sawTaggedNonPointerValue tracks whether the document contains a
	// non-pointer field with validation tags, to decide whether the global
	// null-vs-zero-value info diagnostic applies.
	sawTaggedNonPointerValue bool
}

func (e *schemaExporter) add(severity, pointer, goNS, tag, reason, message string) {
	e.diagnostics = append(e.diagnostics, JSONSchemaDiagnostic{
		Severity:    severity,
		Reason:      reason,
		Pointer:     pointer,
		GoNamespace: goNS,
		Tag:         tag,
		Message:     message,
	})
}

// defName returns the JSON Schema definition name for a named Go type.
func defName(t reflect.Type) string {
	name := t.Name()
	if name == "" {
		return ""
	}
	if pkg := t.PkgPath(); pkg != "" {
		// Keep the last path element to make cross-package names readable while
		// still disambiguating types that share a short package name.
		if i := strings.LastIndexByte(pkg, '/'); i >= 0 {
			pkg = pkg[i+1:]
		}
		return pkg + "." + name
	}
	return name
}

// escapePointer escapes a token for use in an RFC 6901 JSON Pointer.
func escapePointer(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")
	return token
}

// joinPointer appends tokens to a JSON Pointer.
func joinPointer(base string, tokens ...string) string {
	var b strings.Builder
	b.WriteString(base)
	for _, t := range tokens {
		b.WriteByte('/')
		b.WriteString(escapePointer(t))
	}
	return b.String()
}

// collectedField is an exported struct field after embedded-field promotion.
type collectedField struct {
	field reflect.StructField
	depth int
	json  string // effective JSON property name
	goNS  string // Go namespace prefix path
	ptr   string // JSON Pointer prefix
	opts  jsonOptions
}

// structNode builds the schema node for a struct type, handling $defs and
// recursion.
func (e *schemaExporter) structNode(t reflect.Type, goPrefix, ptrPrefix string) *schemaNode {
	if name := defName(t); name != "" {
		if _, ok := e.defs[name]; ok {
			return &schemaNode{ref: "#/$defs/" + name}
		}
		if e.active[t] {
			// Recursive type encountered while it is being built.
			return &schemaNode{ref: "#/$defs/" + name}
		}

		node := &schemaNode{types: []string{"object"}, props: map[string]*schemaNode{}}
		e.defs[name] = node
		e.active[t] = true
		e.fillStruct(t, node, goPrefix, ptrPrefix)
		delete(e.active, t)
		return &schemaNode{ref: "#/$defs/" + name}
	}

	node := &schemaNode{types: []string{"object"}, props: map[string]*schemaNode{}}
	e.fillStruct(t, node, goPrefix, ptrPrefix)
	return node
}

// fillStruct collects promoted fields and builds properties for a struct node.
func (e *schemaExporter) fillStruct(t reflect.Type, node *schemaNode, goPrefix, ptrPrefix string) {
	if _, ok := e.v.structLevelFuncs[t]; ok {
		e.add(DiagnosticError, ptrPrefix, dotted(goPrefix, t.Name()), "", ReasonStructLevel,
			"struct has a registered struct-level validation; cross-field rules are not expressible in JSON Schema")
	}

	fields := e.collectFields(t, goPrefix, ptrPrefix)

	// Deterministic property order.
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		cf := fields[name]
		fldNode := e.fieldNode(cf)
		if fldNode == nil {
			continue
		}
		node.props[name] = fldNode
		if cf.required {
			node.required = append(node.required, name)
		}
	}
	sort.Strings(node.required)
}

type collectedFieldEx struct {
	*collectedField
	required bool
}

// collectFields flattens embedded structs following encoding/json promotion.
func (e *schemaExporter) collectFields(t reflect.Type, goPrefix, ptrPrefix string) map[string]*collectedFieldEx {
	// Winner per JSON name: shallowest depth, then deterministic field index.
	winners := map[string]*collectedFieldEx{}

	type frame struct {
		typ      reflect.Type
		depth    int
		goPrefix string
		ptr      string
	}

	queue := []frame{{typ: t, depth: 0, goPrefix: goPrefix, ptr: ptrPrefix}}
	for len(queue) > 0 {
		fr := queue[0]
		queue = queue[1:]

		for i := 0; i < fr.typ.NumField(); i++ {
			sf := fr.typ.Field(i)
			jsonName, opts := parseJSONTag(sf)

			if sf.Anonymous {
				embed := sf.Type
				if embed.Kind() == reflect.Ptr {
					embed = embed.Elem()
				}
				// An anonymous field with an explicit JSON name is treated as a
				// normal field; only the unnamed form is promoted/flattened.
				if jsonName == "" && embed.Kind() == reflect.Struct && !embed.ConvertibleTo(timeType) {
					queue = append(queue, frame{
						typ:      embed,
						depth:    fr.depth + 1,
						goPrefix: fr.goPrefix, // promoted fields keep the path relative to the enclosing type
						ptr:      fr.ptr,
					})
					continue
				}
				if jsonName == "" {
					e.add(DiagnosticError, fr.ptr, dotted(fr.goPrefix, sf.Name), "", ReasonEmbedded,
						fmt.Sprintf("embedded field %s of kind %s cannot be flattened; modeled as a named property without its promoted fields",
							sf.Name, sf.Type.Kind()))
				}
			}

			// Unexported non-anonymous fields are never (un)marshaled.
			if !sf.Anonymous && sf.PkgPath != "" {
				continue
			}
			if jsonName == "-" {
				continue
			}
			if jsonName == "" {
				jsonName = sf.Name
			}

			cf := &collectedFieldEx{
				collectedField: &collectedField{
					field: sf,
					depth: fr.depth,
					json:  jsonName,
					goNS:  dotted(fr.goPrefix, sf.Name),
					ptr:   joinPointer(fr.ptr, jsonName),
					opts:  opts,
				},
			}

			if existing, ok := winners[jsonName]; ok {
				if existing.depth < cf.depth {
					continue
				}
				if existing.depth == cf.depth {
					// Same-depth conflict: encoding/json ignores both fields.
					e.add(DiagnosticError, cf.ptr, cf.goNS, "", ReasonEmbedded,
						fmt.Sprintf("fields %s and %s both promote to JSON property %q at the same depth; encoding/json ignores both",
							existing.goNS, cf.goNS, jsonName))
					delete(winners, jsonName)
					continue
				}
			}
			winners[jsonName] = cf
		}
	}

	return winners
}

func dotted(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// jsonOptions carries the encoding/json tag options relevant to the exporter.
type jsonOptions struct {
	stringOpt bool
	omitempty bool
}

// parseJSONTag parses an encoding/json struct tag the same way encoding/json
// does for field selection. The returned name is "" when the Go field name
// should be used, and "-" when the field must be skipped.
func parseJSONTag(sf reflect.StructField) (name string, opts jsonOptions) {
	raw, ok := sf.Tag.Lookup("json")
	if !ok {
		return "", jsonOptions{}
	}
	if raw == "-" {
		return "-", jsonOptions{}
	}
	parts := strings.Split(raw, ",")
	name = parts[0]
	for _, p := range parts[1:] {
		switch p {
		case "string":
			opts.stringOpt = true
		case "omitempty":
			opts.omitempty = true
		}
	}
	return name, opts
}

// fieldNode builds the schema for a single (promoted) struct field, applying
// its validation tags. It returns nil when the field is skipped.
func (e *schemaExporter) fieldNode(cf *collectedFieldEx) *schemaNode {
	fld := cf.field
	tag := fld.Tag.Get(e.v.tagName)
	if tag == skipValidationTag {
		// Field excluded from validation; still describe the raw type.
		return e.baseNode(fld.Type, cf, tag)
	}

	if cf.opts.stringOpt {
		node := &schemaNode{types: []string{"string"}}
		if tag != "" {
			e.add(DiagnosticError, cf.ptr, cf.goNS, tag, ReasonStringOption,
				"field uses encoding/json \",string\"; its JSON representation is a quoted string, so numeric/length validation tags cannot be applied to the raw JSON value")
		}
		return node
	}

	// Validation tags only exist on the declaring field. Parse the chain once.
	var first *cTag
	if tag != "" {
		first, _ = e.v.parseFieldTagsRecursive(tag, fld.Name, "", false)
	}

	chainResult := e.processType(fld.Type, first, cf.goNS, cf.ptr)
	node := chainResult.node
	if node == nil {
		node = e.baseNode(fld.Type, cf, tag)
	}

	// Types handled by RegisterCustomTypeFunc validate an extracted runtime
	// value that differs from the JSON shape; flag the gap.
	if e.v.hasCustomFuncs && tag != "" {
		if ft := derefType(fld.Type); e.v.customFuncs[ft] != nil {
			e.add(DiagnosticError, cf.ptr, cf.goNS, tag, ReasonCustomTypeFunc,
				"field's type is handled by RegisterCustomTypeFunc; validation runs against an extracted value, not the JSON shape")
		}
	}

	if chainResult.zeroRejects && fld.Type.Kind() != reflect.Ptr {
		e.sawTaggedNonPointerValue = true
	}

	cf.required = chainResult.presence == presenceRequired
	return node
}

// presence describes whether a property is required by the Go tags.
type presence uint8

const (
	presenceOptional presence = iota
	presenceRequired
)

// typeResult carries the built node plus presence/zero semantics derived from
// the outermost (level-0) validation tags.
type typeResult struct {
	node *schemaNode

	// presenceRequired means the field must be present/non-nil per Go tags.
	presence presence

	// zeroRejects means the Go zero value for the field type fails the
	// translated level-0 constraints.
	zeroRejects bool
}

// baseNode describes a Go type without applying validation tags.
func (e *schemaExporter) baseNode(t reflect.Type, cf *collectedFieldEx, tag string) *schemaNode {
	// Dereference pointers for the base shape; nullability is added by callers.
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	switch {
	case t == timeType:
		return &schemaNode{types: []string{"string"}, format: "date-time"}
	case t == byteSliceType:
		node := &schemaNode{types: []string{"string"}, contentEnc: "base64"}
		if tag != "" {
			e.add(DiagnosticError, cf.ptr, cf.goNS, tag, ReasonByteSlice,
				"[]byte is encoded as a base64 string by encoding/json; slice/string validation tags cannot be translated accurately")
		}
		return node
	}

	switch t.Kind() {
	case reflect.String:
		return &schemaNode{types: []string{"string"}}
	case reflect.Bool:
		return &schemaNode{types: []string{"boolean"}}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return &schemaNode{types: []string{"integer"}}
	case reflect.Float32, reflect.Float64:
		return &schemaNode{types: []string{"number"}}
	case reflect.Struct:
		return e.structNode(t, cf.goNS, cf.ptr)
	case reflect.Slice, reflect.Array:
		node := &schemaNode{types: []string{"array"}}
		if t.Kind() == reflect.Slice {
			node.items = e.baseNode(t.Elem(), childCF(cf, "[]"), "")
		} else {
			node.items = e.baseNode(t.Elem(), childCF(cf, "[]"), "")
		}
		return node
	case reflect.Map:
		node := &schemaNode{types: []string{"object"}}
		node.addProps = e.baseNode(t.Elem(), childCF(cf, "*"), "")
		return node
	case reflect.Interface:
		return &schemaNode{}
	case reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		e.add(DiagnosticError, cf.ptr, cf.goNS, tag, ReasonUnsupportedKind,
			fmt.Sprintf("Go kind %s has no JSON representation", t.Kind()))
		return &schemaNode{nonSchema: true}
	default:
		return &schemaNode{}
	}
}

// childCF builds a lightweight context for nested elements used in base nodes.
func childCF(cf *collectedFieldEx, marker string) *collectedFieldEx {
	return &collectedFieldEx{
		collectedField: &collectedField{
			goNS: cf.goNS + marker,
			ptr:  cf.ptr + "/" + marker,
		},
	}
}

// processType builds the schema node for a field of type t with an already
// parsed tag chain. It handles pointer unwrapping, the level-0 control tags
// (required/omitempty/omitnil/omitzero), collection containers and dive.
func (e *schemaExporter) processType(t reflect.Type, head *cTag, goNS, ptr string) typeResult {
	return e.processTypeCtx(t, head, goNS, ptr, false)
}

// processTypeCtx is processType with an element-context flag: when true, a
// `required` tag constrains the slice/map element value itself (non-empty)
// rather than marking an object property present.
func (e *schemaExporter) processTypeCtx(t reflect.Type, head *cTag, goNS, ptr string, element bool) typeResult {
	res := typeResult{}

	// Pointer unwrapping: track nullability and dereference the Go type used
	// for assertions.
	ptrDepth := 0
	inner := t
	for inner.Kind() == reflect.Ptr {
		inner = inner.Elem()
		ptrDepth++
	}

	var omitEmpty, omitNil, omitZero, hasRequired, isDefaultTag bool
	var structural []*cTag
	hasAnyLevel0Tag := false

	// Walk the chain up to the first dive: everything before it applies at
	// level 0; the node immediately following the dive starts the element
	// chain (which may contain further dives for multidimensional types).
	var elemHead *cTag
	diveFound := false

	for ct := head; ct != nil; ct = ct.next {
		if ct.typeof == typeDive {
			elemHead = ct.next
			diveFound = true
			break
		}
		if !ct.hasTag {
			continue
		}
		hasAnyLevel0Tag = true
		switch ct.typeof {
		case typeOmitEmpty:
			omitEmpty = true
			continue
		case typeOmitNil:
			omitNil = true
			continue
		case typeOmitZero:
			omitZero = true
			continue
		case typeIsDefault:
			isDefaultTag = true
			continue
		case typeStructOnly:
			// Prevents nested struct field validation; fields are still
			// exported in the schema, so emit a diagnostic noting the dropped
			// restriction rather than silently pruning properties.
			if inner.Kind() == reflect.Struct && !inner.ConvertibleTo(timeType) {
				e.add(DiagnosticError, ptr, goNS, ct.tag, ReasonUnsupportedTag,
					"structonly skips validation of the nested struct's fields; this selective behavior cannot be expressed, nested properties remain in the schema")
			}
			continue
		case typeNoStructLevel:
			// Only suppresses struct-level functions, which are already
			// reported separately; nothing structural to translate.
			continue
		}
		if ct.tag == requiredTag {
			hasRequired = true
			continue
		}
		structural = append(structural, ct)
	}

	// Report omitzero: its rule is not expressible for non-nilable kinds.
	if omitZero {
		e.add(DiagnosticError, ptr, goNS, omitzero, ReasonOmitZero,
			"omitzero skips validation when the value is the type's zero value (including non-nil zero pointers/empty slices); this cannot be expressed in JSON Schema")
	}

	// Build the node for the inner (dereferenced) type, including element
	// tags for containers.
	node := e.buildInner(inner, elemHead, structural, diveFound, goNS, ptr)

	if isDefaultTag && len(structural) == 0 && !diveFound {
		node = e.defaultValueNode(inner, ptr, goNS, "isdefault")
	} else if isDefaultTag {
		e.add(DiagnosticError, ptr, goNS, isdefault, ReasonUnsupportedTag,
			"isdefault combined with other tags or a dive cannot be expressed as a single JSON Schema const; the isdefault constraint was dropped")
	}

	// Determine presence/zero semantics from level-0 translated tags.
	if hasRequired {
		if element {
			// `dive,required` constrains the element itself: non-zero scalar,
			// non-nil pointer/interface, and non-empty is not enforced for
			// slices/maps by required (only nil). Express the non-zero form.
			if n := requiredNonEmptyNode(inner); n != nil {
				node = &schemaNode{allOf: []*schemaNode{node, n}}
			}
		} else if inner.Kind() != reflect.Struct || inner.ConvertibleTo(timeType) || e.v.requiredStructEnabled {
			// required on a value struct is a no-op unless
			// requiredStructEnabled.
			res.presence = presenceRequired
		}
	}

	if hasAnyLevel0Tag && !hasRequired && !omitEmpty && !omitNil {
		// Tags apply even when the field is absent; infer from the zero value.
		zero := goZeroValue(inner)
		switch {
		case res.presence == presenceRequired:
			// already decided
		case nodeAccepts(node, zero):
			// zero passes => absence is fine
		default:
			res.presence = presenceRequired
			res.zeroRejects = true
		}
	}

	// When an untranslated level-0 structural tag exists we cannot know the
	// zero-value verdict for sure; surface that as a gap rather than claiming
	// precision.
	if hasUntranslatedLevel0(structural) && !hasRequired && !omitEmpty && !omitNil {
		e.add(DiagnosticInfo, ptr, goNS, "", ReasonImplicitZero,
			"field has tags that only apply when present plus at least one untranslated tag; whether the Go zero value passes is undecidable, so the property was left optional")
		res.presence = presenceOptional
	}

	// Wrap omitempty / omitnil. Both mean "skip all following assertions on the
	// empty/nil value"; modeled with an anyOf that also accepts the empty form.
	switch {
	case omitEmpty:
		node = e.wrapOmitEmpty(inner, node, ptrDepth > 0 || nilableKind(inner.Kind()), goNS, ptr)
	case omitNil && (ptrDepth > 0 || nilableKind(inner.Kind())):
		node = wrapNull(node)
	}

	// Nullability for pointers: a non-required pointer accepts null unless
	// required (or otherwise forced present by level-0 tags).
	if ptrDepth > 0 && res.presence != presenceRequired && !omitEmpty && !omitNil {
		node = wrapNull(node)
	}

	res.node = node
	return res
}

// nilableKind reports whether the (dereferenced) kind itself is nilable in Go.
func nilableKind(k reflect.Kind) bool {
	switch k {
	case reflect.Slice, reflect.Map, reflect.Interface, reflect.Chan, reflect.Func:
		return true
	default:
		return false
	}
}

// derefType removes pointer indirections from a type.
func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// requiredNonEmptyNode returns the JSON Schema assertion corresponding to the
// validator's `required` check for an element value of the given (dereferenced)
// kind: non-zero for scalars. Nilable kinds and pointers are handled via the
// type/null wrapping, so this returns nil for them.
func requiredNonEmptyNode(t reflect.Type) *schemaNode {
	switch t.Kind() {
	case reflect.String:
		return &schemaNode{minLength: int64Ptr(1)}
	case reflect.Bool:
		return &schemaNode{constant: true, hasConstant: true}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		// x != 0
		return &schemaNode{not: &schemaNode{constant: 0, hasConstant: true}}
	case reflect.Float32, reflect.Float64:
		return &schemaNode{not: &schemaNode{constant: 0, hasConstant: true}}
	default:
		return nil
	}
}

func int64Ptr(i int64) *int64 { return &i }

// buildInner constructs the node for the dereferenced type, applying:
//   - level0 structural tags (len/min/max/..., oneof, eq/ne, unique)
//   - element tags from a dive for slices/arrays/maps
func (e *schemaExporter) buildInner(
	t reflect.Type,
	elemHead *cTag,
	level0 []*cTag,
	diveFound bool,
	goNS, ptr string,
) *schemaNode {
	// time.Time and []byte and other special types carry no numeric/length
	// assertions; their tags are reported by the leaf builder.
	base := e.baseNode(t, &collectedFieldEx{collectedField: &collectedField{goNS: goNS, ptr: ptr}}, "")

	switch t.Kind() {
	case reflect.Struct:
		if t.ConvertibleTo(timeType) {
			e.applyLevel0(base, t, level0, goNS, ptr)
			return base
		}
		// Nested struct: level0 tags other than the handled control tags are
		// not meaningful on a struct value.
		e.applyLevel0(base, t, level0, goNS, ptr)
		return base

	case reflect.Slice, reflect.Array:
		if t == byteSliceType {
			if len(level0) > 0 {
				e.add(DiagnosticError, ptr, goNS, chainStringFromTags(level0), ReasonByteSlice,
					"[]byte is encoded as a base64 string by encoding/json; byte/string validation tags cannot be translated accurately")
			}
			return base
		}
		e.applyLevel0(base, t, level0, goNS, ptr)

		if diveFound {
			elemGoNS := goNS + "[]"
			elemPtr := ptr + "/items"
			// A further dive may be nested within elemHead (multidimensional).
			elemRes := e.processTypeCtx(t.Elem(), elemHead, elemGoNS, elemPtr, true)
			base.items = elemRes.node
		}
		return base

	case reflect.Map:
		e.applyLevel0(base, t, level0, goNS, ptr)
		if diveFound {
			e.applyMapDive(t, elemHead, goNS, ptr, base)
		}
		return base

	default:
		e.applyLevel0(base, t, level0, goNS, ptr)
		return base
	}
}

// applyMapDive processes a dive on a map, including keys/endkeys.
func (e *schemaExporter) applyMapDive(t reflect.Type, head *cTag, goNS, ptr string, base *schemaNode) {
	// head is the cTag immediately after dive: it may be a typeKeys node.
	if head != nil && head.typeof == typeKeys {
		if t.Key().Kind() == reflect.String {
			keyNode := e.buildLeafChain(t.Key(), head.keys, goNS+"{key}", ptr+"/propertyNames")
			base.propNames = keyNode
		} else {
			e.add(DiagnosticError, ptr+"/propertyNames", goNS+"{key}", chainString(head.keys), ReasonMapKeys,
				fmt.Sprintf("map key kind %s is not representable: JSON object keys are always strings", t.Key().Kind()))
		}
		// Value tags follow the typeKeys node (keys node is self-contained).
		valueHead := head.next
		if valueHead != nil && valueHead.typeof == typeEndKeys {
			valueHead = valueHead.next
		}
		if hasTags(valueHead) {
			valRes := e.processTypeCtx(t.Elem(), valueHead, goNS+"{}", ptr+"/additionalProperties", true)
			base.addProps = valRes.node
		}
		return
	}

	if hasTags(head) {
		valRes := e.processTypeCtx(t.Elem(), head, goNS+"{}", ptr+"/additionalProperties", true)
		base.addProps = valRes.node
	}
}

func hasTags(head *cTag) bool {
	for ct := head; ct != nil; ct = ct.next {
		if ct.hasTag {
			return true
		}
	}
	return false
}

func chainString(head *cTag) string {
	var parts []string
	for ct := head; ct != nil; ct = ct.next {
		if ct.hasParam {
			parts = append(parts, ct.tag+"="+ct.param)
		} else if ct.tag != "" {
			parts = append(parts, ct.tag)
		}
	}
	return strings.Join(parts, ",")
}

// chainStringFromTags renders a slice of cTags as a comma-separated tag string.
func chainStringFromTags(tags []*cTag) string {
	var parts []string
	for _, ct := range tags {
		if ct == nil || ct.tag == "" {
			continue
		}
		if ct.hasParam {
			parts = append(parts, ct.tag+"="+ct.param)
		} else {
			parts = append(parts, ct.tag)
		}
	}
	return strings.Join(parts, ",")
}

// applyLevel0 merges the level-0 structural tags into a leaf/container node.
// Tags are processed in order; sequential 'or' (typeOr) tags form an anyOf
// group. Any single tag that cannot be translated produces a diagnostic and
// does not modify the node.
func (e *schemaExporter) applyLevel0(node *schemaNode, t reflect.Type, tags []*cTag, goNS, ptr string) {
	var orGroup []*cTag
	flush := func() {
		if len(orGroup) == 0 {
			return
		}
		e.applyOrGroup(node, t, orGroup, goNS, ptr)
		orGroup = nil
	}

	for i := 0; i < len(tags); i++ {
		ct := tags[i]
		if ct.typeof == typeOr {
			orGroup = append(orGroup, ct)
			continue
		}
		flush()
		e.applyOneTag(node, t, ct, goNS, ptr)
	}
	flush()
}

// applyOrGroup builds an anyOf whose branches are the individual 'or'
// alternatives. If any branch cannot be translated the whole group is dropped
// and a diagnostic is emitted, because a partial anyOf would be unsound.
func (e *schemaExporter) applyOrGroup(node *schemaNode, t reflect.Type, group []*cTag, goNS, ptr string) {
	branches := make([]*schemaNode, 0, len(group))
	expressions := make([]string, 0, len(group))
	allOK := true

	for _, ct := range group {
		expressions = append(expressions, tagExpr(ct))
		branch := &schemaNode{}
		if !e.applyOneTag(branch, t, ct, goNS, ptr) {
			allOK = false
			continue
		}
		// applyOneTag emitted diagnostics on failure, but suppress those for the
		// whole-group message by relying on the reason recorded below.
		branches = append(branches, branch)
	}

	if !allOK {
		e.add(DiagnosticError, ptr, goNS, strings.Join(expressions, "|"), ReasonOrBranch,
			"one or more branches of the 'or' expression cannot be translated; the entire expression was omitted")
		return
	}

	if len(branches) > 1 {
		node.anyOf = append(node.anyOf, branches...)
	} else if len(branches) == 1 {
		mergeNode(node, branches[0])
	}
}

func tagExpr(ct *cTag) string {
	if ct.hasParam {
		return ct.tag + "=" + ct.param
	}
	return ct.tag
}

// buildLeafChain builds a fresh node for a leaf type from a full tag chain
// (used for map keys / propertyNames).
func (e *schemaExporter) buildLeafChain(t reflect.Type, head *cTag, goNS, ptr string) *schemaNode {
	node := e.baseNode(t, &collectedFieldEx{collectedField: &collectedField{goNS: goNS, ptr: ptr}}, "")
	var tags []*cTag
	for ct := head; ct != nil; ct = ct.next {
		if ct.hasTag && ct.tag != "" && ct.typeof != typeDive && ct.typeof != typeKeys && ct.typeof != typeEndKeys {
			tags = append(tags, ct)
		}
	}
	e.applyLevel0(node, t, tags, goNS, ptr)
	return node
}

// applyOneTag translates a single atomic tag into the node. It returns false
// (and records a diagnostic) when the tag has no equivalent for this type.
//
// Exactly supported (and only these):
//   - string: len/min/max (rune length), eq/ne (exact value), oneof/noneof
//   - integer/number: len/eq/ne, min/gte, max/lte, gt, lt, oneof/noneof
//   - slice/array/map (level 0): len/min/max/eq/ne (item count), unique
//   - boolean: eq/ne
func (e *schemaExporter) applyOneTag(node *schemaNode, t reflect.Type, ct *cTag, goNS, ptr string) bool {
	if e.isCustomTag(ct.tag) {
		e.unsupported(ct, goNS, ptr, ReasonCustomValidator,
			fmt.Sprintf("tag %q is a user-registered validation function with opaque semantics", ct.tag))
		return false
	}

	switch ct.tag {
	// Control tags handled by the caller.
	case requiredTag, omitempty, omitnil, omitzero, isdefault,
		structOnlyTag, noStructLevelTag, diveTag, keysTag, endKeysTag:
		return true
	}

	switch t.Kind() {
	case reflect.String:
		return e.applyStringTag(node, ct, goNS, ptr)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return e.applyNumberTag(node, ct, t, goNS, ptr)
	case reflect.Bool:
		return e.applyBoolTag(node, ct, goNS, ptr)
	case reflect.Slice, reflect.Array, reflect.Map:
		return e.applyContainerTag(node, t, ct, goNS, ptr)
	case reflect.Struct:
		if t.ConvertibleTo(timeType) || t == timeDurationType {
			return e.applyTimeTag(node, ct, t, goNS, ptr)
		}
	}

	e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
		fmt.Sprintf("tag %q cannot be applied to kind %s in JSON Schema", ct.tag, t.Kind()))
	return false
}

// isCustomTag reports whether a tag resolves to a user-registered function
// rather than a baked-in one. Baked-in names occupy the same map; comparing
// against bakedInValidators distinguishes the two.
func (e *schemaExporter) isCustomTag(tag string) bool {
	if _, baked := bakedInValidators[tag]; baked {
		return false
	}
	if _, baked := bakedInAliases[tag]; baked {
		return false
	}
	if _, registered := e.v.validations[tag]; registered {
		return true
	}
	return false
}

func (e *schemaExporter) unsupported(ct *cTag, goNS, ptr, reason, msg string) {
	e.add(DiagnosticError, ptr, goNS, tagExpr(ct), reason, msg)
}

// applyStringTag handles rune-length and exact-value tags for strings.
func (e *schemaExporter) applyStringTag(node *schemaNode, ct *cTag, goNS, ptr string) bool {
	switch ct.tag {
	case "len":
		n, ok := parseIntParam(ct, e, goNS, ptr)
		if !ok {
			return false
		}
		node.minLength = &n
		node.maxLength = &n
		return true
	case "min", "gte":
		n, ok := parseIntParam(ct, e, goNS, ptr)
		if !ok {
			return false
		}
		node.minLength = &n
		return true
	case "max", "lte":
		n, ok := parseIntParam(ct, e, goNS, ptr)
		if !ok {
			return false
		}
		node.maxLength = &n
		return true
	case "gt":
		n, ok := parseIntParam(ct, e, goNS, ptr)
		if !ok {
			return false
		}
		// string rune length > n  <=>  minLength = n+1
		n++
		node.minLength = &n
		return true
	case "lt":
		n, ok := parseIntParam(ct, e, goNS, ptr)
		if !ok {
			return false
		}
		// string rune length < n  <=>  maxLength = n-1
		n--
		node.maxLength = &n
		return true
	case "eq":
		node.constant = ct.param
		node.hasConstant = true
		node.types = []string{"string"}
		return true
	case "ne":
		node.not = &schemaNode{constant: ct.param, hasConstant: true}
		return true
	case "oneof":
		vals := parseOneOfParam2(ct.param)
		node.enum = make([]interface{}, len(vals))
		for i := range vals {
			node.enum[i] = vals[i]
		}
		return true
	case "noneof":
		vals := parseOneOfParam2(ct.param)
		en := make([]interface{}, len(vals))
		for i := range vals {
			en[i] = vals[i]
		}
		node.not = &schemaNode{enum: en}
		return true
	}
	e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
		fmt.Sprintf("tag %q on a string has no JSON Schema equivalent", ct.tag))
	return false
}

// isIntegerKind reports whether the reflect kind maps to JSON integer.
func isIntegerKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	}
	return false
}

// applyNumberTag handles numeric comparisons and enums. Bounds use the exact
// integer/float token supplied by the tag.
func (e *schemaExporter) applyNumberTag(node *schemaNode, ct *cTag, t reflect.Type, goNS, ptr string) bool {
	isInt := isIntegerKind(t.Kind())

	parseBound := func() (float64, bool) {
		f, err := strconv.ParseFloat(ct.param, 64)
		if err != nil {
			e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
				fmt.Sprintf("tag %q has non-numeric parameter %q", ct.tag, ct.param))
			return 0, false
		}
		return f, true
	}

	switch ct.tag {
	case "min", "gte":
		f, ok := parseBound()
		if !ok {
			return false
		}
		node.minimum = &f
		return true
	case "max", "lte":
		f, ok := parseBound()
		if !ok {
			return false
		}
		node.maximum = &f
		return true
	case "gt":
		f, ok := parseBound()
		if !ok {
			return false
		}
		if isInt {
			// x > n for integers <=> x >= n+1
			f++
			node.minimum = &f
			return true
		}
		// Strict inequality over floats has no exact JSON Schema keyword; emit a
		// diagnostic rather than weakening the rule.
		e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
			"strict gt/lt over floating point values cannot be expressed with minimum/maximum (which are inclusive)")
		return false
	case "lt":
		f, ok := parseBound()
		if !ok {
			return false
		}
		if isInt {
			f--
			node.maximum = &f
			return true
		}
		e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
			"strict gt/lt over floating point values cannot be expressed with minimum/maximum (which are inclusive)")
		return false
	case "len", "eq":
		f, ok := parseBound()
		if !ok {
			return false
		}
		node.minimum = &f
		node.maximum = &f
		return true
	case "ne":
		f, ok := parseBound()
		if !ok {
			return false
		}
		node.not = &schemaNode{constant: f, hasConstant: true}
		return true
	case "oneof":
		return e.applyNumericEnum(node, ct, goNS, ptr, false)
	case "noneof":
		return e.applyNumericEnum(node, ct, goNS, ptr, true)
	}

	e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
		fmt.Sprintf("tag %q on a number has no JSON Schema equivalent", ct.tag))
	return false
}

// applyNumericEnum maps oneof/noneof numeric parameters to enum/not.enum. It
// rejects parameters that do not parse as the field's number kind.
func (e *schemaExporter) applyNumericEnum(node *schemaNode, ct *cTag, goNS, ptr string, negated bool) bool {
	tokens := parseOneOfParam2(ct.param)
	en := make([]interface{}, 0, len(tokens))
	for _, tok := range tokens {
		f, err := strconv.ParseFloat(tok, 64)
		if err != nil {
			e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
				fmt.Sprintf("%s value %q is not numeric", ct.tag, tok))
			return false
		}
		if math.Trunc(f) == f {
			en = append(en, f)
		} else {
			en = append(en, f)
		}
	}
	if negated {
		node.not = &schemaNode{enum: en}
	} else {
		node.enum = en
	}
	return true
}

// applyBoolTag supports eq/ne against booleans.
func (e *schemaExporter) applyBoolTag(node *schemaNode, ct *cTag, goNS, ptr string) bool {
	switch ct.tag {
	case "eq":
		b, err := strconv.ParseBool(ct.param)
		if err != nil {
			e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
				fmt.Sprintf("eq parameter %q is not a boolean", ct.param))
			return false
		}
		node.constant = b
		node.hasConstant = true
		node.types = []string{"boolean"}
		return true
	case "ne":
		b, err := strconv.ParseBool(ct.param)
		if err != nil {
			e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
				fmt.Sprintf("ne parameter %q is not a boolean", ct.param))
			return false
		}
		node.not = &schemaNode{constant: b, hasConstant: true}
		return true
	}
	e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
		fmt.Sprintf("tag %q on a boolean has no JSON Schema equivalent", ct.tag))
	return false
}

// applyContainerTag handles item-count tags for slices/arrays and
// property-count tags for maps at the collection level (element assertions
// come via dive). Go's len/min/max/gt/lt/eq/ne measure element count for both;
// JSON Schema expresses these with minItems/maxItems for arrays and
// minProperties/maxProperties for objects.
func (e *schemaExporter) applyContainerTag(node *schemaNode, t reflect.Type, ct *cTag, goNS, ptr string) bool {
	isMap := t.Kind() == reflect.Map

	setMin := func(n int64) {
		if isMap {
			node.minProps = &n
		} else {
			node.minItems = &n
		}
	}
	setMax := func(n int64) {
		if isMap {
			node.maxProps = &n
		} else {
			node.maxItems = &n
		}
	}

	switch ct.tag {
	case "len", "min", "gte":
		n, ok := parseIntParam(ct, e, goNS, ptr)
		if !ok {
			return false
		}
		setMin(n)
		return true
	case "max", "lte":
		n, ok := parseIntParam(ct, e, goNS, ptr)
		if !ok {
			return false
		}
		setMax(n)
		return true
	case "gt":
		n, ok := parseIntParam(ct, e, goNS, ptr)
		if !ok {
			return false
		}
		n++
		setMin(n)
		return true
	case "lt":
		n, ok := parseIntParam(ct, e, goNS, ptr)
		if !ok {
			return false
		}
		n--
		setMax(n)
		return true
	case "eq":
		n, ok := parseIntParam(ct, e, goNS, ptr)
		if !ok {
			return false
		}
		setMin(n)
		setMax(n)
		return true
	case "ne":
		n, ok := parseIntParam(ct, e, goNS, ptr)
		if !ok {
			return false
		}
		forbidden := &schemaNode{}
		if isMap {
			forbidden.minProps = &n
			forbidden.maxProps = &n
		} else {
			forbidden.minItems = &n
			forbidden.maxItems = &n
		}
		node.not = forbidden
		return true
	case "unique":
		if isMap {
			e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
				"unique on a map checks distinct values, which JSON Schema cannot express")
			return false
		}
		if ct.hasParam {
			// unique=Field for struct elements is not representable.
			e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
				"unique=Field checks uniqueness by a struct field and is not expressible in JSON Schema")
			return false
		}
		node.uniqueItems = true
		return true
	}

	e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
		fmt.Sprintf("tag %q on a %s has no JSON Schema equivalent", ct.tag, t.Kind()))
	return false
}

// applyTimeTag documents that time-based numeric/order tags are dynamic or
// structural and cannot be expressed. The base date-time string type is still
// emitted.
func (e *schemaExporter) applyTimeTag(node *schemaNode, ct *cTag, t reflect.Type, goNS, ptr string) bool {
	if t == timeDurationType {
		// time.Duration is an int64 nanosecond count in JSON; only integer
		// bounds would make sense and the parser accepts durations such as
		// "1h30m". Report it as untranslatable to avoid silent misreading.
		e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
			"time.Duration accepts duration parameters (e.g. 1h30m) that JSON Schema numeric ranges cannot express")
		return false
	}
	e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
		fmt.Sprintf("tag %q on time.Time is dynamic (e.g. compared to time.Now) or structural and cannot be exported", ct.tag))
	return false
}

// parseIntParam parses an integer tag parameter, reporting on failure.
func parseIntParam(ct *cTag, e *schemaExporter, goNS, ptr string) (int64, bool) {
	n, err := strconv.ParseInt(ct.param, 0, 64)
	if err != nil {
		e.unsupported(ct, goNS, ptr, ReasonUnsupportedTag,
			fmt.Sprintf("tag %q expects an integer parameter, got %q", ct.tag, ct.param))
		return 0, false
	}
	return n, true
}

// mergeNode merges single-branch 'or' or compatible assertions into dst.
func mergeNode(dst, src *schemaNode) {
	if src.minimum != nil {
		dst.minimum = src.minimum
	}
	if src.maximum != nil {
		dst.maximum = src.maximum
	}
	if src.minLength != nil {
		dst.minLength = src.minLength
	}
	if src.maxLength != nil {
		dst.maxLength = src.maxLength
	}
	if src.minItems != nil {
		dst.minItems = src.minItems
	}
	if src.maxItems != nil {
		dst.maxItems = src.maxItems
	}
	if src.minProps != nil {
		dst.minProps = src.minProps
	}
	if src.maxProps != nil {
		dst.maxProps = src.maxProps
	}
	if src.hasConstant {
		dst.constant = src.constant
		dst.hasConstant = true
	}
	if len(src.enum) > 0 {
		dst.enum = src.enum
	}
	if src.not != nil {
		dst.not = src.not
	}
	if src.uniqueItems {
		dst.uniqueItems = true
	}
}

// wrapNull returns a node accepting either the given schema or JSON null.
func wrapNull(node *schemaNode) *schemaNode {
	if node == nil {
		return &schemaNode{types: []string{"null"}}
	}
	return &schemaNode{anyOf: []*schemaNode{node, {types: []string{"null"}}}}
}

// wrapOmitEmpty models the validator omitempty tag: following tags run only
// when the value is not empty (hasValue). The empty forms are:
//   - string: ""
//   - number/bool: 0 / false
//   - pointer/interface/slice/map: nil (nil slices and maps count as empty)
func (e *schemaExporter) wrapOmitEmpty(t reflect.Type, node *schemaNode, nilable bool, goNS, ptr string) *schemaNode {
	if nilable {
		return wrapNull(node)
	}

	var empty *schemaNode
	switch t.Kind() {
	case reflect.String:
		empty = &schemaNode{constant: "", hasConstant: true}
	case reflect.Bool:
		empty = &schemaNode{constant: false, hasConstant: true}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		empty = &schemaNode{constant: 0, hasConstant: true}
	case reflect.Struct:
		// omitempty uses hasValue, which for a struct checks IsZero; JSON
		// Schema cannot express "the all-zero struct".
		e.add(DiagnosticError, ptr, goNS, omitempty, ReasonOmitZero,
			"omitempty on a non-pointer struct skips validation for the all-zero struct, which JSON Schema cannot express; constraints are emitted without the empty branch")
		return node
	default:
		return node
	}

	return &schemaNode{anyOf: []*schemaNode{node, empty}}
}

// defaultValueNode models isdefault: value must equal the type's zero value.
func (e *schemaExporter) defaultValueNode(t reflect.Type, ptr, goNS, tag string) *schemaNode {
	switch t.Kind() {
	case reflect.String:
		return &schemaNode{constant: "", hasConstant: true}
	case reflect.Bool:
		return &schemaNode{constant: false, hasConstant: true}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return &schemaNode{constant: 0, hasConstant: true}
	default:
		e.add(DiagnosticError, ptr, goNS, tag, ReasonUnsupportedTag,
			fmt.Sprintf("isdefault on kind %s cannot be expressed as a JSON const", t.Kind()))
		return &schemaNode{}
	}
}

// goZeroValue returns the generic JSON value representing the Go zero value
// for the given dereferenced type.
func goZeroValue(t reflect.Type) interface{} {
	switch t.Kind() {
	case reflect.String:
		return ""
	case reflect.Bool:
		return false
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return 0
	case reflect.Slice, reflect.Map, reflect.Ptr, reflect.Interface:
		return nil
	case reflect.Array:
		n := t.Len()
		arr := make([]interface{}, n)
		z := goZeroValue(t.Elem())
		for i := range arr {
			arr[i] = z
		}
		return arr
	case reflect.Struct:
		return map[string]interface{}{}
	default:
		return nil
	}
}

// hasUntranslatedLevel0 reports whether any level-0 structural tag is outside
// the translatable set.
func hasUntranslatedLevel0(tags []*cTag) bool {
	for _, ct := range tags {
		if ct.tag == "" {
			continue
		}
		if !isLevel0Translatable(ct.tag) {
			return true
		}
	}
	return false
}

// isLevel0Translatable lists the tags the level-0 builder can represent.
func isLevel0Translatable(tag string) bool {
	switch tag {
	case "len", "min", "max", "eq", "ne", "lt", "lte", "gt", "gte",
		"oneof", "noneof", "unique":
		return true
	}
	return false
}

// nodeMap renders a schemaNode into the generic map form used in the export.
func nodeMap(n *schemaNode) map[string]interface{} {
	m := map[string]interface{}{}

	if n.nonSchema {
		return m
	}
	if n.ref != "" {
		m["$ref"] = n.ref
		// Ref nodes are standalone; other fields are absent by construction.
		return m
	}

	if len(n.types) == 1 {
		m["type"] = n.types[0]
	} else if len(n.types) > 1 {
		ts := make([]interface{}, len(n.types))
		for i, t := range n.types {
			ts[i] = t
		}
		m["type"] = ts
	}

	if n.format != "" {
		m["format"] = n.format
	}
	if n.contentEnc != "" {
		m["contentEncoding"] = n.contentEnc
	}
	if n.hasConstant {
		m["const"] = n.constant
	}
	if len(n.enum) > 0 {
		m["enum"] = n.enum
	}
	if n.minimum != nil {
		m["minimum"] = *n.minimum
	}
	if n.maximum != nil {
		m["maximum"] = *n.maximum
	}
	if n.minLength != nil {
		m["minLength"] = *n.minLength
	}
	if n.maxLength != nil {
		m["maxLength"] = *n.maxLength
	}
	if n.minItems != nil {
		m["minItems"] = *n.minItems
	}
	if n.maxItems != nil {
		m["maxItems"] = *n.maxItems
	}
	if n.minProps != nil {
		m["minProperties"] = *n.minProps
	}
	if n.maxProps != nil {
		m["maxProperties"] = *n.maxProps
	}
	if n.uniqueItems {
		m["uniqueItems"] = true
	}
	if n.items != nil {
		m["items"] = nodeMap(n.items)
	}
	if len(n.props) > 0 {
		props := make(map[string]interface{}, len(n.props))
		keys := make([]string, 0, len(n.props))
		for k := range n.props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			props[k] = nodeMap(n.props[k])
		}
		m["properties"] = props
	}
	if len(n.required) > 0 {
		req := make([]interface{}, len(n.required))
		for i, r := range n.required {
			req[i] = r
		}
		m["required"] = req
	}
	if n.addProps != nil {
		m["additionalProperties"] = nodeMap(n.addProps)
	}
	if n.propNames != nil {
		m["propertyNames"] = nodeMap(n.propNames)
	}
	if len(n.anyOf) > 0 {
		arr := make([]interface{}, len(n.anyOf))
		for i, a := range n.anyOf {
			arr[i] = nodeMap(a)
		}
		m["anyOf"] = arr
	}
	if len(n.allOf) > 0 {
		arr := make([]interface{}, len(n.allOf))
		for i, a := range n.allOf {
			arr[i] = nodeMap(a)
		}
		m["allOf"] = arr
	}
	if n.not != nil {
		m["not"] = nodeMap(n.not)
	}

	return m
}

// nodeAccepts evaluates a single generic JSON value against a schemaNode. It
// implements exactly the assertion subset the exporter emits, and is used to
// determine whether the Go zero value satisfies the translated level-0
// constraints (presence inference). $ref nodes are treated as accepting any
// value during the zero check; callers that build refs for structs do so only
// after structural tags are handled on scalar/container nodes.
func nodeAccepts(n *schemaNode, value interface{}) bool {
	if n == nil {
		return true
	}
	if n.nonSchema {
		return true
	}
	if n.ref != "" {
		// Struct reference: a zero struct is the empty object, which satisfies
		// object typing; required handling within the struct is part of the
		// referenced node and is not needed for the field-presence decision.
		return true
	}

	if n.not != nil && nodeAccepts(n.not, value) {
		return false
	}

	if len(n.anyOf) > 0 {
		ok := false
		for _, a := range n.anyOf {
			if nodeAccepts(a, value) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}

	for _, a := range n.allOf {
		if !nodeAccepts(a, value) {
			return false
		}
	}

	if !typeAccepts(n.types, value) {
		return false
	}

	if n.hasConstant && !jsonEqual(n.constant, value) {
		return false
	}

	if len(n.enum) > 0 {
		found := false
		for _, v := range n.enum {
			if jsonEqual(v, value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	switch x := value.(type) {
	case string:
		runes := int64(len([]rune(x)))
		if n.minLength != nil && runes < *n.minLength {
			return false
		}
		if n.maxLength != nil && runes > *n.maxLength {
			return false
		}
	case float64:
		if n.minimum != nil && x < *n.minimum {
			return false
		}
		if n.maximum != nil && x > *n.maximum {
			return false
		}
	case int:
		f := float64(x)
		if n.minimum != nil && f < *n.minimum {
			return false
		}
		if n.maximum != nil && f > *n.maximum {
			return false
		}
	case []interface{}:
		l := int64(len(x))
		if n.minItems != nil && l < *n.minItems {
			return false
		}
		if n.maxItems != nil && l > *n.maxItems {
			return false
		}
		if n.items != nil {
			for _, item := range x {
				if !nodeAccepts(n.items, item) {
					return false
				}
			}
		}
		if n.uniqueItems && !jsonUnique(x) {
			return false
		}
	case map[string]interface{}:
		count := int64(len(x))
		if n.minProps != nil && count < *n.minProps {
			return false
		}
		if n.maxProps != nil && count > *n.maxProps {
			return false
		}
		for _, r := range n.required {
			if _, ok := x[r]; !ok {
				return false
			}
		}
		for k, child := range n.props {
			if v, ok := x[k]; ok && !nodeAccepts(child, v) {
				return false
			}
		}
		if n.addProps != nil {
			for k, v := range x {
				if _, handled := n.props[k]; handled {
					continue
				}
				if !nodeAccepts(n.addProps, v) {
					return false
				}
			}
		}
	case nil:
		// Only "null" typing or no typing accepts nil.
		if len(n.types) > 0 && !containsStr(n.types, "null") {
			return false
		}
	}

	return true
}

// typeAccepts checks a node's declared JSON types against a generic value.
func typeAccepts(types []string, value interface{}) bool {
	if len(types) == 0 {
		return true
	}
	var want string
	switch value.(type) {
	case nil:
		want = "null"
	case bool:
		want = "boolean"
	case string:
		want = "string"
	case float64, int:
		want = "number"
		// Integers satisfy both "number" and "integer".
		for _, t := range types {
			if t == want || (t == "integer" && isJSONInteger(value)) {
				return true
			}
		}
		return false
	case []interface{}:
		want = "array"
	case map[string]interface{}:
		want = "object"
	default:
		want = ""
	}
	return containsStr(types, want)
}

func isJSONInteger(value interface{}) bool {
	switch x := value.(type) {
	case int:
		return true
	case float64:
		return math.Trunc(x) == x
	}
	return false
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// jsonEqual compares generic JSON-decoded values.
func jsonEqual(a, b interface{}) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case int:
		switch bv := b.(type) {
		case int:
			return av == bv
		case float64:
			return float64(av) == bv
		}
		return false
	case float64:
		switch bv := b.(type) {
		case float64:
			return av == bv
		case int:
			return av == float64(bv)
		}
		return false
	case nil:
		return b == nil
	default:
		return false
	}
}

// jsonUnique reports whether an array's scalar elements are unique (JSON
// Schema uniqueItems semantics over the exported element set).
func jsonUnique(xs []interface{}) bool {
	seen := make([]interface{}, 0, len(xs))
	for _, x := range xs {
		for _, s := range seen {
			if jsonEqual(s, x) {
				return false
			}
		}
		seen = append(seen, x)
	}
	return true
}
