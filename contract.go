package paseto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

// ClaimErrorKind categorises a ClaimsContract validation failure.
type ClaimErrorKind int

const (
	// ClaimTypeMismatch means the claim was present, but its JSON value did
	// not match the declared type.
	ClaimTypeMismatch ClaimErrorKind = iota
	// ClaimMissing means a required claim was not present.
	ClaimMissing
	// ClaimNull means a required claim was present, but its value was null.
	ClaimNull
	// ClaimUnknown means a claim was present which the contract did not
	// declare, and unknown fields were rejected.
	ClaimUnknown
	// ClaimTooDeep means the claim nesting exceeded the maximum depth.
	ClaimTooDeep
)

// Sentinel errors, one per ClaimErrorKind. They can be used with errors.Is
// (for example errors.Is(err, paseto.ErrClaimMissing)).
var (
	ErrClaimTypeMismatch = claimKindError{ClaimTypeMismatch}
	ErrClaimMissing      = claimKindError{ClaimMissing}
	ErrClaimNull         = claimKindError{ClaimNull}
	ErrClaimUnknown      = claimKindError{ClaimUnknown}
	ErrClaimTooDeep      = claimKindError{ClaimTooDeep}
)

type claimKindError struct {
	kind ClaimErrorKind
}

func (e claimKindError) Error() string {
	return e.kind.String()
}

// ClaimType describes the JSON type declared for a claim.
type ClaimType int

const (
	ClaimString ClaimType = iota
	ClaimBoolean
	ClaimNumber
	ClaimInteger
	ClaimArray
	ClaimObject
)

func (k ClaimErrorKind) String() string {
	switch k {
	case ClaimTypeMismatch:
		return "type mismatch"
	case ClaimMissing:
		return "missing claim"
	case ClaimNull:
		return "null claim"
	case ClaimUnknown:
		return "unknown claim"
	case ClaimTooDeep:
		return "claim nesting too deep"
	default:
		return "invalid claim"
	}
}

// ClaimError is returned by a ClaimsContract when a claim fails validation.
//
// Path locates the offending claim using dot notation (e.g. "user.roles[0]")
// and Kind categorises the failure. Claim values are never rendered into the
// error message unless IncludeClaimValues is enabled.
type ClaimError struct {
	Path     string
	Kind     ClaimErrorKind
	Expected ClaimType
	Actual   ClaimType
	message  string
}

func (e ClaimError) Error() string {
	return e.message
}

func (e ClaimError) Is(target error) bool {
	_, ok := target.(ClaimError)
	if ok {
		return true
	}

	_, okPointer := target.(*ClaimError)

	return okPointer
}

func (e ClaimError) Unwrap() error {
	return claimKindError{e.Kind}
}

// MaxContractDepth is the maximum supported nesting depth of arrays and
// objects within a token's claims. Tokens nesting deeper than this are
// rejected without being traversed recursively.
const MaxContractDepth = 32

// ClaimSpec declares the type of a single claim. ClaimSpec values are
// produced by the String, Boolean, Number, Integer, Array, and Object
// constructors (and their required variants).
type ClaimSpec interface {
	claimSpec()
}

// ObjectField pairs a claim name with its ClaimSpec.
type ObjectField struct {
	Name string
	Spec ClaimSpec
}

type claimSpecData struct {
	typ           ClaimType
	required      bool
	arrayItems    *claimSpecData
	objectFields  []objectFieldData
	rejectUnknown bool
	rejectSet     bool
}

func (s claimSpecData) claimSpec() {}

type objectFieldData struct {
	name string
	spec *claimSpecData
}

// String declares an optional JSON string claim.
func String() ClaimSpec {
	return claimSpecData{typ: ClaimString}
}

// RequiredString declares a required JSON string claim.
func RequiredString() ClaimSpec {
	return claimSpecData{typ: ClaimString, required: true}
}

// Boolean declares an optional JSON boolean claim.
func Boolean() ClaimSpec {
	return claimSpecData{typ: ClaimBoolean}
}

// RequiredBoolean declares a required JSON boolean claim.
func RequiredBoolean() ClaimSpec {
	return claimSpecData{typ: ClaimBoolean, required: true}
}

// Number declares an optional JSON number claim. Numbers are preserved as
// their original JSON text (see json.Number) and are never coerced through
// float64, so integers beyond float64 precision are not distorted before
// they are checked.
func Number() ClaimSpec {
	return claimSpecData{typ: ClaimNumber}
}

// RequiredNumber declares a required JSON number claim. See Number for how
// precision is preserved.
func RequiredNumber() ClaimSpec {
	return claimSpecData{typ: ClaimNumber, required: true}
}

// Integer declares an optional JSON number claim that must be an integral
// value (no fractional or exponent part). The raw JSON digits are checked
// directly, so values beyond float64 precision are not distorted before
// they are checked.
func Integer() ClaimSpec {
	return claimSpecData{typ: ClaimInteger}
}

// RequiredInteger declares a required integer claim. See Integer.
func RequiredInteger() ClaimSpec {
	return claimSpecData{typ: ClaimInteger, required: true}
}

// Array declares an optional JSON array claim whose elements all conform to
// items. items must not be nil.
func Array(items ClaimSpec) ClaimSpec {
	return claimSpecData{typ: ClaimArray, arrayItems: asSpec(items)}
}

// RequiredArray declares a required JSON array claim. See Array.
func RequiredArray(items ClaimSpec) ClaimSpec {
	return claimSpecData{typ: ClaimArray, required: true, arrayItems: asSpec(items)}
}

// Object declares an optional JSON object claim with the given fields.
// Nested objects inherit the contract's unknown-field policy unless
// RejectObjectUnknownFields or AllowObjectUnknownFields overrides it.
func Object(fields []ObjectField, opts ...ObjectOption) ClaimSpec {
	return newObjectSpec(fields, false, opts)
}

// RequiredObject declares a required JSON object claim. See Object.
func RequiredObject(fields []ObjectField, opts ...ObjectOption) ClaimSpec {
	return newObjectSpec(fields, true, opts)
}

func newObjectSpec(fields []ObjectField, required bool, opts []ObjectOption) claimSpecData {
	spec := claimSpecData{typ: ClaimObject, required: required}
	for _, opt := range opts {
		opt(&spec)
	}

	spec.objectFields = make([]objectFieldData, len(fields))
	for i, field := range fields {
		spec.objectFields[i] = objectFieldData{
			name: field.Name,
			spec: asSpec(field.Spec),
		}
	}

	return spec
}

func asSpec(spec ClaimSpec) *claimSpecData {
	if spec == nil {
		return nil
	}

	s, ok := spec.(claimSpecData)
	if !ok {
		return nil
	}

	return &s
}

// ObjectOption configures a nested object claim.
type ObjectOption func(*claimSpecData)

// RejectObjectUnknownFields causes the nested object to reject any field not
// declared in its field list.
func RejectObjectUnknownFields() ObjectOption {
	return func(spec *claimSpecData) {
		spec.rejectUnknown = true
		spec.rejectSet = true
	}
}

// AllowObjectUnknownFields causes the nested object to permit fields not
// declared in its field list.
func AllowObjectUnknownFields() ObjectOption {
	return func(spec *claimSpecData) {
		spec.rejectUnknown = false
		spec.rejectSet = true
	}
}

// ContractOption configures a ClaimsContract.
type ContractOption func(*ClaimsContract)

// RejectUnknownFields causes the top-level claims object to reject any claim
// not declared in the contract.
func RejectUnknownFields() ContractOption {
	return func(contract *ClaimsContract) {
		contract.rejectUnknown = true
	}
}

// IncludeClaimValues renders offending claim values in error messages. This
// is disabled by default, because claim values may contain sensitive data.
func IncludeClaimValues() ContractOption {
	return func(contract *ClaimsContract) {
		contract.includeValues = true
	}
}

// ClaimsContract is a reusable declaration of the claims a token is permitted
// to contain, which are required, and the JSON type of each.
//
// A contract is intended to run as a parser Rule, which only executes after a
// token has passed cryptographic verification. It does not replace rules such
// as IssuedBy, ForAudience, NotExpired, or ValidAt.
//
// A ClaimsContract is safe for concurrent use once constructed.
type ClaimsContract struct {
	root          claimSpecData
	rejectUnknown bool
	includeValues bool
}

// NewClaimsContract constructs a contract over the given top-level fields and
// returns an error describing any contradictory or invalid definition (for
// example a duplicate field, an empty field name, or a nil array element
// specification).
func NewClaimsContract(fields []ObjectField, opts ...ContractOption) (*ClaimsContract, error) {
	contract := &ClaimsContract{}
	for _, opt := range opts {
		opt(contract)
	}

	root := newObjectSpec(fields, true, nil)
	root.rejectUnknown = contract.rejectUnknown
	root.rejectSet = true

	if err := validateSpec(root, ""); err != nil {
		return nil, err
	}

	contract.root = root

	return contract, nil
}

// MustClaimsContract is like NewClaimsContract but panics if the definitions
// are contradictory. It is intended for use with package-level variable
// declarations.
func MustClaimsContract(fields []ObjectField, opts ...ContractOption) *ClaimsContract {
	contract, err := NewClaimsContract(fields, opts...)
	if err != nil {
		panic(err)
	}

	return contract
}

func validateSpec(spec claimSpecData, path string) error {
	switch spec.typ {
	case ClaimArray:
		if spec.arrayItems == nil {
			return fmt.Errorf("claims contract: array at `%s' has no element specification", path)
		}

		return validateSpec(*spec.arrayItems, path+"[]")
	case ClaimObject:
		seen := make(map[string]bool, len(spec.objectFields))
		for _, field := range spec.objectFields {
			if field.name == "" {
				return fmt.Errorf("claims contract: object at `%s' has a field with an empty name", path)
			}

			if seen[field.name] {
				return fmt.Errorf("claims contract: field `%s' is declared more than once", joinPath(path, field.name))
			}
			seen[field.name] = true

			if field.spec == nil {
				return fmt.Errorf("claims contract: field `%s' has no type specification", joinPath(path, field.name))
			}

			if err := validateSpec(*field.spec, joinPath(path, field.name)); err != nil {
				return err
			}
		}
	}

	return nil
}

// Rule returns a Rule which validates token claims against the contract. The
// rule only runs after cryptographic verification has succeeded.
func (contract *ClaimsContract) Rule() Rule {
	return func(token Token) error {
		return contract.Validate(token)
	}
}

// Validate checks the token's claims against the contract, returning a
// ClaimError describing the first failure, or nil if all claims conform.
func (contract *ClaimsContract) Validate(token Token) error {
	return contract.validateObject(&contract.root, token.claims, "", 1)
}

// Decode validates the token's claims against the contract and, on success,
// unmarshals them into output. JSON numbers are decoded using json.Number
// rather than float64, so integer claims beyond float64 precision are not
// distorted.
func (contract *ClaimsContract) Decode(token Token, output interface{}) error {
	if err := contract.Validate(token); err != nil {
		return err
	}

	decoder := json.NewDecoder(bytes.NewReader(token.ClaimsJSON()))
	decoder.UseNumber()

	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("could not decode validated claims: %w", err)
	}

	return nil
}

func (contract *ClaimsContract) validateObject(spec *claimSpecData, claims map[string]json.RawMessage, path string, depth int) error {
	if depth > MaxContractDepth {
		return contract.newError(path, ClaimTooDeep, 0, 0, nil)
	}

	for i := range spec.objectFields {
		field := &spec.objectFields[i]
		fieldPath := joinPath(path, field.name)

		raw, present := claims[field.name]
		if !present {
			if field.spec.required {
				return contract.newError(fieldPath, ClaimMissing, 0, 0, nil)
			}

			continue
		}

		if isNull(raw) {
			if field.spec.required {
				return contract.newError(fieldPath, ClaimNull, 0, 0, nil)
			}

			continue
		}

		fieldDepth := depth
		if isContainerType(field.spec.typ) {
			fieldDepth = depth + 1
		}

		if err := contract.validateValue(field.spec, raw, fieldPath, fieldDepth); err != nil {
			return err
		}
	}

	if spec.rejectUnknown {
		declared := make(map[string]bool, len(spec.objectFields))
		for _, field := range spec.objectFields {
			declared[field.name] = true
		}

		unknown := make([]string, 0)
		for key := range claims {
			if !declared[key] {
				unknown = append(unknown, key)
			}
		}
		sort.Strings(unknown)

		for _, key := range unknown {
			return contract.newError(joinPath(path, key), ClaimUnknown, 0, 0, claims[key])
		}
	}

	return nil
}

func (contract *ClaimsContract) validateArray(spec *claimSpecData, raw json.RawMessage, path string, depth int) error {
	if depth > MaxContractDepth {
		return contract.newError(path, ClaimTooDeep, 0, 0, nil)
	}

	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return contract.newError(path, ClaimTypeMismatch, spec.typ, actualType(raw), raw)
	}

	for i, element := range elements {
		elementPath := fmt.Sprintf("%s[%d]", path, i)

		if isNull(element) {
			return contract.newError(elementPath, ClaimNull, spec.arrayItems.typ, 0, element)
		}

		elementDepth := depth
		if isContainerType(spec.arrayItems.typ) {
			elementDepth = depth + 1
		}

		if err := contract.validateValue(spec.arrayItems, element, elementPath, elementDepth); err != nil {
			return err
		}
	}

	return nil
}

func (contract *ClaimsContract) validateValue(spec *claimSpecData, raw json.RawMessage, path string, childDepth int) error {
	switch spec.typ {
	case ClaimString, ClaimBoolean:
		if actual := actualType(raw); actual != spec.typ {
			return contract.newError(path, ClaimTypeMismatch, spec.typ, actual, raw)
		}
	case ClaimNumber:
		if actual := actualType(raw); actual != ClaimNumber {
			return contract.newError(path, ClaimTypeMismatch, spec.typ, actual, raw)
		}

		if !validJSONNumber(text(raw)) {
			return contract.newError(path, ClaimTypeMismatch, spec.typ, ClaimNumber, raw)
		}
	case ClaimInteger:
		if actual := actualType(raw); actual != ClaimNumber {
			return contract.newError(path, ClaimTypeMismatch, spec.typ, actual, raw)
		}

		if !validJSONInteger(text(raw)) {
			return contract.newError(path, ClaimTypeMismatch, spec.typ, ClaimNumber, raw)
		}
	case ClaimArray:
		if actualType(raw) != ClaimArray {
			return contract.newError(path, ClaimTypeMismatch, spec.typ, actualType(raw), raw)
		}

		return contract.validateArray(spec, raw, path, childDepth)
	case ClaimObject:
		if actualType(raw) != ClaimObject {
			return contract.newError(path, ClaimTypeMismatch, spec.typ, actualType(raw), raw)
		}

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return contract.newError(path, ClaimTypeMismatch, spec.typ, actualType(raw), raw)
		}

		nested := *spec
		if !nested.rejectSet {
			nested.rejectUnknown = contract.rejectUnknown
		}

		return contract.validateObject(&nested, fields, path, childDepth)
	}

	return nil
}

func (contract *ClaimsContract) newError(path string, kind ClaimErrorKind, expected, actual ClaimType, raw json.RawMessage) error {
	message := contract.errorMessage(path, kind, expected, actual, raw)

	return ClaimError{
		Path:     path,
		Kind:     kind,
		Expected: expected,
		Actual:   actual,
		message:  message,
	}
}

func (contract *ClaimsContract) errorMessage(path string, kind ClaimErrorKind, expected, actual ClaimType, raw json.RawMessage) string {
	location := "claims"
	if path != "" {
		location = "claim `" + path + "'"
	}

	var message string

	switch kind {
	case ClaimTypeMismatch:
		message = fmt.Sprintf("%s has the wrong type: expected %s, got %s", location, typeName(expected), typeName(actual))
	case ClaimMissing:
		message = fmt.Sprintf("required %s is missing", location)
	case ClaimNull:
		if expected >= ClaimString && expected <= ClaimObject {
			message = fmt.Sprintf("required %s was null, but a non-null %s is required", location, typeName(expected))
		} else {
			message = fmt.Sprintf("required %s was null", location)
		}
	case ClaimUnknown:
		message = fmt.Sprintf("%s is not allowed by the claims contract", location)
	case ClaimTooDeep:
		message = fmt.Sprintf("%s nests deeper than the maximum depth of %d", location, MaxContractDepth)
	default:
		message = fmt.Sprintf("%s failed claims contract validation", location)
	}

	if contract.includeValues && raw != nil && !isNull(raw) {
		message += " (value: " + snippet(raw) + ")"
	}

	return message
}

func snippet(raw json.RawMessage) string {
	const maxLen = 64

	value := string(bytes.TrimSpace(raw))
	if len(value) > maxLen {
		value = value[:maxLen] + "..."
	}

	return strconv.Quote(value)
}

func joinPath(base, segment string) string {
	if base == "" {
		return segment
	}

	return base + "." + segment
}

func isContainerType(typ ClaimType) bool {
	return typ == ClaimArray || typ == ClaimObject
}

func text(raw json.RawMessage) string {
	return string(bytes.TrimSpace(raw))
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func actualType(raw json.RawMessage) ClaimType {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return -1
	}

	switch trimmed[0] {
	case '"':
		return ClaimString
	case 't', 'f':
		return ClaimBoolean
	case '[':
		return ClaimArray
	case '{':
		return ClaimObject
	case 'n':
		return -1
	default:
		return ClaimNumber
	}
}

func typeName(typ ClaimType) string {
	switch typ {
	case ClaimString:
		return "string"
	case ClaimBoolean:
		return "boolean"
	case ClaimNumber:
		return "number"
	case ClaimInteger:
		return "integer"
	case ClaimArray:
		return "array"
	case ClaimObject:
		return "object"
	default:
		return "unknown"
	}
}

var (
	jsonNumberPattern  = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
	jsonIntegerPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)
)

func validJSONNumber(value string) bool {
	return jsonNumberPattern.MatchString(value)
}

func validJSONInteger(value string) bool {
	return jsonIntegerPattern.MatchString(value)
}
