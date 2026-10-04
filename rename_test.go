package sjson

import (
	jsongo "encoding/json"
	"math/rand"
	"strconv"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRenameBasic(t *testing.T) {
	json := `{"a":1,"b":2,"c":3}`
	got, err := Rename(json, "b", "x")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"a":1,"x":2,"c":3}` {
		t.Fatalf("got %q", got)
	}
	// string and bytes variants agree
	b, err := RenameBytes([]byte(json), "b", "x")
	if err != nil || string(b) != got {
		t.Fatalf("bytes: %q %v", b, err)
	}
	// value untouched, position untouched
	if gjson.Get(got, "x").Int() != 2 {
		t.Fatal("value not preserved")
	}
	if gjson.Get(got, "b").Exists() {
		t.Fatal("old key still present")
	}
}

func TestRenameSpecialKeys(t *testing.T) {
	// empty key
	json := `{"":1,"a":2}`
	got, err := Rename(json, "", "z")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"z":1,"a":2}` {
		t.Fatalf("got %q", got)
	}

	// dots and backslashes are literal names, not path syntax
	json = `{"a.b\\c":1,"a.b":2}`
	got, err = Rename(json, `a.b\c`, "x.y")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"x.y":1,"a.b":2}` {
		t.Fatalf("got %q", got)
	}

	// old name may contain a dot without being interpreted as a path
	json = `{"a.b":1}`
	got, err = Rename(json, "a.b", "c")
	if err != nil || got != `{"c":1}` {
		t.Fatalf("got %q %v", got, err)
	}

	// new name requiring escaping is properly encoded
	json = `{"a":1}`
	got, err = Rename(json, "a", `q"q`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"q\"q":1}` {
		t.Fatalf("got %q", got)
	}

	// unicode-escaped key compares against the decoded name
	json = `{"\u0041":1}`
	got, err = Rename(json, "A", "B")
	if err != nil || got != `{"B":1}` {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestRenameTargetExists(t *testing.T) {
	json := `{"a":1,"b":2}`
	// default: no overwrite, error and input returned unchanged
	got, err := Rename(json, "a", "b")
	if err == nil {
		t.Fatal("expected error")
	}
	if got != json {
		t.Fatalf("input changed: %q", got)
	}

	// overwrite: source keeps its slot, target value is dropped
	got, err = RenameOptions(json, "a", "b", &Options{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"b":1}` {
		t.Fatalf("got %q", got)
	}

	// overwrite when target is first
	json = `{"a":1,"b":2,"c":3}`
	got, err = RenameOptions(json, "c", "a", &Options{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"b":2,"a":3}` {
		t.Fatalf("got %q", got)
	}

	// overwrite target in the middle, whitespace kept around others
	json = `{"a":1, "b":2, "c":3}`
	got, err = RenameOptions(json, "a", "b", &Options{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"b":1, "c":3}` {
		t.Fatalf("got %q", got)
	}
}

func TestRenameAmbiguous(t *testing.T) {
	// duplicate old keys, distinct encodings
	json := `{"a":1,"\u0061":2,"b":3}`
	if _, err := Rename(json, "a", "z"); err == nil {
		t.Fatal("expected ambiguous old key error")
	}
	// duplicate target keys: error even with overwrite
	json = `{"a":1,"b":2,"\u0062":3}`
	if _, err := RenameOptions(json, "a", "b",
		&Options{Overwrite: true}); err == nil {
		t.Fatal("expected ambiguous target key error")
	}
}

func TestRenameMissing(t *testing.T) {
	json := `{"a":1}`
	got, err := Rename(json, "nope", "z")
	if err == nil {
		t.Fatal("expected error")
	}
	if got != json {
		t.Fatalf("input changed: %q", got)
	}
}

func TestRenameSelf(t *testing.T) {
	json := `{"a":1,"b":2}`
	got, err := Rename(json, "a", "a")
	if err != nil {
		t.Fatal(err)
	}
	if got != json {
		t.Fatalf("expected no change, got %q", got)
	}
	// self rename is a no-op even with duplicated keys
	dup := `{"a":1,"a":2}`
	got, err = Rename(dup, "a", "a")
	if err != nil || got != dup {
		t.Fatalf("self rename: %q %v", got, err)
	}
	// empty key self rename
	got, err = Rename(`{"":1}`, "", "")
	if err != nil || got != `{"":1}` {
		t.Fatalf("empty self rename: %q %v", got, err)
	}
}

func TestRenameNonObject(t *testing.T) {
	if _, err := Rename(`[1,2,3]`, "0", "x"); err == nil {
		t.Fatal("expected error for array")
	}
	if _, err := Rename(`"hello"`, "hello", "x"); err == nil {
		t.Fatal("expected error for string")
	}
}

func TestRenameInPlace(t *testing.T) {
	// successful in-place rename that fits
	json := []byte(`{"abc":1,"b":2}`)
	got, err := RenameBytesOptions(json, "abc", "xy",
		&Options{Optimistic: true, ReplaceInPlace: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"xy":1,"b":2}` {
		t.Fatalf("got %q", got)
	}

	// in-place rename that grows the document falls back to a new slice
	json = []byte(`{"a":1}`)
	got, err = RenameBytesOptions(json, "a", "longname",
		&Options{ReplaceInPlace: true})
	if err != nil || string(got) != `{"longname":1}` {
		t.Fatalf("got %q %v", got, err)
	}

	// overwrite in place with longer new key
	json = []byte(`{"a":1,"bb":2,"c":3}`)
	got, err = RenameBytesOptions(json, "a", "bb", &Options{
		ReplaceInPlace: true, Overwrite: true})
	if err != nil || string(got) != `{"bb":1,"c":3}` {
		t.Fatalf("got %q %v", got, err)
	}

	// in-place rename shrinking into a backing array with spare capacity
	json = make([]byte, len(`{"abc":1}`), 64)
	copy(json, `{"abc":1}`)
	got, err = RenameBytesOptions(json, "abc", "z",
		&Options{ReplaceInPlace: true})
	if err != nil || string(got) != `{"z":1}` {
		t.Fatalf("got %q %v", got, err)
	}
	if cap(got) != 64 {
		t.Fatalf("expected reuse of backing array, cap=%d", cap(got))
	}

	// string API never edits the backing array in place
	str := `{"a":1}`
	sgot, err := RenameOptions(str, "a", "z",
		&Options{ReplaceInPlace: true})
	if err != nil || sgot != `{"z":1}` || str != `{"a":1}` {
		t.Fatalf("got %q %v (input %q)", sgot, err, str)
	}
}

func TestRenameFailureLeavesInputIntact(t *testing.T) {
	// every failure mode with in-place enabled must return the exact
	// original bytes; no half-escaped key may remain
	cases := []struct {
		json, oldName, newName string
		opts                   *Options
	}{
		{`{"a":1,"b":2}`, "a", "b",
			&Options{Optimistic: true, ReplaceInPlace: true}},
		{`{"a":1,"a":2}`, "a", "z",
			&Options{Optimistic: true, ReplaceInPlace: true}},
		{`{"a":1,"b":2,"b":3}`, "a", "b",
			&Options{Optimistic: true, ReplaceInPlace: true, Overwrite: true}},
		{`{"a":1}`, "missing", `zz\"zz`,
			&Options{Optimistic: true, ReplaceInPlace: true}},
		{`[1,2]`, "0", "z",
			&Options{Optimistic: true, ReplaceInPlace: true}},
	}
	for _, c := range cases {
		in := make([]byte, len(c.json))
		copy(in, c.json)
		got, err := RenameBytesOptions(in, c.oldName, c.newName, c.opts)
		if err == nil {
			t.Fatalf("expected error for %q -> %q in %q",
				c.oldName, c.newName, c.json)
		}
		if string(got) != c.json {
			t.Fatalf("input mutated on failure:\nwant %q\ngot  %q",
				c.json, got)
		}
	}
}

func TestRenameNestedAndWhitespace(t *testing.T) {
	// only top level members are renamed; nested same-named keys survive
	json := `{"a": {"a": 1} , "b":[1,{"a":2}]}`
	got, err := Rename(json, "a", "x")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"x": {"a": 1} , "b":[1,{"a":2}]}` {
		t.Fatalf("got %q", got)
	}

	// rename the only member of an object
	got, err = Rename(`  { "a" : null }  `, "a", "z")
	if err != nil {
		t.Fatal(err)
	}
	if got != `  { "z" : null }  ` {
		t.Fatalf("got %q", got)
	}
}

func TestRenameRandomized(t *testing.T) {
	keyAlphabet := []string{
		"", "a", "b", "c", "a.b", `a\b`, "ab", "a", "xy",
		`q"q`, "a b", "\u00e9",
	}
	rng := rand.New(rand.NewSource(42))
	for iter := 0; iter < 3000; iter++ {
		n := 1 + rng.Intn(6)
		type member struct{ key, raw string }
		src := make([]member, n)
		json := "{"
		for i := 0; i < n; i++ {
			if i > 0 {
				json += ","
			}
			k := keyAlphabet[rng.Intn(len(keyAlphabet))]
			kb, _ := jsongo.Marshal(k)
			v := rng.Intn(1000)
			src[i] = member{k, strconv.Itoa(v)}
			json += string(kb) + ":" + strconv.Itoa(v)
		}
		json += "}"

		oldIdx := rng.Intn(n)
		oldName := src[oldIdx].key
		var newName string
		if rng.Intn(2) == 0 {
			newName = keyAlphabet[rng.Intn(len(keyAlphabet))]
		} else {
			newName = "new" + strconv.Itoa(iter)
		}
		overwrite := rng.Intn(2) == 0

		count := func(name string) int {
			c := 0
			for _, m := range src {
				if m.key == name {
					c++
				}
			}
			return c
		}

		out, err := RenameOptions(json, oldName, newName,
			&Options{Overwrite: overwrite})
		if err != nil {
			if out != json {
				t.Fatalf("failure mutated input:\njson=%s\ngot =%s", json, out)
			}
			expectErr := count(oldName) != 1 || count(newName) > 1 ||
				(count(newName) == 1 && !overwrite)
			if !expectErr {
				t.Fatalf("unexpected error for %q->%q in %s: %v",
					oldName, newName, json, err)
			}
			continue
		}
		if oldName == newName {
			if out != json {
				t.Fatalf("self rename changed %s to %s", json, out)
			}
			continue
		}
		var want []member
		for i, m := range src {
			if m.key == newName && i != oldIdx {
				continue
			}
			if i == oldIdx {
				want = append(want, member{newName, m.raw})
			} else {
				want = append(want, m)
			}
		}
		checkOrder := func(got string) {
			t.Helper()
			parsed := gjson.Parse(got)
			if !parsed.IsObject() {
				t.Fatalf("not an object: %s (from %s)", got, json)
			}
			j := 0
			parsed.ForEach(func(k, v gjson.Result) bool {
				if j >= len(want) {
					t.Fatalf("extra member in %s (from %s)", got, json)
				}
				if k.Str != want[j].key || v.Raw != want[j].raw {
					t.Fatalf("member %d: got %s=%s want %s=%s\nfrom %s",
						j, k.Str, v.Raw, want[j].key, want[j].raw, json)
				}
				j++
				return true
			})
			if j != len(want) {
				t.Fatalf("missing members in %s (from %s)", got, json)
			}
		}
		checkOrder(out)

		bout, berr := RenameBytesOptions([]byte(json), oldName, newName,
			&Options{Overwrite: overwrite})
		if berr != nil || string(bout) != out {
			t.Fatalf("bytes mismatch: %q vs %q (%v)", bout, out, berr)
		}
		buf := make([]byte, len(json), len(json)*4+16)
		copy(buf, json)
		iout, ierr := RenameBytesOptions(buf, oldName, newName,
			&Options{Optimistic: true, ReplaceInPlace: true, Overwrite: overwrite})
		if ierr != nil || string(iout) != out {
			t.Fatalf("in-place mismatch: %q vs %q (%v) from %s",
				iout, out, ierr, json)
		}
	}
}
