package gjson

import (
	"sync"
	"testing"
)

func TestPlanConcurrent(t *testing.T) {
	paths := []string{
		"friends.#(last==\"Murphy\")#.first",
		"a.#(z>0)#.x.y",
		"children.@reverse.0",
		"{name.first,age}",
		"vals.#(b==~true)#.a",
		"missing.deep.#.x",
	}
	jsons := []string{
		diffJSON,
		`{"friends":[],"children":[1],"a":[],"vals":[]}`,
		`{"friends":[{"last":"X"}]}`,
		`not json at all`,
		``,
	}
	var plans []*QueryPlan
	for _, p := range paths {
		pl, err := Compile(p)
		if err != nil {
			t.Fatalf("compile %q: %v", p, err)
		}
		plans = append(plans, pl)
	}
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for n := 0; n < 2000; n++ {
				j := jsons[(g+n)%len(jsons)]
				for idx, p := range plans {
					want := Get(j, paths[idx])
					got := p.Get(j)
					if !sameResult(want, got) {
						t.Errorf("concurrent mismatch g=%d path=%q json=%q", g, paths[idx], j)
						return
					}
					// interleaved empty/failing lookups must not pollute
					_ = p.Get(`{}`)
					_ = p.Get(``)
					again := p.Get(j)
					if !sameResult(want, again) {
						t.Errorf("state leaked g=%d path=%q", g, paths[idx])
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestMustCompilePanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	MustCompile("a.#(x==")
}
