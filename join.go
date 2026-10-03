package gjson

// JoinMode selects how unmatched elements are handled by Join and
// JoinArrays.
type JoinMode int

const (
	// JoinInner produces a result only for elements whose keys match.
	JoinInner JoinMode = iota
	// JoinLeft keeps every left element; unmatched ones are emitted once
	// with a null right value.
	JoinLeft
)

// JoinOptions configures a join operation.
type JoinOptions struct {
	// Mode is JoinInner or JoinLeft.
	Mode JoinMode
	// LeftPath resolves the left array from the input document. An empty
	// path treats the document itself as the left array.
	LeftPath string
	// RightPath resolves the right array from the input document.
	RightPath string
	// LeftKey is the key path relative to each left array element.
	LeftKey string
	// RightKey is the key path relative to each right array element.
	RightKey string
	// MatchNull allows explicit null keys to match each other. By default
	// null keys, like missing keys, never match.
	MatchNull bool
}

// JoinResult is a single joined pair. Left is always populated. Right is a
// null Result for unmatched left elements when JoinLeft is used.
type JoinResult struct {
	Left  Result
	Right Result
}

// joinKey is the discriminated value used for matching. String and number
// keys never compare equal, even when their text looks similar.
type joinKey struct {
	typ Type
	str string
	num float64
}

var nullResult = Result{Type: Null, Raw: "null"}

// Join performs an inner or left join between two arrays that live in the
// same JSON document. See JoinArrays for joining arrays from separate
// documents.
func Join(json string, opts JoinOptions) []JoinResult {
	doc := Parse(json)
	left := resolveArray(doc, opts.LeftPath)
	right := resolveArray(doc, opts.RightPath)
	return joinResults(left, right, opts)
}

// JoinArrays performs an inner or left join between two JSON arrays, where
// the left and right arrays come from potentially separate JSON documents.
//
// Every matching key combination is emitted, ordered first by left element
// appearance and then by right element appearance. Elements that lack the
// key path or carry unsupported key types are treated as unmatched.
func JoinArrays(leftJSON, rightJSON string, opts JoinOptions) []JoinResult {
	return joinResults(Parse(leftJSON), Parse(rightJSON), opts)
}

func resolveArray(doc Result, path string) Result {
	if path == "" {
		return doc
	}
	return doc.Get(path)
}

func joinResults(left, right Result, opts JoinOptions) []JoinResult {
	if !left.IsArray() {
		left = Result{Type: Null}
	}
	if !right.IsArray() {
		right = Result{Type: Null}
	}

	// Index right elements by key, retaining every duplicate so a trailing
	// same-key element can never overwrite an earlier one.
	buckets := make(map[joinKey][]Result)
	right.ForEach(func(_, value Result) bool {
		if key, ok := elementKey(value, opts.RightKey, opts.MatchNull); ok {
			buckets[key] = append(buckets[key], value)
		}
		return true
	})

	var results []JoinResult
	left.ForEach(func(_, value Result) bool {
		matches := []Result(nil)
		if key, ok := elementKey(value, opts.LeftKey, opts.MatchNull); ok {
			matches = buckets[key]
		}
		switch {
		case len(matches) > 0:
			for _, match := range matches {
				results = append(results, JoinResult{Left: value, Right: match})
			}
		case opts.Mode == JoinLeft:
			results = append(results, JoinResult{Left: value, Right: nullResult})
		}
		return true
	})

	return results
}

// elementKey extracts the comparable key from an element. Missing keys and
// unsupported types never match; explicit null matches only when matchNull is
// set.
func elementKey(element Result, path string, matchNull bool) (joinKey, bool) {
	key := element
	if path != "" {
		key = element.Get(path)
	}
	switch key.Type {
	case String:
		return joinKey{typ: String, str: key.Str}, true
	case Number:
		return joinKey{typ: Number, num: key.Num}, true
	case Null:
		if matchNull && key.Exists() {
			return joinKey{typ: Null}, true
		}
	}
	return joinKey{}, false
}
