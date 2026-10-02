package sjson

import (
	"math/rand"
	"strconv"
	"testing"
)

func testDeleteMany(t *testing.T, json, expect string, paths ...string) {
	t.Helper()

	got, err := DeleteMany(json, paths...)
	if err != nil {
		t.Fatalf("DeleteMany(%v): %v", paths, err)
	}
	if got != expect {
		t.Fatalf("DeleteMany(%v):\n  json   = %s\n  expect = %s\n  got    = %s",
			paths, json, expect, got)
	}

	gotb, err := DeleteManyBytes([]byte(json), paths...)
	if err != nil {
		t.Fatalf("DeleteManyBytes(%v): %v", paths, err)
	}
	if string(gotb) != expect {
		t.Fatalf("DeleteManyBytes(%v):\n  json   = %s\n  expect = %s\n  got    = %s",
			paths, json, expect, string(gotb))
	}
}

// TestDeleteManyBasic covers ordinary paths and original-position array
// indices.
func TestDeleteManyBasic(t *testing.T) {
	testDeleteMany(t, `{"a":1,"b":2,"c":3}`, `{"b":2}`, "a", "c")
	testDeleteMany(t, `[10,20,30,40]`, `[20,40]`, "0", "2")
	testDeleteMany(t, `[10,20,30,40]`, `[20,40]`, "2", "0")
	testDeleteMany(t, `[10,20,30,40]`, `[30,40]`, "0", "1")
	testDeleteMany(t, `[10,20,30,40]`, `[10,40]`, "1", "2")
	testDeleteMany(t, `[10,20,30,40]`, `[10,20]`, "2", "3")
	testDeleteMany(t, `[10,20,30,40]`, `[]`, "0", "1", "2", "3")
	testDeleteMany(t, `{"a":1,"b":2,"c":3}`, `{}`, "a", "b", "c")

	// nested paths
	testDeleteMany(t,
		`{"o":{"x":1,"y":2,"z":3},"k":9}`,
		`{"o":{"y":2},"k":9}`, "o.x", "o.z")
	testDeleteMany(t,
		`{"a":[1,2,3,4],"b":2}`, `{"a":[2,4],"b":2}`, "a.0", "a.2")

	// last element mixed with ordinary indices
	testDeleteMany(t, `{"a":[1,2,3]}`, `{"a":[2]}`, "a.0", "a.-1")
	testDeleteMany(t, `{"a":[1,2,3]}`, `{"a":[2]}`, "a.-1", "a.0")

	// paths that do not match are no-ops
	testDeleteMany(t, `{"a":1,"b":2}`, `{"b":2}`, "missing", "a")
	testDeleteMany(t, `{"a":1}`, `{"a":1}`, "x.y", "z")

	// no paths at all
	testDeleteMany(t, `{"a":1}`, `{"a":1}`)

	// forced numeric object key and dotted keys
	testDeleteMany(t, `{"1":"v","2":"w"}`, `{"2":"w"}`, ":1")
	testDeleteMany(t, `{"a.b":1,"c":2}`, `{"c":2}`, `a\.b`)
	testDeleteMany(t, `{":a":1,"c":2}`, `{"c":2}`, `\:a`)
}

// TestDeleteManyOrderIndependence shuffles the same path list and checks the
// result never changes.
func TestDeleteManyOrderIndependence(t *testing.T) {
	json := `{"a":1,"b":2,"c":3,"v":[10,20,30,40,50],"k":7}`
	paths := []string{"a", "c", "v.0", "v.2", "v.4", "k"}
	expect := `{"b":2,"v":[20,40]}`
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 100; i++ {
		p := append([]string{}, paths...)
		rng.Shuffle(len(p), func(i, j int) { p[i], p[j] = p[j], p[i] })
		got, err := DeleteMany(json, p...)
		if err != nil {
			t.Fatal(err)
		}
		if got != expect {
			t.Fatalf("order %v: expect %s got %s", p, expect, got)
		}
	}
}

// TestDeleteManyQueries mixes gjson query paths that hit several array
// positions with ordinary paths.
func TestDeleteManyQueries(t *testing.T) {
	json := `{"friends":[
		{"first":"James","age":40},
		{"first":"Roger","age":20},
		{"first":"Ana","age":50}
	],"tag":"x","other":1}`

	// query matching several nested values + ordinary paths
	expect := `{"friends":[
		{"age":40},
		{"first":"Roger","age":20},
		{"age":50}
	],"other":1}`
	got, err := DeleteMany(json, `friends.#(age>25)#.first`, "tag")
	if err != nil {
		t.Fatal(err)
	}
	if got != expect {
		t.Fatalf("expect %s got %s", expect, got)
	}

	// the same call with reversed path order must be identical
	rev, err := DeleteMany(json, "tag", `friends.#(age>25)#.first`)
	if err != nil {
		t.Fatal(err)
	}
	if rev != got {
		t.Fatalf("result depends on path order: %q vs %q", rev, got)
	}

	// query matching several whole objects
	testDeleteMany(t, `{"v":[{"n":1},{"n":2},{"n":3}]}`,
		`{"v":[{"n":2}]}`, `v.#(n!=2)#`)

	// a query that matches nothing alongside an ordinary path
	testDeleteMany(t, `{"v":[1,2,3]}`, `{"v":[1,3]}`,
		`v.#(>99)#`, "v.1")
}

// TestDeleteManyDuplicates checks that the same location is removed only once
// even when reached by several paths.
func TestDeleteManyDuplicates(t *testing.T) {
	testDeleteMany(t, `[10,20,30,40]`, `[20,40]`, "0", "0", "2")
	testDeleteMany(t, `{"a":1,"b":2}`, `{"b":2}`, "a", "a")
	json := `{"v":[{"n":1},{"n":2}]}`
	testDeleteMany(t, json, `{"v":[{"n":2}]}`,
		`v.#(n==1)#`, `v.0`)
	// two overlapping queries that share a match
	testDeleteMany(t, `{"v":[{"n":1,"t":"a"},{"n":2,"t":"b"}]}`,
		`{"v":[{},{"n":2,"t":"b"}]}`,
		`v.#(n==1)#.n`, `v.#(t=="a")#.t`)
}

// TestDeleteManyParent ensures a match nested inside an already deleted
// ancestor is not processed again.
func TestDeleteManyParent(t *testing.T) {
	testDeleteMany(t,
		`{"o":{"x":1,"y":2},"keep":3}`, `{"keep":3}`, "o", "o.x")
	testDeleteMany(t,
		`{"o":{"x":1,"y":2},"keep":3}`, `{"keep":3}`, "o.x", "o")
}

// TestDeleteManyRoot checks the explicit return value when the root document
// itself is deleted.
func TestDeleteManyRoot(t *testing.T) {
	testDeleteMany(t, `{"a":1}`, ``, "@this")
	testDeleteMany(t, `[1,2,3]`, ``, "@this")
	testDeleteMany(t, `  {"a":1}  `, ``, "@this")
	testDeleteMany(t, `123`, ``, "@this")
	// root removal wins over any other path
	testDeleteMany(t, `{"a":1,"b":2}`, ``, "a", "@this")
}

// TestDeleteManyPreserveFormat makes sure whitespace outside the removed
// values is kept untouched.
func TestDeleteManyPreserveFormat(t *testing.T) {
	testDeleteMany(t, ` [1, 2, 3] `, ` [ 2] `, "0", "2")
	testDeleteMany(t,
		"{ \"a\" : 1, \"b\" : 2, \"c\" : 3 }",
		`{ "b" : 2 }`, "a", "c")
}

// TestDeleteManyEmptyPath checks the error path.
func TestDeleteManyEmptyPath(t *testing.T) {
	if _, err := DeleteMany(`{"a":1}`, ""); err == nil {
		t.Fatal("expected an error for an empty path")
	}
	if _, err := DeleteManyBytes([]byte(`{"a":1}`), "a", ""); err == nil {
		t.Fatal("expected an error for an empty path")
	}
}

// TestDeleteManyArrayEquivalence compares DeleteMany against sequential single
// deletes performed in descending order, which is exactly the semantics of
// evaluating indices against the original document.
func TestDeleteManyArrayEquivalence(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for iter := 0; iter < 5000; iter++ {
		n := 1 + rng.Intn(10)
		raw := "["
		for i := 0; i < n; i++ {
			if i > 0 {
				raw += ","
			}
			raw += strconv.Itoa(rng.Intn(1000))
		}
		raw += "]"

		paths := []string{}
		chosen := map[int]bool{}
		for i := 0; i < n; i++ {
			if rng.Intn(2) == 0 {
				chosen[i] = true
				paths = append(paths, strconv.Itoa(i))
			}
		}
		rng.Shuffle(len(paths), func(i, j int) {
			paths[i], paths[j] = paths[j], paths[i]
		})

		many, err := DeleteMany(raw, paths...)
		if err != nil {
			t.Fatal(err)
		}
		seq := raw
		for i := n - 1; i >= 0; i-- {
			if chosen[i] {
				seq, err = Delete(seq, strconv.Itoa(i))
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		if many != seq {
			t.Fatalf("json=%s paths=%v many=%s seq=%s", raw, paths, many, seq)
		}
	}
}

// TestDeleteManyWhitespaceEquivalence runs the same check over documents with
// random whitespace padding around tokens and separators.
func TestDeleteManyWhitespaceEquivalence(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	pads := []string{"", " ", "  ", "\n", " \n "}
	pad := func() string { return pads[rng.Intn(len(pads))] }
	for iter := 0; iter < 5000; iter++ {
		n := 1 + rng.Intn(8)

		// build either an array of numbers or an object
		raw := ""
		kind := rng.Intn(2)
		if kind == 0 {
			raw = "[" + pad()
			for i := 0; i < n; i++ {
				if i > 0 {
					raw += pad() + "," + pad()
				}
				raw += strconv.Itoa(rng.Intn(1000))
			}
			raw += pad() + "]"
		} else {
			raw = "{" + pad()
			for i := 0; i < n; i++ {
				if i > 0 {
					raw += pad() + "," + pad()
				}
				raw += `"k` + strconv.Itoa(i) + `":` + pad() +
					strconv.Itoa(rng.Intn(1000))
			}
			raw += pad() + "}"
		}

		paths := []string{}
		chosen := map[string]bool{}
		for i := 0; i < n; i++ {
			if rng.Intn(2) == 0 {
				var p string
				if kind == 0 {
					p = strconv.Itoa(i)
				} else {
					p = "k" + strconv.Itoa(i)
				}
				chosen[p] = true
				paths = append(paths, p)
			}
		}
		rng.Shuffle(len(paths), func(i, j int) {
			paths[i], paths[j] = paths[j], paths[i]
		})

		many, err := DeleteMany(raw, paths...)
		if err != nil {
			t.Fatal(err)
		}

		// sequential deletes in descending key/index order
		seq := raw
		for i := n - 1; i >= 0; i-- {
			var p string
			if kind == 0 {
				p = strconv.Itoa(i)
			} else {
				p = "k" + strconv.Itoa(i)
			}
			if chosen[p] {
				seq, err = Delete(seq, p)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		if many != seq {
			t.Fatalf("json=%q paths=%v\nmany=%q\nseq =%q",
				raw, paths, many, seq)
		}
	}
}
