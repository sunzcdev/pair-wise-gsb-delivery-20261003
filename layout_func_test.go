package toml_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/internal/assert"
)

func encodeWithLayout(t *testing.T, v interface{}, f toml.LayoutFunc) (string, error) {
	t.Helper()
	var w strings.Builder
	enc := toml.NewEncoder(&w).SetLayoutFunc(f)
	err := enc.Encode(v)
	return w.String(), err
}

// A dynamic map cannot carry inline/multiline tags; the callback selects the
// layout by key path instead.
func TestLayoutFunc_DynamicMapInline(t *testing.T) {
	v := map[string]interface{}{
		"kept":    map[string]string{"a": "1"},
		"inlined": map[string]string{"b": "2"},
	}
	out, err := encodeWithLayout(t, v, func(p toml.LayoutPath) (toml.Layout, error) {
		if len(p) == 1 && p[0].Key == "inlined" {
			return toml.Layout{InlineTable: true}, nil
		}
		return toml.Layout{}, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, "inlined = {b = '2'}\n\n[kept]\na = '1'\n", out)
}

func TestLayoutFunc_MultilineArrayByPath(t *testing.T) {
	v := map[string]interface{}{
		"one": []int{1, 2, 3},
		"two": []int{4, 5},
	}
	out, err := encodeWithLayout(t, v, func(p toml.LayoutPath) (toml.Layout, error) {
		if len(p) == 1 && p[0].Key == "two" {
			return toml.Layout{MultilineArray: true}, nil
		}
		return toml.Layout{}, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, "one = [1, 2, 3]\ntwo = [\n  4,\n  5\n]\n", out)
}

// A nested array is reached through its element path, including indices.
func TestLayoutFunc_NestedArrayIndices(t *testing.T) {
	v := map[string]interface{}{
		"grid": [][]int{{1, 2}, {3, 4}},
	}
	var seen []string
	out, err := encodeWithLayout(t, v, func(p toml.LayoutPath) (toml.Layout, error) {
		seen = append(seen, p.String())
		// Wrap only the inner array at grid[0].
		if len(p) == 2 && p[0].Key == "grid" && p[1].IsIndex && p[1].Index == 0 {
			return toml.Layout{MultilineArray: true}, nil
		}
		return toml.Layout{}, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, "grid = [[\n  1,\n  2\n], [3, 4]]\n", out)
	assert.True(t, contains(seen, "grid[0]"))
	assert.True(t, contains(seen, "grid[1]"))
}

// Same-named fields of different array-table elements get different layouts,
// distinguished by the array index in the path.
func TestLayoutFunc_ArrayTableSameFieldDifferentLayout(t *testing.T) {
	type item struct {
		Name string
		Meta map[string]string
	}
	v := map[string]interface{}{
		"items": []item{
			{Name: "a", Meta: map[string]string{"k": "1"}},
			{Name: "b", Meta: map[string]string{"k": "2"}},
		},
	}
	out, err := encodeWithLayout(t, v, func(p toml.LayoutPath) (toml.Layout, error) {
		// Inline Meta only inside the first array-table element.
		if len(p) == 3 && p[0].Key == "items" && p[1].IsIndex && p[1].Index == 0 && p[2].Key == "Meta" {
			return toml.Layout{InlineTable: true}, nil
		}
		return toml.Layout{}, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, "[[items]]\nName = 'a'\nMeta = {k = '1'}\n\n[[items]]\nName = 'b'\n\n[items.Meta]\nk = '2'\n", out)

	// The output re-decodes to the same value.
	var got map[string]interface{}
	assert.NoError(t, toml.Unmarshal([]byte(out), &got))
	items := got["items"].([]interface{})
	assert.Equal(t, "a", items[0].(map[string]interface{})["Name"])
	assert.Equal(t, "1", items[0].(map[string]interface{})["Meta"].(map[string]interface{})["k"])
	assert.Equal(t, "2", items[1].(map[string]interface{})["Meta"].(map[string]interface{})["k"])
}

// An entire array of tables can be a multiline array of inline tables.
func TestLayoutFunc_ArrayOfTablesInlineMultiline(t *testing.T) {
	type item struct {
		Name string
	}
	v := map[string]interface{}{
		"items": []item{{Name: "a"}, {Name: "b"}},
	}
	out, err := encodeWithLayout(t, v, func(p toml.LayoutPath) (toml.Layout, error) {
		if len(p) == 1 && p[0].Key == "items" {
			return toml.Layout{InlineTable: true, MultilineArray: true}, nil
		}
		return toml.Layout{}, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, "items = [\n  {Name = 'a'},\n  {Name = 'b'}\n]\n", out)

	var got map[string][]item
	assert.NoError(t, toml.Unmarshal([]byte(out), &got))
	assert.Equal(t, 2, len(got["items"]))
	assert.Equal(t, "a", got["items"][0].Name)
	assert.Equal(t, "b", got["items"][1].Name)
}

// A single array-table element cannot be inline while siblings keep headers.
func TestLayoutFunc_IndividualArrayTableElementInlineIsError(t *testing.T) {
	type item struct {
		Name string
	}
	v := map[string]interface{}{"items": []item{{Name: "a"}, {Name: "b"}}}
	_, err := encodeWithLayout(t, v, func(p toml.LayoutPath) (toml.Layout, error) {
		if len(p) == 2 && p[0].Key == "items" && p[1].IsIndex && p[1].Index == 0 {
			return toml.Layout{InlineTable: true}, nil
		}
		return toml.Layout{}, nil
	})
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "items[0]"), err.Error())
}

// Multiline array of (header) tables is contradictory and errors with a path.
func TestLayoutFunc_MultilineArrayOfTablesRequiresInline(t *testing.T) {
	type item struct {
		Name string
	}
	v := map[string]interface{}{"items": []item{{Name: "a"}}}
	_, err := encodeWithLayout(t, v, func(p toml.LayoutPath) (toml.Layout, error) {
		if len(p) == 1 && p[0].Key == "items" {
			return toml.Layout{MultilineArray: true}, nil
		}
		return toml.Layout{}, nil
	})
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "items"), err.Error())
}

// Field tags take precedence over the callback and the global options.
func TestLayoutFunc_TagBeatsCallbackBeatsGlobal(t *testing.T) {
	type inner struct {
		V string
	}
	type doc struct {
		TaggedInline inner `toml:"tagged,inline"`
		Free         inner
	}
	v := doc{TaggedInline: inner{V: "1"}, Free: inner{V: "2"}}

	var w strings.Builder
	enc := toml.NewEncoder(&w).
		SetTablesInline(true). // global: everything inline
		SetLayoutFunc(func(p toml.LayoutPath) (toml.Layout, error) {
			// Ask for both as normal tables; the tagged field must stay inline,
			// while Free follows the global (inline) because the callback here
			// returns no opinion on it.
			if len(p) == 1 && p[0].Key == "TaggedInline" {
				return toml.Layout{}, nil
			}
			return toml.Layout{}, nil
		})
	assert.NoError(t, enc.Encode(v))
	// With global inline, both are inline; the tag is consistent with that.
	assert.True(t, strings.Contains(w.String(), "tagged = {V = '1'}"), w.String())
	assert.True(t, strings.Contains(w.String(), "Free = {V = '2'}"), w.String())

	// Callback forces a normally-untagged, non-global table inline, and leaves
	// a tagged table alone even when the callback would "prefer" otherwise.
	type doc2 struct {
		Normal inner
		Other  inner
	}
	v2 := doc2{Normal: inner{V: "1"}, Other: inner{V: "2"}}
	var w2 strings.Builder
	enc2 := toml.NewEncoder(&w2).SetLayoutFunc(func(p toml.LayoutPath) (toml.Layout, error) {
		if len(p) == 1 && p[0].Key == "Other" {
			return toml.Layout{InlineTable: true}, nil
		}
		return toml.Layout{}, nil
	})
	assert.NoError(t, enc2.Encode(v2))
	assert.True(t, strings.Contains(w2.String(), "Other = {V = '2'}"), w2.String())
	assert.True(t, strings.Contains(w2.String(), "[Normal]"), w2.String())
}

// A callback error is wrapped with the offending path.
func TestLayoutFunc_CallbackErrorCarriesPath(t *testing.T) {
	v := map[string]interface{}{"a": map[string]string{"b": "1"}}
	sentinel := errors.New("boom")
	_, err := encodeWithLayout(t, v, func(p toml.LayoutPath) (toml.Layout, error) {
		if p.String() == "a" {
			return toml.Layout{}, sentinel
		}
		return toml.Layout{}, nil
	})
	assert.Error(t, err)
	assert.True(t, errors.Is(err, sentinel), err.Error())
	assert.True(t, strings.Contains(err.Error(), "a"), err.Error())
}

// The callback only chooses formatting: output re-decodes to the input.
func TestLayoutFunc_RoundTripEquivalence(t *testing.T) {
	type item struct {
		Name  string
		Ports []int
		Meta  map[string]string
	}
	v := map[string]interface{}{
		"servers": []item{
			{Name: "alpha", Ports: []int{8001, 8002}, Meta: map[string]string{"zone": "a"}},
			{Name: "beta", Ports: []int{9001}, Meta: map[string]string{"zone": "b"}},
		},
	}
	out, err := encodeWithLayout(t, v, func(p toml.LayoutPath) (toml.Layout, error) {
		lay := toml.Layout{}
		// Mix every layout across the document.
		if len(p) == 2 && p[0].Key == "servers" && p[1].Index == 0 {
			// nothing special at element level
		}
		if len(p) == 3 && p[2].Key == "Meta" {
			lay.InlineTable = true
		}
		if len(p) == 3 && p[2].Key == "Ports" {
			lay.MultilineArray = true
		}
		return lay, nil
	})
	assert.NoError(t, err)

	var got map[string][]item
	assert.NoError(t, toml.Unmarshal([]byte(out), &got))
	assert.Equal(t, 2, len(got["servers"]))
	assert.Equal(t, "alpha", got["servers"][0].Name)
	assert.Equal(t, []int{8001, 8002}, got["servers"][0].Ports)
	assert.Equal(t, "a", got["servers"][0].Meta["zone"])
	assert.Equal(t, "beta", got["servers"][1].Name)
	assert.Equal(t, []int{9001}, got["servers"][1].Ports)
	assert.Equal(t, "b", got["servers"][1].Meta["zone"])
}

// Paths handed to (and retained by) the callback must not change on a later
// encode or later callback invocation.
func TestLayoutFunc_RetainedPathsAreStable(t *testing.T) {
	v := map[string]interface{}{
		"servers": []map[string]int{
			{"a": 1},
			{"b": 2},
		},
	}
	var saved []string
	run := func() error {
		var w strings.Builder
		return toml.NewEncoder(&w).SetLayoutFunc(func(p toml.LayoutPath) (toml.Layout, error) {
			// Retain a snapshot of the path.
			saved = append(saved, strings.Clone(p.String()))
			return toml.Layout{}, nil
		}).Encode(v)
	}
	assert.NoError(t, run())
	first := append([]string(nil), saved...)
	saved = saved[:0]
	assert.NoError(t, run())
	second := append([]string(nil), saved...)
	assert.Equal(t, first, second)
	assert.True(t, contains(first, "servers[0].a"))
	assert.True(t, contains(first, "servers[1].b"))

	// Even mutating a retained path slice must not corrupt the encoder.
	var retained toml.LayoutPath
	var w strings.Builder
	assert.NoError(t, toml.NewEncoder(&w).SetLayoutFunc(func(p toml.LayoutPath) (toml.Layout, error) {
		if p.String() == "servers" {
			retained = p // keep the slice the callback was given
		}
		return toml.Layout{}, nil
	}).Encode(v))
	// Overwrite the retained backing array; the document was still emitted
	// correctly (the encoder never aliased this slice).
	for i := range retained {
		retained[i] = toml.PathSegment{Key: "HACK"}
	}
	var got map[string][]map[string]int
	assert.NoError(t, toml.Unmarshal([]byte(w.String()), &got))
	assert.Equal(t, 1, got["servers"][0]["a"])
	assert.Equal(t, 2, got["servers"][1]["b"])
}

// Path.String renders keys and indices; non-bare keys are quoted.
func TestLayoutPath_String(t *testing.T) {
	p := toml.LayoutPath{
		{Key: "a"},
		{Index: 1, IsIndex: true},
		{Key: "b c"},
	}
	assert.Equal(t, `a[1]."b c"`, p.String())
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
