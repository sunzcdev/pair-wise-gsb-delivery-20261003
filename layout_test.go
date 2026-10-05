package toml_test

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/internal/assert"
)

func encodeWithLayout(t *testing.T, v interface{}, f toml.LayoutFunc, opts ...func(*toml.Encoder)) (string, error) {
	t.Helper()
	var b strings.Builder
	enc := toml.NewEncoder(&b).SetLayoutFunc(f)
	for _, o := range opts {
		o(enc)
	}
	err := enc.Encode(v)
	return b.String(), err
}

// recordLayout collects a copy of every path the callback is handed.
func recordLayout() (toml.LayoutFunc, *[][]toml.LayoutStep) {
	var seen [][]toml.LayoutStep
	return func(p []toml.LayoutStep) (toml.Layout, error) {
		seen = append(seen, p)
		return toml.Layout{}, nil
	}, &seen
}

func roundTrips(t *testing.T, out string, want interface{}) {
	t.Helper()
	got := reflect.New(reflect.TypeOf(want)).Interface()
	assert.NoError(t, toml.Unmarshal([]byte(out), got))
	assert.Equal(t, want, reflect.ValueOf(got).Elem().Interface())
}

func pathString(p []toml.LayoutStep) string {
	var b strings.Builder
	for _, s := range p {
		if s.IsIndex() {
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(s.Index))
			b.WriteByte(']')
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.Key)
	}
	return b.String()
}

// TestLayoutMapKeyInline verifies the callback can inline a dynamically keyed
// map table that cannot carry a struct tag.
func TestLayoutMapKeyInline(t *testing.T) {
	doc := map[string]interface{}{
		"people": map[string]interface{}{
			"alice": map[string]int{"age": 30},
		},
		"nums": []int{1, 2},
	}
	out, err := encodeWithLayout(t, doc, func(p []toml.LayoutStep) (toml.Layout, error) {
		if len(p) >= 1 && p[0].Key == "people" {
			return toml.InlineTable(), nil
		}
		return toml.Layout{}, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, "nums = [1, 2]\npeople = {alice = {age = 30}}\n", out)

	var back map[string]interface{}
	assert.NoError(t, toml.Unmarshal([]byte(out), &back))
	assert.Equal(t, int64(30), back["people"].(map[string]interface{})["alice"].(map[string]interface{})["age"].(int64))
}

// TestLayoutArrayMultilineByPath chooses multiline for one array and single
// line for another through the callback.
func TestLayoutArrayMultilineByPath(t *testing.T) {
	doc := struct {
		Multi []int
		Flat  []int
	}{Multi: []int{1, 2, 3}, Flat: []int{4, 5}}
	out, err := encodeWithLayout(t, doc, func(p []toml.LayoutStep) (toml.Layout, error) {
		if len(p) == 1 && p[0].Key == "Multi" {
			return toml.MultilineArray(), nil
		}
		return toml.Layout{}, nil
	})
	assert.NoError(t, err)
	assert.Equal(t,
		"Multi = [\n  1,\n  2,\n  3\n]\nFlat = [4, 5]\n",
		out)
	roundTrips(t, out, doc)
}

// TestLayoutNestedArrayElementsDiffer ensures that a same-named field reached
// through different array-table elements can take different layouts, and that
// indices appear in the path.
func TestLayoutNestedArrayElementsDiffer(t *testing.T) {
	type elem struct {
		Name string
		Sub  map[string]int
	}
	type doc struct {
		Items []elem
	}
	v := doc{Items: []elem{
		{Name: "a", Sub: map[string]int{"x": 1}},
		{Name: "b", Sub: map[string]int{"y": 2}},
	}}

	var captured []string
	out, err := encodeWithLayout(t, v, func(p []toml.LayoutStep) (toml.Layout, error) {
		captured = append(captured, pathString(p))
		// Inline Sub only inside the first array-table element.
		inline := false
		for _, s := range p {
			if s.IsIndex() {
				inline = s.Index == 0
			}
			if s.IsKey() && s.Key == "Sub" && inline {
				return toml.InlineTable(), nil
			}
		}
		return toml.Layout{}, nil
	})
	assert.NoError(t, err)
	assert.Equal(t,
		"[[Items]]\nName = 'a'\nSub = {x = 1}\n\n[[Items]]\nName = 'b'\n\n[Items.Sub]\ny = 2\n",
		out)

	// Both indexed paths must have been visited.
	assert.True(t, contains(captured, "Items[0].Sub"))
	assert.True(t, contains(captured, "Items[1].Sub"))

	roundTrips(t, out, v)
}

// TestLayoutPathStableWhenSaved guarantees that slices handed to the callback
// are copies: mutating a retained path after the call does not change a later
// encode, and the encoder never observes the mutation.
func TestLayoutPathStableWhenSaved(t *testing.T) {
	doc := struct{ A map[string]int }{A: map[string]int{"k": 1}}

	f, seen := recordLayout()
	var first strings.Builder
	assert.NoError(t, toml.NewEncoder(&first).SetLayoutFunc(f).Encode(doc))

	// Mutate every retained slice.
	for _, p := range *seen {
		for i := range p {
			p[i] = toml.LayoutStep{Key: "HACKED", Index: 99}
		}
	}

	// A fresh callback and encode must produce the same, valid output.
	var second strings.Builder
	assert.NoError(t, toml.NewEncoder(&second).SetLayoutFunc(func(p []toml.LayoutStep) (toml.Layout, error) {
		return toml.Layout{}, nil
	}).Encode(doc))
	assert.Equal(t, first.String(), second.String())

	// The callback also must not be able to perturb the encoder's own stack:
	// reusing the same callback that mutates its argument while encoding must
	// still produce correct headers.
	var third strings.Builder
	assert.NoError(t, toml.NewEncoder(&third).SetLayoutFunc(func(p []toml.LayoutStep) (toml.Layout, error) {
		for i := range p {
			p[i] = toml.LayoutStep{Key: "HACKED", Index: 99}
		}
		return toml.Layout{}, nil
	}).Encode(doc))
	assert.Equal(t, "\n[A]\nk = 1\n", "\n"+third.String())
}

// TestLayoutCallbackOnlyForTablesAndArrays pins that scalars never invoke the
// callback, while the root, tables, arrays, and the members of array-table
// elements do.
func TestLayoutCallbackOnlyForTablesAndArrays(t *testing.T) {
	doc := map[string]interface{}{
		"scalar": 1,
		"table":  map[string]int{"k": 1},
		"arr":    []int{1, 2},
		"tables": []map[string]map[string]int{{"nested": {"a": 1}}, {"nested": {"b": 2}}},
	}
	f, seen := recordLayout()
	_, err := encodeWithLayout(t, doc, f)
	assert.NoError(t, err)

	var paths []string
	for _, p := range *seen {
		paths = append(paths, pathString(p))
	}
	assert.True(t, contains(paths, ""))                 // root
	assert.True(t, contains(paths, "table"))            // table
	assert.True(t, contains(paths, "arr"))              // plain array
	assert.True(t, contains(paths, "tables"))           // array of tables entry
	assert.True(t, contains(paths, "tables[0].nested")) // member of element 0
	assert.True(t, contains(paths, "tables[1].nested")) // member of element 1
	assert.False(t, contains(paths, "scalar"))          // scalar skipped
	assert.False(t, contains(paths, "arr[0]"))          // scalar element skipped
	assert.False(t, contains(paths, "table.k"))         // scalar member skipped
}

// TestLayoutTagPrecedence verifies that an "inline" tag wins over a callback
// asking for a header table, and a "multiline" tag wins over the callback and
// the global option.
func TestLayoutTagPrecedence(t *testing.T) {
	type inner struct{ A int }
	doc := struct {
		T inner `toml:"t,inline"`
		N []int `toml:"n,multiline"`
	}{T: inner{A: 1}, N: []int{1, 2}}

	out, err := encodeWithLayout(t, doc, func(p []toml.LayoutStep) (toml.Layout, error) {
		return toml.HeaderTable(), nil
	})
	assert.NoError(t, err)
	assert.Equal(t, "t = {A = 1}\nn = [\n  1,\n  2\n]\n", out)
}

// TestLayoutCallbackOverridesGlobal verifies the callback sits between tags
// and global options: it can force a header table against SetTablesInline and
// a single-line array against SetArraysMultiline.
func TestLayoutCallbackOverridesGlobal(t *testing.T) {
	type inner struct{ A int }
	doc := struct {
		Head inner
		Flat []int
		Def  inner
	}{Head: inner{1}, Flat: []int{1, 2}, Def: inner{3}}

	out, err := encodeWithLayout(t, doc, func(p []toml.LayoutStep) (toml.Layout, error) {
		if len(p) == 1 && p[0].Key == "Head" {
			return toml.HeaderTable(), nil
		}
		if len(p) == 1 && p[0].Key == "Flat" {
			return toml.SinglelineArray(), nil
		}
		return toml.Layout{}, nil
	}, func(e *toml.Encoder) {
		e.SetTablesInline(true)
		e.SetArraysMultiline(true)
	})
	assert.NoError(t, err)
	assert.Equal(t,
		"Flat = [1, 2]\nDef = {A = 3}\n\n[Head]\nA = 1\n",
		out)
	roundTrips(t, out, doc)
}

// TestLayoutInvalidCombos covers the combinations the encoder must reject
// with an error that names the path.
func TestLayoutInvalidCombos(t *testing.T) {
	doc := map[string]interface{}{
		"outer": map[string]interface{}{"inner": map[string]int{"x": 1}},
	}

	t.Run("inline document root", func(t *testing.T) {
		_, err := encodeWithLayout(t, map[string]int{"a": 1}, func(p []toml.LayoutStep) (toml.Layout, error) {
			if len(p) == 0 {
				return toml.InlineTable(), nil
			}
			return toml.Layout{}, nil
		})
		assert.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "<root>"), err.Error())
	})

	t.Run("header table inside inline table", func(t *testing.T) {
		_, err := encodeWithLayout(t, doc, func(p []toml.LayoutStep) (toml.Layout, error) {
			var lay toml.Layout
			for _, s := range p {
				if s.Key == "outer" {
					lay = toml.InlineTable()
				}
				if s.Key == "inner" {
					lay = toml.HeaderTable()
				}
			}
			return lay, nil
		})
		assert.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "outer.inner"), err.Error())
	})

	t.Run("callback error carries path", func(t *testing.T) {
		sentinel := errors.New("boom")
		_, err := encodeWithLayout(t, doc, func(p []toml.LayoutStep) (toml.Layout, error) {
			if len(p) == 1 && p[0].Key == "outer" {
				return toml.Layout{}, sentinel
			}
			return toml.Layout{}, nil
		})
		assert.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "outer"), err.Error())
		assert.True(t, errors.Is(err, sentinel))
	})

	t.Run("callback error at root", func(t *testing.T) {
		sentinel := errors.New("root-boom")
		_, err := encodeWithLayout(t, map[string]int{"a": 1}, func(p []toml.LayoutStep) (toml.Layout, error) {
			if len(p) == 0 {
				return toml.Layout{}, sentinel
			}
			return toml.Layout{}, nil
		})
		assert.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "<root>"), err.Error())
		assert.True(t, errors.Is(err, sentinel))
	})

	t.Run("quoted key in error path", func(t *testing.T) {
		// A key that needs quoting must appear quoted in the error message,
		// and an array-index path renders as key[index]. A plain (inline)
		// array of maps invokes the callback per element.
		doc := map[string]interface{}{
			"weird.key": []map[string]int{{"v": 1}},
		}
		_, err0 := encodeWithLayout(t, doc, func(p []toml.LayoutStep) (toml.Layout, error) {
			if len(p) == 1 && p[0].Key == "weird.key" {
				return toml.InlineTable(), nil
			}
			return toml.Layout{}, nil
		})
		assert.NoError(t, err0)
		_, err := encodeWithLayout(t, doc, func(p []toml.LayoutStep) (toml.Layout, error) {
			if len(p) == 1 && p[0].Key == "weird.key" {
				return toml.InlineTable(), nil
			}
			for _, s := range p {
				if s.IsIndex() {
					return toml.Layout{}, errors.New("idx")
				}
			}
			return toml.Layout{}, nil
		})
		assert.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), `'weird.key'[0]`), err.Error())
	})

	t.Run("nil pointer value does not invoke callback", func(t *testing.T) {
		type sub struct{ A int }
		doc := struct {
			P *sub
			S sub
		}{P: nil, S: sub{A: 1}}
		f, seen := recordLayout()
		out, err := encodeWithLayout(t, doc, f)
		assert.NoError(t, err)
		assert.Equal(t, "[S]\nA = 1\n", out)
		for _, p := range *seen {
			assert.False(t, len(p) == 1 && p[0].Key == "P")
		}
	})
}

// TestLayoutArrayOfTablesInline renders a whole array of tables inline by
// selecting Inline at the array path, and confirms it round-trips.
func TestLayoutArrayOfTablesInline(t *testing.T) {
	type item struct{ X int }
	doc := struct{ Items []item }{Items: []item{{1}, {2}}}
	out, err := encodeWithLayout(t, doc, func(p []toml.LayoutStep) (toml.Layout, error) {
		if len(p) == 1 && p[0].Key == "Items" {
			return toml.InlineTable(), nil
		}
		return toml.Layout{}, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, "Items = [{X = 1}, {X = 2}]\n", out)
	roundTrips(t, out, doc)
}

// TestLayoutMemberOfArrayTableMultiline chooses a multiline array for a
// same-named member of only the first array-table element, proving the path
// (including the index) drives layout decisions inside array tables.
func TestLayoutMemberOfArrayTableMultiline(t *testing.T) {
	type item struct {
		Tags []int
	}
	doc := struct{ Items []item }{Items: []item{{Tags: []int{1, 2}}, {Tags: []int{3, 4}}}}
	out, err := encodeWithLayout(t, doc, func(p []toml.LayoutStep) (toml.Layout, error) {
		for _, s := range p {
			if s.IsIndex() && s.Index == 0 {
				return toml.MultilineArray(), nil
			}
		}
		return toml.Layout{}, nil
	})
	assert.NoError(t, err)
	assert.Equal(t,
		"[[Items]]\nTags = [\n  1,\n  2\n]\n\n[[Items]]\nTags = [3, 4]\n",
		out)
	roundTrips(t, out, doc)
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
