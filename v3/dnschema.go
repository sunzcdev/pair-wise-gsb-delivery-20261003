package ldap

import (
	"errors"
	"fmt"
	"strings"
)

// ErrIndeterminateEquality is returned (wrapped) by the schema-aware DN
// comparison functions when equality cannot be determined, for example when
// an attribute type is unknown to the schema or its equality matching rule
// is not supported. It is never silently assumed that two values are equal
// in these cases.
var ErrIndeterminateEquality = errors.New("ldap: cannot determine DN equality")

// AttributeTypeSchema describes the properties of an attribute type that are
// needed for schema-aware distinguished name comparison.
type AttributeTypeSchema struct {
	// OID is the numeric OID of the attribute type.
	OID string
	// Names are the registered names of the attribute type (aliases).
	Names []string
	// EqualityMatchingRule is the name or numeric OID of the equality
	// matching rule defined for the attribute type.
	EqualityMatchingRule string
}

// Schema resolves attribute types for schema-aware DN comparison.
// Implementations may be backed by a directory's subschema subentry or be
// supplied directly by the caller.
type Schema interface {
	// AttributeType returns the schema of the attribute type identified by
	// any of its names or its numeric OID. ok is false when the attribute
	// type is unknown to the schema.
	AttributeType(nameOrOID string) (at AttributeTypeSchema, ok bool)
}

// NewSchema returns a Schema backed by the given attribute type definitions,
// as could be parsed from a subschema subentry. Attribute types are
// registered under their OID and under each of their names, so lookups by
// any alias resolve to the same definition.
func NewSchema(attributeTypes ...AttributeTypeSchema) Schema {
	schema := make(mapSchema, len(attributeTypes))
	for _, at := range attributeTypes {
		if at.OID != "" {
			schema[foldString(at.OID)] = at
		}
		for _, name := range at.Names {
			schema[foldString(name)] = at
		}
	}
	return schema
}

type mapSchema map[string]AttributeTypeSchema

func (s mapSchema) AttributeType(nameOrOID string) (AttributeTypeSchema, bool) {
	at, ok := s[foldString(nameOrOID)]
	return at, ok
}

// equalityMatchingRuleNormalizers maps equality matching rule names and
// numeric OIDs (folded) to the function normalizing an attribute value
// according to that rule. Two values are equal under the rule if and only
// if their normalized forms are identical.
var equalityMatchingRuleNormalizers = map[string]func(value string, schema Schema) (string, error){}

func registerEqualityMatchingRule(normalizer func(value string, schema Schema) (string, error), name string, oid string) {
	equalityMatchingRuleNormalizers[foldString(name)] = normalizer
	equalityMatchingRuleNormalizers[foldString(oid)] = normalizer
}

func init() {
	identity := func(value string, _ Schema) (string, error) { return value, nil }
	registerEqualityMatchingRule(identity, "caseExactMatch", "2.5.13.5")
	registerEqualityMatchingRule(identity, "caseExactIA5Match", "1.3.6.1.4.1.1466.109.114.1")
	registerEqualityMatchingRule(identity, "objectIdentifierMatch", "2.5.13.0")
	registerEqualityMatchingRule(identity, "objectIdentifierFirstComponentMatch", "2.5.13.30")

	caseIgnore := func(value string, _ Schema) (string, error) { return normalizeCaseIgnore(value), nil }
	registerEqualityMatchingRule(caseIgnore, "caseIgnoreMatch", "2.5.13.2")
	registerEqualityMatchingRule(caseIgnore, "caseIgnoreIA5Match", "1.3.6.1.4.1.1466.109.114.2")
	registerEqualityMatchingRule(caseIgnore, "caseIgnoreListMatch", "2.5.13.4")

	registerEqualityMatchingRule(func(value string, _ Schema) (string, error) {
		return strings.ReplaceAll(value, " ", ""), nil
	}, "numericStringMatch", "2.5.13.8")

	registerEqualityMatchingRule(func(value string, _ Schema) (string, error) {
		value = strings.ReplaceAll(value, " ", "")
		return strings.ReplaceAll(value, "-", ""), nil
	}, "telephoneNumberMatch", "2.5.13.20")

	registerEqualityMatchingRule(normalizeDNValue, "distinguishedNameMatch", "2.5.13.1")
}

// normalizeCaseIgnore approximates the case-ignore preparation of RFC 4518:
// insignificant spaces are removed and the result is case folded.
func normalizeCaseIgnore(value string) string {
	return foldString(strings.Join(strings.Fields(value), " "))
}

// normalizeDNValue normalizes a value asserted under distinguishedNameMatch
// by parsing it as a DN and normalizing it with the same schema.
func normalizeDNValue(value string, schema Schema) (string, error) {
	dn, err := ParseDN(value)
	if err != nil {
		return "", fmt.Errorf("%w: invalid distinguished name value %q: %v", ErrIndeterminateEquality, value, err)
	}
	normalized, err := dn.Normalize(schema)
	if err != nil {
		return "", err
	}
	return normalized.String(), nil
}

func normalizeAttributeValue(rule string, value string, schema Schema) (string, error) {
	normalizer, ok := equalityMatchingRuleNormalizers[foldString(rule)]
	if !ok {
		return "", fmt.Errorf("%w: unknown equality matching rule %q", ErrIndeterminateEquality, rule)
	}
	return normalizer(value, schema)
}

// EqualWithSchema returns true if the DNs are equal when each attribute value
// is compared using the equality matching rule of its attribute type, as
// resolved through schema. Attribute types may be written as any registered
// name or as their numeric OID.
//
// Unlike EqualFold it does not assume every value is case-insensitive, and
// unlike Equal it does not assume every value is case-sensitive. When
// equality cannot be determined - because an attribute type is unknown to
// the schema, has no equality matching rule, or uses a matching rule that is
// not supported - it returns false together with an error wrapping
// ErrIndeterminateEquality instead of guessing.
func (d *DN) EqualWithSchema(other *DN, schema Schema) (bool, error) {
	if len(d.RDNs) != len(other.RDNs) {
		return false, nil
	}
	for i := range d.RDNs {
		equal, err := d.RDNs[i].equalWithSchema(other.RDNs[i], schema)
		if err != nil || !equal {
			return equal, err
		}
	}
	return true, nil
}

func (r *RelativeDN) equalWithSchema(other *RelativeDN, schema Schema) (bool, error) {
	if len(r.Attributes) != len(other.Attributes) {
		return false, nil
	}
	normalized, err := r.normalize(schema)
	if err != nil {
		return false, err
	}
	normalizedOther, err := other.normalize(schema)
	if err != nil {
		return false, err
	}
	// The normalized attribute types are canonical and the values are
	// normalized per their equality matching rule, so the plain set
	// containment test applies.
	return normalized.hasAllAttributes(normalizedOther.Attributes) && normalizedOther.hasAllAttributes(normalized.Attributes), nil
}

// Normalize returns a copy of d in which every attribute type is replaced by
// its canonical form (the numeric OID when the schema provides one) and every
// attribute value is normalized according to the equality matching rule of
// its attribute type. The receiver is not modified.
//
// It returns an error wrapping ErrIndeterminateEquality when an attribute
// type or its equality matching rule cannot be resolved.
func (d *DN) Normalize(schema Schema) (*DN, error) {
	normalized := &DN{RDNs: make([]*RelativeDN, len(d.RDNs))}
	for i, rdn := range d.RDNs {
		normalizedRDN, err := rdn.normalize(schema)
		if err != nil {
			return nil, err
		}
		normalized.RDNs[i] = normalizedRDN
	}
	return normalized, nil
}

func (r *RelativeDN) normalize(schema Schema) (*RelativeDN, error) {
	normalized := &RelativeDN{Attributes: make([]*AttributeTypeAndValue, len(r.Attributes))}
	for i, attr := range r.Attributes {
		normalizedAttr, err := attr.normalize(schema)
		if err != nil {
			return nil, err
		}
		normalized.Attributes[i] = normalizedAttr
	}
	return normalized, nil
}

func (a *AttributeTypeAndValue) normalize(schema Schema) (*AttributeTypeAndValue, error) {
	at, ok := schema.AttributeType(a.Type)
	if !ok {
		return nil, fmt.Errorf("%w: unknown attribute type %q", ErrIndeterminateEquality, a.Type)
	}
	if at.EqualityMatchingRule == "" {
		return nil, fmt.Errorf("%w: attribute type %q has no equality matching rule", ErrIndeterminateEquality, a.Type)
	}
	value, err := normalizeAttributeValue(at.EqualityMatchingRule, a.Value, schema)
	if err != nil {
		return nil, err
	}
	canonicalType := at.OID
	if canonicalType == "" && len(at.Names) > 0 {
		canonicalType = at.Names[0]
	}
	if canonicalType == "" {
		canonicalType = a.Type
	}
	return &AttributeTypeAndValue{Type: foldString(canonicalType), Value: value}, nil
}
