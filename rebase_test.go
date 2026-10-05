package jsonpatch

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestRebasePatchAutomaticCases(t *testing.T) {
	cases := []struct {
		name      string
		original  string
		patchA    string
		patchB    string
		rebased   string
		finalJSON string
	}{
		{
			name:      "independent object fields",
			original:  `{"a":1,"b":2}`,
			patchA:    `[{"op":"replace","path":"/a","value":10}]`,
			patchB:    `[{"op":"replace","path":"/b","value":20}]`,
			rebased:   `[{"op":"replace","path":"/b","value":20}]`,
			finalJSON: `{"a":10,"b":20}`,
		},
		{
			name:      "object key pointer is escaped",
			original:  `{"a/b":1}`,
			patchA:    `[{"op":"add","path":"/other","value":true}]`,
			patchB:    `[{"op":"replace","path":"/a~1b","value":2}]`,
			rebased:   `[{"op":"replace","path":"/a~1b","value":2}]`,
			finalJSON: `{"a/b":2,"other":true}`,
		},
		{
			name:      "insert before shifts B index",
			original:  `[1,2,3]`,
			patchA:    `[{"op":"add","path":"/0","value":0}]`,
			patchB:    `[{"op":"replace","path":"/1","value":20}]`,
			rebased:   `[{"op":"replace","path":"/2","value":20}]`,
			finalJSON: `[0,1,20,3]`,
		},
		{
			name:      "delete earlier shifts B remove index",
			original:  `[1,2,3]`,
			patchA:    `[{"op":"remove","path":"/0"}]`,
			patchB:    `[{"op":"remove","path":"/2"}]`,
			rebased:   `[{"op":"remove","path":"/1"}]`,
			finalJSON: `[2]`,
		},
		{
			name:      "unrelated delete keeps targeted element",
			original:  `[1,2,3]`,
			patchA:    `[{"op":"remove","path":"/1"}]`,
			patchB:    `[{"op":"replace","path":"/0","value":10}]`,
			rebased:   `[{"op":"replace","path":"/0","value":10}]`,
			finalJSON: `[10,3]`,
		},
		{
			name:      "delete insert anchors keeps boundary",
			original:  `[1,2,3,4,5]`,
			patchA:    `[{"op":"remove","path":"/1"},{"op":"remove","path":"/1"}]`,
			patchB:    `[{"op":"add","path":"/2","value":"x"}]`,
			rebased:   `[{"op":"add","path":"/1","value":"x"}]`,
			finalJSON: `[1,"x",4,5]`,
		},
		{
			name:      "A moves B target object",
			original:  `{"items":[{"id":1},{"id":2}],"selected":null}`,
			patchA:    `[{"op":"move","from":"/items/1","path":"/selected"}]`,
			patchB:    `[{"op":"replace","path":"/items/1/id","value":20}]`,
			rebased:   `[{"op":"replace","path":"/selected/id","value":20}]`,
			finalJSON: `{"items":[{"id":1}],"selected":{"id":20}}`,
		},
		{
			name:      "same value replacement is redundant",
			original:  `{"a":1}`,
			patchA:    `[{"op":"replace","path":"/a","value":10}]`,
			patchB:    `[{"op":"replace","path":"/a","value":10}]`,
			rebased:   `[]`,
			finalJSON: `{"a":10}`,
		},
		{
			name:      "same remove is redundant",
			original:  `{"a":1,"b":2}`,
			patchA:    `[{"op":"remove","path":"/a"}]`,
			patchB:    `[{"op":"remove","path":"/a"}]`,
			rebased:   `[]`,
			finalJSON: `{"b":2}`,
		},
		{
			name:      "array move keeps identity",
			original:  `{"items":["a","b","c"]}`,
			patchA:    `[{"op":"remove","path":"/items/0"}]`,
			patchB:    `[{"op":"move","from":"/items/1","path":"/items/2"}]`,
			rebased:   `[{"op":"move","from":"/items/0","path":"/items/1"}]`,
			finalJSON: `{"items":["c","b"]}`,
		},
		{
			name:      "insert before same array move",
			original:  `["a","b","c"]`,
			patchA:    `[{"op":"add","path":"/0","value":"z"}]`,
			patchB:    `[{"op":"move","from":"/2","path":"/1"}]`,
			rebased:   `[{"op":"move","from":"/3","path":"/2"}]`,
			finalJSON: `["z","a","c","b"]`,
		},
		{
			name:      "copy follows moved source",
			original:  `{"items":[{"id":1},{"id":2}],"copy":null}`,
			patchA:    `[{"op":"move","from":"/items/0","path":"/selected"}]`,
			patchB:    `[{"op":"copy","from":"/items/0","path":"/copy"}]`,
			rebased:   `[{"op":"copy","from":"/selected","path":"/copy"}]`,
			finalJSON: `{"copy":{"id":1},"items":[{"id":2}],"selected":{"id":1}}`,
		},
		{
			name:      "copy includes A edit",
			original:  `{"item":{"id":1,"v":1},"copy":null}`,
			patchA:    `[{"op":"replace","path":"/item/v","value":10}]`,
			patchB:    `[{"op":"copy","from":"/item","path":"/copy"}]`,
			rebased:   `[{"op":"copy","from":"/item","path":"/copy"}]`,
			finalJSON: `{"copy":{"id":1,"v":10},"item":{"id":1,"v":10}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rebased, err := RebasePatch([]byte(tc.original), []byte(tc.patchA), []byte(tc.patchB))
			if err != nil {
				t.Fatalf("RebasePatch returned error: %v", err)
			}

			got, err := json.Marshal(rebased)
			if err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(string(got), tc.rebased) {
				t.Fatalf("rebased patch = %s, want %s", got, tc.rebased)
			}

			appliedA, err := applyPatch(tc.original, tc.patchA)
			if err != nil {
				t.Fatal(err)
			}
			final, err := rebased.Apply([]byte(appliedA))
			if err != nil {
				t.Fatalf("rebased patch does not apply: %v", err)
			}
			if !compareJSON(string(final), tc.finalJSON) {
				t.Fatalf("final = %s, want %s", final, tc.finalJSON)
			}
		})
	}
}

func TestRebasePatchConflicts(t *testing.T) {
	cases := []struct {
		name     string
		original string
		patchA   string
		patchB   string
		conflict RebaseConflict
	}{
		{
			name:     "same leaf different values",
			original: `{"a":1}`,
			patchA:   `[{"op":"replace","path":"/a","value":10}]`,
			patchB:   `[{"op":"replace","path":"/a","value":20}]`,
			conflict: RebaseConflict{AOperationIndex: 0, BOperationIndex: 0, Path: "/a"},
		},
		{
			name:     "A deletes ancestor B modifies",
			original: `{"parent":{"child":1}}`,
			patchA:   `[{"op":"remove","path":"/parent"}]`,
			patchB:   `[{"op":"replace","path":"/parent/child","value":2}]`,
			conflict: RebaseConflict{AOperationIndex: 0, BOperationIndex: 0, Path: "/parent/child"},
		},
		{
			name:     "A replaces ancestor B modifies",
			original: `{"parent":{"child":1}}`,
			patchA:   `[{"op":"replace","path":"/parent","value":{"other":2}}]`,
			patchB:   `[{"op":"replace","path":"/parent/child","value":3}]`,
			conflict: RebaseConflict{AOperationIndex: 0, BOperationIndex: 0, Path: "/parent/child"},
		},
		{
			name:     "A edits subtree B replaces",
			original: `{"parent":{"a":1,"b":2}}`,
			patchA:   `[{"op":"replace","path":"/parent/a","value":10}]`,
			patchB:   `[{"op":"replace","path":"/parent","value":{"a":20,"b":2}}]`,
			conflict: RebaseConflict{AOperationIndex: 0, BOperationIndex: 0, Path: "/parent"},
		},
		{
			name:     "A modifies node B removes",
			original: `{"a":1}`,
			patchA:   `[{"op":"replace","path":"/a","value":10}]`,
			patchB:   `[{"op":"remove","path":"/a"}]`,
			conflict: RebaseConflict{AOperationIndex: 0, BOperationIndex: 0, Path: "/a"},
		},
		{
			name:     "operation indexes preserved",
			original: `{"a":1,"b":2}`,
			patchA:   `[{"op":"test","path":"/b","value":2},{"op":"replace","path":"/a","value":10}]`,
			patchB:   `[{"op":"test","path":"/b","value":2},{"op":"replace","path":"/a","value":20}]`,
			conflict: RebaseConflict{AOperationIndex: 1, BOperationIndex: 1, Path: "/a"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RebasePatch([]byte(tc.original), []byte(tc.patchA), []byte(tc.patchB))
			if err == nil {
				t.Fatal("expected conflict, got nil")
			}

			var conflictErr *ConflictError
			if !errors.As(err, &conflictErr) {
				t.Fatalf("error = %T %v, want *ConflictError", err, err)
			}
			if len(conflictErr.Conflicts) != 1 {
				t.Fatalf("conflicts = %d, want 1", len(conflictErr.Conflicts))
			}

			got := conflictErr.Conflicts[0]
			if got.AOperationIndex != tc.conflict.AOperationIndex ||
				got.BOperationIndex != tc.conflict.BOperationIndex ||
				got.Path != tc.conflict.Path {
				t.Fatalf("conflict = %#v, want indexes (%d,%d) path %q", got, tc.conflict.AOperationIndex, tc.conflict.BOperationIndex, tc.conflict.Path)
			}
		})
	}

	t.Run("all conflicts are collected with both indexes", func(t *testing.T) {
		original := `{"a":1,"b":2,"parent":{"child":3}}`
		patchA := `[{"op":"replace","path":"/a","value":10},{"op":"remove","path":"/parent"},{"op":"replace","path":"/b","value":20}]`
		patchB := `[{"op":"replace","path":"/a","value":11},{"op":"replace","path":"/parent/child","value":30},{"op":"replace","path":"/b","value":21}]`

		_, err := RebasePatch([]byte(original), []byte(patchA), []byte(patchB))
		if err == nil {
			t.Fatal("expected conflicts, got nil")
		}

		var conflictErr *ConflictError
		if !errors.As(err, &conflictErr) {
			t.Fatalf("error = %T %v, want *ConflictError", err, err)
		}

		got := [][2]int{}
		for _, conflict := range conflictErr.Conflicts {
			got = append(got, [2]int{conflict.AOperationIndex, conflict.BOperationIndex})
		}
		want := [][2]int{{0, 0}, {1, 1}, {2, 2}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("indexes = %v, want %v", got, want)
		}
	})

	t.Run("conflict does not hide later independent operation", func(t *testing.T) {
		original := `{"a":{"x":1},"b":2}`
		patchA := `[{"op":"replace","path":"/a/x","value":10},{"op":"replace","path":"/b","value":20}]`
		patchB := `[{"op":"replace","path":"/a/x","value":11},{"op":"replace","path":"/b","value":21}]`

		_, err := RebasePatch([]byte(original), []byte(patchA), []byte(patchB))
		if err == nil {
			t.Fatal("expected conflicts")
		}

		var conflictErr *ConflictError
		errors.As(err, &conflictErr)
		if conflictErr == nil || len(conflictErr.Conflicts) != 2 {
			t.Fatalf("conflicts = %#v, want 2", err)
		}
	})
}

func jsonEqual(a, b string) bool {
	var va, vb interface{}
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
