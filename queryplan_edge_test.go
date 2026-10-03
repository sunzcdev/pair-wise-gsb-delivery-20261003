package gjson

import "testing"

func TestPlanEdgeCases(t *testing.T) {
	cases := []struct{ j, p string }{
		{`{"a":[{"x":1},{"x":2}]}`, "a.#.x|#"},
		{`{"a":[{"x":[10,20]},{"x":[30]}]}`, "a.#.x.0"},
		{`{"a":[{"x":[10,20]},{"x":[30]}]}`, "a.#.x.#"},
		{`{"a":[{"x":[10,20]},{"x":[30]}]}`, "a.#.x|0"},
		{`{"a":[1,2,3]}`, "a.#|#"},
		{`{"a":[{"x":1}]}`, "a.#(x==1)|x"},
		{`{"a":[{"x":1}]}`, "a.#(x==1).x|#"},
		{`{"nope":[]}`, "a.#.x"},
		{`{"a":[]}`, "a.#.x"},
		{`{"a":[]}`, "a.#(x==1)#"},
		{`{"a":[1,2]}`, "a.#.zzz"},
		{`{"a":[{"b":{"c":5}}]}`, "a.#.b.c"},
		{`{"a":[{"b":{"c":5}}]}`, "a.#(b.c==5)#.b.c"},
		{`{"f":{"a.b":1}}`, `f.a\.b`},
		{`{"f":{"a*b":1}}`, `f.a*b`},
	}
	for _, c := range cases {
		w := Get(c.j, c.p)
		pl, err := Compile(c.p)
		if err != nil {
			t.Fatalf("compile %q: %v", c.p, err)
		}
		g := pl.Get(c.j)
		if !sameResult(w, g) {
			t.Fatalf("%q on %q\n want type=%v raw=%q idx=%v idxs=%v\n got  type=%v raw=%q idx=%v idxs=%v",
				c.p, c.j, w.Type, w.Raw, w.Index, w.Indexes, g.Type, g.Raw, g.Index, g.Indexes)
		}
	}
}
