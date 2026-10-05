package paseto

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// MaxContractDepth is the maximum nesting depth a ClaimsContract traverses at
// runtime. It bounds the work done validating a cryptographically verified but
// adversarially nested token. The top-level claims object is depth 1.
const MaxContractDepth = 32

// ClaimKind is the category of value expected for a claim.
type ClaimKind int

const (
	// String expects a JSON string.
	String ClaimKind = iota + 1
	// Boolean expects a JSON boolean.
	Boolean
	// Number expects a raw JSON number. Numbers are handled without any
	// lossy conversion through float64.
	Number
	// Integer expects a JSON number carrying no fractional or exponent
	// remainder. Values outside the float64 range are checked exactly using
	// arbitrary precision arithmetic.
	Integer
	// Array expects a JSON array whose elements all match an element spec.
	Array
	// Object expects a JSON object whose fields match a field map.
	Object
)

// FieldSpec describes a single claim: its expected kind, whether it is
// required, and (for arrays and objects) the structure of its contents.
//
// Construct FieldSpec values with StringSpec, BooleanSpec, NumberSpec,
// IntegerSpec, ArraySpec, and ObjectSpec; mark one optional with Optional.
type FieldSpec struct {
	kind ClaimKind
	// required defaults to true when a spec is constructed with one of the
	// helper constructors below.
	required bool

	// element describes the kind of every element of an array.
	element *FieldSpec
	// fields describes the fields of an object.
	fields map[string]FieldSpec
}

// StringSpec returns a FieldSpec expecting a JSON string.
func StringSpec() FieldSpec { return FieldSpec{kind: String, required: true} }

// BooleanSpec returns a FieldSpec expecting a JSON boolean.
func BooleanSpec() FieldSpec {
	return FieldSpec{kind: Boolean, required: true}
}

// NumberSpec returns a FieldSpec expecting a raw JSON number. Numbers are not
// converted through float64, so values which cannot be represented exactly as
// a float64 are not corrupted before being checked.
func NumberSpec() FieldSpec { return FieldSpec{kind: Number, required: true} }

// IntegerSpec returns a FieldSpec expecting a JSON number with no fractional
// part. Integer literals beyond float64 precision are checked exactly.
func IntegerSpec() FieldSpec { return FieldSpec{kind: Integer, required: true} }

// ArraySpec returns a FieldSpec expecting a JSON array in which every element
// matches element.
func ArraySpec(element FieldSpec) FieldSpec {
	elementCopy := element
	return FieldSpec{kind: Array, required: true, element: &elementCopy}
}

// ObjectSpec returns a FieldSpec expecting a JSON object with the given
// fields. Fields absent from the map are allowed unless the owning contract
// rejects unknown fields.
func ObjectSpec(fields map[string]FieldSpec) FieldSpec {
	return FieldSpec{kind: Object, required: true, fields: fields}
}

// Optional marks a field spec as not required. A missing optional field is
// allowed; an explicit null is still a null/type failure.
func (s FieldSpec) Optional() FieldSpec {
	s.required = false
	return s
}

// ClaimsContract is a reusable description of the claims a token may carry. It
// validates claim kinds and required fields, and can optionally reject fields
// that were not declared.
//
// A contract is intended to run as a Rule after a token has passed
// cryptographic verification. It does not replace existing rules such as
// ForAudience, IssuedBy, or NotExpired.
//
// The zero value is not usable; construct one with NewClaimsContract.
type ClaimsContract struct {
	fields map[string]FieldSpec
	reject bool
}

// NewClaimsContract begins building a reusable claims contract from the given
// top-level field specifications. Contradictory or otherwise invalid
// definitions (for example a nil array element, an empty field name, or a
// self-referencing nested object) are reported at construction time.
//
// Unknown (undeclared) fields are accepted by default. Use
// RejectUnknownFields to disallow them.
func NewClaimsContract(fields map[string]FieldSpec) (*ClaimsContract, error) {
	contract := &ClaimsContract{fields: fields}
	if err := contract.validateDefinition(); err != nil {
		return nil, err
	}
	return contract, nil
}

// RejectUnknownFields declares that any field not explicitly allowed by the
// contract is an error. This applies to the top-level claims and to every
// nested object described by the contract.
func (c *ClaimsContract) RejectUnknownFields() (*ClaimsContract, error) {
	c.reject = true
	if err := c.validateDefinition(); err != nil {
		return nil, err
	}
	return c, nil
}

// ClaimCategory classifies the way in which a particular claim failed a
// contract.
type ClaimCategory int

const (
	// MissingClaim means that a required field was absent. This is distinct
	// from NullClaim.
	MissingClaim ClaimCategory = iota + 1
	// NullClaim means that a field was present but its value was JSON null.
	NullClaim
	// WrongType means that a field was present but its JSON value did not
	// match the expected kind.
	WrongType
	// UnknownClaim means that a field was present which the contract did not
	// declare while unknown fields were being rejected.
	UnknownClaim
	// ClaimTooDeep means that the value was nested more deeply than the
	// runtime recursion limit allows.
	ClaimTooDeep
)

// ClaimError is returned when a claim fails a ClaimsContract.
//
// Path is always reported, along with a Category. The offending value is
// never included in the error message or exposed by this type.
type ClaimError struct {
	// Path identifies the offending claim, for example "role" or
	// "groups[0].name".
	Path string
	// Category classifies the failure.
	Category ClaimCategory
	// Expected is the kind that was expected. It is zero for failures which
	// are not kind specific (missing, null, unknown, or too deep).
	Expected ClaimKind

	message string
}

func (e ClaimError) Error() string {
	return e.message
}

// Is reports whether the target is a ClaimError, so that
// errors.Is(err, ClaimError{}) detects any claim failure.
func (ClaimError) Is(target error) bool {
	_, ok := target.(ClaimError)
	return ok
}

func claimError(path string, category ClaimCategory, expected ClaimKind, message string) ClaimError {
	return ClaimError{
		Path:     path,
		Category: category,
		Expected: expected,
		message:  message,
	}
}

func kindName(kind ClaimKind) string {
	switch kind {
	case String:
		return "string"
	case Boolean:
		return "boolean"
	case Number:
		return "number"
	case Integer:
		return "integer"
	case Array:
		return "array"
	case Object:
		return "object"
	default:
		return "unknown"
	}
}

func joinPath(base, name string) string {
	if base == "" {
		return name
	}
	return base + "." + name
}

func indexPath(base string, index int) string {
	return fmt.Sprintf("%s[%d]", base, index)
}

// CheckClaims validates the token's claims against the contract. It is
// exported so a contract can be applied directly, but it is normally used as
// a parser rule via Rule or Parser.AddClaimsContract.
func (c *ClaimsContract) CheckClaims(token Token) error {
	return checkObject(token.claims, c.fields, c.reject, "", 1)
}

// Rule returns a Rule which validates tokens against the contract. As with any
// rule, this only runs after cryptographic verification has succeeded.
func (c *ClaimsContract) Rule() Rule {
	return c.CheckClaims
}

// checkObject validates a JSON object (the top-level claims, or the value of
// an Object field) against a set of field specs. depth counts nesting, with
// the top-level claims object at depth 1.
func checkObject(object map[string]json.RawMessage, fields map[string]FieldSpec, rejectUnknown bool, path string, depth int) error {
	if depth > MaxContractDepth {
		return tooDeep(path)
	}

	for name, spec := range fields {
		fieldPath := joinPath(path, name)
		raw, present := object[name]
		if !present {
			if spec.required {
				return claimError(fieldPath, MissingClaim, spec.kind,
					fmt.Sprintf("required claim `%s' is missing", fieldPath))
			}

			continue
		}

		if err := checkValue(raw, spec, rejectUnknown, fieldPath, depth); err != nil {
			return err
		}
	}

	if rejectUnknown {
		for name := range object {
			if _, declared := fields[name]; !declared {
				fieldPath := joinPath(path, name)
				return claimError(fieldPath, UnknownClaim, 0,
					fmt.Sprintf("unknown claim `%s' is not allowed", fieldPath))
			}
		}
	}

	return nil
}

// checkArray validates each element of a JSON array against one element spec.
func checkArray(elements []json.RawMessage, element FieldSpec, rejectUnknown bool, path string, depth int) error {
	if depth > MaxContractDepth {
		return tooDeep(path)
	}

	for index, raw := range elements {
		if err := checkValue(raw, element, rejectUnknown, indexPath(path, index), depth); err != nil {
			return err
		}
	}

	return nil
}

// checkValue validates a single raw JSON value against a field spec.
func checkValue(raw json.RawMessage, spec FieldSpec, rejectUnknown bool, path string, depth int) error {
	if isJSONNull(raw) {
		return claimError(path, NullClaim, spec.kind,
			fmt.Sprintf("claim `%s' must not be null", path))
	}

	switch spec.kind {
	case String:
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return wrongType(path, String)
		}
	case Boolean:
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return wrongType(path, Boolean)
		}
	case Number:
		if !isJSONNumber(raw) {
			return wrongType(path, Number)
		}
	case Integer:
		if !isJSONNumber(raw) {
			return wrongType(path, Integer)
		}
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil || !isIntegerNumber(number) {
			return wrongType(path, Integer)
		}
	case Array:
		var elements []json.RawMessage
		if err := json.Unmarshal(raw, &elements); err != nil {
			return wrongType(path, Array)
		}
		return checkArray(elements, *spec.element, rejectUnknown, path, depth+1)
	case Object:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return wrongType(path, Object)
		}
		return checkObject(object, spec.fields, rejectUnknown, path, depth+1)
	default:
		return fmt.Errorf("claim `%s': internal error, unsupported claim kind", path)
	}

	return nil
}

func wrongType(path string, expected ClaimKind) ClaimError {
	return claimError(path, WrongType, expected,
		fmt.Sprintf("claim `%s' is not of the expected type %s", path, kindName(expected)))
}

func tooDeep(path string) ClaimError {
	return claimError(path, ClaimTooDeep, 0,
		fmt.Sprintf("claim `%s' exceeds the maximum allowed nesting depth of %d", displayPath(path), MaxContractDepth))
}

func displayPath(path string) string {
	if path == "" {
		return "$"
	}
	return path
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// isJSONNumber reports whether raw is a single JSON number token. Decoding
// into json.Number avoids ever converting through float64.
func isJSONNumber(raw json.RawMessage) bool {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()

	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return false
	}
	if decoder.More() {
		return false
	}
	return true
}

// isIntegerNumber reports whether a JSON number denotes an integer, i.e. it
// has no fractional part. Arbitrary precision rationals are used so integers
// beyond float64 precision are never rounded before the check.
func isIntegerNumber(number json.Number) bool {
	r, ok := new(big.Rat).SetString(number.String())
	if !ok {
		return false
	}
	return r.IsInt()
}

// validateDefinition rejects contradictory or unusable contract definitions.
// See validateFieldTree for the structural checks performed.
func (c *ClaimsContract) validateDefinition() error {
	return validateFieldTree(c.fields)
}

// validateFieldTree walks a tree of field specs, reporting contradictory
// definitions. Object fields are traversed structurally (field specs are held
// by value), while array elements carry a stable *FieldSpec pointer and are
// tracked as ancestors so a pointer cycle is reported. A depth bound provides
// a structural backstop regardless of how a cycle was assembled.
func validateFieldTree(topLevel map[string]FieldSpec) error {
	arrayAncestors := make(map[*FieldSpec]bool)

	var walk func(spec FieldSpec, path string, depth int) error
	walk = func(spec FieldSpec, path string, depth int) error {
		if depth > MaxContractDepth+1 {
			return fmt.Errorf("invalid claims contract: field `%s' is nested more deeply than %d levels", path, MaxContractDepth)
		}

		switch spec.kind {
		case Array:
			if spec.element == nil {
				return fmt.Errorf("invalid claims contract: array field `%s' must declare an element type", path)
			}
			if arrayAncestors[spec.element] {
				return fmt.Errorf("invalid claims contract: field `%s' is part of a self-referencing definition", path)
			}
			arrayAncestors[spec.element] = true
			defer delete(arrayAncestors, spec.element)
			if err := walk(*spec.element, path+"[]", depth+1); err != nil {
				return err
			}
		case Object:
			for name, child := range spec.fields {
				if name == "" {
					return fmt.Errorf("invalid claims contract: object field `%s' contains an empty field name", path)
				}
				if err := walk(child, joinPath(path, name), depth+1); err != nil {
					return err
				}
			}
		case String, Boolean, Number, Integer:
			// no further structure to check
		default:
			return fmt.Errorf("invalid claims contract: field `%s' has no recognised kind", path)
		}

		return nil
	}

	for name, spec := range topLevel {
		if name == "" {
			return fmt.Errorf("invalid claims contract: top-level field name must not be empty")
		}
		if err := walk(spec, name, 1); err != nil {
			return err
		}
	}

	return nil
}
