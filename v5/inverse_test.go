package jsonpatch

import (
	"strings"
	"testing"
)

func applyWithInverse(doc, patch string) (string, Patch, error) {
	obj, err := DecodePatch([]byte(patch))

	if err != nil {
		return "", nil, err
	}

	out, inverse, err := obj.ApplyWithInverse([]byte(doc))

	if err != nil {
		return "", nil, err
	}

	return string(out), inverse, nil
}

// TestApplyWithInverseRoundTrip applies each patch, then applies the
// generated inverse patch to the result, and checks that the final document
// is semantically equal to the original.
func TestApplyWithInverseRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		doc   string
		patch string
	}{
		{
			"add new key",
			`{"foo": "bar"}`,
			`[{"op": "add", "path": "/baz", "value": "qux"}]`,
		},
		{
			"add overwrites existing key",
			`{"foo": "bar", "baz": "qux"}`,
			`[{"op": "add", "path": "/baz", "value": "changed"}]`,
		},
		{
			"add inserts into array",
			`{"foo": ["a", "b", "c"]}`,
			`[{"op": "add", "path": "/foo/1", "value": "x"}]`,
		},
		{
			"add appends to array with dash",
			`{"foo": ["a", "b"]}`,
			`[{"op": "add", "path": "/foo/-", "value": "c"}]`,
		},
		{
			"add to nested array",
			`{"foo": [["a", "b"], ["c", "d"]]}`,
			`[{"op": "add", "path": "/foo/0/1", "value": "x"}, {"op": "add", "path": "/foo/1/-", "value": "y"}]`,
		},
		{
			"remove key",
			`{"foo": "bar", "baz": {"nested": [1, 2, 3]}}`,
			`[{"op": "remove", "path": "/baz"}]`,
		},
		{
			"remove array element",
			`{"foo": ["a", "b", "c"]}`,
			`[{"op": "remove", "path": "/foo/1"}]`,
		},
		{
			"replace nested value",
			`{"foo": {"bar": [1, {"baz": "old"}]}}`,
			`[{"op": "replace", "path": "/foo/bar/1/baz", "value": "new"}]`,
		},
		{
			"replace root document",
			`{"foo": "bar", "baz": [1, 2, 3]}`,
			`[{"op": "replace", "path": "", "value": {"completely": "different"}}]`,
		},
		{
			"add replaces root document",
			`{"foo": "bar"}`,
			`[{"op": "add", "path": "", "value": ["root", "array"]}]`,
		},
		{
			"move within same array forward",
			`{"foo": ["a", "b", "c", "d"]}`,
			`[{"op": "move", "from": "/foo/0", "path": "/foo/2"}]`,
		},
		{
			"move within same array backward",
			`{"foo": ["a", "b", "c", "d"]}`,
			`[{"op": "move", "from": "/foo/3", "path": "/foo/0"}]`,
		},
		{
			"move within same array to end",
			`{"foo": ["a", "b", "c"]}`,
			`[{"op": "move", "from": "/foo/0", "path": "/foo/-"}]`,
		},
		{
			"move across objects",
			`{"foo": {"bar": "baz"}, "qux": {}}`,
			`[{"op": "move", "from": "/foo/bar", "path": "/qux/bar"}]`,
		},
		{
			"move overwrites existing key",
			`{"foo": "new", "bar": "old"}`,
			`[{"op": "move", "from": "/foo", "path": "/bar"}]`,
		},
		{
			"move from array to object",
			`{"foo": ["a", "b"], "bar": {}}`,
			`[{"op": "move", "from": "/foo/0", "path": "/bar/x"}]`,
		},
		{
			"copy to new key",
			`{"foo": {"bar": "baz"}}`,
			`[{"op": "copy", "from": "/foo/bar", "path": "/qux"}]`,
		},
		{
			"copy overwrites existing key",
			`{"foo": "new", "bar": "old"}`,
			`[{"op": "copy", "from": "/foo", "path": "/bar"}]`,
		},
		{
			"copy into array",
			`{"foo": ["a", "b"]}`,
			`[{"op": "copy", "from": "/foo/0", "path": "/foo/-"}]`,
		},
		{
			"escaped tilde and slash keys",
			`{"a/b": {"m~n": [1, 2]}, "~/": {"~0~1": "tilde"}}`,
			`[
				{"op": "replace", "path": "/a~1b/m~0n/0", "value": 42},
				{"op": "add", "path": "/a~1b/new~0key", "value": {"deep": true}},
				{"op": "replace", "path": "/~0~1/~00~01", "value": "esc"},
				{"op": "remove", "path": "/a~1b/m~0n/1"}
			]`,
		},
		{
			"test then mutate",
			`{"foo": ["bar", "baz"], "num": 1}`,
			`[
				{"op": "test", "path": "/foo/1", "value": "baz"},
				{"op": "replace", "path": "/num", "value": 2},
				{"op": "test", "path": "/num", "value": 2}
			]`,
		},
		{
			"nested arrays",
			`{"matrix": [[1, 2], [3, [4, 5]]], "tag": "keep"}`,
			`[
				{"op": "add", "path": "/matrix/1/1/-", "value": 6},
				{"op": "move", "from": "/matrix/0/0", "path": "/matrix/1/0"},
				{"op": "replace", "path": "/tag", "value": "changed"}
			]`,
		},
		{
			"multi operation sequence",
			`{"a": 1, "b": {"c": [1, 2, 3]}, "d": "x"}`,
			`[
				{"op": "test", "path": "/a", "value": 1},
				{"op": "add", "path": "/b/c/-", "value": 4},
				{"op": "remove", "path": "/d"},
				{"op": "move", "from": "/b/c/0", "path": "/b/c/2"},
				{"op": "copy", "from": "/a", "path": "/b/a2"},
				{"op": "replace", "path": "/a", "value": 100}
			]`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			patch, err := DecodePatch([]byte(c.patch))
			if err != nil {
				t.Fatalf("unable to decode patch: %s", err)
			}

			mutated, inverse, err := patch.ApplyWithInverse([]byte(c.doc))
			if err != nil {
				t.Fatalf("ApplyWithInverse failed: %s", err)
			}

			if inverse == nil {
				t.Fatalf("expected an inverse patch, got nil")
			}

			restored, err := inverse.Apply(mutated)
			if err != nil {
				t.Fatalf("applying inverse patch failed: %s", err)
			}

			if !compareJSON(string(restored), c.doc) {
				t.Errorf("round trip mismatch:\noriginal:  %s\nrestored:  %s\nmutated:   %s", c.doc, restored, mutated)
			}
		})
	}
}

// TestApplyWithInverseResult checks that the mutated document and the inverse
// patch itself match expectations for a move within a single array, where the
// inverse cannot just swap "from" and "path".
func TestApplyWithInverseResult(t *testing.T) {
	doc := `{"foo": ["a", "b", "c", "d"]}`
	patch, err := DecodePatch([]byte(`[{"op": "move", "from": "/foo/0", "path": "/foo/2"}]`))
	if err != nil {
		t.Fatalf("unable to decode patch: %s", err)
	}

	mutated, inverse, err := patch.ApplyWithInverse([]byte(doc))
	if err != nil {
		t.Fatalf("ApplyWithInverse failed: %s", err)
	}

	if !compareJSON(string(mutated), `{"foo": ["b", "c", "a", "d"]}`) {
		t.Errorf("unexpected mutated document: %s", mutated)
	}

	if len(inverse) != 2 {
		t.Fatalf("expected 2 inverse operations, got %d", len(inverse))
	}

	if kind := inverse[0].Kind(); kind != "remove" {
		t.Errorf("expected first inverse op to be remove, got %s", kind)
	}

	if path, _ := inverse[0].Path(); path != "/foo/2" {
		t.Errorf("expected first inverse op path /foo/2, got %s", path)
	}

	if kind := inverse[1].Kind(); kind != "add" {
		t.Errorf("expected second inverse op to be add, got %s", kind)
	}

	if path, _ := inverse[1].Path(); path != "/foo/0" {
		t.Errorf("expected second inverse op path /foo/0, got %s", path)
	}
}

// TestApplyWithInverseTestOp checks that a "test" operation produces a test
// in the inverse patch, so undoing re-validates the expected state.
func TestApplyWithInverseTestOp(t *testing.T) {
	doc := `{"foo": "bar"}`
	patch, err := DecodePatch([]byte(`[
		{"op": "test", "path": "/foo", "value": "bar"},
		{"op": "replace", "path": "/foo", "value": "baz"}
	]`))
	if err != nil {
		t.Fatalf("unable to decode patch: %s", err)
	}

	mutated, inverse, err := patch.ApplyWithInverse([]byte(doc))
	if err != nil {
		t.Fatalf("ApplyWithInverse failed: %s", err)
	}

	if len(inverse) != 2 {
		t.Fatalf("expected 2 inverse operations, got %d", len(inverse))
	}

	if kind := inverse[1].Kind(); kind != "test" {
		t.Errorf("expected last inverse op to be test, got %s", kind)
	}

	restored, err := inverse.Apply(mutated)
	if err != nil {
		t.Fatalf("applying inverse patch failed: %s", err)
	}

	if !compareJSON(string(restored), doc) {
		t.Errorf("round trip mismatch: original %s, restored %s", doc, restored)
	}
}

// TestApplyWithInverseFailure ensures a failing patch yields no document and
// no inverse patch, never a partial one.
func TestApplyWithInverseFailure(t *testing.T) {
	cases := []struct {
		name  string
		doc   string
		patch string
	}{
		{"failing test", `{"foo": "bar"}`, `[{"op": "test", "path": "/foo", "value": "nope"}]`},
		{"remove missing", `{"foo": "bar"}`, `[{"op": "remove", "path": "/baz"}]`},
		{"replace missing", `{"foo": "bar"}`, `[{"op": "replace", "path": "/baz", "value": 1}]`},
		{"add missing parent", `{"foo": "bar"}`, `[{"op": "add", "path": "/baz/qux", "value": 1}]`},
		{"move missing from", `{"foo": "bar"}`, `[{"op": "move", "from": "/baz", "path": "/qux"}]`},
		{"fail after success", `{"foo": "bar"}`, `[{"op": "add", "path": "/baz", "value": 1}, {"op": "remove", "path": "/missing"}]`},
		{"invalid index", `{"foo": ["a"]}`, `[{"op": "remove", "path": "/foo/5"}]`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			patch, err := DecodePatch([]byte(c.patch))
			if err != nil {
				t.Fatalf("unable to decode patch: %s", err)
			}

			mutated, inverse, err := patch.ApplyWithInverse([]byte(c.doc))
			if err == nil {
				t.Fatalf("expected an error, got none (mutated: %s)", mutated)
			}

			if mutated != nil {
				t.Errorf("expected nil mutated document on failure, got %s", mutated)
			}

			if inverse != nil {
				t.Errorf("expected nil inverse patch on failure, got %v", inverse)
			}
		})
	}
}

// TestApplyWithInverseOptions ensures options are honored and that
// EnsurePathExistsOnAdd is rejected instead of producing a wrong inverse.
func TestApplyWithInverseOptions(t *testing.T) {
	doc := `{"foo": "bar"}`
	patch, err := DecodePatch([]byte(`[{"op": "add", "path": "/baz/qux", "value": 1}]`))
	if err != nil {
		t.Fatalf("unable to decode patch: %s", err)
	}

	options := NewApplyOptions()
	options.EnsurePathExistsOnAdd = true

	_, inverse, err := patch.ApplyWithInverseWithOptions([]byte(doc), options)
	if err == nil {
		t.Fatalf("expected an error for EnsurePathExistsOnAdd, got none")
	}

	if inverse != nil {
		t.Errorf("expected nil inverse patch on failure, got %v", inverse)
	}

	if !strings.Contains(err.Error(), "EnsurePathExistsOnAdd") {
		t.Errorf("expected error to mention EnsurePathExistsOnAdd, got: %s", err)
	}
}
