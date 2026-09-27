package jsonpatch

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func applyAndEqual(t *testing.T, original, modified []byte, patch Patch) {
	t.Helper()
	patched, err := patch.Apply(original)
	if err != nil {
		t.Fatalf("applying generated patch failed: %v\npatch: %v", err, patch)
	}
	got, err := decodeDiffDoc(patched)
	if err != nil {
		t.Fatalf("patched document is invalid JSON: %v", err)
	}
	want, err := decodeDiffDoc(modified)
	if err != nil {
		t.Fatalf("modified document is invalid JSON: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("patched document mismatch\ngot:  %s\nwant: %s\npatch: %v", patched, modified, patch)
	}
}

func TestCreatePatchMoveReorderAndUpdate(t *testing.T) {
	original := []byte(`{
		"services": [
			{"name": "web", "replicas": 3, "image": "web:1.0"},
			{"name": "api", "replicas": 2, "image": "api:1.4"},
			{"name": "cache", "replicas": 1, "image": "redis:7"}
		]
	}`)
	modified := []byte(`{
		"services": [
			{"name": "api", "replicas": 5, "image": "api:1.4"},
			{"name": "web", "replicas": 3, "image": "web:2.0"},
			{"name": "queue", "replicas": 1, "image": "mq:3"}
		]
	}`)

	patch, err := CreatePatch(original, modified, WithArrayIdentity("/services", "name"))
	if err != nil {
		t.Fatalf("CreatePatch failed: %v", err)
	}

	var moves, removes, adds, replaces int
	for _, op := range patch {
		switch string(*op["op"]) {
		case `"move"`:
			moves++
		case `"remove"`:
			removes++
		case `"add"`:
			adds++
		case `"replace"`:
			replaces++
		}
	}
	if moves != 1 || removes != 1 || adds != 1 || replaces != 2 {
		t.Fatalf("expected 1 move, 1 remove, 1 add, 2 replaces; got %d/%d/%d/%d\npatch: %v",
			moves, removes, adds, replaces, patch)
	}
	applyAndEqual(t, original, modified, patch)
}

func TestCreatePatchPositionalArrayByDefault(t *testing.T) {
	original := []byte(`{"list": [1, 2, 3]}`)
	modified := []byte(`{"list": [3, 2]}`)

	patch, err := CreatePatch(original, modified)
	if err != nil {
		t.Fatalf("CreatePatch failed: %v", err)
	}
	if len(patch) != 2 {
		t.Fatalf("expected 2 operations, got %d: %v", len(patch), patch)
	}
	if string(*patch[0]["op"]) != `"replace"` || string(*patch[0]["path"]) != `"/list/0"` {
		t.Fatalf("expected replace of /list/0, got %v", patch[0])
	}
	if string(*patch[1]["op"]) != `"remove"` || string(*patch[1]["path"]) != `"/list/2"` {
		t.Fatalf("expected remove of /list/2, got %v", patch[1])
	}
	applyAndEqual(t, original, modified, patch)
}

func TestCreatePatchDuplicateIdentity(t *testing.T) {
	original := []byte(`{"items": [{"id": "a"}, {"id": "a"}]}`)
	modified := []byte(`{"items": [{"id": "a"}]}`)

	_, err := CreatePatch(original, modified, WithArrayIdentity("/items", "id"))
	if !errors.Is(err, ErrDuplicateIdentity) {
		t.Fatalf("expected ErrDuplicateIdentity, got %v", err)
	}
}

func TestCreatePatchMissingIdentity(t *testing.T) {
	original := []byte(`{"items": [{"id": "a"}, {"name": "no-id"}]}`)
	modified := []byte(`{"items": [{"id": "a"}]}`)

	_, err := CreatePatch(original, modified, WithArrayIdentity("/items", "id"))
	if !errors.Is(err, ErrMissingIdentity) {
		t.Fatalf("expected ErrMissingIdentity, got %v", err)
	}
}

func TestCreatePatchInvalidIdentity(t *testing.T) {
	original := []byte(`{"items": [{"id": "a"}, {"id": {"nested": true}}]}`)
	modified := []byte(`{"items": [{"id": "a"}]}`)

	_, err := CreatePatch(original, modified, WithArrayIdentity("/items", "id"))
	if !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("expected ErrInvalidIdentity, got %v", err)
	}
}

func TestCreatePatchIdentityFallbackPositional(t *testing.T) {
	original := []byte(`{"items": [{"id": "a", "v": 1}, {"id": "a", "v": 2}]}`)
	modified := []byte(`{"items": [{"id": "a", "v": 1}]}`)

	patch, err := CreatePatch(original, modified,
		WithArrayIdentity("/items", "id"),
		WithIdentityMismatchMode(IdentityMismatchPositional),
	)
	if err != nil {
		t.Fatalf("CreatePatch with positional fallback failed: %v", err)
	}
	if len(patch) != 1 || string(*patch[0]["op"]) != `"remove"` {
		t.Fatalf("expected a single positional remove, got %v", patch)
	}
	applyAndEqual(t, original, modified, patch)
}

func TestCreatePatchNoChanges(t *testing.T) {
	doc := []byte(`{"a": [1, {"b": "c"}], "d": null}`)
	patch, err := CreatePatch(doc, doc)
	if err != nil {
		t.Fatalf("CreatePatch failed: %v", err)
	}
	if len(patch) != 0 {
		t.Fatalf("expected empty patch, got %v", patch)
	}
}

func TestCreatePatchRootReplace(t *testing.T) {
	patch, err := CreatePatch([]byte(`{"a": 1}`), []byte(`[1, 2]`))
	if err != nil {
		t.Fatalf("CreatePatch failed: %v", err)
	}
	if len(patch) != 1 || string(*patch[0]["op"]) != `"replace"` || string(*patch[0]["path"]) != `""` {
		t.Fatalf("expected single root replace, got %v", patch)
	}
	applyAndEqual(t, []byte(`{"a": 1}`), []byte(`[1, 2]`), patch)
}

func TestCreatePatchEscapedKeys(t *testing.T) {
	original := []byte(`{"a/b": {"~key": 1}}`)
	modified := []byte(`{"a/b": {"~key": 2}}`)

	patch, err := CreatePatch(original, modified)
	if err != nil {
		t.Fatalf("CreatePatch failed: %v", err)
	}
	if string(*patch[0]["path"]) != `"/a~1b/~0key"` {
		t.Fatalf("expected escaped path, got %v", patch[0])
	}
	applyAndEqual(t, original, modified, patch)
}

func TestCreatePatchNestedKeyedArrays(t *testing.T) {
	original := []byte(`{"spec": {"containers": [
		{"name": "app", "env": [{"name": "A", "value": "1"}, {"name": "B", "value": "2"}]},
		{"name": "sidecar", "env": []}
	]}}`)
	modified := []byte(`{"spec": {"containers": [
		{"name": "sidecar", "env": []},
		{"name": "app", "env": [{"name": "B", "value": "3"}, {"name": "A", "value": "1"}]}
	]}}`)

	patch, err := CreatePatch(original, modified,
		WithArrayIdentity("/spec/containers", "name"),
		WithArrayIdentity("/spec/containers/1/env", "name"),
	)
	if err != nil {
		t.Fatalf("CreatePatch failed: %v", err)
	}
	applyAndEqual(t, original, modified, patch)

	var foundMove, foundReplace bool
	for _, op := range patch {
		if string(*op["op"]) == `"move"` && strings.HasPrefix(string(*op["path"]), `"/spec/containers/1/env/`) {
			foundMove = true
		}
		if string(*op["op"]) == `"replace"` && string(*op["path"]) == `"/spec/containers/1/env/0/value"` {
			foundReplace = true
		}
	}
	if !foundMove || !foundReplace {
		t.Fatalf("expected nested env move and value replace, got %v", patch)
	}
}

func TestCreatePatchNumericIdentity(t *testing.T) {
	original := []byte(`{"rules": [{"id": 1, "on": true}, {"id": 2, "on": false}]}`)
	modified := []byte(`{"rules": [{"id": 2, "on": false}, {"id": 1, "on": true}]}`)

	patch, err := CreatePatch(original, modified, WithArrayIdentity("/rules", "id"))
	if err != nil {
		t.Fatalf("CreatePatch failed: %v", err)
	}
	if len(patch) != 1 || string(*patch[0]["op"]) != `"move"` {
		t.Fatalf("expected a single move, got %v", patch)
	}
	applyAndEqual(t, original, modified, patch)
}

func TestCreatePatchDeterministic(t *testing.T) {
	original := []byte(`{"z": 1, "a": 2, "m": {"y": 1, "b": 2}}`)
	modified := []byte(`{"a": 3, "m": {"b": 4, "c": 5}, "n": true}`)

	first, err := CreatePatch(original, modified)
	if err != nil {
		t.Fatalf("CreatePatch failed: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := CreatePatch(original, modified)
		if err != nil {
			t.Fatalf("CreatePatch failed: %v", err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("non-deterministic patch:\nfirst: %v\nagain: %v", first, again)
		}
	}
	applyAndEqual(t, original, modified, first)
}
