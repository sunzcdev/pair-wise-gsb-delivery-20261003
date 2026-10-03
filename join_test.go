package gjson

import "testing"

func joinRaws(results []JoinResult) string {
	out := "["
	for i, r := range results {
		if i > 0 {
			out += ","
		}
		out += `{"left":` + r.Left.Raw + `,"right":` + r.Right.Raw + `}`
	}
	return out + "]"
}

func TestJoinArraysInner(t *testing.T) {
	left := `[{"id":1,"n":"a"},{"id":2,"n":"b"},{"id":3,"n":"c"}]`
	right := `[{"id":2,"v":"x"},{"id":3,"v":"y"},{"id":4,"v":"z"}]`
	results := JoinArrays(left, right, JoinOptions{
		Mode:     JoinInner,
		LeftKey:  "id",
		RightKey: "id",
	})
	expect := `[{"left":{"id":2,"n":"b"},"right":{"id":2,"v":"x"}},` +
		`{"left":{"id":3,"n":"c"},"right":{"id":3,"v":"y"}}]`
	assert(t, joinRaws(results) == expect)
}

func TestJoinArraysLeftUnmatched(t *testing.T) {
	left := `[{"id":1},{"id":2},{"id":3}]`
	right := `[{"id":2,"v":"x"}]`
	results := JoinArrays(left, right, JoinOptions{
		Mode:     JoinLeft,
		LeftKey:  "id",
		RightKey: "id",
	})
	assert(t, len(results) == 3)
	assert(t, results[0].Right.Type == Null)
	assert(t, results[0].Right.Raw == "null")
	assert(t, results[1].Right.Get("v").String() == "x")
	assert(t, results[2].Right.Type == Null)
	// Each unmatched left record appears exactly once.
	assert(t, joinRaws(results) ==
		`[{"left":{"id":1},"right":null},{"left":{"id":2},"right":{"id":2,"v":"x"}},{"left":{"id":3},"right":null}]`)
}

func TestJoinArraysDuplicatesCartesian(t *testing.T) {
	left := `[{"id":1,"n":"l1"},{"id":1,"n":"l2"},{"id":2,"n":"l3"}]`
	right := `[{"id":1,"v":"r1"},{"id":1,"v":"r2"},{"id":2,"v":"r3"}]`
	results := JoinArrays(left, right, JoinOptions{
		Mode:     JoinInner,
		LeftKey:  "id",
		RightKey: "id",
	})
	// Cartesian product, ordered by left appearance then right appearance.
	expect := `[` +
		`{"left":{"id":1,"n":"l1"},"right":{"id":1,"v":"r1"}},` +
		`{"left":{"id":1,"n":"l1"},"right":{"id":1,"v":"r2"}},` +
		`{"left":{"id":1,"n":"l2"},"right":{"id":1,"v":"r1"}},` +
		`{"left":{"id":1,"n":"l2"},"right":{"id":1,"v":"r2"}},` +
		`{"left":{"id":2,"n":"l3"},"right":{"id":2,"v":"r3"}}` +
		`]`
	assert(t, joinRaws(results) == expect)
}

func TestJoinArraysStringAndNumberDoNotMix(t *testing.T) {
	left := `[{"id":"1"}]`
	right := `[{"id":1},{"id":"1"}]`
	inner := JoinArrays(left, right, JoinOptions{
		Mode:     JoinInner,
		LeftKey:  "id",
		RightKey: "id",
	})
	assert(t, len(inner) == 1)
	assert(t, inner[0].Right.Get("id").Type == String)

	leftJoin := JoinArrays(left, right, JoinOptions{
		Mode:     JoinLeft,
		LeftKey:  "id",
		RightKey: "id",
	})
	assert(t, len(leftJoin) == 1)
	assert(t, leftJoin[0].Right.Get("id").Type == String)
}

func TestJoinArraysMissingKeys(t *testing.T) {
	left := `[{"id":1},{}]`
	right := `[{"id":1}]`
	inner := JoinArrays(left, right, JoinOptions{
		Mode:     JoinInner,
		LeftKey:  "id",
		RightKey: "id",
	})
	assert(t, len(inner) == 1)

	leftJoin := JoinArrays(left, right, JoinOptions{
		Mode:     JoinLeft,
		LeftKey:  "id",
		RightKey: "id",
	})
	assert(t, len(leftJoin) == 2)
	assert(t, leftJoin[1].Right.Type == Null)
}

func TestJoinArraysExplicitNull(t *testing.T) {
	left := `[{"id":null,"n":"a"},{"id":1,"n":"b"}]`
	right := `[{"id":null,"v":"x"}]`

	without := JoinArrays(left, right, JoinOptions{
		Mode:     JoinInner,
		LeftKey:  "id",
		RightKey: "id",
	})
	assert(t, len(without) == 0)

	withoutLeft := JoinArrays(left, right, JoinOptions{
		Mode:     JoinLeft,
		LeftKey:  "id",
		RightKey: "id",
	})
	assert(t, len(withoutLeft) == 2)
	assert(t, withoutLeft[0].Right.Type == Null)

	with := JoinArrays(left, right, JoinOptions{
		Mode:      JoinInner,
		LeftKey:   "id",
		RightKey:  "id",
		MatchNull: true,
	})
	assert(t, len(with) == 1)
	assert(t, with[0].Left.Get("n").String() == "a")
	assert(t, with[0].Right.Get("v").String() == "x")

	// Explicit null must not match a missing key even with MatchNull on.
	missingLeft := JoinArrays(`[{}]`, right, JoinOptions{
		Mode:      JoinInner,
		LeftKey:   "id",
		RightKey:  "id",
		MatchNull: true,
	})
	assert(t, len(missingLeft) == 0)
}

func TestJoinArraysNestedKeyPaths(t *testing.T) {
	left := `[{"k":{"x":1},"n":"a"}]`
	right := `[{"k":{"x":1},"v":"b"}]`
	results := JoinArrays(left, right, JoinOptions{
		Mode:     JoinInner,
		LeftKey:  "k.x",
		RightKey: "k.x",
	})
	assert(t, len(results) == 1)
	assert(t, results[0].Left.Get("n").String() == "a")
	assert(t, results[0].Right.Get("v").String() == "b")
}

func TestJoinDocumentPaths(t *testing.T) {
	json := `{
		"left":  [{"id":1,"n":"a"},{"id":2,"n":"b"}],
		"right": [{"id":1,"v":"x"}]
	}`
	inner := Join(json, JoinOptions{
		Mode:      JoinInner,
		LeftPath:  "left",
		RightPath: "right",
		LeftKey:   "id",
		RightKey:  "id",
	})
	assert(t, len(inner) == 1)
	assert(t, inner[0].Left.Get("n").String() == "a")

	leftJoin := Join(json, JoinOptions{
		Mode:      JoinLeft,
		LeftPath:  "left",
		RightPath: "right",
		LeftKey:   "id",
		RightKey:  "id",
	})
	assert(t, len(leftJoin) == 2)
	assert(t, leftJoin[1].Right.Type == Null)
}

func TestJoinArraysRootElementKey(t *testing.T) {
	left := `[1,2]`
	right := `[2,3]`
	results := JoinArrays(left, right, JoinOptions{Mode: JoinInner})
	assert(t, len(results) == 1)
	assert(t, results[0].Left.Int() == 2)
	assert(t, results[0].Right.Int() == 2)
}

func TestJoinArraysNonArrayInputs(t *testing.T) {
	results := JoinArrays(`{"id":1}`, `[{"id":1}]`, JoinOptions{
		Mode:     JoinLeft,
		LeftKey:  "id",
		RightKey: "id",
	})
	assert(t, len(results) == 0)

	results = JoinArrays(`[{"id":1}]`, `{"id":1}`, JoinOptions{
		Mode:     JoinLeft,
		LeftKey:  "id",
		RightKey: "id",
	})
	assert(t, len(results) == 1)
	assert(t, results[0].Right.Type == Null)
}
