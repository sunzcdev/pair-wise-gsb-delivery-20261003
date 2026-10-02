// Copyright (c) 2013 Kelsey Hightower. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package envconfig

import (
	"os"
	"strings"
	"testing"
)

type quotedCollectionSpec struct {
	Names   []string          `envconfig:"NAMES" collection_syntax:"true"`
	Numbers []int             `envconfig:"NUMBERS" collection_syntax:"true"`
	Codes   map[string]int    `envconfig:"CODES" collection_syntax:"true"`
	Pairs   map[string]string `envconfig:"PAIRS" collection_syntax:"true"`
}

func collect(t *testing.T, setEnv func()) (*quotedCollectionSpec, error) {
	t.Helper()
	os.Clearenv()
	setEnv()
	var s quotedCollectionSpec
	err := Process("", &s)
	return &s, err
}

func TestCollectionSliceQuotedEscaped(t *testing.T) {
	s, err := collect(t, func() {
		// A comma is allowed inside a quoted or escaped element.
		os.Setenv("NAMES", `"smith, john", plain\,one, "with ""quote"""`)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"smith, john", "plain,one", `with "quote"`}
	if len(s.Names) != len(want) {
		t.Fatalf("expected %#v, got %#v", want, s.Names)
	}
	for i := range want {
		if s.Names[i] != want[i] {
			t.Errorf("element %d: expected %q, got %q", i+1, want[i], s.Names[i])
		}
	}
}

func TestCollectionSliceEscapedSeparatorsUnquoted(t *testing.T) {
	s, err := collect(t, func() {
		os.Setenv("NAMES", `a\,b,c\\d`)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a,b", `c\d`}
	if strings.Join(s.Names, "|") != strings.Join(want, "|") {
		t.Fatalf("expected %#v, got %#v", want, s.Names)
	}
}

func TestCollectionEmptyCollection(t *testing.T) {
	s, err := collect(t, func() { os.Setenv("NAMES", "") })
	if err != nil {
		t.Fatal(err)
	}
	if s.Names == nil {
		t.Fatal("expected allocated empty slice, got nil")
	}
	if len(s.Names) != 0 {
		t.Fatalf("expected empty slice, got %#v", s.Names)
	}

	s, err = collect(t, func() { os.Setenv("CODES", "   ") })
	if err != nil {
		t.Fatal(err)
	}
	if s.Codes == nil {
		t.Fatal("expected allocated empty map, got nil")
	}
	if len(s.Codes) != 0 {
		t.Fatalf("expected empty map, got %#v", s.Codes)
	}
}

func TestCollectionSingleEmptyString(t *testing.T) {
	s, err := collect(t, func() { os.Setenv("NAMES", `""`) })
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Names) != 1 || s.Names[0] != "" {
		t.Fatalf(`expected [""], got %#v`, s.Names)
	}

	// Explicit empty strings can also appear between other elements.
	s, err = collect(t, func() { os.Setenv("NAMES", `a,"",b`) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "", "b"}
	if strings.Join(s.Names, "|") != strings.Join(want, "|") {
		t.Fatalf("expected %#v, got %#v", want, s.Names)
	}
}

func TestCollectionLeadingTrailingWhitespace(t *testing.T) {
	s, err := collect(t, func() { os.Setenv("NAMES", "  a  , b ,\"  kept  \"") })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "  kept  "}
	if strings.Join(s.Names, "|") != strings.Join(want, "|") {
		t.Fatalf("expected %#v, got %#v", want, s.Names)
	}
}

func TestCollectionMapQuotedKeysAndValues(t *testing.T) {
	s, err := collect(t, func() {
		// Commas and colons live inside quoted/escaped pieces.
		os.Setenv("CODES", `"re,d":1,"g:reen":2`)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Codes) != 2 || s.Codes["re,d"] != 1 || s.Codes["g:reen"] != 2 {
		t.Fatalf("expected quoted keys, got %#v", s.Codes)
	}
}

func TestCollectionMapColonInValueNotResplit(t *testing.T) {
	s, err := collect(t, func() {
		os.Setenv("PAIRS", `url:http://example:8080/end,time:"12:30:00"`)
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Pairs["url"] != "http://example:8080/end" {
		t.Errorf("expected url value preserved, got %#v", s.Pairs)
	}
	if s.Pairs["time"] != "12:30:00" {
		t.Errorf("expected time value preserved, got %#v", s.Pairs)
	}
}

func TestCollectionMapDuplicateKeysLastWins(t *testing.T) {
	s, err := collect(t, func() {
		os.Setenv("CODES", `a:1,a:2,a:3`)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Codes) != 1 || s.Codes["a"] != 3 {
		t.Fatalf("expected duplicate key to resolve to 3, got %#v", s.Codes)
	}
}

func TestCollectionElementsStillConverted(t *testing.T) {
	s, err := collect(t, func() { os.Setenv("NUMBERS", `1, "2" ,3`) })
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Numbers) != 3 || s.Numbers[0] != 1 || s.Numbers[1] != 2 || s.Numbers[2] != 3 {
		t.Fatalf("expected [1 2 3], got %#v", s.Numbers)
	}

	// A failing element conversion keeps going through the normal type
	// conversion path and reports the offending position.
	_, err = collect(t, func() { os.Setenv("NUMBERS", `1,oops,3`) })
	if err == nil || !strings.Contains(err.Error(), "element 2") {
		t.Fatalf("expected element 2 conversion error, got %v", err)
	}
}

type collectionDecoder struct{ got string }

func (d *collectionDecoder) Decode(value string) error {
	d.got = value
	return nil
}

func TestCollectionElementHonorsDecoder(t *testing.T) {
	var s struct {
		Items []collectionDecoder `envconfig:"ITEMS" collection_syntax:"true"`
	}
	os.Clearenv()
	os.Setenv("ITEMS", ` a ,"b,c"`)
	if err := Process("", &s); err != nil {
		t.Fatal(err)
	}
	if s.Items[0].got != "a" || s.Items[1].got != "b,c" {
		t.Fatalf("expected decoded elements, got %#v", s.Items)
	}
}

func TestCollectionUnterminatedQuoteReportsPosition(t *testing.T) {
	cases := []struct {
		value string
		pos   string
	}{
		{`a,"unterminated,c`, "element 2"},
		{`a,b,"oops`, "element 3"},
		{`a:1,"oops:2`, "map item 2"},
	}
	for _, tc := range cases {
		os.Clearenv()
		var spec interface{}
		if strings.Contains(tc.pos, "map item") {
			spec = &struct {
				V map[string]int `collection_syntax:"true"`
			}{}
		} else {
			spec = &struct {
				V []string `collection_syntax:"true"`
			}{}
		}
		os.Setenv("V", tc.value)
		err := Process("", spec)
		if err == nil || !strings.Contains(err.Error(), tc.pos) ||
			!strings.Contains(err.Error(), "unterminated quote") {
			t.Errorf("value %q: expected %s unterminated quote error, got %v", tc.value, tc.pos, err)
		}
	}
}

func TestCollectionDanglingEscapeReportsPosition(t *testing.T) {
	cases := []struct {
		value string
		pos   string
	}{
		{`a,b\`, "element 2"},
		{`x:1,y:2\`, "map item 2"},
	}
	for _, tc := range cases {
		os.Clearenv()
		var spec interface{}
		if strings.Contains(tc.pos, "map item") {
			spec = &struct {
				V map[string]int `collection_syntax:"true"`
			}{}
		} else {
			spec = &struct {
				V []string `collection_syntax:"true"`
			}{}
		}
		os.Setenv("V", tc.value)
		err := Process("", spec)
		if err == nil || !strings.Contains(err.Error(), tc.pos) ||
			!strings.Contains(err.Error(), "dangling escape") {
			t.Errorf("value %q: expected %s dangling escape error, got %v", tc.value, tc.pos, err)
		}
	}
}

func TestCollectionMapMissingColonReportsPosition(t *testing.T) {
	os.Clearenv()
	var s struct {
		Codes map[string]int `envconfig:"CODES" collection_syntax:"true"`
	}
	os.Setenv("CODES", `a:1,broken,c:3`)
	err := Process("", &s)
	if err == nil || !strings.Contains(err.Error(), "map item 2") {
		t.Fatalf("expected map item 2 error, got %v", err)
	}
}

func TestCollectionSyntaxIsOptIn(t *testing.T) {
	// Without the tag, parsing results are identical to the legacy syntax:
	// quotes and backslashes are plain characters and a colon inside a value
	// causes the legacy map parser to reject the item just as before.
	os.Clearenv()
	var legacy struct {
		Names []string          `envconfig:"NAMES"`
		Pairs map[string]string `envconfig:"PAIRS"`
	}
	os.Setenv("NAMES", `"a,b",c`)
	os.Setenv("PAIRS", `a:1,b:2:3`)
	err := Process("", &legacy)
	if err == nil {
		t.Fatal("expected legacy invalid map item error, got nil")
	}
	if !strings.Contains(err.Error(), `invalid map item: "b:2:3"`) {
		t.Fatalf("expected unchanged legacy error message, got %v", err)
	}
	if strings.Join(legacy.Names, "|") != `"a|b"|c` {
		t.Fatalf("expected legacy raw splitting, got %#v", legacy.Names)
	}
}

func TestCollectionDefaultTagAlsoParsed(t *testing.T) {
	os.Clearenv()
	var s struct {
		Names []string `envconfig:"NAMES" default:"\"a,b\",c" collection_syntax:"true"`
	}
	if err := Process("", &s); err != nil {
		t.Fatal(err)
	}
	want := []string{"a,b", "c"}
	if strings.Join(s.Names, "|") != strings.Join(want, "|") {
		t.Fatalf("expected default parsed with collection syntax, got %#v", s.Names)
	}
}
