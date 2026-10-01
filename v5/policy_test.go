package jsonpatch

import (
	"errors"
	"strings"
	"testing"
)

const policyTestDoc = `{
	"customer": {
		"name": "Alice",
		"phone": "123-456",
		"email": "alice@example.com",
		"idCard": "110101199001011234"
	},
	"tags": ["vip", "newsletter"]
}`

// contactPolicy only allows updating the customer's contact fields. The ID
// card number can neither be modified nor read (e.g. via copy/move/test).
func contactPolicy() *AccessPolicy {
	return &AccessPolicy{
		Read: func(path string) bool {
			return path != "/customer/idCard"
		},
		Write: func(path string) bool {
			switch path {
			case "/customer/phone", "/customer/email", "/customer/name":
				return true
			}
			return false
		},
	}
}

func applyWithPolicy(t *testing.T, doc, patch string, policy *AccessPolicy) (string, error) {
	t.Helper()

	p, err := DecodePatch([]byte(patch))
	if err != nil {
		t.Fatalf("unable to decode patch: %s", err)
	}

	out, err := p.ApplyWithPolicy([]byte(doc), policy)
	return string(out), err
}

func assertPolicyViolation(t *testing.T, err error, opIndex int, perm Permission, path string) {
	t.Helper()

	var pve *PolicyViolationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *PolicyViolationError, got %T: %v", err, err)
	}
	if pve.OpIndex != opIndex {
		t.Fatalf("expected op index %d, got %d", opIndex, pve.OpIndex)
	}
	if pve.Permission != perm {
		t.Fatalf("expected permission %q, got %q", perm, pve.Permission)
	}
	if pve.Path != path {
		t.Fatalf("expected path %q, got %q", path, pve.Path)
	}
}

func TestApplyWithPolicyAllowsContactUpdate(t *testing.T) {
	out, err := applyWithPolicy(t, policyTestDoc,
		`[{"op": "replace", "path": "/customer/phone", "value": "987-654"}]`,
		contactPolicy())
	if err != nil {
		t.Fatalf("expected contact update to be allowed, got: %s", err)
	}
	if !strings.Contains(out, `"987-654"`) {
		t.Fatalf("expected updated phone in result, got: %s", out)
	}
}

func TestApplyWithPolicyDeniesProtectedWrite(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op": "replace", "path": "/customer/idCard", "value": "999"}]`,
		contactPolicy())
	assertPolicyViolation(t, err, 0, PermissionWrite, "/customer/idCard")

	if strings.Contains(err.Error(), "110101199001011234") || strings.Contains(err.Error(), "999") {
		t.Fatalf("error must not contain protected values: %s", err)
	}
}

func TestApplyWithPolicyDeniesParentReplace(t *testing.T) {
	// Replacing the whole parent object drops the protected child, which
	// must be caught even though the operation targets the parent path.
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op": "replace", "path": "/customer", "value": {"name": "Alice", "phone": "123-456"}}]`,
		contactPolicy())
	assertPolicyViolation(t, err, 0, PermissionWrite, "/customer/idCard")
}

func TestApplyWithPolicyDeniesProtectedRemove(t *testing.T) {
	// The parent path itself is writable, but removing it also removes the
	// protected child, which must still be caught.
	policy := &AccessPolicy{
		Write: func(path string) bool {
			return path != "/customer/idCard"
		},
	}

	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op": "remove", "path": "/customer"}]`,
		policy)
	assertPolicyViolation(t, err, 0, PermissionWrite, "/customer/idCard")
}

func TestApplyWithPolicyDeniesCopyRead(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op": "copy", "from": "/customer/idCard", "path": "/customer/name"}]`,
		contactPolicy())
	assertPolicyViolation(t, err, 0, PermissionRead, "/customer/idCard")

	if strings.Contains(err.Error(), "110101199001011234") {
		t.Fatalf("error must not contain protected values: %s", err)
	}
}

func TestApplyWithPolicyDeniesCopyOfParent(t *testing.T) {
	// Copying a parent object reads the protected child as well.
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op": "copy", "from": "/customer", "path": "/backup"}]`,
		contactPolicy())
	assertPolicyViolation(t, err, 0, PermissionRead, "/customer/idCard")
}

func TestApplyWithPolicyDeniesMove(t *testing.T) {
	// move reads the source path and removes the source content.
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op": "move", "from": "/customer/idCard", "path": "/customer/name"}]`,
		contactPolicy())
	assertPolicyViolation(t, err, 0, PermissionRead, "/customer/idCard")
}

func TestApplyWithPolicyDeniesTest(t *testing.T) {
	// A test operation would leak whether the protected value matches.
	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op": "test", "path": "/customer/idCard", "value": "110101199001011234"}]`,
		contactPolicy())
	assertPolicyViolation(t, err, 0, PermissionRead, "/customer/idCard")
}

func TestApplyWithPolicyReportsOperationIndex(t *testing.T) {
	_, err := applyWithPolicy(t, policyTestDoc,
		`[
			{"op": "replace", "path": "/customer/phone", "value": "987-654"},
			{"op": "replace", "path": "/customer/idCard", "value": "999"}
		]`,
		contactPolicy())
	assertPolicyViolation(t, err, 1, PermissionWrite, "/customer/idCard")
}

func TestApplyWithPolicyChecksDocumentAfterEachStep(t *testing.T) {
	// The first operation renames the protected field's parent key, so the
	// second operation's effect must be evaluated against the updated doc.
	policy := &AccessPolicy{
		Write: func(path string) bool {
			return path != "/customer/idCard"
		},
	}

	_, err := applyWithPolicy(t, policyTestDoc,
		`[
			{"op": "add", "path": "/customer/idCard2", "value": "x"},
			{"op": "remove", "path": "/customer/idCard2"}
		]`,
		policy)
	if err != nil {
		t.Fatalf("expected unrelated add/remove to be allowed, got: %s", err)
	}

	_, err = applyWithPolicy(t, policyTestDoc,
		`[
			{"op": "replace", "path": "/customer/phone", "value": "987-654"},
			{"op": "remove", "path": "/customer/idCard"}
		]`,
		policy)
	assertPolicyViolation(t, err, 1, PermissionWrite, "/customer/idCard")
}

func TestApplyWithPolicyArrayShift(t *testing.T) {
	// Inserting into an array shifts later elements; the protected element
	// moves to a new path, and the write at its old path must be checked.
	policy := &AccessPolicy{
		Write: func(path string) bool {
			return path != "/tags/1"
		},
	}

	_, err := applyWithPolicy(t, policyTestDoc,
		`[{"op": "add", "path": "/tags/0", "value": "new"}]`,
		policy)
	assertPolicyViolation(t, err, 0, PermissionWrite, "/tags/1")
}

func TestApplyWithPolicyAllowsMoveOfWritableFields(t *testing.T) {
	out, err := applyWithPolicy(t, policyTestDoc,
		`[{"op": "move", "from": "/customer/phone", "path": "/customer/email"}]`,
		contactPolicy())
	if err != nil {
		t.Fatalf("expected move of writable fields to be allowed, got: %s", err)
	}
	if !strings.Contains(out, `"email": "123-456"`) && !strings.Contains(out, `"email":"123-456"`) {
		t.Fatalf("expected phone value moved to email, got: %s", out)
	}
}

func TestApplyWithPolicyNilPolicyMatchesApply(t *testing.T) {
	patch := `[{"op": "replace", "path": "/customer/idCard", "value": "999"}]`

	p, err := DecodePatch([]byte(patch))
	if err != nil {
		t.Fatalf("unable to decode patch: %s", err)
	}

	withPolicy, err := p.ApplyWithPolicy([]byte(policyTestDoc), nil)
	if err != nil {
		t.Fatalf("nil policy should allow everything, got: %s", err)
	}

	plain, err := p.Apply([]byte(policyTestDoc))
	if err != nil {
		t.Fatalf("plain Apply failed: %s", err)
	}

	if string(withPolicy) != string(plain) {
		t.Fatalf("nil policy result differs from Apply: %s vs %s", withPolicy, plain)
	}
}

func TestApplyWithPolicyNilFieldsAllowEverything(t *testing.T) {
	out, err := applyWithPolicy(t, policyTestDoc,
		`[{"op": "replace", "path": "/customer/idCard", "value": "999"}]`,
		&AccessPolicy{})
	if err != nil {
		t.Fatalf("empty policy should allow everything, got: %s", err)
	}
	if !strings.Contains(out, `"999"`) {
		t.Fatalf("expected updated value in result, got: %s", out)
	}
}
