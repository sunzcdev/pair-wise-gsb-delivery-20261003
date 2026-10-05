package ldap

import (
	"strings"
	"testing"
)

// testSchema returns a subschema covering the matching rules exercised by
// the schema-aware DN comparison tests.
func testSchema(t *testing.T) *Subschema {
	t.Helper()
	schema, err := NewSubschema(
		"( 2.5.4.3 NAME ( 'cn' 'commonName' ) SUP name )",
		"( 2.5.4.4 NAME 'sn' SUP name )",
		"( 2.5.4.41 NAME 'name' EQUALITY caseIgnoreMatch )",
		"( 2.5.4.10 NAME 'o' EQUALITY caseIgnoreMatch )",
		"( 1.3.6.1.4.1.999.1 NAME 'exactLabel' EQUALITY caseExactMatch )",
		"( 1.3.6.1.4.1.999.2 NAME 'mysteryAttr' EQUALITY madeUpMatch )",
		"( 1.3.6.1.4.1.999.3 NAME 'rulelessAttr' )",
	)
	if err != nil {
		t.Fatalf("failed to build test subschema: %v", err)
	}
	return schema
}

func TestEqualWithSchemaCaseIgnore(t *testing.T) {
	schema := testSchema(t)

	a := mustParseDN(t, "cn=John Doe,o=Example")
	b := mustParseDN(t, "CN=john  doe,O=example")

	equal, err := a.EqualWithSchema(b, schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equal {
		t.Error("expected DNs to be equal under caseIgnoreMatch")
	}
}

func TestEqualWithSchemaCaseExact(t *testing.T) {
	schema := testSchema(t)

	// caseExactMatch: case differences are significant.
	a := mustParseDN(t, "exactLabel=Sales")
	b := mustParseDN(t, "exactLabel=sales")
	equal, err := a.EqualWithSchema(b, schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if equal {
		t.Error("expected DNs to differ under caseExactMatch")
	}

	// caseExactMatch: insignificant spaces are not significant.
	c := mustParseDN(t, `exactLabel=Sales\ \ Team`)
	d := mustParseDN(t, "exactLabel=Sales Team")
	equal, err = c.EqualWithSchema(d, schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equal {
		t.Error("expected DNs to be equal under caseExactMatch with insignificant spaces")
	}
}

func TestEqualWithSchemaOIDAlias(t *testing.T) {
	schema := testSchema(t)

	// The attribute type is given by name in one DN and by OID in the
	// other; both resolve to the same schema definition.
	a := mustParseDN(t, "cn=John,o=Example")
	b := mustParseDN(t, "2.5.4.3=john,O=example")

	equal, err := a.EqualWithSchema(b, schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equal {
		t.Error("expected DNs to be equal when the attribute type is given as name and OID")
	}
}

func TestEqualWithSchemaMultiValuedRDN(t *testing.T) {
	schema := testSchema(t)

	// Attribute order within a multi-valued RDN is not significant.
	a := mustParseDN(t, "cn=John+sn=Doe,o=Example")
	b := mustParseDN(t, "SN=doe+CN=john,O=example")

	equal, err := a.EqualWithSchema(b, schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equal {
		t.Error("expected multi-valued RDNs to be equal regardless of attribute order")
	}

	c := mustParseDN(t, "cn=John+sn=Smith,o=Example")
	equal, err = a.EqualWithSchema(c, schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if equal {
		t.Error("expected multi-valued RDNs with different values to differ")
	}
}

func TestEqualWithSchemaIndeterminate(t *testing.T) {
	schema := testSchema(t)

	testcases := []struct {
		name    string
		a       string
		b       string
		wantErr string
	}{
		{
			name:    "unknown matching rule",
			a:       "mysteryAttr=FOO",
			b:       "mysteryAttr=foo",
			wantErr: `equality matching rule "madeUpMatch"`,
		},
		{
			name:    "unknown attribute type",
			a:       "unknownAttr=foo",
			b:       "unknownAttr=foo",
			wantErr: `attribute type "unknownAttr" is not known`,
		},
		{
			name:    "no equality rule in schema",
			a:       "rulelessAttr=foo",
			b:       "rulelessAttr=foo",
			wantErr: `has no equality matching rule`,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			a := mustParseDN(t, tc.a)
			b := mustParseDN(t, tc.b)
			equal, err := a.EqualWithSchema(b, schema)
			if err == nil {
				t.Fatalf("expected an indeterminate result, got equal=%v", equal)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error to mention %q, got %q", tc.wantErr, err.Error())
			}
			if equal {
				t.Error("indeterminate comparison must not claim equality")
			}
		})
	}
}

func TestEqualWithSchemaInjectedResolver(t *testing.T) {
	// The caller injects its own schema knowledge without a subschema.
	resolver := SchemaResolverFunc(func(nameOrOID string) (AttributeType, bool) {
		if strings.EqualFold(nameOrOID, "uid") || nameOrOID == "0.9.2342.19200300.100.1.1" {
			return AttributeType{OID: "0.9.2342.19200300.100.1.1", EqualityMatchingRule: "caseIgnoreMatch"}, true
		}
		return AttributeType{}, false
	})

	a := mustParseDN(t, "uid=Alice")
	b := mustParseDN(t, "0.9.2342.19200300.100.1.1=alice")

	equal, err := a.EqualWithSchema(b, resolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equal {
		t.Error("expected DNs to be equal with injected resolver")
	}
}

func TestNormalizeDoesNotModifyOriginal(t *testing.T) {
	schema := testSchema(t)

	dn := mustParseDN(t, "CN=John  Doe,O=Example")
	original := dn.String()

	normalized, err := dn.Normalize(schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if dn.String() != original {
		t.Errorf("Normalize modified the original DN: got %q, want %q", dn.String(), original)
	}
	if normalized == dn {
		t.Error("Normalize must return a new DN, not the receiver")
	}
	// The normalized form uses canonical OIDs and folded, space-collapsed values.
	if got, want := normalized.String(), "2.5.4.3=john doe,2.5.4.10=example"; got != want {
		t.Errorf("unexpected normalized DN: got %q, want %q", got, want)
	}
}

func TestSubschemaSuperiorInheritance(t *testing.T) {
	schema := testSchema(t)

	// cn and sn have no EQUALITY of their own; they inherit caseIgnoreMatch
	// from their superior "name".
	attributeType, ok := schema.AttributeType("sn")
	if !ok {
		t.Fatal("expected sn to be known to the subschema")
	}
	if attributeType.OID != "2.5.4.4" {
		t.Errorf("unexpected OID: got %q, want %q", attributeType.OID, "2.5.4.4")
	}
	if attributeType.EqualityMatchingRule != "caseIgnoreMatch" {
		t.Errorf("unexpected equality rule: got %q, want %q", attributeType.EqualityMatchingRule, "caseIgnoreMatch")
	}

	if _, ok := schema.AttributeType("doesNotExist"); ok {
		t.Error("expected unknown attribute type to be reported as unknown")
	}
}

func TestNewSubschemaInvalidDescription(t *testing.T) {
	if _, err := NewSubschema("not a description"); err == nil {
		t.Error("expected an error for an invalid attribute type description")
	}
}

func mustParseDN(t *testing.T, str string) *DN {
	t.Helper()
	dn, err := ParseDN(str)
	if err != nil {
		t.Fatalf("failed to parse DN %q: %v", str, err)
	}
	return dn
}
