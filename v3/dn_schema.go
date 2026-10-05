package ldap

import (
	"fmt"
	"strings"
)

// AttributeType describes the schema properties of an attribute type that are
// relevant for DN comparison.
type AttributeType struct {
	// OID is the canonical numeric OID of the attribute type.
	OID string
	// EqualityMatchingRule is the name or OID of the equality matching rule
	// defined for the attribute type. It is empty when the schema does not
	// define one (directly or through a superior attribute type).
	EqualityMatchingRule string
}

// SchemaResolver resolves attribute types, given as a name or an OID, to
// their schema definition. A resolver can be built from a directory
// subschema (see NewSubschema) or injected by the caller (see
// SchemaResolverFunc).
type SchemaResolver interface {
	// AttributeType returns the schema definition for the given attribute
	// type name or OID, and whether the attribute type is known.
	AttributeType(nameOrOID string) (AttributeType, bool)
}

// SchemaResolverFunc adapts a function to the SchemaResolver interface,
// allowing callers to inject their own schema knowledge.
type SchemaResolverFunc func(nameOrOID string) (AttributeType, bool)

// AttributeType implements SchemaResolver.
func (f SchemaResolverFunc) AttributeType(nameOrOID string) (AttributeType, bool) {
	return f(nameOrOID)
}

// Subschema is a SchemaResolver backed by attribute type definitions as
// found in the "attributeTypes" attribute of a subschema subentry.
type Subschema struct {
	// attributeTypes maps each attribute type OID and each of its names
	// (lowercased) to the parsed definition.
	attributeTypes map[string]subschemaAttributeType
}

type subschemaAttributeType struct {
	oid      string
	superior string
	equality string
}

// NewSubschema parses RFC 4512 AttributeTypeDescription values (for example
// the values of the "attributeTypes" attribute of a subschema subentry)
// into a SchemaResolver. Attribute types without an EQUALITY rule inherit
// the equality matching rule of their SUP(erior) attribute type.
func NewSubschema(attributeTypeDescriptions ...string) (*Subschema, error) {
	s := &Subschema{attributeTypes: make(map[string]subschemaAttributeType)}
	for _, description := range attributeTypeDescriptions {
		oid, names, superior, equality, err := parseAttributeTypeDescription(description)
		if err != nil {
			return nil, err
		}
		definition := subschemaAttributeType{oid: oid, superior: superior, equality: equality}
		s.attributeTypes[strings.ToLower(oid)] = definition
		for _, name := range names {
			s.attributeTypes[strings.ToLower(name)] = definition
		}
	}
	return s, nil
}

// AttributeType implements SchemaResolver.
func (s *Subschema) AttributeType(nameOrOID string) (AttributeType, bool) {
	definition, ok := s.attributeTypes[strings.ToLower(nameOrOID)]
	if !ok {
		return AttributeType{}, false
	}
	return AttributeType{
		OID:                  definition.oid,
		EqualityMatchingRule: s.resolveEquality(definition.oid, make(map[string]bool)),
	}, true
}

// resolveEquality walks the SUP chain of the attribute type identified by
// oid until an equality matching rule is found.
func (s *Subschema) resolveEquality(oid string, seen map[string]bool) string {
	if seen[oid] {
		return ""
	}
	seen[oid] = true
	definition, ok := s.attributeTypes[strings.ToLower(oid)]
	if !ok {
		return ""
	}
	if definition.equality != "" {
		return definition.equality
	}
	if definition.superior != "" {
		return s.resolveEquality(definition.superior, seen)
	}
	return ""
}

// parseAttributeTypeDescription parses an RFC 4512 AttributeTypeDescription
// such as:
//
//	( 2.5.4.3 NAME ( 'cn' 'commonName' ) SUP name )
func parseAttributeTypeDescription(description string) (oid string, names []string, superior string, equality string, err error) {
	tokens, err := tokenizeSchemaDescription(description)
	if err != nil {
		return "", nil, "", "", fmt.Errorf("failed to parse attribute type description %q: %w", description, err)
	}
	if len(tokens) < 3 || tokens[0] != "(" || tokens[len(tokens)-1] != ")" {
		return "", nil, "", "", fmt.Errorf("failed to parse attribute type description %q: expected parenthesized description", description)
	}

	oid = tokens[1]
	for i := 2; i < len(tokens)-1; i++ {
		switch strings.ToUpper(tokens[i]) {
		case "NAME":
			i++
			if i < len(tokens)-1 && tokens[i] == "(" {
				for i++; i < len(tokens)-1 && tokens[i] != ")"; i++ {
					names = append(names, tokens[i])
				}
			} else if i < len(tokens)-1 {
				names = append(names, tokens[i])
			}
		case "SUP":
			if i+1 < len(tokens)-1 {
				superior = tokens[i+1]
				i++
			}
		case "EQUALITY":
			if i+1 < len(tokens)-1 {
				equality = tokens[i+1]
				i++
			}
		case "DESC", "ORDERING", "SUBSTR", "SYNTAX", "USAGE":
			// Keywords followed by a single value that is not relevant
			// for DN comparison.
			i++
		default:
			// Flags (OBSOLETE, SINGLE-VALUE, COLLECTIVE,
			// NO-USER-MODIFICATION) and extensions carry no value or
			// values we do not need; skip them.
		}
	}
	return oid, names, superior, equality, nil
}

// tokenizeSchemaDescription splits a schema description into parentheses,
// quoted strings and bare tokens.
func tokenizeSchemaDescription(description string) ([]string, error) {
	var tokens []string
	for i := 0; i < len(description); {
		switch char := description[i]; {
		case char == ' ' || char == '\t' || char == '\n' || char == '\r':
			i++
		case char == '(' || char == ')':
			tokens = append(tokens, string(char))
			i++
		case char == '\'':
			end := i + 1
			for end < len(description) && description[end] != '\'' {
				end++
			}
			if end >= len(description) {
				return nil, fmt.Errorf("unterminated quoted string")
			}
			tokens = append(tokens, description[i+1:end])
			i = end + 1
		default:
			end := i
			for end < len(description) && !strings.ContainsRune(" \t\n\r()'", rune(description[end])) {
				end++
			}
			tokens = append(tokens, description[i:end])
			i = end
		}
	}
	return tokens, nil
}

// matchingRuleNormalizers maps equality matching rule names (lowercased) and
// their OIDs to the normalization applied before comparison. Rules not
// listed here are unknown to this package: comparisons depending on them
// fail with an error instead of guessing.
var matchingRuleNormalizers = map[string]func(string) string{
	// caseIgnoreMatch
	"caseignorematch": normalizeCaseIgnore,
	"2.5.13.2":        normalizeCaseIgnore,
	// caseExactMatch
	"caseexactmatch": normalizeCaseExact,
	"2.5.13.5":       normalizeCaseExact,
	// objectIdentifierMatch
	"objectidentifiermatch": normalizeObjectIdentifier,
	"2.5.13.0":              normalizeObjectIdentifier,
	// numericStringMatch
	"numericstringmatch": normalizeNumericString,
	"2.5.13.8":           normalizeNumericString,
	// telephoneNumberMatch
	"telephonenumbermatch": normalizeTelephoneNumber,
	"2.5.13.20":            normalizeTelephoneNumber,
	// octetStringMatch
	"octetstringmatch": normalizeOctetString,
	"2.5.13.17":        normalizeOctetString,
}

// normalizeCaseIgnore applies the insignificant-space handling of RFC 4518
// and folds case, as required by caseIgnoreMatch.
func normalizeCaseIgnore(value string) string {
	return foldString(stripInsignificantSpaces(value))
}

// normalizeCaseExact applies the insignificant-space handling of RFC 4518
// while preserving case, as required by caseExactMatch.
func normalizeCaseExact(value string) string {
	return stripInsignificantSpaces(value)
}

// stripInsignificantSpaces removes leading and trailing spaces and collapses
// runs of interior spaces into a single space (RFC 4518 section 2.6.1,
// simplified to the space character).
func stripInsignificantSpaces(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func normalizeObjectIdentifier(value string) string {
	return strings.TrimSpace(value)
}

func normalizeNumericString(value string) string {
	return strings.ReplaceAll(value, " ", "")
}

func normalizeTelephoneNumber(value string) string {
	value = strings.ReplaceAll(value, " ", "")
	value = strings.ReplaceAll(value, "-", "")
	return foldString(value)
}

func normalizeOctetString(value string) string {
	return value
}

// Normalize returns a copy of the DN in which every attribute type is
// replaced by its canonical OID and every value is normalized according to
// the equality matching rule of its attribute type, as resolved through the
// given schema. The receiver is not modified.
//
// An error is returned when equality cannot be determined, for example when
// an attribute type is unknown to the schema or its equality matching rule
// is not supported.
func (d *DN) Normalize(schema SchemaResolver) (*DN, error) {
	normalized := &DN{RDNs: make([]*RelativeDN, len(d.RDNs))}
	for i, rdn := range d.RDNs {
		normalizedRDN := &RelativeDN{Attributes: make([]*AttributeTypeAndValue, len(rdn.Attributes))}
		for j, attribute := range rdn.Attributes {
			normalizedAttribute, err := attribute.normalize(schema)
			if err != nil {
				return nil, err
			}
			normalizedRDN.Attributes[j] = normalizedAttribute
		}
		normalized.RDNs[i] = normalizedRDN
	}
	return normalized, nil
}

func (a *AttributeTypeAndValue) normalize(schema SchemaResolver) (*AttributeTypeAndValue, error) {
	attributeType, ok := schema.AttributeType(a.Type)
	if !ok {
		return nil, fmt.Errorf("cannot determine equality: attribute type %q is not known to the schema", a.Type)
	}
	if attributeType.EqualityMatchingRule == "" {
		return nil, fmt.Errorf("cannot determine equality: attribute type %q has no equality matching rule in the schema", a.Type)
	}
	normalizer, ok := matchingRuleNormalizers[strings.ToLower(attributeType.EqualityMatchingRule)]
	if !ok {
		return nil, fmt.Errorf("cannot determine equality: equality matching rule %q of attribute type %q is not supported", attributeType.EqualityMatchingRule, a.Type)
	}
	return &AttributeTypeAndValue{Type: attributeType.OID, Value: normalizer(a.Value)}, nil
}

// EqualWithSchema reports whether the DN is equal to the other DN under the
// equality matching rules of the given schema: both DNs are normalized (see
// Normalize) and the normalized forms are compared. Neither DN is modified.
//
// Unlike Equal and EqualFold, which apply a fixed case policy to every
// value, EqualWithSchema honors the directory's own matching rules (for
// example caseExactMatch versus caseIgnoreMatch). An error is returned when
// equality cannot be determined, for example when an attribute type or its
// equality matching rule is unknown; in that case no fallback comparison is
// attempted.
func (d *DN) EqualWithSchema(other *DN, schema SchemaResolver) (bool, error) {
	normalized, err := d.Normalize(schema)
	if err != nil {
		return false, err
	}
	normalizedOther, err := other.Normalize(schema)
	if err != nil {
		return false, err
	}
	return normalized.Equal(normalizedOther), nil
}
