package webdav

import (
	"errors"
	"net/http"
	"testing"
)

func TestParseETag(t *testing.T) {
	cases := []struct {
		in   string
		want ETag
		ok   bool
	}{
		{`"abc"`, ETag{OpaqueTag: "abc"}, true},
		{`W/"abc"`, ETag{OpaqueTag: "abc", Weak: true}, true},
		{`w/"abc"`, ETag{OpaqueTag: "abc", Weak: true}, true},
		{`  "abc"  `, ETag{OpaqueTag: "abc"}, true},
		{`"a\"b\\c"`, ETag{OpaqueTag: `a"b\c`}, true},
		{"\"café\"", ETag{OpaqueTag: "café"}, true},
		{`"`, ETag{}, false},
		{`abc`, ETag{}, false},
		{`W/abc`, ETag{}, false},
		{`"abc`, ETag{}, false},
		{`"a"b"`, ETag{}, false},
		{`"a	b"`, ETag{}, false},
		{``, ETag{}, false},
	}
	for _, tc := range cases {
		got, err := ParseETag(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("ParseETag(%q): unexpected error: %v", tc.in, err)
				continue
			}
			if got != tc.want {
				t.Errorf("ParseETag(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		} else if err == nil {
			t.Errorf("ParseETag(%q): expected error, got %+v", tc.in, got)
		}
	}
}

func TestETagStringRoundtrip(t *testing.T) {
	for _, in := range []string{`"abc"`, `W/"abc"`, `"a\"b\\c"`, `"café"`} {
		et, err := ParseETag(in)
		if err != nil {
			t.Fatalf("ParseETag(%q): %v", in, err)
		}
		if got := et.String(); got != in {
			t.Errorf("String() = %q, want %q", got, in)
		}
	}
}

func TestParseConditionalMatch(t *testing.T) {
	cases := []struct {
		in       string
		wildcard bool
		tags     []string
		weak     []bool
		ok       bool
	}{
		{"", false, nil, nil, true},
		{"  ", false, nil, nil, true},
		{"*", true, nil, nil, true},
		{`"abc"`, false, []string{"abc"}, []bool{false}, true},
		{`"a", "b", W/"c"`, false, []string{"a", "b", "c"}, []bool{false, false, true}, true},
		{` * `, true, nil, nil, true},
		{`"a", *`, false, nil, nil, false},
		{`*, "a"`, false, nil, nil, false},
		{`"a",`, false, nil, nil, false},
		{`, "a"`, false, nil, nil, false},
		{`"a",  , "b"`, false, nil, nil, false},
		{`abc`, false, nil, nil, false},
		{`"a" "b"`, false, nil, nil, false},
		{`"a`, false, nil, nil, false},
	}
	for _, tc := range cases {
		got, err := ParseConditionalMatch(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("ParseConditionalMatch(%q): err=%v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if err != nil {
			continue
		}
		if got.IsWildcard() != tc.wildcard {
			t.Errorf("ParseConditionalMatch(%q): wildcard=%v, want %v", tc.in, got.IsWildcard(), tc.wildcard)
		}
		if len(got.ETags) != len(tc.tags) {
			t.Errorf("ParseConditionalMatch(%q): %d tags, want %d", tc.in, len(got.ETags), len(tc.tags))
			continue
		}
		for i, opaque := range tc.tags {
			if got.ETags[i].OpaqueTag != opaque || got.ETags[i].Weak != tc.weak[i] {
				t.Errorf("ParseConditionalMatch(%q): tag %d = %+v, want {%s weak=%v}",
					tc.in, i, got.ETags[i], opaque, tc.weak[i])
			}
		}
	}
}

func TestConditionalMatchString(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"*":               "*",
		`"a"`:             `"a"`,
		`"a", "b", W/"c"`: `"a", "b", W/"c"`,
	}
	for in, want := range cases {
		cm, err := ParseConditionalMatch(in)
		if err != nil {
			t.Fatalf("ParseConditionalMatch(%q): %v", in, err)
		}
		if got := cm.String(); got != want {
			t.Errorf("String() for %q = %q, want %q", in, got, want)
		}
	}
}

func TestStrongWeakMatch(t *testing.T) {
	s := func(x string) ETag { return ETag{OpaqueTag: x} }
	w := func(x string) ETag { return ETag{OpaqueTag: x, Weak: true} }

	strongList := ConditionalMatch{ETags: []ETag{s("a"), w("b")}}
	weakList := ConditionalMatch{ETags: []ETag{w("a"), s("b")}}
	wildcard := ConditionalMatch{Wildcard: true}

	if !strongList.StrongMatch(s("a")) {
		t.Errorf("strong list should strongly match a strong current tag")
	}
	if strongList.StrongMatch(s("b")) {
		t.Errorf("weak candidate must not match under strong comparison")
	}
	if strongList.StrongMatch(w("a")) {
		t.Errorf("weak current tag must not match under If-Match")
	}
	if strongList.StrongMatch(s("c")) {
		t.Errorf("unrelated tag must not match")
	}

	if !weakList.WeakMatch(w("a")) || !weakList.WeakMatch(s("a")) || !weakList.WeakMatch(w("b")) {
		t.Errorf("weak comparison ignores the weak indicator")
	}
	if weakList.WeakMatch(s("c")) {
		t.Errorf("unrelated tag must not weakly match")
	}

	if !wildcard.StrongMatch(s("a")) || !wildcard.WeakMatch(w("a")) {
		t.Errorf("wildcard matches any current tag")
	}
}

func TestConditionalPutCheck(t *testing.T) {
	s := func(x string) ETag { return ETag{OpaqueTag: x} }
	w := func(x string) ETag { return ETag{OpaqueTag: x, Weak: true} }

	parse := func(m, n string) ConditionalPut {
		cp, err := ParseConditionalPutHeaders(m, n)
		if err != nil {
			t.Fatalf("ParseConditionalPutHeaders(%q, %q): %v", m, n, err)
		}
		return cp
	}
	expectStatus := func(err error, code int) {
		t.Helper()
		got, ok := HTTPErrorStatusCode(err)
		if !ok || got != code {
			t.Errorf("err = %v, want HTTP %d", err, code)
		}
	}

	// No conditions.
	if err := (ConditionalPut{}).Check(s("a"), true); err != nil {
		t.Errorf("unset condition must pass: %v", err)
	}

	// If-Match list, strong comparison.
	if err := parse(`"a", "b"`, "").Check(s("b"), true); err != nil {
		t.Errorf("matching If-Match list must pass: %v", err)
	}
	expectStatus(parse(`"a"`, "").Check(w("a"), true), http.StatusPreconditionFailed)
	expectStatus(parse(`W/"a"`, "").Check(s("a"), true), http.StatusPreconditionFailed)
	expectStatus(parse(`"a"`, "").Check(s("a"), false), http.StatusPreconditionFailed)
	expectStatus(parse("*", "").Check(s("a"), false), http.StatusPreconditionFailed)

	// If-None-Match, weak comparison.
	if err := parse("", `W/"a"`).Check(s("a"), false); err != nil {
		t.Errorf("If-None-Match allows create on missing resource: %v", err)
	}
	if err := parse("", `"a"`).Check(s("b"), true); err != nil {
		t.Errorf("If-None-Match passes on a different tag: %v", err)
	}
	expectStatus(parse("", `"a"`).Check(w("a"), true), http.StatusPreconditionFailed)
	expectStatus(parse("", `W/"a"`).Check(s("a"), true), http.StatusPreconditionFailed)
	expectStatus(parse("", "*").Check(s("a"), true), http.StatusPreconditionFailed)
	if err := parse("", "*").Check(s("a"), false); err != nil {
		t.Errorf("If-None-Match: * only fails when the resource exists: %v", err)
	}
}

func TestConditionalMatchNotSupported(t *testing.T) {
	err := NewConditionalMatchNotSupportedError()
	if !IsConditionalMatchNotSupported(err) {
		t.Errorf("IsConditionalMatchNotSupported should detect the sentinel error")
	}
	if code, ok := HTTPErrorStatusCode(err); !ok || code != http.StatusNotImplemented {
		t.Errorf("err = %v, want HTTP 501", err)
	}
	if !errors.Is(err, ErrConditionalMatchNotSupported) {
		t.Errorf("error must wrap ErrConditionalMatchNotSupported")
	}
}
