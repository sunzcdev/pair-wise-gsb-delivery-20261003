package sjson

import (
	"errors"
	"testing"

	"github.com/tidwall/gjson"
)

func TestUpdateOrderArrayIndexShift(t *testing.T) {
	// Appending with "-1" grows the array, so the following operation can
	// target the brand new index 3.
	res, err := Update(`{"arr":["a","b","c"]}`,
		UpdateOp{Op: OpSetRaw, Path: "arr.-1", Raw: `"d"`},
		UpdateOp{Op: OpSetRaw, Path: "arr.3", Raw: `"D"`},
	)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.Get(res, "arr").Raw != `["a","b","c","D"]` {
		t.Fatalf("unexpected result: %s", res)
	}

	// Delete shifts too: after deleting index 0 the old index 1 ("b")
	// is now at index 0.
	res, err = Update(`{"arr":["a","b","c"]}`,
		UpdateOp{Op: OpDelete, Path: "arr.0"},
		UpdateOp{Op: OpSetRaw, Path: "arr.0", Raw: `"B"`},
	)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.Get(res, "arr").Raw != `["B","c"]` {
		t.Fatalf("unexpected result: %s", res)
	}
}

func TestUpdateOrderDependsOnCreatedParent(t *testing.T) {
	// The second update creates the parent object that the third update
	// depends on.
	res, err := Update(`{}`,
		UpdateOp{Op: OpSetRaw, Path: "a.x", Raw: `1`},
		UpdateOp{Op: OpSetRaw, Path: "a.y", Raw: `{"z":2}`},
		UpdateOp{Op: OpSet, Path: "a.y.w", Value: 3},
		UpdateOp{Op: OpDelete, Path: "a.x"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if sortJSON(res) != `{"a":{"y":{"w":3,"z":2}}}` {
		t.Fatalf("unexpected result: %s", res)
	}
}

func TestUpdateCheckIntermediate(t *testing.T) {
	// Checks see the intermediate result: after the set, x is 10;
	// after the delete, z must be missing.
	res, err := Update(`{"x":1,"z":0}`,
		UpdateOp{Op: OpSetRaw, Path: "x", Raw: `10`},
		UpdateOp{Op: OpCheck, Path: "x", Raw: `10`},
		UpdateOp{Op: OpDelete, Path: "z"},
		UpdateOp{Op: OpCheck, Path: "z", Missing: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if sortJSON(res) != `{"x":10}` {
		t.Fatalf("unexpected result: %s", res)
	}

	// A failing check aborts the batch.
	_, err = Update(`{"x":1}`,
		UpdateOp{Op: OpSetRaw, Path: "x", Raw: `10`},
		UpdateOp{Op: OpCheck, Path: "x", Raw: `11`},
		UpdateOp{Op: OpSetRaw, Path: "y", Raw: `1`},
	)
	var uerr *UpdateError
	if !errors.As(err, &uerr) || uerr.OpIndex != 1 {
		t.Fatalf("expected UpdateError at index 1, got %v", err)
	}
}

func TestUpdateCheckMissingVsNull(t *testing.T) {
	// Explicit null exists; a missing check must fail while a raw-null
	// check must succeed.
	_, err := Update(`{"x":null}`,
		UpdateOp{Op: OpCheck, Path: "x", Missing: true},
	)
	if err == nil {
		t.Fatal("expected missing-check to fail on null")
	}

	res, err := Update(`{"x":null}`,
		UpdateOp{Op: OpCheck, Path: "x", Raw: `null`},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res != `{"x":null}` {
		t.Fatalf("unexpected result: %s", res)
	}

	// A truly missing path passes the missing check but fails a raw check.
	_, err = Update(`{}`,
		UpdateOp{Op: OpCheck, Path: "x", Raw: `null`},
	)
	if err == nil {
		t.Fatal("expected raw-null check to fail on missing path")
	}
	res, err = Update(`{}`,
		UpdateOp{Op: OpCheck, Path: "x", Missing: true},
	)
	if err != nil || res != `{}` {
		t.Fatalf("unexpected: %s, %v", res, err)
	}
}

func TestUpdateAtomicRollback(t *testing.T) {
	orig := `{"x":1}`
	_, err := Update(orig,
		UpdateOp{Op: OpSetRaw, Path: "x", Raw: `2`},
		UpdateOp{Op: OpSetRaw, Path: "", Raw: `3`},
		UpdateOp{Op: OpSetRaw, Path: "y", Raw: `4`},
	)
	var uerr *UpdateError
	if !errors.As(err, &uerr) || uerr.OpIndex != 1 || uerr.Op.Op != OpSetRaw {
		t.Fatalf("expected UpdateError at index 1, got %#v", err)
	}

	// Bytes version: input bytes remain exactly as before.
	origb := []byte(`{"x":1}`)
	back := make([]byte, len(origb))
	copy(back, origb)
	_, err = UpdateBytes(origb,
		UpdateOp{Op: OpSetRaw, Path: "x", Raw: `2`},
		UpdateOp{Op: OpDelete, Path: ""},
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if string(origb) != string(back) {
		t.Fatalf("input bytes mutated: %q", origb)
	}

	// Unknown op type is reported with its index.
	_, err = Update(orig,
		UpdateOp{Op: OpSetRaw, Path: "x", Raw: `2`},
		UpdateOp{Op: "bogus", Path: "x"},
	)
	if !errors.As(err, &uerr) || uerr.OpIndex != 1 {
		t.Fatalf("expected UpdateError at index 1, got %#v", err)
	}
}

func TestUpdateReplaceInPlace(t *testing.T) {
	opts := &Options{Optimistic: true, ReplaceInPlace: true}

	// Success that fits inside the input backing array reuses it.
	json := make([]byte, 0, 64)
	json = append(json, `{"x":100000}`...)
	back := json
	res, err := UpdateBytesOptions(json, opts,
		UpdateOp{Op: OpSetRaw, Path: "x", Raw: `2`},
		UpdateOp{Op: OpCheck, Path: "x", Raw: `2`},
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"x":2}` {
		t.Fatalf("unexpected result: %q", res)
	}
	if &res[:1][0] != &back[:1][0] {
		t.Fatal("expected the input backing array to be reused")
	}

	// Failure must preserve the original input bytes even with
	// ReplaceInPlace enabled.
	json2 := make([]byte, 0, 64)
	json2 = append(json2, `{"x":100000}`...)
	orig2 := make([]byte, len(json2))
	copy(orig2, json2)
	res2, err := UpdateBytesOptions(json2, opts,
		UpdateOp{Op: OpSetRaw, Path: "x", Raw: `2`},
		UpdateOp{Op: OpCheck, Path: "x", Raw: `3`},
	)
	if err == nil {
		t.Fatal("expected check error")
	}
	if string(json2) != string(orig2) {
		t.Fatalf("input bytes mutated on failure: %q", json2)
	}
	if res2 != nil && string(res2) != string(orig2) {
		t.Fatalf("returned bytes should match input: %q", res2)
	}
}

func TestUpdateStringVariant(t *testing.T) {
	// ReplaceInPlace on a string is ignored and the input stays usable.
	opts := &Options{Optimistic: true, ReplaceInPlace: true}
	orig := `{"x":1}`
	res, err := UpdateOptions(orig, opts,
		UpdateOp{Op: OpSet, Path: "y", Value: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	if sortJSON(res) != `{"x":1,"y":2}` || orig != `{"x":1}` {
		t.Fatalf("unexpected: %s (orig %s)", res, orig)
	}
}
