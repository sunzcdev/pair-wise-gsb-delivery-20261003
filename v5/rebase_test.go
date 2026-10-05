package jsonpatch

import (
	"testing"

	"github.com/evanphx/json-patch/v5/internal/json"
)

func TestRebasePatchAutomaticCases(t *testing.T) {
	tests := []struct {
		name     string
		original string
		patchA   string
		patchB   string
		merged   string
		expected Patch
	}{
		{
			name:     "independent object fields",
			original: `{"name":"old","age":1}`,
			patchA:   `[{"op":"replace","path":"/name","value":"A"}]`,
			patchB:   `[{"op":"replace","path":"/age","value":2}]`,
			merged:   `{"name":"A","age":2}`,
			expected: Patch{
				{"op": stringRaw("replace"), "path": stringRaw("/age"), "value": rawMessage(`2`)},
			},
		},
		{
			name:     "array insertion shifts B's element reference",
			original: `{"items":[1,2,3]}`,
			patchA:   `[{"op":"add","path":"/items/0","value":0}]`,
			patchB:   `[{"op":"replace","path":"/items/1","value":"B"}]`,
			merged:   `{"items":[0,1,"B",3]}`,
			expected: Patch{
				{"op": stringRaw("replace"), "path": stringRaw("/items/2"), "value": rawMessage(`"B"`)},
			},
		},
		{
			name:     "array deletion shifts B's element reference",
			original: `{"items":[1,2,3]}`,
			patchA:   `[{"op":"remove","path":"/items/1"}]`,
			patchB:   `[{"op":"replace","path":"/items/2","value":"B"}]`,
			merged:   `{"items":[1,"B"]}`,
			expected: Patch{
				{"op": stringRaw("replace"), "path": stringRaw("/items/1"), "value": rawMessage(`"B"`)},
			},
		},
		{
			name:     "array move retargets by element identity",
			original: `{"items":[{"id":1,"name":"one"},{"id":2,"name":"two"},{"id":3,"name":"three"}]}`,
			patchA:   `[{"op":"move","from":"/items/1","path":"/items/2"}]`,
			patchB:   `[{"op":"replace","path":"/items/1/name","value":"B"}]`,
			merged:   `{"items":[{"id":1,"name":"one"},{"id":3,"name":"three"},{"id":2,"name":"B"}]}`,
			expected: Patch{
				{"op": stringRaw("replace"), "path": stringRaw("/items/2/name"), "value": rawMessage(`"B"`)},
			},
		},
		{
			name:     "matching deletion is dropped",
			original: `{"items":[1,2,3],"keep":true}`,
			patchA:   `[{"op":"remove","path":"/items/1"}]`,
			patchB:   `[{"op":"remove","path":"/items/1"},{"op":"replace","path":"/keep","value":false}]`,
			merged:   `{"items":[1,3],"keep":false}`,
			expected: Patch{
				{"op": stringRaw("replace"), "path": stringRaw("/keep"), "value": rawMessage(`false`)},
			},
		},
		{
			name:     "object move follows unrelated object add",
			original: `{"a":{"id":1},"b":2}`,
			patchA:   `[{"op":"add","path":"/c","value":3}]`,
			patchB:   `[{"op":"move","from":"/a","path":"/d"}]`,
			merged:   `{"b":2,"c":3,"d":{"id":1}}`,
			expected: Patch{
				{"op": stringRaw("move"), "from": stringRaw("/a"), "path": stringRaw("/d")},
			},
		},
		{
			name:     "copy source and destination both shift",
			original: `{"items":[1,2]}`,
			patchA:   `[{"op":"add","path":"/items/0","value":0}]`,
			patchB:   `[{"op":"copy","from":"/items/1","path":"/items/2"}]`,
			merged:   `{"items":[0,1,2,2]}`,
			expected: Patch{
				{"op": stringRaw("copy"), "from": stringRaw("/items/2"), "path": stringRaw("/items/3")},
			},
		},
		{
			name:     "copy follows element moved by A",
			original: `{"items":[{"id":1},{"id":2}],"copy":null}`,
			patchA:   `[{"op":"move","from":"/items/1","path":"/moved"}]`,
			patchB:   `[{"op":"copy","from":"/items/1","path":"/copy"}]`,
			merged:   `{"items":[{"id":1}],"copy":{"id":2},"moved":{"id":2}}`,
			expected: Patch{
				{"op": stringRaw("copy"), "from": stringRaw("/moved"), "path": stringRaw("/copy")},
			},
		},
		{
			name:     "same-array move after insertion maps insertion slot",
			original: `{"v":[1,2,3]}`,
			patchA:   `[{"op":"add","path":"/v/0","value":0}]`,
			patchB:   `[{"op":"move","from":"/v/1","path":"/v/2"}]`,
			merged:   `{"v":[0,1,3,2]}`,
			expected: Patch{
				{"op": stringRaw("move"), "from": stringRaw("/v/2"), "path": stringRaw("/v/3")},
			},
		},
		{
			name:     "backward move after deletion maps identity",
			original: `{"v":[1,2,3,4]}`,
			patchA:   `[{"op":"remove","path":"/v/0"}]`,
			patchB:   `[{"op":"move","from":"/v/2","path":"/v/0"}]`,
			merged:   `{"v":[3,2,4]}`,
			expected: Patch{
				{"op": stringRaw("move"), "from": stringRaw("/v/1"), "path": stringRaw("/v/0")},
			},
		},
		{
			name:     "cross-array move maps source identity",
			original: `{"a":[1,2],"b":[]}`,
			patchA:   `[{"op":"add","path":"/a/0","value":0}]`,
			patchB:   `[{"op":"move","from":"/a/1","path":"/b/0"}]`,
			merged:   `{"a":[0,1],"b":[2]}`,
			expected: Patch{
				{"op": stringRaw("move"), "from": stringRaw("/a/2"), "path": stringRaw("/b/0")},
			},
		},
		{
			name:     "array add keeps existing element at destination index",
			original: `{"items":[1,2,3]}`,
			patchA:   `[{"op":"add","path":"/items/1","value":0}]`,
			patchB:   `[{"op":"replace","path":"/items/2","value":"B"}]`,
			merged:   `{"items":[1,0,2,"B"]}`,
			expected: Patch{
				{"op": stringRaw("replace"), "path": stringRaw("/items/3"), "value": rawMessage(`"B"`)},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rebasedData, err := RebasePatch([]byte(test.original), []byte(test.patchA), []byte(test.patchB))
			if err != nil {
				t.Fatalf("RebasePatch returned error: %v", err)
			}
			var got Patch
			if err := json.Unmarshal(rebasedData, &got); err != nil {
				t.Fatal(err)
			}
			wantData, _ := json.Marshal(test.expected)
			if !Equal(rebasedData, wantData) {
				wantData, _ := json.Marshal(test.expected)
				t.Fatalf("rebased patch = %s, want %s", rebasedData, wantData)
			}

			patchA := mustDecodePatch(t, test.patchA)
			rebased := mustDecodePatch(t, string(rebasedData))
			aResult := mustApply(t, test.original, patchA)
			gotResult := mustApply(t, string(aResult), rebased)
			if !Equal(gotResult, []byte(test.merged)) {
				t.Fatalf("rebased result = %s, want merged result %s", gotResult, test.merged)
			}
		})
	}
}

func TestRebasePatchManualConflicts(t *testing.T) {
	tests := []struct {
		name       string
		original   string
		patchA     string
		patchB     string
		wantReason string
		wantPath   string
		wantA      int
		wantB      int
	}{
		{
			name:       "A replaces B target with different value",
			original:   `{"name":"old","root":{"value":1}}`,
			patchA:     `[{"op":"replace","path":"/root","value":{"value":2}}]`,
			patchB:     `[{"op":"replace","path":"/root","value":{"value":3}}]`,
			wantReason: rebaseTargetChanged,
			wantPath:   "/root",
			wantA:      1,
			wantB:      1,
		},
		{
			name:       "A deletes B element",
			original:   `{"items":[1,2,3]}`,
			patchA:     `[{"op":"remove","path":"/items/1"}]`,
			patchB:     `[{"op":"replace","path":"/items/1","value":"B"}]`,
			wantReason: rebaseTargetDeleted,
			wantPath:   "/items/1",
			wantA:      1,
			wantB:      1,
		},
		{
			name:       "A deletes ancestor of B operation",
			original:   `{"profile":{"name":"old"}}`,
			patchA:     `[{"op":"remove","path":"/profile"}]`,
			patchB:     `[{"op":"replace","path":"/profile/name","value":"B"}]`,
			wantReason: rebaseAncestorDeleted,
			wantPath:   "/profile/name",
			wantA:      1,
			wantB:      1,
		},
		{
			name:       "conflict preserves later operation numbers",
			original:   `{"a":1,"b":2,"profile":{"name":"old"}}`,
			patchA:     `[{"op":"replace","path":"/a","value":2},{"op":"remove","path":"/profile"}]`,
			patchB:     `[{"op":"replace","path":"/b","value":3},{"op":"replace","path":"/profile/name","value":"B"}]`,
			wantReason: rebaseAncestorDeleted,
			wantPath:   "/profile/name",
			wantA:      2,
			wantB:      2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := RebasePatch([]byte(test.original), []byte(test.patchA), []byte(test.patchB))
			conflict, ok := err.(*RebaseConflict)
			if !ok {
				t.Fatalf("error = %#v, want *RebaseConflict", err)
			}
			if conflict.Reason != test.wantReason || conflict.Path != test.wantPath ||
				conflict.AOperation != test.wantA || conflict.BOperation != test.wantB {
				t.Fatalf("conflict = %#v, want reason %q path %q A=%d B=%d",
					conflict, test.wantReason, test.wantPath, test.wantA, test.wantB)
			}
		})
	}
}

func rawMessage(value string) *json.RawMessage {
	raw := json.RawMessage(value)
	return &raw
}

func mustDecodePatch(t *testing.T, data string) Patch {
	t.Helper()
	patch, err := DecodePatch([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return patch
}

func mustApply(t *testing.T, doc string, patch Patch) []byte {
	t.Helper()
	result, err := patch.Apply([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return result
}
