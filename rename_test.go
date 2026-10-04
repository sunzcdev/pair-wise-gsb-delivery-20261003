package sjson

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRenameBasic(t *testing.T) {
	tests := []struct {
		name      string
		json      string
		oldName   string
		newName   string
		overwrite bool
		expect    string
	}{
		{
			name:    "middle member keeps value and position",
			json:    `{"a":1,"b":2,"c":3}`,
			oldName: "b", newName: "x",
			expect: `{"a":1,"x":2,"c":3}`,
		},
		{
			name:    "first member",
			json:    `{"a":1,"b":2}`,
			oldName: "a", newName: "z",
			expect: `{"z":1,"b":2}`,
		},
		{
			name:    "last member",
			json:    `{"a":1,"b":2}`,
			oldName: "b", newName: "z",
			expect: `{"a":1,"z":2}`,
		},
		{
			name:    "only member",
			json:    `{"a":{"nested":[1,2,3]}}`,
			oldName: "a", newName: "b",
			expect: `{"b":{"nested":[1,2,3]}}`,
		},
		{
			name:    "empty old key",
			json:    `{"":1,"b":2}`,
			oldName: "", newName: "z",
			expect: `{"z":1,"b":2}`,
		},
		{
			name:    "empty new key",
			json:    `{"a":1,"b":2}`,
			oldName: "a", newName: "",
			expect: `{"":1,"b":2}`,
		},
		{
			name:    "dot is literal",
			json:    `{"a.b":1,"c":2}`,
			oldName: "a.b", newName: "d.e",
			expect: `{"d.e":1,"c":2}`,
		},
		{
			name:    "name with query chars is literal",
			json:    `{"a#b?":1}`,
			oldName: "a#b?", newName: "x*y",
			expect: `{"x*y":1}`,
		},
		{
			name:    "backslash in key decoded",
			json:    `{"a\\b":1}`,
			oldName: `a\b`, newName: `c\d`,
			expect: `{"c\\d":1}`,
		},
		{
			name:    "unicode escapes decoded before comparison",
			json:    `{"a\u00e9":1}`,
			oldName: "aé", newName: "bé",
			expect: `{"bé":1}`,
		},
		{
			name:    "shrinking key keeps whitespace",
			json:    "{ \"a\" : 1 ,\n \"b\" : 2 }",
			oldName: "a", newName: "z",
			expect: "{ \"z\" : 1 ,\n \"b\" : 2 }",
		},
		{
			name:    "growing key allocates correctly",
			json:    `{"a":1}`,
			oldName: "a", newName: "longer",
			expect: `{"longer":1}`,
		},
		{
			name:    "overwrite target in middle",
			json:    `{"a":1,"b":2,"c":3}`,
			oldName: "a", newName: "c", overwrite: true,
			expect: `{"c":1,"b":2}`,
		},
		{
			name:    "overwrite target first",
			json:    `{"a":1,"b":2,"c":3}`,
			oldName: "c", newName: "a", overwrite: true,
			expect: `{"a":3,"b":2}`,
		},
		{
			name:    "overwrite target adjacent middle",
			json:    `{"a":1,"b":2,"c":3}`,
			oldName: "b", newName: "c", overwrite: true,
			expect: `{"a":1,"c":2}`,
		},
		{
			name:    "overwrite target adjacent before",
			json:    `{"a":1,"b":2,"c":3}`,
			oldName: "c", newName: "b", overwrite: true,
			expect: `{"a":1,"b":3}`,
		},
		{
			name:    "overwrite only member",
			json:    `{"a":1}`,
			oldName: "a", newName: "b", overwrite: true,
			expect: `{"b":1}`,
		},
		{
			name:    "new key needs escaping",
			json:    `{"a":1}`,
			oldName: "a", newName: `"q"`,
			expect: `{"\"q\"":1}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Rename(tc.json, tc.oldName, tc.newName, tc.overwrite)
			if err != nil {
				t.Fatal(err)
			}
			if sortJSON(got) != sortJSON(tc.expect) {
				t.Fatalf("Rename: expected %s, got %s", tc.expect, got)
			}
			gotb, err := RenameBytes([]byte(tc.json), tc.oldName, tc.newName, tc.overwrite)
			if err != nil {
				t.Fatal(err)
			}
			if sortJSON(string(gotb)) != sortJSON(tc.expect) {
				t.Fatalf("RenameBytes: expected %s, got %s", tc.expect, gotb)
			}
		})
	}
}

func TestRenameSelf(t *testing.T) {
	doc := `{"a":1,"b":2}`
	out, err := Rename(doc, "a", "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if out != doc {
		t.Fatalf("expected unchanged document, got %s", out)
	}
	outb, err := RenameBytes([]byte(doc), "b", "b", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(outb) != doc {
		t.Fatalf("expected unchanged document, got %s", outb)
	}
}

func TestRenameErrors(t *testing.T) {
	tests := []struct {
		name           string
		json           string
		oldName        string
		newName        string
		overwrite      bool
		wantErr        string
		inputUnchanged bool
	}{
		{
			name:    "missing old key",
			json:    `{"a":1}`,
			oldName: "x", newName: "y",
			wantErr:        "was not found",
			inputUnchanged: true,
		},
		{
			name:    "target exists without overwrite",
			json:    `{"a":1,"b":2}`,
			oldName: "a", newName: "b",
			wantErr:        "already exists",
			inputUnchanged: true,
		},
		{
			name:    "duplicate old key",
			json:    `{"a":1,"a":2}`,
			oldName: "a", newName: "z",
			wantErr:        "multiple members named",
			inputUnchanged: true,
		},
		{
			name:    "duplicate target key",
			json:    `{"a":1,"b":2,"b":3}`,
			oldName: "a", newName: "b", overwrite: true,
			wantErr:        "multiple members named",
			inputUnchanged: true,
		},
		{
			name:    "root is array",
			json:    `[1,2,3]`,
			oldName: "1", newName: "z",
			wantErr:        "must be an object",
			inputUnchanged: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Rename(tc.json, tc.oldName, tc.newName, tc.overwrite)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v (%s)", tc.wantErr, err, out)
			}
			if tc.inputUnchanged && out != tc.json {
				t.Fatalf("input changed on failure: %s", out)
			}
		})
	}
}

func TestRenameInPlace(t *testing.T) {
	doc := `{"aaa":1,"bbb":2,"ccc":3}`
	jsonb := []byte(doc)
	opts := &Options{Optimistic: true, ReplaceInPlace: true}
	res, err := RenameBytesOptions(jsonb, "bbb", "z", opts)
	if err != nil {
		t.Fatal(err)
	}
	if sortJSON(string(res)) != sortJSON(`{"aaa":1,"z":2,"ccc":3}`) {
		t.Fatalf("got %s", res)
	}
	if &res[0] != &jsonb[0] {
		t.Fatal("expected replacement to happen in place")
	}

	jsonb2 := []byte(`{"a":1}`)
	res2, err := RenameBytesOptions(jsonb2, "a", "longer-name", opts)
	if err != nil {
		t.Fatal(err)
	}
	if sortJSON(string(res2)) != sortJSON(`{"longer-name":1}`) {
		t.Fatalf("got %s", res2)
	}
}

func TestRenameInPlaceNoHalfKeyOnFailure(t *testing.T) {
	doc := `{"aaa":1,"bbb":2}`
	jsonb := []byte(doc)
	opts := &Options{Optimistic: true, ReplaceInPlace: true}
	res, err := RenameBytesOptions(jsonb, "aaa", "bbb", opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if string(jsonb) != doc {
		t.Fatalf("input was partially modified: %s", jsonb)
	}
	if string(res) != doc {
		t.Fatalf("returned bytes were partially modified: %s", res)
	}

	doc2 := `{"aaa":1,"aaa":2,"z":3}`
	jsonb2 := []byte(doc2)
	_, err = RenameBytesOptions(jsonb2, "aaa", "qq", opts)
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	if string(jsonb2) != doc2 {
		t.Fatalf("input was partially modified: %s", jsonb2)
	}
}

func TestRenameSpecialKeysRoundTrip(t *testing.T) {
	in := map[string]interface{}{
		"":       1,
		"a.b":    2,
		`a\b`:    3,
		`a"b`:    4,
		"normal": 5,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		oldName, newName string
	}{
		{"", "z"},
		{"a.b", "c.d.e"},
		{`a\b`, `x\y`},
		{`a"b`, `x"y`},
		{"normal", "end.key"},
	}
	for _, c := range cases {
		got, err := Rename(string(b), c.oldName, c.newName, false)
		if err != nil {
			t.Fatalf("rename %q -> %q: %v", c.oldName, c.newName, err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(got), &m); err != nil {
			t.Fatalf("invalid json after rename %q -> %q: %v\n%s",
				c.oldName, c.newName, err, got)
		}
		if _, ok := m[c.oldName]; ok {
			t.Fatalf("old key %q still present", c.oldName)
		}
		if _, ok := m[c.newName]; !ok {
			t.Fatalf("new key %q missing in %s", c.newName, got)
		}
	}
}

func TestRenameModesConsistency(t *testing.T) {
	docs := []string{
		`{}`,
		`{"a":1}`,
		`{"a":1,"b":2,"c":3}`,
		`{ "a" : 1 , "b" : [1,2] , "c":{} }`,
		`{"a.b":1,"a\\b":2,"":3,"b":4}`,
		`{"same":1,"same":2}`,
		`{"x":1,"x":2,"y":3}`,
	}
	keyPool := []string{"a", "b", "c", "a.b", `a\b`, "", "same", "x", "z", "k"}
	for _, doc := range docs {
		for _, oldName := range keyPool {
			for _, newName := range keyPool {
				for _, overwrite := range []bool{false, true} {
					want, wantErr := Rename(doc, oldName, newName, overwrite)

					b1 := []byte(doc)
					gotB, errB := RenameBytes(b1, oldName, newName, overwrite)
					if (errB == nil) != (wantErr == nil) {
						t.Fatalf("bytes err mismatch for doc=%s %q->%q: %v vs %v",
							doc, oldName, newName, errB, wantErr)
					}
					if wantErr == nil &&
						sortJSON(string(gotB)) != sortJSON(want) {
						t.Fatalf("bytes mismatch for doc=%s %q->%q: %s vs %s",
							doc, oldName, newName, gotB, want)
					}

					b2 := make([]byte, len(doc))
					copy(b2, doc)
					opts := &Options{
						Optimistic: true, ReplaceInPlace: true,
						Overwrite: overwrite,
					}
					gotI, errI := RenameBytesOptions(b2, oldName, newName, opts)
					if (errI == nil) != (wantErr == nil) {
						t.Fatalf("inplace err mismatch for doc=%s %q->%q: %v vs %v",
							doc, oldName, newName, errI, wantErr)
					}
					if wantErr != nil {
						if string(b2) != doc {
							t.Fatalf("input corrupted after error doc=%s %q->%q: %s",
								doc, oldName, newName, b2)
						}
						if string(gotI) != doc {
							t.Fatalf("return corrupted after error doc=%s %q->%q: %s",
								doc, oldName, newName, gotI)
						}
						continue
					}
					if sortJSON(string(gotI)) != sortJSON(want) {
						t.Fatalf("inplace mismatch for doc=%s %q->%q: %s vs %s",
							doc, oldName, newName, gotI, want)
					}
					var probe interface{}
					if json.Unmarshal(gotI, &probe) != nil {
						t.Fatalf("invalid json for doc=%s %q->%q: %s",
							doc, oldName, newName, gotI)
					}
				}
			}
		}
	}
}

func TestRenameRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	keys := []string{"a", "bb", "ccc", "d.dd", `e\e`, "", "é", "x*x"}
	mkKey := func() string {
		return keys[rng.Intn(len(keys))]
	}
	for iter := 0; iter < 4000; iter++ {
		n := rng.Intn(6)
		// assemble an object manually with controlled key distribution
		var b strings.Builder
		b.WriteByte('{')
		pairs := make([]testKVP, n)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			k := mkKey()
			kb, _ := json.Marshal(k)
			v := rng.Intn(1000)
			b.Write(kb)
			b.WriteByte(':')
			b.WriteString(fmt.Sprint(v))
			pairs[i] = testKVP{k, fmt.Sprint(v)}
		}
		b.WriteByte('}')
		doc := b.String()
		var probe map[string]interface{}
		if json.Unmarshal([]byte(doc), &probe) != nil {
			t.Fatalf("bad fixture: %s", doc)
		}

		oldName := keys[rng.Intn(len(keys))]
		newName := keys[rng.Intn(len(keys))]
		overwrite := rng.Intn(2) == 0

		ref, refErr := referenceRename(doc, oldName, newName, overwrite)

		got, err := Rename(doc, oldName, newName, overwrite)
		if (err == nil) != (refErr == nil) {
			t.Fatalf("iter %d doc=%s %q->%q ow=%v: err %v, want err %v",
				iter, doc, oldName, newName, overwrite, err, refErr)
		}
		if refErr == nil {
			if !jsonEqualRef(got, ref) {
				t.Fatalf("iter %d doc=%s %q->%q ow=%v:\n got %s\nwant %s",
					iter, doc, oldName, newName, overwrite, got, ref)
			}
			if oldName == newName {
				if got != doc {
					t.Fatalf("iter %d self-rename changed document: %s", iter, got)
				}
			} else {
				wantPos := indexOf(pairs, oldName)
				if wantPos < 0 {
					t.Fatalf("iter %d fixture missing old key %q: %s", iter, oldName, doc)
				}
				if ni := indexOf(pairs, newName); ni >= 0 && ni < wantPos {
					wantPos--
				}
				gotKeys := orderedKeys(got)
				if wantPos >= len(gotKeys) {
					t.Fatalf("iter %d bad result keys for doc=%s: %s", iter, doc, got)
				}
				if gotKeys[wantPos] != newName {
					t.Fatalf("iter %d position not preserved doc=%s %q->%q: %s",
						iter, doc, oldName, newName, got)
				}
			}
		}

		in := []byte(doc)
		opts := &Options{
			Optimistic: true, ReplaceInPlace: true, Overwrite: overwrite,
		}
		gotb, ierr := RenameBytesOptions(in, oldName, newName, opts)
		if (ierr == nil) != (refErr == nil) {
			t.Fatalf("iter %d inplace err mismatch: %v vs %v", iter, ierr, refErr)
		}
		if refErr != nil {
			if string(in) != doc || string(gotb) != doc {
				t.Fatalf("iter %d doc corrupted after error: %s / %s",
					iter, in, gotb)
			}
			continue
		}
		if !jsonEqualRef(string(gotb), ref) {
			t.Fatalf("iter %d inplace doc=%s %q->%q:\n got %s\nwant %s",
				iter, doc, oldName, newName, gotb, ref)
		}
	}
}

type testKVP struct{ k, v string }

func indexOf(pairs []testKVP, key string) int {
	for i, p := range pairs {
		if p.k == key {
			return i
		}
	}
	return -1
}

func orderedKeys(doc string) []string {
	dec := json.NewDecoder(strings.NewReader(doc))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		var v interface{}
		if err := dec.Decode(&v); err != nil {
			return nil
		}
		keys = append(keys, tok.(string))
	}
	return keys
}

func jsonEqualRef(a, b string) bool {
	var va, vb interface{}
	if json.Unmarshal([]byte(a), &va) != nil ||
		json.Unmarshal([]byte(b), &vb) != nil {
		return false
	}
	return sortJSON(a) == sortJSON(b)
}

// referenceRename models the required behavior over an ordered member
// list, mirroring duplicate detection and position preservation.
func referenceRename(doc, oldName, newName string,
	overwrite bool) (string, error) {
	if oldName == newName {
		return doc, nil
	}
	res := gjson.Parse(doc)
	if res.Type != gjson.JSON || len(res.Raw) == 0 || res.Raw[0] != '{' {
		return "", errRefObject
	}
	type kvp struct {
		k  string
		vb []byte
	}
	var pairs []kvp
	var ambiguous bool
	res.ForEach(func(k, v gjson.Result) bool {
		if k.Str == oldName || k.Str == newName {
			count := 0
			res.ForEach(func(k2, _ gjson.Result) bool {
				if k2.Str == k.Str {
					count++
				}
				return true
			})
			if count > 1 {
				ambiguous = true
			}
		}
		pairs = append(pairs, kvp{k.Str, []byte(v.Raw)})
		return true
	})
	if ambiguous {
		return "", errRefAmbiguous
	}
	var oldIdxs, newIdxs []int
	for i, p := range pairs {
		if p.k == oldName {
			oldIdxs = append(oldIdxs, i)
		}
		if p.k == newName {
			newIdxs = append(newIdxs, i)
		}
	}
	if len(oldIdxs) == 0 {
		return "", errRefNotFound
	}
	if len(oldIdxs) > 1 || len(newIdxs) > 1 {
		return "", errRefAmbiguous
	}
	if len(newIdxs) == 1 && !overwrite {
		return "", errRefExists
	}
	pairs[oldIdxs[0]].k = newName
	if len(newIdxs) == 1 {
		ni := newIdxs[0]
		pairs = append(pairs[:ni], pairs[ni+1:]...)
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(p.k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(p.vb)
	}
	b.WriteByte('}')
	return b.String(), nil
}

var (
	errRefObject    = errSentinel("object required")
	errRefNotFound  = errSentinel("not found")
	errRefAmbiguous = errSentinel("ambiguous")
	errRefExists    = errSentinel("exists")
)

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
