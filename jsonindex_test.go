package buntdb

import (
	"errors"
	"fmt"
	"testing"
)

var errAbort = errors.New("abort")

func TestParseJSONDecimal(t *testing.T) {
	tests := []struct {
		a, b string
		want int // -1: a<b, 0: equal, 1: a>b
	}{
		{"0", "0", 0},
		{"000", "-0", 0},
		{"1", "1.0", 0},
		{"1000", "1e3", 0},
		{"1000", "1.0e3", 0},
		{"0.001", "1e-3", 0},
		{"1.5e2", "150", 0},
		{"-1.5E2", "-150.0", 0},
		{"9007199254740992", "9007199254740993", -1},
		{"9007199254740993", "9007199254740992", 1},
		{"-9007199254740993", "-9007199254740992", -1},
		{"9007199254740993", "9.007199254740993e15", 0},
		{"1e400", "9e399", 1},
		{"1e-400", "9e-401", 1},
		{"-2", "-1", -1},
		{"-0.5", "-0.25", -1},
		{"0.9999999999999999999999", "1", -1},
		{"123.456", "123.457", -1},
		{"123.4560", "123.456", 0},
	}
	for _, tt := range tests {
		got := cmpJSONNumber(tt.a, tt.b)
		if got != tt.want {
			t.Fatalf("cmpJSONNumber(%q,%q)=%d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func cmpJSONNumber(a, b string) int {
	lessAB := jsonNumberLess(a, b)
	lessBA := jsonNumberLess(b, a)
	switch {
	case lessAB:
		return -1
	case lessBA:
		return 1
	default:
		return 0
	}
}

func TestIndexJSONFieldsBigNumbers(t *testing.T) {
	less := IndexJSONFields(JSONField{Path: "n"})
	// Adjacent integers beyond 2^53 that float64 cannot distinguish.
	lo := `{"n":9007199254740992}`
	hi := `{"n":9007199254740993}`
	if !less(lo, hi) || less(hi, lo) {
		t.Fatalf("big integer ordering wrong: %q vs %q", lo, hi)
	}
	// Exponent forms and negatives.
	cases := [][2]string{
		{`{"n":1e-3}`, `{"n":0.002}`},
		{`{"n":-1e400}`, `{"n":1e-400}`},
		{`{"n":-9007199254740993}`, `{"n":-9007199254740992}`},
		{`{"n":1.5e3}`, `{"n":1501}`},
	}
	for _, c := range cases {
		if !less(c[0], c[1]) || less(c[1], c[0]) {
			t.Fatalf("expected %s < %s", c[0], c[1])
		}
	}
}

// TestLegacyIndexJSONUnchanged pins the historical behavior of IndexJSON,
// including float64-based numeric comparison: adjacent integers above 2^53
// collapse to the same value. The new API must not silently alter this.
func TestLegacyIndexJSONUnchanged(t *testing.T) {
	less := IndexJSON("n")
	a := `{"n":9007199254740992}`
	b := `{"n":9007199254740993}`
	if less(a, b) || less(b, a) {
		t.Fatalf("legacy IndexJSON float64 behavior changed: %q vs %q", a, b)
	}
}

func TestIndexJSONFieldsTypeOrder(t *testing.T) {
	less := IndexJSONFields(JSONField{Path: "v"})
	// Default: missing < null < false < true < number < string < json
	ordered := []string{
		`{"other":1}`,
		`{"v":null}`,
		`{"v":false}`,
		`{"v":true}`,
		`{"v":-5}`,
		`{"v":0}`,
		`{"v":1e10}`,
		`{"v":"apple"}`,
		`{"v":"banana"}`,
		`{"v":[1,2]}`,
		`{"v":{"a":1}}`,
	}
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if !less(ordered[i], ordered[j]) {
				t.Fatalf("type order violated: %q should be less than %q",
					ordered[i], ordered[j])
			}
			if less(ordered[j], ordered[i]) {
				t.Fatalf("type order violated: %q should be greater than %q",
					ordered[j], ordered[i])
			}
		}
	}
	// Case-insensitive strings by default.
	if !less(`{"v":"abc"}`, `{"v":"Bcd"}`) {
		t.Fatal("expected case-insensitive string ordering")
	}
	// Case-sensitive opt-in.
	lessCS := IndexJSONFields(JSONField{Path: "v", CaseSensitive: true})
	if lessCS(`{"v":"abc"}`, `{"v":"Bcd"}`) {
		t.Fatal("case-sensitive ordering should put uppercase B before a")
	}
	// Custom type order puts numbers before null, missing last.
	lessCustom := IndexJSONFields(JSONField{
		Path: "v",
		TypeOrder: []string{
			JSONNumber, JSONFalse, JSONTrue, JSONString, JSONNull, JSONMissing,
		},
	})
	if !lessCustom(`{"v":1}`, `{"v":null}`) {
		t.Fatal("custom type order: number should precede null")
	}
	if !lessCustom(`{"v":null}`, `{"other":1}`) {
		t.Fatal("custom type order: null should precede missing")
	}
}

func TestIndexJSONFieldsCompound(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Category ascending, score descending, both with exact numbers.
	err = db.CreateIndex("cat_score", "*", IndexJSONFields(
		JSONField{Path: "category"},
		JSONField{Path: "score", Desc: true},
	))
	if err != nil {
		t.Fatal(err)
	}
	mustSet := func(key, val string) {
		if err := db.Update(func(tx *Tx) error {
			_, _, err := tx.Set(key, val, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	mustSet("1", `{"category":"b","score":9007199254740993}`)
	mustSet("2", `{"category":"a","score":20}`)
	mustSet("3", `{"category":"a","score":9007199254740993}`)
	mustSet("4", `{"category":"a","score":9007199254740992}`)
	mustSet("5", `{"category":"b","score":5}`)

	var keys []string
	db.View(func(tx *Tx) error {
		return tx.Ascend("cat_score", func(key, value string) bool {
			keys = append(keys, key)
			return true
		})
	})
	want := []string{"3", "4", "2", "1", "5"}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("compound order = %v, want %v", keys, want)
	}
}

func TestIndexJSONFieldsUpdateAndRollback(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateIndex("n", "*", IndexJSONFieldExact("n")); err != nil {
		t.Fatal(err)
	}
	mustSet := func(key, val string) {
		if err := db.Update(func(tx *Tx) error {
			_, _, err := tx.Set(key, val, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	mustSet("1", `{"n":9007199254740993}`)
	mustSet("2", `{"n":9007199254740992}`)
	mustSet("3", `{"n":-1e3}`)

	order := func() []string {
		var keys []string
		db.View(func(tx *Tx) error {
			return tx.Ascend("n", func(key, value string) bool {
				keys = append(keys, key)
				return true
			})
		})
		return keys
	}
	if got := fmt.Sprint(order()); got != "[3 2 1]" {
		t.Fatalf("initial order = %s, want [3 2 1]", got)
	}

	// Commit an update that changes the ordering.
	mustSet("3", `{"n":9007199254740994}`)
	if got := fmt.Sprint(order()); got != "[2 1 3]" {
		t.Fatalf("post-update order = %s, want [2 1 3]", got)
	}

	// Roll back the next change (returning an error aborts the managed
	// transaction): index results must reflect the prior state.
	err = db.Update(func(tx *Tx) error {
		if _, _, err := tx.Set("3", `{"n":-5}`, nil); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("expected errAbort, got %v", err)
	}
	if got := fmt.Sprint(order()); got != "[2 1 3]" {
		t.Fatalf("post-rollback order = %s, want [2 1 3]", got)
	}

	// Dropping an index inside a rolled-back transaction restores it and
	// its contents.
	err = db.Update(func(tx *Tx) error {
		if err := tx.DropIndex("n"); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("expected errAbort, got %v", err)
	}
	if got := fmt.Sprint(order()); got != "[2 1 3]" {
		t.Fatalf("post-drop-rollback order = %s, want [2 1 3]", got)
	}
}

func TestIndexJSONFieldsCustomComparatorStillWorks(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Plain user-supplied comparator interface remains accepted and used.
	custom := func(a, b string) bool { return a > b }
	if err := db.CreateIndex("rev", "*", custom); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"1", "2", "3"} {
		if err := db.Update(func(tx *Tx) error {
			_, _, err := tx.Set(k, "v"+k, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	var values []string
	db.View(func(tx *Tx) error {
		return tx.Ascend("rev", func(key, value string) bool {
			values = append(values, value)
			return true
		})
	})
	if fmt.Sprint(values) != "[v3 v2 v1]" {
		t.Fatalf("custom comparator order = %v, want [v3 v2 v1]", values)
	}
}

func ExampleIndexJSONFields() {
	db, _ := Open(":memory:")
	// Category ascending, large-integer score descending, exact precision.
	db.CreateIndex("ranking", "*", IndexJSONFields(
		JSONField{Path: "category"},
		JSONField{Path: "score", Desc: true},
	))
	db.Update(func(tx *Tx) error {
		tx.Set("1", `{"category":"a","score":9007199254740993}`, nil)
		tx.Set("2", `{"category":"a","score":9007199254740992}`, nil)
		return nil
	})
	db.View(func(tx *Tx) error {
		tx.Ascend("ranking", func(key, value string) bool {
			fmt.Println(key, value)
			return true
		})
		return nil
	})
	// Output:
	// 1 {"category":"a","score":9007199254740993}
	// 2 {"category":"a","score":9007199254740992}
}
