package jsonpatch

import (
	"fmt"
	"testing"

	"github.com/evanphx/json-patch/v5/internal/json"
)

func patchMarshal(p Patch) ([]byte, error) {
	return json.Marshal(p)
}

// roundTrip applies patch to doc, then the returned inverse patch to the
// result, and asserts the final document is semantically equal to the
// original. It also verifies that the inverse patch can be serialized and
// re-decoded before being applied.
func roundTrip(t *testing.T, doc, patchJSON, wantResult string) {
	t.Helper()

	patch, err := DecodePatch([]byte(patchJSON))
	if err != nil {
		t.Fatalf("decoding patch: %v\npatch: %q\nvalid: %v", err, patchJSON, json.Valid([]byte(patchJSON)))
	}

	result, inverse, err := patch.ApplyWithInverse([]byte(doc))
	if err != nil {
		t.Fatalf("applying patch: %v\npatch: %s", err, patchJSON)
	}

	if wantResult != "" && !compareJSON(string(result), wantResult) {
		t.Fatalf("forward result mismatch:\n got:  %s\n want: %s", result, wantResult)
	}

	if inverse == nil {
		t.Fatalf("expected non-nil inverse patch")
	}

	encoded, err := inverse.MarshalJSONForTest()
	if err != nil {
		t.Fatalf("encoding inverse: %v", err)
	}

	decoded, err := DecodePatch(encoded)
	if err != nil {
		t.Fatalf("inverse patch is not a decodable RFC 6902 patch: %v\ninverse: %s\nvalid json: %v\ndoc: %s\npatch: %s", err, encoded, json.Valid(encoded), doc, patchJSON)
	}

	restored, err := decoded.Apply(result)
	if err != nil {
		t.Fatalf("applying inverse patch: %v\ninverse: %s", err, encoded)
	}

	if !Equal(restored, []byte(doc)) {
		t.Fatalf("inverse did not restore the original document:\n original: %s\n restored: %s\n inverse:  %s", doc, restored, encoded)
	}
}

func (p Patch) MarshalJSONForTest() ([]byte, error) {
	return patchMarshal(p)
}

func TestInverseAddObjectOverwritesOldValue(t *testing.T) {
	// An add that targets an existing object key overwrites the old value,
	// so the inverse must be a replace carrying that old value, not a
	// remove.
	doc := `{"foo":"bar","n":{"x":1}}`
	patchJSON := `[{"op":"add","path":"/foo","value":"baz"},{"op":"add","path":"/n/x","value":2}]`

	roundTrip(t, doc, patchJSON, `{"foo":"baz","n":{"x":2}}`)

	patch, _ := DecodePatch([]byte(patchJSON))
	_, inverse, err := patch.ApplyWithInverse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}

	if got := inverse[0].Kind(); got != "replace" || inverse[1].Kind() != "replace" {
		t.Fatalf("expected both inverse ops to be replace, got %q and %q", inverse[0].Kind(), inverse[1].Kind())
	}
}

func TestInverseAddIntoNestedArrayWithEscapedPath(t *testing.T) {
	// Escaped keys (~1, ~0, including a literal tilde followed by 0) and a
	// deeply nested array index in one document.
	doc := `{"a/b":[{"k~0":[[1,2],[3,4]]},{"keep":[true]}]}`
	patchJSON := `[
		{"op":"add","path":"/a~1b/0/k~00/1/0","value":"x"},
		{"op":"add","path":"/a~1b/1/keep/-","value":false}
	]`

	roundTrip(t, doc, patchJSON, `{"a/b":[{"k~0":[[1,2],["x",3,4]]},{"keep":[true,false]}]}`)
}

func TestInverseMoveInsideSameArray(t *testing.T) {
	cases := []struct {
		name      string
		doc       string
		patchJSON string
	}{
		{
			"move forward",
			`{"arr":[0,1,2,3,4]}`,
			`[{"op":"move","from":"/arr/1","path":"/arr/3"}]`,
		},
		{
			"move backward",
			`{"arr":[0,1,2,3,4]}`,
			`[{"op":"move","from":"/arr/3","path":"/arr/1"}]`,
		},
		{
			"move to end",
			`{"arr":[0,1,2,3,4]}`,
			`[{"op":"move","from":"/arr/0","path":"/arr/-"}]`,
		},
		{
			"move from end with negative index",
			`{"arr":[0,1,2,3,4]}`,
			`[{"op":"move","from":"/arr/-1","path":"/arr/0"}]`,
		},
		{
			"nested array move",
			`{"outer":[[{"a":1},"b","c"],[{"d":2}]]}`,
			`[{"op":"move","from":"/outer/0/0","path":"/outer/0/2"}]`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			roundTrip(t, c.doc, c.patchJSON, "")
		})
	}
}

func TestInverseMoveInsideSameArrayAllIndexPairs(t *testing.T) {
	// Exhaustively move every element of a small array to every legal
	// position and confirm the inverse restores the exact original order.
	for length := 2; length <= 6; length++ {
		for from := 0; from < length; from++ {
			for to := 0; to < length; to++ {
				if from == to {
					continue
				}

				doc := `{"arr":[`
				for i := 0; i < length; i++ {
					if i > 0 {
						doc += ","
					}
					doc += fmt.Sprintf(`"v%d"`, i)
				}
				doc += `]}`

				patchJSON := fmt.Sprintf(
					`[{"op":"move","from":"/arr/%d","path":"/arr/%d"}]`,
					from, to,
				)

				patch, err := DecodePatch([]byte(patchJSON))
				if err != nil {
					t.Fatal(err)
				}

				result, inverse, err := patch.ApplyWithInverse([]byte(doc))
				if err != nil {
					t.Fatalf("move %d->%d in length %d: %v", from, to, length, err)
				}

				restored, err := inverse.Apply(result)
				if err != nil {
					t.Fatalf("inverse of move %d->%d in length %d: %v", from, to, length, err)
				}

				if !Equal(restored, []byte(doc)) {
					t.Fatalf("move %d->%d in length %d: inverse restored %s from %s",
						from, to, length, restored, result)
				}
			}
		}
	}
}

func TestInverseMoveAcrossContainers(t *testing.T) {
	// Move between two different arrays (insertion shifts both) and between
	// an array and an object key that already holds a value (overwrite).
	doc := `{"left":[1,2,3],"right":[4,5],"box":{"old":"keepme"}}`
	patchJSON := `[
		{"op":"move","from":"/left/1","path":"/right/0"},
		{"op":"move","from":"/right/2","path":"/box/old"}
	]`

	roundTrip(t, doc, patchJSON, `{"left":[1,3],"right":[2,4],"box":{"old":5}}`)
}

func TestInverseRootReplaced(t *testing.T) {
	cases := []struct {
		doc       string
		patchJSON string
		want      string
	}{
		{
			`{"old":"value","n":[1,2,3]}`,
			`[{"op":"replace","path":"","value":{"new":[true,null]}}]`,
			`{"new":[true,null]}`,
		},
		{
			`{"old":"value"}`,
			`[{"op":"add","path":"","value":[1,2,3]}]`,
			`[1,2,3]`,
		},
		{
			`[1,2,3]`,
			`[{"op":"replace","path":"","value":{"k":"v"}}]`,
			`{"k":"v"}`,
		},
	}

	for _, c := range cases {
		roundTrip(t, c.doc, c.patchJSON, c.want)
	}
}

func TestInverseTestFailureReturnsNoPatch(t *testing.T) {
	doc := `{"foo":"bar"}`
	patchJSON := `[
		{"op":"add","path":"/baz","value":"qux"},
		{"op":"test","path":"/foo","value":"wrong"}
	]`

	patch, err := DecodePatch([]byte(patchJSON))
	if err != nil {
		t.Fatal(err)
	}

	result, inverse, err := patch.ApplyWithInverse([]byte(doc))
	if err == nil {
		t.Fatalf("expected test failure, got result %s", result)
	}

	if inverse != nil {
		t.Fatalf("expected nil inverse on failed test, got %#v", inverse)
	}

	if result != nil {
		t.Fatalf("expected nil result on failed test, got %s", result)
	}
}

func TestInversePassingTestIsKeptAsGuard(t *testing.T) {
	doc := `{"foo":"bar","arr":[1,2]}`
	patchJSON := `[
		{"op":"test","path":"/foo","value":"bar"},
		{"op":"add","path":"/arr/-","value":3}
	]`

	patch, err := DecodePatch([]byte(patchJSON))
	if err != nil {
		t.Fatal(err)
	}

	result, inverse, err := patch.ApplyWithInverse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}

	if len(inverse) != 2 || inverse[0].Kind() != "remove" || inverse[1].Kind() != "test" {
		t.Fatalf("expected [remove, test] inverse, got %#v", inverse)
	}

	restored, err := inverse.Apply(result)
	if err != nil {
		t.Fatalf("inverse with test guard failed: %v", err)
	}

	if !Equal(restored, []byte(doc)) {
		t.Fatalf("restored %s, want %s", restored, doc)
	}
}

func TestInverseRemoveReplaceCopyAndEscapedKeys(t *testing.T) {
	doc := `{"a/b":[{"k~z":"v0"},{"second":[1,2]}],"drop":null}`
	patchJSON := `[
		{"op":"remove","path":"/a~1b/0/k~0z"},
		{"op":"replace","path":"/a~1b/1/second/0","value":"one"},
		{"op":"copy","from":"/drop","path":"/a~1b/-"}
	]`

	roundTrip(t, doc, patchJSON, `{"a/b":[{},{"second":["one",2]},null],"drop":null}`)
}

func TestInverseEmptyPatch(t *testing.T) {
	patch, err := DecodePatch([]byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}

	result, inverse, err := patch.ApplyWithInverse([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}

	if inverse == nil || len(inverse) != 0 {
		t.Fatalf("expected empty non-nil inverse, got %#v", inverse)
	}

	encoded, err := patchMarshal(inverse)
	if err != nil || string(encoded) != "[]" {
		t.Fatalf("expected inverse to marshal as [], got %s, err %v", encoded, err)
	}

	if !Equal(result, []byte(`{"a":1}`)) {
		t.Fatalf("document changed: %s", result)
	}
}

func TestInverseEnsurePathExistsOnAdd(t *testing.T) {
	// The patch creates two intermediate objects; the inverse must remove
	// them again rather than leaving empty containers behind.
	doc := `{"existing":1}`
	patchJSON := `[{"op":"add","path":"/a/b/c","value":"deep"}]`

	patch, err := DecodePatch([]byte(patchJSON))
	if err != nil {
		t.Fatal(err)
	}

	options := NewApplyOptions()
	options.EnsurePathExistsOnAdd = true

	result, inverse, err := patch.ApplyWithInverseAndOptions([]byte(doc), options)
	if err != nil {
		t.Fatal(err)
	}

	if !compareJSON(string(result), `{"existing":1,"a":{"b":{"c":"deep"}}}`) {
		t.Fatalf("unexpected result: %s", result)
	}

	restored, err := inverse.Apply(result)
	if err != nil {
		t.Fatal(err)
	}

	if !Equal(restored, []byte(doc)) {
		t.Fatalf("expected inverse to remove created parents, got %s", restored)
	}
}

func TestInverseFailureLeavesNoPartialPatch(t *testing.T) {
	// The first op succeeds, the second fails: the returned inverse must be
	// nil so callers cannot mistake a partial inverse for a full undo.
	doc := `{"arr":[1,2],"x":1}`
	patchJSON := `[
		{"op":"add","path":"/y","value":2},
		{"op":"remove","path":"/arr/9"}
	]`

	patch, err := DecodePatch([]byte(patchJSON))
	if err != nil {
		t.Fatal(err)
	}

	result, inverse, err := patch.ApplyWithInverse([]byte(doc))
	if err == nil {
		t.Fatalf("expected an error, got %s", result)
	}

	if inverse != nil {
		t.Fatalf("inverse must be nil when any operation fails, got %#v", inverse)
	}
}

func TestInverseArrayRootOperations(t *testing.T) {
	doc := `[{"name":"a"},{"name":"b"}]`
	patchJSON := `[
		{"op":"add","path":"/1/name","value":"B"},
		{"op":"remove","path":"/0"}
	]`

	roundTrip(t, doc, patchJSON, `[{"name":"B"}]`)
}
