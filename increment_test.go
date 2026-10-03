package sjson

import (
	"strings"
	"testing"
)

func TestIncrementBasic(t *testing.T) {
	tests := []struct {
		name  string
		json  string
		path  string
		delta interface{}
		want  string
	}{
		{name: "zero plus one", json: `{"n":0}`, path: "n", delta: "1",
			want: `{"n":1}`},
		{name: "int plus int", json: `{"n":41}`, path: "n", delta: "1",
			want: `{"n":42}`},
		{name: "carry", json: `{"n":999}`, path: "n", delta: "1",
			want: `{"n":1000}`},
		{name: "multi digit delta", json: `{"n":10}`, path: "n",
			delta: "234", want: `{"n":244}`},
		{name: "subtract to positive", json: `{"n":10}`, path: "n",
			delta: "-3", want: `{"n":7}`},
		{name: "subtract to zero", json: `{"n":3}`, path: "n", delta: "-3",
			want: `{"n":0}`},
		{name: "cross zero negative", json: `{"n":3}`, path: "n",
			delta: "-10", want: `{"n":-7}`},
		{name: "negative to positive", json: `{"n":-3}`, path: "n",
			delta: "10", want: `{"n":7}`},
		{name: "go int delta", json: `{"n":41}`, path: "n", delta: 1,
			want: `{"n":42}`},
		{name: "nested path", json: `{"a":{"b":5}}`, path: "a.b",
			delta: "2", want: `{"a":{"b":7}}`},
		{name: "array element", json: `{"a":[1,2,3]}`, path: "a.1",
			delta: "10", want: `{"a":[1,12,3]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Increment(tt.json, tt.path, tt.delta)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIncrementDecimals(t *testing.T) {
	tests := []struct {
		json, path, delta, want string
	}{
		{`{"n":1.5}`, "n", "0.25", `{"n":1.75}`},
		{`{"n":1.50}`, "n", "0.5", `{"n":2}`},
		{`{"n":0.1}`, "n", "0.2", `{"n":0.3}`},
		{`{"n":1.2}`, "n", "-2.5", `{"n":-1.3}`},
		{`{"n":-1.2}`, "n", "2.5", `{"n":1.3}`},
		{`{"n":0.0001}`, "n", "0.001", `{"n":0.0011}`},
		{`{"n":100}`, "n", "0.1", `{"n":100.1}`},
		{`{"n":0.9999}`, "n", "0.0001", `{"n":1}`},
		{`{"n":1}`, "n", "0.00", `{"n":1}`},
		{`{"n":-0.0}`, "n", "1", `{"n":1}`},
	}
	for _, tt := range tests {
		got, err := Increment(tt.json, tt.path, tt.delta)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tt.json, err)
		}
		if got != tt.want {
			t.Fatalf("%s + %s: got %q, want %q", tt.json, tt.delta,
				got, tt.want)
		}
	}
}

func TestIncrementExponents(t *testing.T) {
	tests := []struct {
		json, path, delta, want string
	}{
		{`{"n":1e3}`, "n", "1", `{"n":1001}`},
		{`{"n":1E3}`, "n", "1", `{"n":1001}`},
		{`{"n":1e+2}`, "n", "1", `{"n":101}`},
		{`{"n":1000}`, "n", "1e3", `{"n":2000}`},
		{`{"n":1.5e2}`, "n", "5", `{"n":155}`},
		{`{"n":1}`, "n", "1.5e2", `{"n":151}`},
		{`{"n":5e-1}`, "n", "0.5", `{"n":1}`},
		{`{"n":1.23e2}`, "n", "0.01", `{"n":123.01}`},
		{`{"n":10}`, "n", "-1e1", `{"n":0}`},
	}
	for _, tt := range tests {
		got, err := Increment(tt.json, tt.path, tt.delta)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tt.json, err)
		}
		if got != tt.want {
			t.Fatalf("%s + %s: got %q, want %q", tt.json, tt.delta,
				got, tt.want)
		}
	}
}

func TestIncrementLargeExact(t *testing.T) {
	// 2^53 + 1, which float64 cannot distinguish from 2^53.
	big := "9007199254740993"
	json := `{"n":` + big + `}`
	got, err := Increment(json, "n", "1")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"n":9007199254740994}`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	huge := strings.Repeat("9", 100)
	want := "1" + strings.Repeat("0", 100)
	got, err = Increment(`{"n":`+huge+`}`, "n", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"n":`+want+`}` {
		t.Fatalf("large carry mismatch")
	}

	got, err = Increment(`{"n":1e100}`, "n", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"n":`+"1"+strings.Repeat("0", 99)+"1}" {
		t.Fatalf("exponent expansion mismatch: %s", got)
	}
}

func TestIncrementMissing(t *testing.T) {
	if got, err := Increment(`{"a":1}`, "b", "1"); err != nil ||
		got != `{"a":1,"b":1}` {
		t.Fatalf("missing defaults to 0: got %q err=%v", got, err)
	}
	init := "10"
	got, err := IncrementOptions(`{"a":1}`, "b", "5", &Options{
		IncrementInitial: &init,
	})
	if err != nil || got != `{"a":1,"b":15}` {
		t.Fatalf("initial used for missing: got %q err=%v", got, err)
	}
	got, err = IncrementOptions(``, "a.b", "1", nil)
	if err != nil || got != `{"a":{"b":1}}` {
		t.Fatalf("nested missing: got %q err=%v", got, err)
	}
	got, err = IncrementOptions(``, "a", "0", &Options{
		IncrementInitial: &init,
	})
	if err != nil || got != `{"a":10}` {
		t.Fatalf("initial with zero delta: got %q err=%v", got, err)
	}
	// initial is ignored when the target exists
	got, err = IncrementOptions(`{"b":1}`, "b", "1", &Options{
		IncrementInitial: &init,
	})
	if err != nil || got != `{"b":2}` {
		t.Fatalf("initial ignored when present: got %q err=%v", got, err)
	}
}

func TestIncrementPreservesFormatting(t *testing.T) {
	in := "{\n  \"name\": \"x\",\n  \"count\": 41,\n  \"tail\": true\n}"
	want := "{\n  \"name\": \"x\",\n  \"count\": 42,\n  \"tail\": true\n}"
	got, err := Increment(in, "count", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestIncrementNonNumbers(t *testing.T) {
	badTargets := []string{
		`{"n":"10"}`,
		`{"n":null}`,
		`{"n":true}`,
		`{"n":{}}`,
		`{"n":[1]}`,
	}
	for _, j := range badTargets {
		if got, err := Increment(j, "n", "1"); err == nil {
			t.Fatalf("expected error for %s, got %q", j, got)
		} else if !strings.Contains(err.Error(), "non-number") {
			t.Fatalf("for %s unexpected error: %v", j, err)
		}
	}
	badDeltas := []string{"", "x", "01", "1.", "1e", "+", "1.2.3",
		"nan", "Inf", " 1", "1 "}
	for _, d := range badDeltas {
		if _, err := Increment(`{"n":1}`, "n", d); err == nil {
			t.Fatalf("expected error for delta %q", d)
		}
	}
	if _, err := Increment(`{"n":1}`, "n", 1.5); err == nil {
		t.Fatal("float delta must be rejected")
	}
	if _, err := Increment(`{"n":1}`, "n", true); err == nil {
		t.Fatal("bool delta must be rejected")
	}
	init := "not-a-number"
	if _, err := IncrementOptions(`{}`, "n", "1", &Options{
		IncrementInitial: &init,
	}); err == nil {
		t.Fatal("invalid initial must be rejected")
	}
	if _, err := Increment(`{"n":1}`, "", "1"); err == nil {
		t.Fatal("empty path must be rejected")
	}
}

func TestIncrementResultLengthLimit(t *testing.T) {
	opts := &Options{MaxResultLen: 5}
	if got, err := IncrementOptions(`{"n":9999}`, "n", "1", opts); err != nil ||
		got != `{"n":10000}` {
		t.Fatalf("boundary ok: got %q err=%v", got, err)
	}
	if _, err := IncrementOptions(`{"n":99999}`, "n", "1", opts); err == nil {
		t.Fatal("over-limit must error")
	}
	// document is untouched on limit error
	in := `{"n":99999}`
	got, err := IncrementOptions(in, "n", "1", opts)
	if err == nil {
		t.Fatal("expected error")
	}
	if got != "" {
		t.Fatalf("expected no result on error, got %q", got)
	}
	// exponent expansion is also capped
	if _, err := IncrementOptions(`{"n":1e10}`, "n", "1",
		&Options{MaxResultLen: 5}); err == nil {
		t.Fatal("expanded result over limit must error")
	}
	// default safety cap
	if _, err := Increment(`{"n":1e100000}`, "n", "1"); err == nil {
		t.Fatal("default cap must reject gigantic result")
	}
}

func TestIncrementBytes(t *testing.T) {
	got, err := IncrementBytes([]byte(`{"n":41}`), "n", []byte("1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"n":42}` {
		t.Fatalf("got %q", got)
	}
}

func TestIncrementQueryMatches(t *testing.T) {
	in := `{"friends":[{"age":1},{"age":2},{"age":3}]}`
	got, err := Increment(in, "friends.#.age", "1")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"friends":[{"age":2},{"age":3},{"age":4}]}`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := Increment(
		`{"friends":[{"age":1},{"age":"x"}]}`, "friends.#.age",
		"1"); err == nil {
		t.Fatal("non-number match must error")
	} else if !strings.Contains(err.Error(), "non-number") {
		t.Fatalf("unexpected error: %v", err)
	}
	// a query matching nothing leaves the document unchanged
	got2, err := Increment(in, "friends.#(age>100).age", "1")
	if err != nil {
		t.Fatalf("zero matches should not error: %v", err)
	}
	if got2 != in {
		t.Fatalf("zero matches changed the document: %q", got2)
	}
}

func TestIncrementRootNumber(t *testing.T) {
	if got, err := Increment(`41`, "@this", "1"); err != nil || got != "42" {
		t.Fatalf("root number got %q err=%v", got, err)
	}
}

func TestIncrementOptionsAndBytes(t *testing.T) {
	in := []byte(`{"n":1}`)
	got, err := IncrementBytesOptions(in, "n", "1", &Options{
		Optimistic:     true,
		ReplaceInPlace: true,
		MaxResultLen:   10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"n":2}` {
		t.Fatalf("got %q", got)
	}
	// uint64 delta
	if v, err := Increment(`{"n":1}`, "n", uint64(41)); err != nil ||
		v != `{"n":42}` {
		t.Fatalf("uint64 delta: got %q err=%v", v, err)
	}
	// int64 delta, nested array
	if v, err := Increment(`{"a":[10]}`, "a.0", int64(-3)); err != nil ||
		v != `{"a":[7]}` {
		t.Fatalf("int64 delta: got %q err=%v", v, err)
	}
}
