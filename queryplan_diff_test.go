package gjson

import "testing"

const diffJSON = `{
  "name": {"first": "Tom", "last": "Anderson"},
  "age": 37,
  "children": ["Sara", "Alex", "Jack"],
  "fav.movie": "Deer Hunter",
  "friends": [
    {"first": "Dale", "last": "Murphy", "age": 44, "nets": ["ig", "fb", "tw"]},
    {"first": "Roger", "last": "Craig", "age": 68, "nets": ["fb", "tw"]},
    {"first": "Jane", "last": "Murphy", "age": 47, "nets": ["ig", "tw"]}
  ],
  "a": [{"x": {"y": 1}, "z": 9}, {"x": {"y": 2}, "z": 8}],
  "vals": [
    {"a": 1, "b": "data"},
    {"a": 2, "b": true},
    {"a": 3, "b": false},
    {"a": 4, "b": "0"},
    {"a": 5, "b": 0},
    {"a": 6, "b": "1"},
    {"a": 10, "b": null},
    {"a": 11}
  ],
  "loggy": {"programmers": [{"email": "a@x"}, {"email": "b@y"}]},
  "lastly": {"end...ing": 42, "yay": 1},
  "arr": [0, 1, 2, 3, {"hello": "world"}]
}`

var diffPaths = []string{
	"",
	"age",
	"name",
	"name.first",
	"name.last",
	"name.missing",
	"noop.what is a wren?",
	"children",
	"children.0",
	"children.1",
	"children.2",
	"children.3",
	"children.#",
	"children.#.0",
	"children.#.#",
	"children.@reverse",
	"children.@reverse.0",
	"children.@reverse.#",
	"fav\\.movie",
	"lastly.end\\.\\.\\.ing",
	"lastly.yay",
	"child*.2",
	"c?ildren.0",
	"friends.#",
	"friends.1",
	"friends.1.first",
	"friends.1.nets.0",
	"friends.2.nets.#",
	"friends.#(last==\"Murphy\").first",
	"friends.#(last==\"Murphy\")#.first",
	"friends.#(age>45)#.last",
	"friends.#(first%\"D*\").last",
	"friends.#(first!%\"D*\").last",
	"friends.#(nets.#(==\"fb\"))#.first",
	"friends.#(age>=44)#.age",
	"friends.#(age<45).first",
	"friends.#(last==\"Murphy\")#.0",
	"friends.#(last==\"Murphy\")#|0",
	"friends.#(last==\"Murphy\")#|#",
	"friends.#(age==68)#",
	"friends.#(age==68)",
	"friends.#(nope==1)",
	"friends.#(nets.#(==\"zz\"))#.first",
	"friends.#(last==\"Murphy\")#.last",
	"friends.#(last==\"Murphy\")#.age",
	"vals.#(b==~true)#.a",
	"vals.#(b==~false)#.a",
	"vals.#(b==~null)#.a",
	"vals.#(b==~*)#.a",
	"vals.#(b!=~*)#.a",
	"a.#.x.y",
	"a.#.x",
	"a.#(z==8).x.y",
	"a.#(z==8)#.x",
	"a.#(z==8)#|0",
	"a.#(x.y==2).z",
	"a.#.z",
	"a.#.x|y",
	"loggy.programmers.0.email",
	"loggy.programmers.#.email",
	"loggy.programmers.1.email",
	"arr.0",
	"arr.3",
	"arr.4.hello",
	"arr.4.#",
	"arr.#",
	"@this",
	"@this.age",
	"@reverse",
	"@reverse.children.0",
	"@keys",
	"@values",
	"@flatten",
	"{name.first,age}",
	"{name.first,age,children.0}",
	"{name.first,\"m\":friends.#(age>45)#.last}",
	"[children.0,children.2]",
	"[friends.#(age==68).first,age]",
	"!true",
	"!42",
	"!\"hello\"",
	"!{\"a\":1}",
	"!true.age",
	"{\"x\":!1,\"y\":!false}",
	"age|name.first",
	"friends|0|first",
	"friends|#",
	"name|first",
	"@pretty",
	"@pretty:{\"sortKeys\":true}",
	"@pretty:{\"sortKeys\":true}.age",
	"children.@reverse|0",
	"friends.#(last==\"Murphy\")#|0.first",
	"friends.#(nets.1==\"tw\").first",
	"a.0.x.y",
	"a.1.z",
	"missing.#.x",
	"missing.0",
	"vals.#(a>8)#|#",
	"vals.#(a>8)#.a",
}

func sameResult(a, b Result) bool {
	if a.Type != b.Type || a.Raw != b.Raw || a.Str != b.Str ||
		a.Num != b.Num || a.Index != b.Index {
		return false
	}
	if len(a.Indexes) != len(b.Indexes) {
		return false
	}
	for i := range a.Indexes {
		if a.Indexes[i] != b.Indexes[i] {
			return false
		}
	}
	return true
}

func TestPlanDiff(t *testing.T) {
	for _, path := range diffPaths {
		want := Get(diffJSON, path)
		p, err := Compile(path)
		if err != nil {
			t.Fatalf("Compile(%q) error: %v", path, err)
		}
		got := p.Get(diffJSON)
		if !sameResult(want, got) {
			t.Fatalf("path %q\n want %+v raw=%q idx=%v idxs=%v\n got  %+v raw=%q idx=%v idxs=%v",
				path, want.Type, want.Raw, want.Index, want.Indexes,
				got.Type, got.Raw, got.Index, got.Indexes)
		}
		gb := p.GetBytes([]byte(diffJSON))
		wb := GetBytes([]byte(diffJSON), path)
		if gb.Raw != wb.Raw || gb.Str != wb.Str {
			t.Fatalf("bytes path %q\n want raw=%q str=%q\n got  raw=%q str=%q",
				path, wb.Raw, wb.Str, gb.Raw, gb.Str)
		}
	}
}

func TestPlanCompileErrors(t *testing.T) {
	bad := []struct {
		path string
		off  int
	}{
		{"friends.#(last==", 16},
		{"friends.#(last==\"abc", 16},
		{"friends.#(age>1", 15},
		{"a.#(x.y==2", 10},
		{"{name.first,age", 15},
		{"[children.0", 11},
		{"#(x==1", 6},
		{"#(a==\"x)", 5},
	}
	for _, b := range bad {
		path := b.path
		p, err := Compile(path)
		if err == nil {
			t.Fatalf("expected error compiling %q, got plan (raw=%q)",
				path, p.Get(diffJSON).Raw)
		}
		pe, ok := err.(*PathError)
		if !ok {
			t.Fatalf("expected *PathError for %q, got %T", path, err)
		}
		if pe.Offset != b.off {
			t.Fatalf("offset for %q = %d, want %d (%v)",
				path, pe.Offset, b.off, pe)
		}
	}
}
