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

package bleve

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	regexp_char_filter "github.com/blevesearch/bleve/v2/analysis/char/regexp"
	"github.com/blevesearch/bleve/v2/index/scorch"
	"github.com/blevesearch/bleve/v2/search/highlight/highlighter/html"
)

func setupOffsetMappingIndex(t *testing.T, charFilters map[string]map[string]interface{},
	filterOrder []string, doc map[string]interface{},
) Index {
	idxMapping := NewIndexMapping()
	for name, config := range charFilters {
		config["type"] = regexp_char_filter.Name
		if err := idxMapping.AddCustomCharFilter(name, config); err != nil {
			t.Fatal(err)
		}
	}
	if err := idxMapping.AddCustomAnalyzer("offset_mapping", map[string]interface{}{
		"type":         custom.Name,
		"tokenizer":    "unicode",
		"char_filters": filterOrder,
		"token_filters": []string{
			"to_lower",
		},
	}); err != nil {
		t.Fatal(err)
	}
	idxMapping.DefaultAnalyzer = "offset_mapping"
	idxMapping.StoreDynamic = true

	tmpIndexPath := createTmpIndexPath(t)
	t.Cleanup(func() { cleanupTmpIndexPath(t, tmpIndexPath) })

	idx, err := NewUsing(tmpIndexPath, idxMapping, scorch.Name, Config.DefaultKVStore, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err = idx.Index("doc", doc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := idx.Close(); err != nil {
			t.Fatal(err)
		}
	})
	return idx
}

func searchHighlight(t *testing.T, idx Index, q string) string {
	query := NewMatchQuery(q)
	sreq := NewSearchRequest(query)
	sreq.Fields = []string{"*"}
	sreq.Highlight = NewHighlightWithStyle(html.Name)

	sres, err := idx.Search(sreq)
	if err != nil {
		t.Fatal(err)
	}
	if sres.Total != 1 {
		t.Fatalf("expected 1 hit, got: %v", sres.Total)
	}
	fragments := sres.Hits[0].Fragments["status"]
	if len(fragments) != 1 {
		t.Fatalf("expected 1 fragment, got: %v", sres.Hits[0].Fragments)
	}
	return fragments[0]
}

// HTML entity expansion and tag removal shift offsets; highlighting must
// still land on the stored original text without splitting multi-byte
// characters or inserting markers inside the original entity.
func TestHighlightWithCharFilterOffsetMapping(t *testing.T) {
	idx := setupOffsetMappingIndex(t,
		map[string]map[string]interface{}{
			"amp_entity":  {"regexp": `&amp;`, "replace": "&"},
			"tag_remover": {"regexp": `<[^>]+>`, "replace": ""},
		},
		[]string{"amp_entity", "tag_remover"},
		map[string]interface{}{
			"status": "<p>Tom &amp; Jerry</p> <p>café 中文 résumé</p>",
		})

	fragment := searchHighlight(t, idx, "jerry")
	expected := "&lt;p&gt;Tom &amp;amp; <mark>Jerry</mark>&lt;/p&gt;" +
		" &lt;p&gt;café 中文 résumé&lt;/p&gt;"
	if fragment != expected {
		t.Fatalf("expected fragment:\n%s\ngot:\n%s", expected, fragment)
	}
	if !utf8.ValidString(fragment) {
		t.Fatalf("fragment is not valid UTF-8: %q", fragment)
	}
	if !strings.Contains(fragment, "&amp;amp;") {
		t.Fatalf("fragment does not contain the intact original entity: %q", fragment)
	}

	// the multi-byte term itself must be highlighted as a whole
	fragment = searchHighlight(t, idx, "résumé")
	expected = "&lt;p&gt;Tom &amp;amp; Jerry&lt;/p&gt;" +
		" &lt;p&gt;café 中文 <mark>résumé</mark>&lt;/p&gt;"
	if fragment != expected {
		t.Fatalf("expected fragment:\n%s\ngot:\n%s", expected, fragment)
	}
	if !utf8.ValidString(fragment) {
		t.Fatalf("fragment is not valid UTF-8: %q", fragment)
	}
}

// Two chained replacements must compose: a token produced by the second
// replacement maps all the way back to the original HTML entity, so the
// highlight wraps the whole entity instead of landing inside it.
func TestHighlightWithTwoLayerCharFilterOffsetMapping(t *testing.T) {
	idx := setupOffsetMappingIndex(t,
		map[string]map[string]interface{}{
			"amp_entity": {"regexp": `&amp;`, "replace": "&"},
			"amp_word":   {"regexp": `&`, "replace": "and"},
		},
		[]string{"amp_entity", "amp_word"},
		map[string]interface{}{
			"status": "fish &amp; chips",
		})

	fragment := searchHighlight(t, idx, "and")
	expected := "fish <mark>&amp;amp;</mark> chips"
	if fragment != expected {
		t.Fatalf("expected fragment:\n%s\ngot:\n%s", expected, fragment)
	}
	if strings.Contains(fragment, "&<mark>") || strings.Contains(fragment, "</mark>;") {
		t.Fatalf("highlight markers landed inside the original entity: %q", fragment)
	}
}
