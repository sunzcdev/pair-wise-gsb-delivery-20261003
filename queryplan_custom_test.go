package gjson

import (
	"strings"
	"testing"
)

func TestPlanCustomModifier(t *testing.T) {
	AddModifier("upper", func(json, arg string) string {
		return strings.ToUpper(json)
	})
	j := `{"children":["sara","alex"]}`
	for _, p := range []string{"children.@upper", "@upper", "children.@upper.0"} {
		w := Get(j, p)
		pl, err := Compile(p)
		if err != nil {
			t.Fatal(err)
		}
		g := pl.Get(j)
		if w.Raw != g.Raw {
			t.Fatalf("%q want=%q got=%q", p, w.Raw, g.Raw)
		}
	}
}
