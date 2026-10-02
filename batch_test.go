package sjson

import (
	"strings"
	"testing"
)

func TestBatchOrderArrayIndex(t *testing.T) {
	// the delete shifts the array indexes, so the following set must
	// operate on the intermediate result.
	res, err := Batch(`{"arr":[1,2,3]}`,
		DeleteOp("arr.0"),
		SetOp("arr.0", 9),
	)
	if err != nil {
		t.Fatal(err)
	}
	if res != `{"arr":[9,3]}` {
		t.Fatalf("expected '%v', got '%v'", `{"arr":[9,3]}`, res)
	}
	// reversed order produces a different result, proving order matters.
	res, err = Batch(`{"arr":[1,2,3]}`,
		SetOp("arr.0", 9),
		DeleteOp("arr.0"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if res != `{"arr":[2,3]}` {
		t.Fatalf("expected '%v', got '%v'", `{"arr":[2,3]}`, res)
	}
}
func TestBatchDependsOnCreatedParent(t *testing.T) {
	// the second and third operations rely on the parent object
	// created by the first operation.
	res, err := Batch(`{}`,
		SetOp("a.b", 1),
		SetOp("a.c", 2),
		SetRawOp("a.d", `{"e":[true]}`),
		CheckOp("a.c", 2),
	)
	if err != nil {
		t.Fatal(err)
	}
	expect := `{"a":{"b":1,"c":2,"d":{"e":[true]}}}`
	if res != expect {
		t.Fatalf("expected '%v', got '%v'", expect, res)
	}
}

func TestBatchAtomicOnFailure(t *testing.T) {
	// the third operation fails; the first two must not be delivered.
	orig := `{"a":1,"b":2}`
	res, err := Batch(orig,
		SetOp("a", 100),
		DeleteOp("b"),
		CheckOp("a", 999),
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	berr, ok := err.(*BatchError)
	if !ok {
		t.Fatalf("expected *BatchError, got %T", err)
	}
	if berr.Index != 2 {
		t.Fatalf("expected index 2, got %d", berr.Index)
	}
	if res != orig {
		t.Fatalf("expected original json '%v', got '%v'", orig, res)
	}
	if !strings.Contains(err.Error(), "operation 2") {
		t.Fatalf("expected operation index in error, got '%v'", err)
	}
}
func TestBatchCheckMissingVsNull(t *testing.T) {
	json := `{"a":null,"b":1}`
	// null value passes a null check
	if _, err := Batch(json, CheckOp("a", nil)); err != nil {
		t.Fatal(err)
	}
	// missing path fails even when expecting null
	_, err := Batch(json, CheckOp("c", nil))
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected missing error, got '%v'", err)
	}
	// null actual vs non-null expected
	_, err = Batch(json, CheckOp("a", 1))
	if err == nil || !strings.Contains(err.Error(), "got null") {
		t.Fatalf("expected null mismatch error, got '%v'", err)
	}
	// non-null actual vs null expected
	_, err = Batch(json, CheckOp("b", nil))
	if err == nil || !strings.Contains(err.Error(), "expected null") {
		t.Fatalf("expected null expectation error, got '%v'", err)
	}
	// value mismatch
	_, err = Batch(json, CheckOp("b", 2))
	if err == nil || !strings.Contains(err.Error(), "expected 2, got 1") {
		t.Fatalf("expected mismatch error, got '%v'", err)
	}
}

func TestBatchCheckKinds(t *testing.T) {
	json := `{"s":"hi","n":1.5,"t":true}`
	if _, err := Batch(json,
		CheckOp("s", "hi"),
		CheckOp("n", 1.5),
		CheckOp("t", true),
	); err != nil {
		t.Fatal(err)
	}
}
func TestBatchReplaceInPlaceFailureRestoresInput(t *testing.T) {
	orig := `{"name":"tom","age":37}`
	input := []byte(orig)
	opts := &Options{Optimistic: true, ReplaceInPlace: true}
	res, err := BatchBytesOptions(input, []BatchOp{
		SetOp("name", "bob"),
		SetOp("age", 41),
		CheckOp("name", "alice"),
	}, opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	berr, ok := err.(*BatchError)
	if !ok {
		t.Fatalf("expected *BatchError, got %T", err)
	}
	if berr.Index != 2 {
		t.Fatalf("expected index 2, got %d", berr.Index)
	}
	// the input bytes must be fully restored even though the first
	// operations replaced values in-place.
	if string(input) != orig {
		t.Fatalf("expected input restored to '%v', got '%v'",
			orig, string(input))
	}
	if string(res) != orig {
		t.Fatalf("expected original json '%v', got '%v'",
			orig, string(res))
	}
}

func TestBatchReplaceInPlaceSuccess(t *testing.T) {
	input := []byte(`{"name":"tom","age":37}`)
	opts := &Options{Optimistic: true, ReplaceInPlace: true}
	res, err := BatchBytesOptions(input, []BatchOp{
		SetOp("name", "bob"),
		SetOp("age", 41),
		CheckOp("name", "bob"),
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"name":"bob","age":41}` {
		t.Fatalf("unexpected result '%v'", string(res))
	}
}

func TestBatchDeleteMissingIsNoChange(t *testing.T) {
	// deleting a missing path is a no-op, not a failure.
	res, err := Batch(`{"a":1}`,
		DeleteOp("zz"),
		SetOp("b", 2),
	)
	if err != nil {
		t.Fatal(err)
	}
	if res != `{"a":1,"b":2}` {
		t.Fatalf("unexpected result '%v'", res)
	}
}
