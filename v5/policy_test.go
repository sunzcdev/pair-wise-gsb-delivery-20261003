package jsonpatch

import (
	"errors"
	"strings"
	"testing"
)

const policyTestIDNumber = "110101199001011234"

const policyTestDoc = `{"customer":{"name":"Alice","idNumber":"` + policyTestIDNumber + `","contact":{"phone":"123","email":"a@example.com"}}}`

// contactOnlyPolicy allows writes below /customer/contact only, and forbids
// reading the customer's idNumber.
func contactOnlyPolicy() *AccessPolicy {
	return &AccessPolicy{
		Read: func(path string) bool {
			return path != "/customer/idNumber" && !strings.HasPrefix(path, "/customer/idNumber/")
		},
		Write: func(path string) bool {
			return path == "/customer/contact" || strings.HasPrefix(path, "/customer/contact/")
		},
	}
}

func applyWithPolicy(t *testing.T, doc, patch string, policy *AccessPolicy) (string, error) {
	t.Helper()

	p, err := DecodePatch([]byte(patch))
	if err != nil {
		t.Fatalf("failed to decode patch: %v", err)
	}

	out, err := p.ApplyWithPolicy([]byte(doc), policy)
	if err != nil {
		return "", err
	}

	return string(out), nil
}

func assertPolicyViolation(t *testing.T, err error, opIndex int, perm Permission, path string) *PolicyViolationError {
	t.Helper()

	var violation *PolicyViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("expected PolicyViolationError, got %T: %v", err, err)
	}

	if violation.OpIndex != opIndex {
		t.Errorf("expected op index %d, got %d", opIndex, violation.OpIndex)
	}
	if violation.Permission != perm {
		t.Errorf("expected permission %q, got %q", perm, violation.Permission)
	}
	if violation.Path != path {
		t.Errorf("expected path %q, got %q", path, violation.Path)
	}

	if strings.Contains(err.Error(), policyTestIDNumber) {
		t.Errorf("error message leaks protected value: %q", err.Error())
	}

	return violation
}

func TestApplyWithPolicyAllowsContactUpdate(t *testing.T) {
	out, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"replace","path":"/customer/contact/phone","value":"456"}]`,
		contactOnlyPolicy())
	if err != nil {
		t.Fatalf("expected patch to apply, got: %v", err)
	}

	if !strings.Contains(out, `"phone":"456"`) {
		t.Errorf("expected updated phone in output, got: %s", out)
	}
	if !strings.Contains(out, policyTestIDNumber) {
		t.Errorf("expected untouched idNumber in output, got: %s", out)
	}
}

func TestApplyWithPolicyDeniesIDNumberWrite(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"replace","path":"/customer/idNumber","value":"000"}]`,
		contactOnlyPolicy())

	assertPolicyViolation(t, err, 0, PermissionWrite, "/customer/idNumber")
}

func TestApplyWithPolicyReportsOperationIndex(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[
			{"op":"replace","path":"/customer/contact/phone","value":"456"},
			{"op":"add","path":"/customer/idNumber","value":"000"}
		]`,
		contactOnlyPolicy())

	assertPolicyViolation(t, err, 1, PermissionWrite, "/customer/idNumber")
}

func TestApplyWithPolicyParentReplaceAffectsProtectedChild(t *testing.T) {
	// The parent itself is writable here, but replacing the whole /customer
	// object would also replace the protected idNumber below it.
	policy := &AccessPolicy{
		Write: func(path string) bool {
			return path != "/customer/idNumber" && !strings.HasPrefix(path, "/customer/idNumber/")
		},
	}

	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"replace","path":"/customer","value":{"contact":{"phone":"9"}}}]`,
		policy)

	assertPolicyViolation(t, err, 0, PermissionWrite, "/customer/idNumber")
}

func TestApplyWithPolicyRootReplaceAffectsProtectedChild(t *testing.T) {
	policy := &AccessPolicy{
		Write: func(path string) bool {
			return path != "/customer/idNumber" && !strings.HasPrefix(path, "/customer/idNumber/")
		},
	}

	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"replace","path":"","value":{"customer":{"contact":{"phone":"9"}}}}]`,
		policy)

	assertPolicyViolation(t, err, 0, PermissionWrite, "/customer/idNumber")
}

func TestApplyWithPolicyCopyCannotReadProtectedValue(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"copy","from":"/customer/idNumber","path":"/customer/contact/backup"}]`,
		contactOnlyPolicy())

	assertPolicyViolation(t, err, 0, PermissionRead, "/customer/idNumber")
}

func TestApplyWithPolicyCopyFromParentReadsProtectedChild(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"copy","from":"/customer","path":"/customer/contact/backup"}]`,
		contactOnlyPolicy())

	assertPolicyViolation(t, err, 0, PermissionRead, "/customer/idNumber")
}

func TestApplyWithPolicyMoveReadsAndRemovesSource(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"move","from":"/customer/idNumber","path":"/customer/contact/id"}]`,
		contactOnlyPolicy())

	assertPolicyViolation(t, err, 0, PermissionRead, "/customer/idNumber")
}

func TestApplyWithPolicyMoveChecksDestination(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"move","from":"/customer/contact/email","path":"/customer/idNumber"}]`,
		contactOnlyPolicy())

	assertPolicyViolation(t, err, 0, PermissionWrite, "/customer/idNumber")
}

func TestApplyWithPolicyMoveWithinWritableSubtree(t *testing.T) {
	out, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"move","from":"/customer/contact/email","path":"/customer/contact/backupEmail"}]`,
		contactOnlyPolicy())
	if err != nil {
		t.Fatalf("expected patch to apply, got: %v", err)
	}

	if !strings.Contains(out, `"backupEmail":"a@example.com"`) {
		t.Errorf("expected moved value in output, got: %s", out)
	}
}

func TestApplyWithPolicyTestCannotProbeProtectedValue(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[
			{"op":"test","path":"/customer/idNumber","value":"`+policyTestIDNumber+`"},
			{"op":"replace","path":"/customer/contact/phone","value":"456"}
		]`,
		contactOnlyPolicy())

	assertPolicyViolation(t, err, 0, PermissionRead, "/customer/idNumber")
}

func TestApplyWithPolicyTestOnParentReadsProtectedChild(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"test","path":"/customer","value":{"name":"Alice"}}]`,
		contactOnlyPolicy())

	assertPolicyViolation(t, err, 0, PermissionRead, "/customer/idNumber")
}

func TestApplyWithPolicyEvaluatesDocumentAfterEachStep(t *testing.T) {
	policy := &AccessPolicy{
		Read: func(path string) bool {
			return path != "/customer/idNumber" && !strings.HasPrefix(path, "/customer/idNumber/")
		},
	}

	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"copy","from":"/customer","path":"/backup"}]`, policy)
	assertPolicyViolation(t, err, 0, PermissionRead, "/customer/idNumber")

	out, err := applyWithPolicy(t, policyTestDoc,
		`[
			{"op":"remove","path":"/customer/idNumber"},
			{"op":"copy","from":"/customer","path":"/backup"}
		]`, policy)
	if err != nil {
		t.Fatalf("expected patch to apply, got: %v", err)
	}

	if !strings.Contains(out, `"backup":{"name":"Alice"`) {
		t.Errorf("expected copied subtree in output, got: %s", out)
	}
}

func TestApplyWithPolicyRemoveOfProtectedPath(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op":"remove","path":"/customer/contact"}]`,
		contactOnlyPolicy())
	if err != nil {
		t.Fatalf("expected removal of writable subtree to apply, got: %v", err)
	}

	_, err = applyWithPolicy(t, policyTestDoc,
		`[{"op":"remove","path":"/customer"}]`,
		contactOnlyPolicy())
	assertPolicyViolation(t, err, 0, PermissionWrite, "/customer")
}

func TestApplyWithPolicyNilPolicyBehavesLikeApply(t *testing.T) {
	patch := `[{"op":"replace","path":"/customer/idNumber","value":"000"}]`

	p, err := DecodePatch([]byte(patch))
	if err != nil {
		t.Fatalf("failed to decode patch: %v", err)
	}

	withPolicy, err := p.ApplyWithPolicy([]byte(policyTestDoc), nil)
	if err != nil {
		t.Fatalf("expected nil policy to apply, got: %v", err)
	}

	plain, err := p.Apply([]byte(policyTestDoc))
	if err != nil {
		t.Fatalf("expected plain Apply to succeed, got: %v", err)
	}

	if !compareJSON(string(withPolicy), string(plain)) {
		t.Errorf("expected identical results, got %s and %s", withPolicy, plain)
	}
}

func TestApplyWithPolicyArrayDescendants(t *testing.T) {
	doc := `{"customers":[{"idNumber":"` + policyTestIDNumber + `","phone":"1"}]}`
	policy := &AccessPolicy{
		Write: func(path string) bool {
			return !strings.HasSuffix(path, "/idNumber")
		},
	}

	_, err := applyWithPolicy(t, doc,
		`[{"op":"replace","path":"/customers","value":[]}]`, policy)
	assertPolicyViolation(t, err, 0, PermissionWrite, "/customers/0/idNumber")

	out, err := applyWithPolicy(t, doc,
		`[{"op":"replace","path":"/customers/0/phone","value":"2"}]`, policy)
	if err != nil {
		t.Fatalf("expected patch to apply, got: %v", err)
	}
	if !strings.Contains(out, `"phone":"2"`) {
		t.Errorf("expected updated phone in output, got: %s", out)
	}
}

func TestPolicyViolationErrorMessage(t *testing.T) {
	err := &PolicyViolationError{OpIndex: 2, Operation: "copy", Permission: PermissionRead, Path: "/customer/idNumber"}

	msg := err.Error()
	for _, want := range []string{"2", "copy", "read", "/customer/idNumber"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error message to contain %q, got %q", want, msg)
		}
	}
}
