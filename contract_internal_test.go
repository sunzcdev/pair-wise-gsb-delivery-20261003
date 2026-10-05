package paseto

import "testing"

func TestContractDefinitionRejectsCycle(t *testing.T) {
	obj := ObjectSpec(map[string]FieldSpec{})
	obj.fields["self"] = FieldSpec{kind: Array, required: true, element: &obj}

	contract := &ClaimsContract{fields: map[string]FieldSpec{"root": obj}}
	if err := contract.validateDefinition(); err == nil {
		t.Fatal("expected a self-referencing definition to be rejected")
	}
}

func TestContractDefinitionRejectsNilArrayElement(t *testing.T) {
	contract := &ClaimsContract{fields: map[string]FieldSpec{
		"arr": {kind: Array},
	}}
	if err := contract.validateDefinition(); err == nil {
		t.Fatal("expected an array without an element type to be rejected")
	}
}

func TestContractDefinitionRejectsUnknownKind(t *testing.T) {
	contract := &ClaimsContract{fields: map[string]FieldSpec{
		"bad": {kind: 0, required: true},
	}}
	if err := contract.validateDefinition(); err == nil {
		t.Fatal("expected a spec with no recognised kind to be rejected")
	}
}
