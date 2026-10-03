package gjson

import (
	"math/rand"
	"strconv"
	"testing"
)

// TestPlanRandomDiff generates random (json, path) pairs and asserts that a
// compiled plan returns exactly what Get returns.
func TestPlanRandomDiff(t *testing.T) {
	rng := rand.New(rand.NewSource(12345))
	for iter := 0; iter < 150000; iter++ {
		j := randJSON(rng, 0)
		path := randPath(rng, 0)
		want := Get(j, path)
		p, err := Compile(path)
		if err != nil {
			// Any path that compiles successfully must match Get; compile
			// errors are exercised separately.
			continue
		}
		got := p.Get(j)
		if !sameResult(want, got) {
			t.Fatalf("iter %d json=%q path=%q\n want type=%v raw=%q str=%q idx=%v idxs=%v\n got  type=%v raw=%q str=%q idx=%v idxs=%v",
				iter, j, path,
				want.Type, want.Raw, want.Str, want.Index, want.Indexes,
				got.Type, got.Raw, got.Str, got.Index, got.Indexes)
		}
		gb := p.GetBytes([]byte(j))
		wb := GetBytes([]byte(j), path)
		if gb.Raw != wb.Raw || gb.Str != wb.Str {
			t.Fatalf("bytes iter %d json=%q path=%q want=%q/%q got=%q/%q",
				iter, j, path, wb.Raw, wb.Str, gb.Raw, gb.Str)
		}
	}
}

var jsonKeys = []string{"a", "b", "name", "x", "y", "0", "a.b", "a*b", "k?y", "net", "age", "last"}

func randJSON(rng *rand.Rand, depth int) string {
	if depth > 3 || rng.Intn(3) == 0 {
		return randScalar(rng)
	}
	if rng.Intn(2) == 0 {
		n := rng.Intn(4)
		s := "["
		for i := 0; i < n; i++ {
			if i > 0 {
				s += ","
			}
			s += randJSON(rng, depth+1)
		}
		return s + "]"
	}
	n := rng.Intn(4)
	s := "{"
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ","
		}
		key := jsonKeys[rng.Intn(len(jsonKeys))]
		s += "\"" + escapeJSONKey(key) + "\":" + randJSON(rng, depth+1)
	}
	return s + "}"
}

func escapeJSONKey(k string) string {
	return k
}

func randScalar(rng *rand.Rand) string {
	switch rng.Intn(6) {
	case 0:
		return []string{"\"hi\"", "\"Murphy\"", "\"Dale\"", "\"D*\"", "\"a.b\"", "\"*a*\"", "true", "false", "null"}[rng.Intn(9)]
	case 1:
		return string(rune('0' + rng.Intn(10)))
	case 2:
		return strconv.Itoa(rng.Intn(100))
	default:
		return []string{"1", "2", "3", "44", "0", "true", "false", "null"}[rng.Intn(8)]
	}
}

var pathKeys = []string{"a", "b", "name", "x", "y", "0", "age", "last", "net", "k", "first"}

func randPath(rng *rand.Rand, depth int) string {
	var s string
	n := 1 + rng.Intn(3)
	for i := 0; i < n; i++ {
		if i > 0 {
			if rng.Intn(5) == 0 {
				s += "|"
			} else {
				s += "."
			}
		}
		s += randSegment(rng)
	}
	if rng.Intn(6) == 0 {
		s = "@this." + s
	}
	if rng.Intn(10) == 0 {
		// wrap into a multipath
		if rng.Intn(2) == 0 {
			s = "[" + s + "," + pathKeys[rng.Intn(len(pathKeys))] + "]"
		} else {
			s = "{\"k\":" + s + "}"
		}
	}
	return s
}

func randSegment(rng *rand.Rand) string {
	switch rng.Intn(12) {
	case 0:
		return "#"
	case 1:
		return "#(" + randQueryBody(rng) + ")"
	case 2:
		return "#(" + randQueryBody(rng) + ")#"
	case 3:
		return "#." + pathKeys[rng.Intn(len(pathKeys))]
	case 4:
		return "@reverse"
	case 5:
		return pathKeys[rng.Intn(len(pathKeys))]
	case 6:
		return strconv.Itoa(rng.Intn(4))
	case 7:
		return "a\\.b"
	case 8:
		return "@pretty:{\"sortKeys\":true}"
	case 9:
		return "@reverse.0"
	case 10:
		return pathKeys[rng.Intn(len(pathKeys))] + "*"
	default:
		return "#(" + pathKeys[rng.Intn(len(pathKeys))] + ".#(" +
			randQueryBody(rng) + ")"
	}
}

func randQueryBody(rng *rand.Rand) string {
	ops := []string{"==", "!=", "<", "<=", ">", ">=", "%", "!%"}
	if rng.Intn(3) == 0 {
		return pathKeys[rng.Intn(len(pathKeys))]
	}
	key := pathKeys[rng.Intn(len(pathKeys))]
	op := ops[rng.Intn(len(ops))]
	val := []string{"\"Murphy\"", "\"D*\"", "\"x\"", "1", "2", "44", "true", "false", "~true", "~null", "~*"}[rng.Intn(11)]
	return key + op + val
}
