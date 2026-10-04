package sjson

import (
	"strings"
	"testing"
)

func testIncrement(t *testing.T, expect, json, path, delta string) {
	t.Helper()
	got, err := Increment(json, path, delta)
	if err != nil {
		t.Fatalf("json=%s path=%s delta=%s: %v", json, path, delta, err)
	}
	if got != expect {
		t.Fatalf("json=%s path=%s delta=%s: expected '%s', got '%s'",
			json, path, delta, expect, got)
	}
}

func testIncrementErr(t *testing.T, json, path, delta string,
	opts *IncrementOptions) {
	t.Helper()
	got, err := IncrementWithOptions(json, path, delta, opts)
	if err == nil {
		t.Fatalf("json=%s path=%s delta=%s: expected error, got '%s'",
			json, path, delta, got)
	}
	if got != json {
		t.Fatalf("json=%s path=%s delta=%s: json modified on error: '%s'",
			json, path, delta, got)
	}
}

func TestIncrementBasic(t *testing.T) {
	testIncrement(t, `{"a":3}`, `{"a":1}`, "a", "2")
	testIncrement(t, `{"a":0}`, `{"a":0}`, "a", "0")
	testIncrement(t, `{"a":0}`, `{"a":1}`, "a", "-1")
	testIncrement(t, `{"a":-3}`, `{"a":-1}`, "a", "-2")
	// nested paths and arrays
	testIncrement(t, `{"a":{"b":11}}`, `{"a":{"b":10}}`, "a.b", "1")
	testIncrement(t, `{"a":[1,12]}`, `{"a":[1,2]}`, "a.1", "10")
}

func TestIncrementCarry(t *testing.T) {
	testIncrement(t, `{"a":1000}`, `{"a":999}`, "a", "1")
	testIncrement(t, `{"a":10000}`, `{"a":9999}`, "a", "1")
	testIncrement(t, `{"a":1000000000}`, `{"a":999999999}`, "a", "1")
	testIncrement(t, `{"a":-1000}`, `{"a":-999}`, "a", "-1")
	testIncrement(t, `{"a":10}`, `{"a":9.9}`, "a", "0.1")
	testIncrement(t, `{"a":1}`, `{"a":0.999}`, "a", "0.001")
}

func TestIncrementCrossZero(t *testing.T) {
	testIncrement(t, `{"a":2}`, `{"a":-5}`, "a", "7")
	testIncrement(t, `{"a":-2}`, `{"a":5}`, "a", "-7")
	testIncrement(t, `{"a":0}`, `{"a":-5}`, "a", "5")
	testIncrement(t, `{"a":0}`, `{"a":5}`, "a", "-5")
	testIncrement(t, `{"a":0.5}`, `{"a":-0.25}`, "a", "0.75")
	testIncrement(t, `{"a":-0.005}`, `{"a":1}`, "a", "-1.005")
}

func TestIncrementDecimalAlign(t *testing.T) {
	testIncrement(t, `{"a":1.75}`, `{"a":1.5}`, "a", "0.25")
	testIncrement(t, `{"a":0.3}`, `{"a":0.1}`, "a", "0.2")
	testIncrement(t, `{"a":3}`, `{"a":1.5}`, "a", "1.5")
	testIncrement(t, `{"a":1.5}`, `{"a":1.50}`, "a", "0")
	testIncrement(t, `{"a":0.000000000000000001}`, `{"a":0}`, "a",
		"0.000000000000000001")
	// exact beyond float64 precision
	testIncrement(t,
		`{"a":0.300000000000000000000000001}`,
		`{"a":0.300000000000000000000000001}`, "a", "0")
	testIncrement(t,
		`{"a":0.300000000000000000000000002}`,
		`{"a":0.300000000000000000000000001}`, "a",
		"0.000000000000000000000000001")
}

func TestIncrementExponent(t *testing.T) {
	testIncrement(t, `{"a":1001}`, `{"a":1e3}`, "a", "1")
	testIncrement(t, `{"a":1001}`, `{"a":1E3}`, "a", "1")
	testIncrement(t, `{"a":1.015}`, `{"a":1.5e-2}`, "a", "1")
	testIncrement(t, `{"a":200}`, `{"a":2.5E2}`, "a", "-50")
	testIncrement(t, `{"a":0.015}`, `{"a":15}`, "a", "-14.985")
	testIncrement(t, `{"a":1500}`, `{"a":1000}`, "a", "5e2")
	testIncrement(t, `{"a":1000.5}`, `{"a":1000}`, "a", "5e-1")
	testIncrement(t, `{"a":-1500}`, `{"a":-1000}`, "a", "-5e+2")
}

func TestIncrementBigIntegers(t *testing.T) {
	// 2^53 + 1 must be exact
	testIncrement(t, `{"a":9007199254740994}`,
		`{"a":9007199254740993}`, "a", "1")
	testIncrement(t, `{"a":18014398509481986}`,
		`{"a":9007199254740993}`, "a", "9007199254740993")
	// way beyond 64 bits
	testIncrement(t,
		`{"a":1234567890123456789012345678901234567891}`,
		`{"a":1234567890123456789012345678901234567890}`, "a", "1")
	testIncrement(t,
		`{"a":-1234567890123456789012345678901234567889}`,
		`{"a":-1234567890123456789012345678901234567890}`, "a", "1")
}

func TestIncrementMissing(t *testing.T) {
	// missing target without an initial value is an error
	testIncrementErr(t, `{"a":1}`, "b", "5", nil)
	// missing target with an initial value
	got, err := IncrementWithOptions(`{"a":1}`, "b", "5",
		&IncrementOptions{Initial: "10"})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"a":1,"b":15}` {
		t.Fatalf("expected '%s', got '%s'", `{"a":1,"b":15}`, got)
	}
	// missing nested path with an initial value
	got, err = IncrementWithOptions(`{}`, "a.b", "0.5",
		&IncrementOptions{Initial: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"a":{"b":0.5}}` {
		t.Fatalf("expected '%s', got '%s'", `{"a":{"b":0.5}}`, got)
	}
	// invalid initial value
	testIncrementErr(t, `{}`, "a", "1", &IncrementOptions{Initial: "abc"})
}

func TestIncrementNonNumeric(t *testing.T) {
	// null and strings are not converted automatically
	testIncrementErr(t, `{"a":null}`, "a", "1", nil)
	testIncrementErr(t, `{"a":"10"}`, "a", "1", nil)
	testIncrementErr(t, `{"a":true}`, "a", "1", nil)
	testIncrementErr(t, `{"a":{}}`, "a", "1", nil)
	testIncrementErr(t, `{"a":[]}`, "a", "1", nil)
}

func TestIncrementInvalidDelta(t *testing.T) {
	for _, delta := range []string{
		"", "abc", "1.2.3", "01", ".5", "1.", "+1", "1e", "1e+", "0x10",
		"NaN", "Infinity", "1,5", "--1",
	} {
		testIncrementErr(t, `{"a":1}`, "a", delta, nil)
	}
}

func TestIncrementMaxLength(t *testing.T) {
	// default limit
	testIncrementErr(t, `{"a":1}`, "a", "1e1000", nil)
	// custom limit
	testIncrementErr(t, `{"a":1}`, "a", "1e100",
		&IncrementOptions{MaxLength: 10})
	// custom limit on a missing target
	testIncrementErr(t, `{}`, "a", "1e100",
		&IncrementOptions{Initial: "0", MaxLength: 10})
	// result just fits
	got, err := IncrementWithOptions(`{"a":1}`, "a", "98",
		&IncrementOptions{MaxLength: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"a":99}` {
		t.Fatalf("expected '%s', got '%s'", `{"a":99}`, got)
	}
	// result one byte too long
	testIncrementErr(t, `{"a":1}`, "a", "99",
		&IncrementOptions{MaxLength: 2})
	// huge exponents fail fast without huge allocations
	testIncrementErr(t, `{"a":1}`, "a", "1e999999999", nil)
	testIncrementErr(t, `{"a":1e999999999}`, "a", "1", nil)
	testIncrementErr(t, `{"a":1}`, "a", "1e-999999999", nil)
}

func TestIncrementPreservesFormatting(t *testing.T) {
	json := "{  \"a\" : 1 ,\n  \"b\": [1, 2],\t\"c\": {\"d\": true} }"
	expect := "{  \"a\" : 4 ,\n  \"b\": [1, 2],\t\"c\": {\"d\": true} }"
	got, err := Increment(json, "a", "3")
	if err != nil {
		t.Fatal(err)
	}
	if got != expect {
		t.Fatalf("expected '%s', got '%s'", expect, got)
	}
	// only the target number changes inside a nested object
	json = `{"a": {"b":  1.5 , "c": 2}, "d": "1"}`
	expect = `{"a": {"b":  2.5 , "c": 2}, "d": "1"}`
	got, err = Increment(json, "a.b", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got != expect {
		t.Fatalf("expected '%s', got '%s'", expect, got)
	}
}

func TestIncrementValidJSON(t *testing.T) {
	// the result of an increment must always be valid json
	cases := []struct {
		json, path, delta string
	}{
		{`{"a":1}`, "a", "1e300"},
		{`{"a":1}`, "a", "-1e-300"},
		{`{"a":999.999}`, "a", "0.001"},
		{`{"a":-999.999}`, "a", "-0.001"},
	}
	for _, c := range cases {
		got, err := Increment(c.json, c.path, c.delta)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, `"a":`) {
			t.Fatalf("unexpected result '%s'", got)
		}
		if sortJSON(got) == "" {
			t.Fatalf("invalid json '%s'", got)
		}
	}
}
