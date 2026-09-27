//  Copyright (c) 2026 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package analysis_test

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2/analysis"
	regexpcharfilter "github.com/blevesearch/bleve/v2/analysis/char/regexp"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
)

// checkSpan verifies that the filtered-text span [fStart, fEnd) maps back
// to the expected substring of the original text, and that the mapped
// offsets do not split a UTF-8 character.
func checkSpan(t *testing.T, m *analysis.OffsetMapping, orig string, fStart, fEnd int, want string) {
	t.Helper()
	start, end := m.MapStart(fStart), m.MapEnd(fEnd)
	if start < 0 || end > len(orig) || start > end {
		t.Fatalf("mapped span [%d, %d) out of bounds for original of length %d", start, end, len(orig))
	}
	if got := orig[start:end]; got != want {
		t.Errorf("filtered span [%d, %d) mapped to original [%d, %d) = %q, want %q",
			fStart, fEnd, start, end, got, want)
	}
	if !utf8.ValidString(orig[start:end]) {
		t.Errorf("mapped span [%d, %d) splits a UTF-8 character in %q", start, end, orig)
	}
}

func filteredSpan(filtered, sub string) (int, int) {
	start := strings.Index(filtered, sub)
	return start, start + len(sub)
}

func TestOffsetMappingHTMLEntityExpansion(t *testing.T) {
	orig := "Tom &amp; Jerry"
	filter := regexpcharfilter.New(regexp.MustCompile(`&amp;`), []byte("&"))

	filtered, mapping := analysis.ApplyCharFilters([]analysis.CharFilter{filter}, []byte(orig))
	if string(filtered) != "Tom & Jerry" {
		t.Fatalf("expected filtered text %q, got %q", "Tom & Jerry", filtered)
	}
	if mapping == nil {
		t.Fatal("expected a non-nil offset mapping")
	}

	start, end := filteredSpan(string(filtered), "Jerry")
	checkSpan(t, mapping, orig, start, end, "Jerry")
	start, end = filteredSpan(string(filtered), "&")
	checkSpan(t, mapping, orig, start, end, "&amp;")
	start, end = filteredSpan(string(filtered), "Tom")
	checkSpan(t, mapping, orig, start, end, "Tom")
}

func TestOffsetMappingDeletedTags(t *testing.T) {
	orig := "<b>bold</b> move"
	filter := regexpcharfilter.New(regexp.MustCompile(`<[^>]+>`), []byte(""))

	filtered, mapping := analysis.ApplyCharFilters([]analysis.CharFilter{filter}, []byte(orig))
	if string(filtered) != "bold move" {
		t.Fatalf("expected filtered text %q, got %q", "bold move", filtered)
	}

	start, end := filteredSpan(string(filtered), "bold")
	checkSpan(t, mapping, orig, start, end, "bold")
	start, end = filteredSpan(string(filtered), "move")
	checkSpan(t, mapping, orig, start, end, "move")
}

func TestOffsetMappingMultiByteCharacters(t *testing.T) {
	orig := "café <i>中文</i> résumé"
	filter := regexpcharfilter.New(regexp.MustCompile(`<[^>]+>`), []byte(""))

	filtered, mapping := analysis.ApplyCharFilters([]analysis.CharFilter{filter}, []byte(orig))
	if string(filtered) != "café 中文 résumé" {
		t.Fatalf("expected filtered text %q, got %q", "café 中文 résumé", filtered)
	}

	for _, word := range []string{"café", "中文", "résumé"} {
		start, end := filteredSpan(string(filtered), word)
		checkSpan(t, mapping, orig, start, end, word)
	}

	// replacing a multi-byte character itself must not split its encoding
	orig2 := "café au lait"
	fold := regexpcharfilter.New(regexp.MustCompile("é"), []byte("e"))
	filtered2, mapping2 := analysis.ApplyCharFilters([]analysis.CharFilter{fold}, []byte(orig2))
	if string(filtered2) != "cafe au lait" {
		t.Fatalf("expected filtered text %q, got %q", "cafe au lait", filtered2)
	}
	start, end := filteredSpan(string(filtered2), "au")
	checkSpan(t, mapping2, orig2, start, end, "au")
	start, end = filteredSpan(string(filtered2), "cafe")
	checkSpan(t, mapping2, orig2, start, end, "café")
}

func TestOffsetMappingComposesAcrossFilters(t *testing.T) {
	orig := "fish &amp; chips"
	expandEntity := regexpcharfilter.New(regexp.MustCompile(`&amp;`), []byte("&"))
	expandAmpersand := regexpcharfilter.New(regexp.MustCompile(`&`), []byte("and"))

	filtered, mapping := analysis.ApplyCharFilters(
		[]analysis.CharFilter{expandEntity, expandAmpersand}, []byte(orig))
	if string(filtered) != "fish and chips" {
		t.Fatalf("expected filtered text %q, got %q", "fish and chips", filtered)
	}

	// the token produced by the second replacement maps all the way back
	// to the original HTML entity
	start, end := filteredSpan(string(filtered), "and")
	checkSpan(t, mapping, orig, start, end, "&amp;")
	start, end = filteredSpan(string(filtered), "chips")
	checkSpan(t, mapping, orig, start, end, "chips")
	start, end = filteredSpan(string(filtered), "fish")
	checkSpan(t, mapping, orig, start, end, "fish")
}

func TestOffsetMappingWithCaptureGroupExpansion(t *testing.T) {
	orig := "temp 1 and hum 2"
	filter := regexpcharfilter.New(regexp.MustCompile(`([a-z])\s+(\d)`), []byte(`ooooo$1-$2`))

	filtered, mapping := analysis.ApplyCharFilters([]analysis.CharFilter{filter}, []byte(orig))
	if string(filtered) != "temooooop-1 and huooooom-2" {
		t.Fatalf("unexpected filtered text %q", filtered)
	}

	start, end := filteredSpan(string(filtered), "and")
	checkSpan(t, mapping, orig, start, end, "and")
	// the expanded replacement maps back to the original match
	start, end = filteredSpan(string(filtered), "ooooom-2")
	checkSpan(t, mapping, orig, start, end, "m 2")
}

// identityOnlyFilter implements only analysis.CharFilter, not
// analysis.OffsetMappingCharFilter.
type identityOnlyFilter struct{}

func (identityOnlyFilter) Filter(input []byte) []byte {
	return bytes.ToUpper(input)
}

// lengthChangingFilter implements only analysis.CharFilter and changes the
// length of the input without providing any mapping.
type lengthChangingFilter struct{}

func (lengthChangingFilter) Filter(input []byte) []byte {
	return bytes.ReplaceAll(input, []byte("x"), nil)
}

func TestApplyCharFiltersCompatibility(t *testing.T) {
	// filters that do not provide a mapping are treated as identity:
	// the composed mapping is nil and offsets pass through unchanged,
	// exactly as before offset mappings existed
	filtered, mapping := analysis.ApplyCharFilters(
		[]analysis.CharFilter{identityOnlyFilter{}}, []byte("hello"))
	if string(filtered) != "HELLO" {
		t.Fatalf("expected filtered text %q, got %q", "HELLO", filtered)
	}
	if mapping != nil {
		t.Fatalf("expected nil mapping from filters without mapping support, got %v", mapping)
	}
	if got := mapping.MapStart(3); got != 3 {
		t.Fatalf("expected nil mapping to pass offsets through, got %d", got)
	}

	// a length-changing filter without mapping support must not break the
	// chain; offsets are passed through (the historical behavior)
	filtered, mapping = analysis.ApplyCharFilters(
		[]analysis.CharFilter{lengthChangingFilter{}}, []byte("xox"))
	if string(filtered) != "o" {
		t.Fatalf("expected filtered text %q, got %q", "o", filtered)
	}
	if mapping != nil {
		t.Fatalf("expected nil mapping from filters without mapping support, got %v", mapping)
	}

	// filters without mapping support compose fine with filters that have it
	filtered, mapping = analysis.ApplyCharFilters(
		[]analysis.CharFilter{
			identityOnlyFilter{},
			regexpcharfilter.New(regexp.MustCompile(`&AMP;`), []byte("&")),
		}, []byte("A &AMP; B"))
	if string(filtered) != "A & B" {
		t.Fatalf("expected filtered text %q, got %q", "A & B", filtered)
	}
	if mapping == nil {
		t.Fatal("expected a non-nil offset mapping")
	}
	start, end := filteredSpan(string(filtered), "B")
	checkSpan(t, mapping, "A &AMP; B", start, end, "B")
}

func TestDefaultAnalyzerRemapsTokenOffsets(t *testing.T) {
	orig := "Tom &amp; Jerry café"
	analyzer := &analysis.DefaultAnalyzer{
		CharFilters: []analysis.CharFilter{
			regexpcharfilter.New(regexp.MustCompile(`&amp;`), []byte("&")),
			regexpcharfilter.New(regexp.MustCompile(`<[^>]+>`), []byte("")),
		},
		Tokenizer: unicode.NewUnicodeTokenizer(),
	}

	tokens, mapping := analyzer.AnalyzeWithOffsetMapping([]byte(orig))
	if mapping == nil {
		t.Fatal("expected a non-nil offset mapping")
	}

	terms := make(map[string][]int)
	for _, token := range tokens {
		terms[string(token.Term)] = []int{token.Start, token.End}
	}
	for _, want := range []string{"Tom", "Jerry", "café"} {
		span, ok := terms[want]
		if !ok {
			t.Fatalf("expected token %q in stream %v", want, tokens)
		}
		if got := orig[span[0]:span[1]]; got != want {
			t.Errorf("token %q has offsets [%d, %d) slicing original to %q",
				want, span[0], span[1], got)
		}
	}

	// Analyze returns the same remapped tokens
	tokens2 := analyzer.Analyze([]byte(orig))
	if len(tokens2) != len(tokens) {
		t.Fatalf("Analyze returned %d tokens, AnalyzeWithOffsetMapping %d", len(tokens2), len(tokens))
	}
	for i, token := range tokens2 {
		if token.Start != tokens[i].Start || token.End != tokens[i].End {
			t.Errorf("token %d offsets differ: Analyze [%d, %d), AnalyzeWithOffsetMapping [%d, %d)",
				i, token.Start, token.End, tokens[i].Start, tokens[i].End)
		}
	}
}

func TestDefaultAnalyzerWithoutMappingIsUnchanged(t *testing.T) {
	// analyzers whose char filters provide no mapping behave exactly as
	// before: token offsets are the offsets in the filtered text
	analyzer := &analysis.DefaultAnalyzer{
		CharFilters: []analysis.CharFilter{identityOnlyFilter{}},
		Tokenizer:   unicode.NewUnicodeTokenizer(),
	}
	tokens, mapping := analyzer.AnalyzeWithOffsetMapping([]byte("tom jerry"))
	if mapping != nil {
		t.Fatalf("expected nil mapping, got %v", mapping)
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %v", tokens)
	}
	if tokens[1].Start != 4 || tokens[1].End != 9 {
		t.Fatalf("expected unmodified offsets [4, 9) for second token, got [%d, %d)",
			tokens[1].Start, tokens[1].End)
	}
	if string(tokens[1].Term) != "JERRY" {
		t.Fatalf("expected upper-cased term, got %q", tokens[1].Term)
	}
}
