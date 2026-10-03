package sjson

import (
	"testing"

	"github.com/tidwall/gjson"
)

func setPath(t *testing.T, json string, path Path, value interface{}, expect string) {
	t.Helper()
	got, err := SetPaths(json, path, value)
	if err != nil {
		t.Fatalf("SetPaths(%q): %v", path, err)
	}
	if got != expect {
		t.Fatalf("SetPaths(%q): expected %q, got %q", path, expect, got)
	}

	gotb, err := SetBytesPaths([]byte(json), path, value)
	if err != nil {
		t.Fatalf("SetBytesPaths(%q): %v", path, err)
	}
	if string(gotb) != expect {
		t.Fatalf("SetBytesPaths(%q): expected %q, got %q", path, expect, string(gotb))
	}
}

func setPathErr(t *testing.T, json string, path Path, value interface{}) {
	t.Helper()
	if _, err := SetPaths(json, path, value); err == nil {
		t.Fatalf("SetPaths(%q): expected an error, got nil", path)
	}
	if _, err := SetBytesPaths([]byte(json), path, value); err == nil {
		t.Fatalf("SetBytesPaths(%q): expected an error, got nil", path)
	}
}

func deletePath(t *testing.T, json string, path Path, expect string) {
	t.Helper()
	got, err := DeletePaths(json, path)
	if err != nil {
		t.Fatalf("DeletePaths(%q): %v", path, err)
	}
	if got != expect {
		t.Fatalf("DeletePaths(%q): expected %q, got %q", path, expect, got)
	}
	gotb, err := DeleteBytesPaths([]byte(json), path)
	if err != nil {
		t.Fatalf("DeleteBytesPaths(%q): %v", path, err)
	}
	if string(gotb) != expect {
		t.Fatalf("DeleteBytesPaths(%q): expected %q, got %q", path, expect, string(gotb))
	}
}

func deletePathErr(t *testing.T, json string, path Path) {
	t.Helper()
	if _, err := DeletePaths(json, path); err == nil {
		t.Fatalf("DeletePaths(%q): expected an error, got nil", path)
	}
}

func TestTypedBasicSet(t *testing.T) {
	setPath(t, `{"a":1}`, Path{Key("b")}, 2, `{"a":1,"b":2}`)
	setPath(t, `[1,2]`, Path{Index(0)}, 9, `[9,2]`)
	setPath(t, `[1,2]`, Path{Append()}, 3, `[1,2,3]`)

	// Nested traversal.
	setPath(t, `{"a":{"b":1}}`, Path{Key("a"), Key("b")}, 2,
		`{"a":{"b":2}}`)
	setPath(t, `{"a":[1,2]}`, Path{Key("a"), Index(1)}, 9,
		`{"a":[1,9]}`)
	setPath(t, `[{"a":1}]`, Path{Index(0), Key("a")}, 2,
		`[{"a":2}]`)
}

func TestTypedLiteralKeys(t *testing.T) {
	// "0" is an object key, not an array index.
	setPath(t, `{"0":1}`, Path{Key("0")}, 2, `{"0":2}`)
	// "a.b" is a single object key, not two segments.
	setPath(t, `{"a.b":1}`, Path{Key("a.b")}, 2, `{"a.b":2}`)
	// "#" is an object key, not a gjson query.
	setPath(t, `{"#":1}`, Path{Key("#")}, 2, `{"#":2}`)
	// The empty string is a valid object key.
	setPath(t, `{"":1}`, Path{Key("")}, 2, `{"":2}`)

	// Creating the same special keys from scratch.
	setPath(t, `{}`, Path{Key("0")}, 1, `{"0":1}`)
	setPath(t, `{}`, Path{Key("a.b")}, 1, `{"a.b":1}`)
	setPath(t, `{}`, Path{Key("#")}, 1, `{"#":1}`)
	setPath(t, `{}`, Path{Key("")}, 1, `{"":1}`)

	// Special key nested under a normal key.
	setPath(t, `{"x":{}}`, Path{Key("x"), Key("a.b")}, 1,
		`{"x":{"a.b":1}}`)
}

func TestTypedContainerCreation(t *testing.T) {
	// Next segment is a key -> object container is created.
	setPath(t, ``, Path{Key("a"), Key("b")}, 1, `{"a":{"b":1}}`)
	// Next segment is an index -> array container is created.
	setPath(t, ``, Path{Key("a"), Index(0)}, 1, `{"a":[1]}`)
	// Next segment is an append -> array container is created.
	setPath(t, ``, Path{Key("a"), Append()}, 1, `{"a":[1]}`)

	// Root container follows the first segment type.
	setPath(t, ``, Path{Index(0)}, 1, `[1]`)
	setPath(t, ``, Path{Append()}, 1, `[1]`)
	setPath(t, ``, Path{Key("a")}, 1, `{"a":1}`)

	// Deep chains choose each container from its child segment.
	setPath(t, ``, Path{Key("a"), Index(0), Key("b"), Append()}, 1,
		`{"a":[{"b":[1]}]}`)
}

func TestTypedMissingParents(t *testing.T) {
	setPath(t, `{"a":{}}`, Path{Key("a"), Key("b"), Key("c")}, 1,
		`{"a":{"b":{"c":1}}}`)
	setPath(t, `{"a":[]}`, Path{Key("a"), Index(0), Key("b")}, 1,
		`{"a":[{"b":1}]}`)
	setPath(t, `{"a":[]}`, Path{Key("a"), Append(), Key("b")}, 1,
		`{"a":[{"b":1}]}`)
}

func TestTypedArrayPadding(t *testing.T) {
	// Setting a new index past the end pads with null.
	setPath(t, `[1,2]`, Path{Index(4)}, 5, `[1,2,null,null,5]`)
	setPath(t, ``, Path{Index(4)}, 5, `[null,null,null,null,5]`)
	setPath(t, `[]`, Path{Index(2)}, 5, `[null,null,5]`)
	setPath(t, `[1]`, Path{Index(2)}, 5, `[1,null,5]`)

	// Padding a nested newly created array.
	setPath(t, `{"a":[1]}`, Path{Key("a"), Index(3)}, 5,
		`{"a":[1,null,null,5]}`)
	// A fresh array created because the next segment is an index.
	setPath(t, ``, Path{Key("a"), Index(2)}, 5,
		`{"a":[null,null,5]}`)
}

func TestTypedAppend(t *testing.T) {
	setPath(t, `[]`, Path{Append()}, "x", `["x"]`)
	setPath(t, `[1,2]`, Path{Append()}, 3, `[1,2,3]`)
	setPath(t, `{"a":[1]}`, Path{Key("a"), Append()}, 2,
		`{"a":[1,2]}`)
	// Appending after a complex last element keeps it intact.
	setPath(t, `[{"x":1}]`, Path{Append()}, 2, `[{"x":1},2]`)
	// Whitespace around members is tolerated.
	setPath(t, `[ 1 , 2 ]`, Path{Append()}, 3, `[ 1 , 2 ,3]`)
	// Append always grows; repeated appends stack.
	out, err := SetPaths(`[]`, Path{Append()}, 1)
	if err != nil {
		t.Fatal(err)
	}
	out, err = SetPaths(out, Path{Append()}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if out != `[1,2]` {
		t.Fatalf("expected [1,2], got %q", out)
	}
}

func TestTypedConflicts(t *testing.T) {
	// A key segment cannot be applied to an existing array.
	setPathErr(t, `[]`, Path{Key("a")}, 1)
	setPathErr(t, `[1]`, Path{Key("a")}, 1)
	setPathErr(t, `{"a":[]}`, Path{Key("a"), Key("b")}, 1)

	// An index/append segment cannot be applied to an existing object.
	setPathErr(t, `{}`, Path{Index(0)}, 1)
	setPathErr(t, `{"a":{}}`, Path{Key("a"), Index(0)}, 1)
	setPathErr(t, `{}`, Path{Append()}, 1)

	// Descending through a scalar must not replace the whole subtree.
	setPathErr(t, `{"a":1}`, Path{Key("a"), Key("b")}, 1)
	setPathErr(t, `{"a":1}`, Path{Key("a"), Index(0)}, 1)
	setPathErr(t, `{"a":[1,2]}`, Path{Key("a"), Index(0), Key("b")}, 1)

	// A scalar document must never be overwritten.
	setPathErr(t, `5`, Path{Key("a")}, 1)
	setPathErr(t, `"str"`, Path{Index(0)}, 1)
	setPathErr(t, `true`, Path{Append()}, 1)

	// Conflicts are also reported for deletes.
	deletePathErr(t, `[]`, Path{Key("a")})
	deletePathErr(t, `{}`, Path{Index(0)})
	deletePathErr(t, `{"a":1}`, Path{Key("a"), Key("b")})
	deletePathErr(t, `5`, Path{Key("a")})
}

func TestTypedDelete(t *testing.T) {
	deletePath(t, `{"a":1,"b":2}`, Path{Key("a")}, `{"b":2}`)
	deletePath(t, `[1,2,3]`, Path{Index(0)}, `[2,3]`)
	deletePath(t, `[1,2,3]`, Path{Index(1)}, `[1,3]`)

	// Append segment deletes the current last element.
	deletePath(t, `[1,2,3]`, Path{Append()}, `[1,2]`)
	deletePath(t, `{"a":[1,2]}`, Path{Key("a"), Append()}, `{"a":[1]}`)

	// Nested delete.
	deletePath(t, `{"a":{"b":1}}`, Path{Key("a"), Key("b")}, `{"a":{}}`)

	// Literal special keys are reachable for deletion.
	deletePath(t, `{"0":1}`, Path{Key("0")}, `{}`)
	deletePath(t, `{"a.b":1}`, Path{Key("a.b")}, `{}`)
	deletePath(t, `{"#":1}`, Path{Key("#")}, `{}`)
	deletePath(t, `{"":1}`, Path{Key("")}, `{}`)
}

func TestTypedDeleteOutOfRangeAndMissing(t *testing.T) {
	// Deleting a missing object key is a no-op.
	deletePath(t, `{"a":1}`, Path{Key("b")}, `{"a":1}`)
	deletePath(t, `{"a":{"b":1}}`, Path{Key("a"), Key("c")},
		`{"a":{"b":1}}`)

	// Deleting an index at or past the length is a no-op.
	deletePath(t, `[1,2]`, Path{Index(2)}, `[1,2]`)
	deletePath(t, `[1,2]`, Path{Index(9)}, `[1,2]`)
	deletePath(t, `[]`, Path{Index(0)}, `[]`)

	// Deleting via append on an empty array is a no-op.
	deletePath(t, `[]`, Path{Append()}, `[]`)
	deletePath(t, `{"a":[]}`, Path{Key("a"), Append()}, `{"a":[]}`)

	// Deleting anything from an empty document is a no-op.
	deletePath(t, ``, Path{Key("a")}, ``)
	deletePath(t, `   `, Path{Index(0)}, `   `)
}

func TestTypedStringSemanticsUnchanged(t *testing.T) {
	// The string entry keeps its legacy behavior: numeric text reaches
	// array elements and "-1" appends.
	got, err := Set(`[1,2]`, "0", 9)
	if err != nil || got != `[9,2]` {
		t.Fatalf("legacy numeric index broken: %q %v", got, err)
	}
	got, err = Set(`[1]`, "-1", 2)
	if err != nil || got != `[1,2]` {
		t.Fatalf("legacy -1 append broken: %q %v", got, err)
	}
	// Legacy string API replaces an incompatible scalar subtree.
	got, err = Set(`{"a":1}`, "a.b", 2)
	if err != nil || got != `{"a":{"b":2}}` {
		t.Fatalf("legacy scalar overwrite changed: %q %v", got, err)
	}
}

func TestTypedRawAndWhitespace(t *testing.T) {
	got, err := SetRawPaths(`{"a":1}`, Path{Key("b")}, `{"c":2}`)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.Valid(got) || gjson.Get(got, "b.c").Int() != 2 {
		t.Fatalf("unexpected raw result: %q", got)
	}

	// Surrounding whitespace is preserved.
	setPath(t, `  {"a":1}  `, Path{Key("b")}, 2, `  {"a":1,"b":2}  `)
	setPath(t, ` [1] `, Path{Append()}, 2, ` [1,2] `)
}

func TestTypedChaining(t *testing.T) {
	path := Path{}.Key("users").Key("a.b").Index(2).Append()
	setPath(t, ``, path, 1, `{"users":{"a.b":[null,null,[1]]}}`)

	// Negative indices are rejected at construction.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for negative index")
			}
		}()
		Index(-1)
	}()
}

func TestTypedEmptyPath(t *testing.T) {
	if _, err := SetPaths(`{}`, nil, 1); err == nil {
		t.Fatal("expected error for empty path")
	}
	if _, err := DeletePaths(`{}`, Path{}); err == nil {
		t.Fatal("expected error for empty path")
	}
}
