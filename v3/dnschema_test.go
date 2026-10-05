package ldap

import (
	"errors"
	"strings"
	"testing"
)

var testDNSchema = NewSchema(
	AttributeTypeSchema{OID: "2.5.4.3", Names: []string{"cn", "commonName"}, EqualityMatchingRule: "caseIgnoreMatch"},
	AttributeTypeSchema{OID: "2.5.4.11", Names: []string{"ou", "organizationalUnitName"}, EqualityMatchingRule: "caseIgnoreMatch"},
	AttributeTypeSchema{OID: "0.9.2342.19200300.100.1.25", Names: []string{"dc", "domainComponent"}, EqualityMatchingRule: "caseIgnoreIA5Match"},
	AttributeTypeSchema{OID: "2.5.4.46", Names: []string{"dnQualifier"}, EqualityMatchingRule: "caseExactMatch"},
	AttributeTypeSchema{OID: "2.5.4.49", Names: []string{"distinguishedName"}, EqualityMatchingRule: "distinguishedNameMatch"},
	AttributeTypeSchema{OID: "1.3.6.1.4.1.99999.1", Names: []string{"x-unknownRule"}, EqualityMatchingRule: "madeUpMatch"},
	AttributeTypeSchema{OID: "1.3.6.1.4.1.99999.2", Names: []string{"x-noRule"}},
)

func mustParseDN(t *testing.T, str string) *DN {
	t.Helper()
	dn, err := ParseDN(str)
	if err != nil {
		t.Fatalf("failed to parse DN %q: %v", str, err)
	}
	return dn
}

func assertEqualWithSchema(t *testing.T, a, b string, want bool) {
	t.Helper()
	equal, err := mustParseDN(t, a).EqualWithSchema(mustParseDN(t, b), testDNSchema)
	if err != nil {
		t.Fatalf("EqualWithSchema(%q, %q) returned unexpected error: %v", a, b, err)
	}
	if equal != want {
		t.Errorf("EqualWithSchema(%q, %q) = %v, want %v", a, b, equal, want)
	}
}

func TestDNEqualWithSchemaCaseExact(t *testing.T) {
	assertEqualWithSchema(t, "dnQualifier=ABC-123,dc=com", "dnQualifier=ABC-123,dc=com", true)
	assertEqualWithSchema(t, "dnQualifier=ABC-123,dc=com", "dnQualifier=abc-123,dc=com", false)
}

func TestDNEqualWithSchemaCaseIgnore(t *testing.T) {
	assertEqualWithSchema(t, "cn=Smith,dc=example,dc=com", "CN=smith,DC=Example,DC=COM", true)
	assertEqualWithSchema(t, "cn=Smith,dc=example,dc=com", "cn=Jones,dc=example,dc=com", false)
	// Insignificant spaces are not part of the asserted value.
	assertEqualWithSchema(t, "cn=Van  Gogh", "cn=van gogh", true)
	assertEqualWithSchema(t, "cn= Smith ", "cn=smith", true)
}

func TestDNEqualWithSchemaOIDAlias(t *testing.T) {
	// Attribute types given as numeric OID, short name or long name all
	// resolve to the same attribute type.
	assertEqualWithSchema(t, "2.5.4.3=Smith,dc=example,dc=com", "commonName=Smith,DC=EXAMPLE,DC=COM", true)
	assertEqualWithSchema(t, "0.9.2342.19200300.100.1.25=com", "dc=com", true)
}

func TestDNEqualWithSchemaMultiValuedRDN(t *testing.T) {
	assertEqualWithSchema(t, "cn=foo+ou=bar,dc=com", "OU=bar+CN=FOO,dc=com", true)
	assertEqualWithSchema(t, "cn=foo+ou=bar,dc=com", "cn=foo+ou=baz,dc=com", false)
	assertEqualWithSchema(t, "cn=foo+ou=bar,dc=com", "cn=foo,dc=com", false)
}

func TestDNEqualWithSchemaDistinguishedNameMatch(t *testing.T) {
	assertEqualWithSchema(t,
		"distinguishedName=cn=Smith,dc=com",
		"distinguishedName=CN=smith,DC=COM",
		true)
	assertEqualWithSchema(t,
		"distinguishedName=cn=Smith,dc=com",
		"distinguishedName=cn=Jones,dc=com",
		false)
}

func TestDNEqualWithSchemaIndeterminate(t *testing.T) {
	for _, test := range []struct {
		name string
		a    string
		b    string
		want string
	}{
		{"unknown matching rule", "x-unknownRule=Foo", "x-unknownRule=foo", `unknown equality matching rule "madeUpMatch"`},
		{"unknown attribute type", "nosuchattr=a", "nosuchattr=a", `unknown attribute type "nosuchattr"`},
		{"no equality matching rule", "x-noRule=a", "x-noRule=a", `has no equality matching rule`},
	} {
		t.Run(test.name, func(t *testing.T) {
			equal, err := mustParseDN(t, test.a).EqualWithSchema(mustParseDN(t, test.b), testDNSchema)
			if err == nil {
				t.Fatalf("EqualWithSchema(%q, %q) = %v, expected an error", test.a, test.b, equal)
			}
			if !errors.Is(err, ErrIndeterminateEquality) {
				t.Errorf("error %v does not wrap ErrIndeterminateEquality", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error %q does not explain %q", err.Error(), test.want)
			}
			if equal {
				t.Errorf("EqualWithSchema(%q, %q) claimed equality despite indeterminate result", test.a, test.b)
			}
		})
	}
}

func TestDNNormalize(t *testing.T) {
	dn := mustParseDN(t, "CN=Smith+OU=Sales,dc=Example,dc=COM")
	original := dn.String()

	normalized, err := dn.Normalize(testDNSchema)
	if err != nil {
		t.Fatalf("Normalize returned unexpected error: %v", err)
	}

	want := "2.5.4.11=sales+2.5.4.3=smith,0.9.2342.19200300.100.1.25=example,0.9.2342.19200300.100.1.25=com"
	if normalized.String() != want {
		t.Errorf("Normalize().String() = %q, want %q", normalized.String(), want)
	}

	// The original DN must not be rewritten by normalization.
	if dn.String() != original {
		t.Errorf("Normalize modified the receiver: got %q, want %q", dn.String(), original)
	}
}

func TestDNNormalizeIndeterminate(t *testing.T) {
	if _, err := mustParseDN(t, "nosuchattr=a").Normalize(testDNSchema); !errors.Is(err, ErrIndeterminateEquality) {
		t.Errorf("Normalize with unknown attribute type returned %v, want ErrIndeterminateEquality", err)
	}
}

// injectedSchema demonstrates that callers can supply their own Schema
// implementation instead of one built from subschema definitions.
type injectedSchema map[string]AttributeTypeSchema

func (s injectedSchema) AttributeType(nameOrOID string) (AttributeTypeSchema, bool) {
	at, ok := s[foldString(nameOrOID)]
	return at, ok
}

func TestDNEqualWithInjectedSchema(t *testing.T) {
	schema := injectedSchema{
		"uid": {OID: "0.9.2342.19200300.100.1.1", Names: []string{"uid"}, EqualityMatchingRule: "caseExactMatch"},
	}
	equal, err := mustParseDN(t, "uid=Alice").EqualWithSchema(mustParseDN(t, "uid=alice"), schema)
	if err != nil {
		t.Fatalf("EqualWithSchema returned unexpected error: %v", err)
	}
	if equal {
		t.Error("EqualWithSchema(uid=Alice, uid=alice) = true under caseExactMatch, want false")
	}
}
