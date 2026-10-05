package gojq

import (
	"reflect"
	"testing"
)

func TestBuiltinTopKEntries(t *testing.T) {
	for _, src := range []string{
		"def top_k($k; g): _top_k($k; g);",
		"def top_k_by($k; g; f): _top_k_by($k; g; f);",
	} {
		q, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		fd := q.FuncDefs[0]
		got := builtinFuncDefs[fd.Name]
		found := false
		for _, g := range got {
			if reflect.DeepEqual(g, fd) {
				found = true
			}
		}
		if !found {
			t.Errorf("builtinFuncDefs[%q] does not contain the parsed definition\nparse: %#v\ngot:   %#v",
				fd.Name, fd, got)
		}
	}
	if _, ok := builtinFuncDefs["_top_k"]; !ok {
		t.Error("missing _top_k placeholder")
	}
	if _, ok := builtinFuncDefs["_top_k_by"]; !ok {
		t.Error("missing _top_k_by placeholder")
	}
}
