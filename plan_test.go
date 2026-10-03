package gjson

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func resultEqual(a, b Result) bool {
	if a.Type != b.Type || a.Raw != b.Raw || a.Str != b.Str ||
		a.Num != b.Num || a.Index != b.Index {
		return false
	}
	if !reflect.DeepEqual(a.Indexes, b.Indexes) {
		return false
	}
	return true
}

func assertPlan(t *testing.T, json, path string) {
	t.Helper()
	plan, err := Compile(path)
	if err != nil {
		t.Fatalf("Compile(%q) error: %v", path, err)
	}
	got := plan.Get(json)
	want := Get(json, path)
	if !resultEqual(got, want) {
		t.Fatalf("path %q:\n want %#v\n  got %#v", path, want, got)
	}
	if ps := plan.String(); ps != path {
		t.Fatalf("String() = %q, want %q", ps, path)
	}
}

func TestPlanBasic(t *testing.T) {
	json := readmeJSON
	paths := []string{
		"name",
		"name.last",
		"age",
		"children",
		"children.#",
		"children.1",
		"child*.2",
		"c?ildren.0",
		"friends.#.first",
		"friends.1.last",
		"friends.#(last==\"Murphy\").first",
		"friends.#(age>45)#.last",
		"friends.#(first%\"D*\")",
		"friends.#(age!=68).age",
		"friends.#(last=Murphy)#",
		"favorites.movies.#(rating>4.5)",
		"favorites.movies.#(rating>4.5).title",
		"name|@this",
		"children|@reverse|0",
		`name\.last`,
		`name\.middle`,
		"@this",
		"@ugly",
		"@reverse",
		`@pretty:{"sortKeys":true}`,
		`@pretty:{"sortKeys":true}|@ugly`,
		"children|@reverse|#",
		"!true",
		"!false",
		"!null",
		"!inf",
		"!nan",
		"!42",
		`!"hello"`,
		`!{"a":1}`,
		`!{"a":1}.a`,
		`!["x","y"].1`,
		`!{"name":{"first":"Tom"}}.{name.first}.first`,
		`[name.last,age]`,
		`{"last":name.last,"years":age}`,
		`{name.last,"foo":!"bar"}`,
		`[!true,!false,!null]`,
		`friends.#.first|@reverse|0`,
		`info.friends|0|first`,
		"missing.deep.path",
		"children.99",
		"",
		"*",
		"friends.#(age)",
		"friends.#(missing)",
		"friends.#(age<50)#.first",
		`@flatten`,
		"favorites.movies.#.title",
	}
	for _, path := range paths {
		assertPlan(t, json, path)
	}
}

func TestPlanEscapedDots(t *testing.T) {
	json := `{"a.b":{"c.d":1},"x\\|y":2,"a.b.c":3,"dot.end":4}`
	paths := []string{
		`a\.b.c\.d`,
		`a\.b`,
		`x\\\|y`,
		`a\.b\.c`,
		`dot\.end`,
		`a\.b|@this`,
		`a\.b.c\.d|@this`,
	}
	for _, path := range paths {
		assertPlan(t, json, path)
	}
}

func TestPlanQueryStrings(t *testing.T) {
	json := `{
		"people": [
			{"name": "a|b", "city": "NY, C", "note": "say \"hi\""},
			{"name": "x.y", "city": "LA", "note": "plain"},
			{"name": "a|b", "city": "SF", "note": "q\\u0075ote"}
		]
	}`
	paths := []string{
		`people.#(name=="a|b").city`,
		`people.#(name=="a|b")#.city`,
		`people.#(city=="NY, C")`,
		`people.#(note=="say \"hi\"")`,
		`people.#(name%="x*")`,
		`people.#[name=="x.y"]#`,
		`people.#(name=="a|b")|#`,
		`people.#(name=="a|b")|@reverse`,
		`people.#(note=="q\\u0075ote")`,
	}
	for _, path := range paths {
		assertPlan(t, json, path)
	}
}

func TestPlanNestedSubselectors(t *testing.T) {
	json := readmeJSON
	paths := []string{
		`[friends.#.first, friends.#.last]`,
		`{"fs":friends.#(age>40)#.first}`,
		`{name.last,[children.0,children.1]}`,
		`{"a":{"b":name.first}}`,
		`[!{"x":1}.x, age]`,
		`{"k":@reverse}`,
		`{name.first}|@ugly`,
		`[friends.0.first,"literal",!99]`,
	}
	for _, path := range paths {
		assertPlan(t, json, path)
	}
}

func TestPlanModifierJSONArgs(t *testing.T) {
	json := `{"other":{"hello":"world"},"arr":[1,2,3,4,5,6]}`
	paths := []string{
		`@pretty:{"sortKeys":true}`,
		`@pretty:{"sortKeys":true}|@ugly`,
		`arr|@pretty:{"indent":"  "}`,
		`@join:{"sep":"-"}`,
		`@group:{"key":"a"}`,
		`arr|@reverse|@pretty:{"width":20}`,
		`other.@this`,
		`@case:upper`,
	}
	AddModifier("case", func(json, arg string) string {
		if arg == "upper" {
			return toUpperJSON(json)
		}
		return json
	})
	for _, path := range paths {
		assertPlan(t, json, path)
	}
}

func toUpperJSON(js string) string {
	var v interface{}
	if err := json.Unmarshal([]byte(js), &v); err != nil {
		return js
	}
	var walk func(v interface{}) interface{}
	walk = func(v interface{}) interface{} {
		switch x := v.(type) {
		case string:
			return strings.ToUpper(x)
		case map[string]interface{}:
			for k, vv := range x {
				x[k] = walk(vv)
			}
		case []interface{}:
			for i, vv := range x {
				x[i] = walk(vv)
			}
		}
		return v
	}
	b, _ := json.Marshal(walk(v))
	return string(b)
}

func TestPlanCompileErrors(t *testing.T) {
	cases := []struct {
		path     string
		position int
	}{
		{`friends.#(age>`, 8},
		{`friends.#[age>`, 8},
		{`friends.#(a=="x)`, 8},
		{`[name.last, age`, 0},
		{`{"a":1`, 0},
		{`[a, "unterminated]`, 0},
		{`@pretty:{"sortKeys":true`, 8},
		{`@mod:["unclosed"`, 4},
		{`@mod:"unclosed`, 4},
		{`!{"a":1`, 0},
		{`!["a"`, 0},
		{`!"unclosed`, 0},
	}
	for _, c := range cases {
		_, err := Compile(c.path)
		if err == nil {
			t.Fatalf("Compile(%q) expected error, got nil", c.path)
		}
		pe, ok := err.(*PathError)
		if !ok {
			t.Fatalf("Compile(%q) error type %T, want *PathError",
				c.path, err)
		}
		if pe.Position != c.position {
			t.Fatalf("Compile(%q) position = %d, want %d (%v)",
				c.path, pe.Position, c.position, err)
		}
	}
	if _, err := Compile(`ok.path`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPlanConcurrentAndIsolation(t *testing.T) {
	plan := MustCompile(`friends.#(age>40)#.first`)
	inputs := []string{
		readmeJSON,
		`{"friends":[{"age":41,"first":"x"}]}`,
		`{"friends":[]}`,
		`{"friends":[{"age":1,"first":"y"}]}`,
		`{}`,
		`not json`,
		`{"friends":[{"age":99,"first":"z"},{"age":50,"first":"q"}]}`,
	}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				json := inputs[(g+n)%len(inputs)]
				got := plan.Get(json)
				want := Get(json, `friends.#(age>40)#.first`)
				if !resultEqual(got, want) {
					t.Errorf("concurrent mismatch on %q", json)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// a failed/empty execution must not pollute the next one
	empty := plan.Get(`{}`)
	if empty.Exists() {
		t.Fatalf("expected empty result, got %#v", empty)
	}
	good := plan.Get(readmeJSON)
	if !good.Exists() || good.String() != `["Dale","Jane"]` {
		t.Fatalf("expected Dale,Jane after empty call, got %#v", good)
	}
}

func TestPlanGetBytes(t *testing.T) {
	json := []byte(readmeJSON)
	plan := MustCompile(`friends.#(age>45).first`)
	got := plan.GetBytes(json)
	want := GetBytes(json, `friends.#(age>45).first`)
	if !resultEqual(got, want) {
		t.Fatalf("GetBytes mismatch:\n want %#v\n  got %#v", want, got)
	}
	if plan.GetBytes(nil).Exists() {
		t.Fatal("nil bytes should be empty")
	}
}

// TestPlanRandomDifferential compiles randomized valid JSON and randomized
// paths and checks Plan.Get against Get.
func TestPlanRandomDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	pathAtoms := []string{
		"a", "b", "c", "name", "0", "1", "2", "#",
		"#.a", "#(a)", `#(a=="x")`, `#(b>1)#`, "*", "?",
		"@this", "@reverse", "!", "!true",
	}
	separators := []string{".", "|"}
	for iter := 0; iter < 3000; iter++ {
		json := randomJSONForPlan(rng, 0)
		depth := 1 + rng.Intn(4)
		path := pathAtoms[rng.Intn(len(pathAtoms))]
		for d := 0; d < depth; d++ {
			path += separators[rng.Intn(len(separators))] +
				pathAtoms[rng.Intn(len(pathAtoms))]
		}
		plan, err := Compile(path)
		if err != nil {
			// some random combinations are legitimately invalid
			continue
		}
		got := plan.Get(json)
		want := Get(json, path)
		if !resultEqual(got, want) {
			t.Fatalf("iter %d path %q json %s\n want %#v\n  got %#v",
				iter, path, json, want, got)
		}
	}
}

func randomJSONForPlan(rng *rand.Rand, depth int) string {
	if depth > 3 {
		switch rng.Intn(3) {
		case 0:
			return fmt.Sprintf("%d", rng.Intn(10))
		case 1:
			return `"x"`
		default:
			return "true"
		}
	}
	switch rng.Intn(3) {
	case 0:
		n := rng.Intn(4)
		s := "["
		for i := 0; i < n; i++ {
			if i > 0 {
				s += ","
			}
			s += randomJSONForPlan(rng, depth+1)
		}
		return s + "]"
	default:
		n := rng.Intn(4)
		s := "{"
		for i := 0; i < n; i++ {
			if i > 0 {
				s += ","
			}
			s += fmt.Sprintf(`"%s":`, []string{"a", "b", "c"}[rng.Intn(3)])
			s += randomJSONForPlan(rng, depth+1)
		}
		return s + "}"
	}
}
