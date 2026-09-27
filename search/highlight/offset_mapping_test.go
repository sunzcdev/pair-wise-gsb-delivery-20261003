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

package highlight_test

import (
	"regexp"
	"testing"

	"github.com/blevesearch/bleve/v2/analysis"
	regexpcharfilter "github.com/blevesearch/bleve/v2/analysis/char/regexp"
	"github.com/blevesearch/bleve/v2/search/highlight"
)

func TestTermLocationsMapOffsets(t *testing.T) {
	orig := "Tom &amp; Jerry"
	filtered, mapping := analysis.ApplyCharFilters([]analysis.CharFilter{
		regexpcharfilter.New(regexp.MustCompile(`&amp;`), []byte("&")),
	}, []byte(orig))
	if string(filtered) != "Tom & Jerry" {
		t.Fatalf("expected filtered text %q, got %q", "Tom & Jerry", filtered)
	}

	// locations as produced by analyzing the filtered text
	locations := highlight.TermLocations{
		&highlight.TermLocation{Term: "jerry", Start: 6, End: 11},
		&highlight.TermLocation{Term: "tom", Start: 0, End: 3},
	}
	locations.MapOffsets(mapping)

	if got := orig[locations[0].Start:locations[0].End]; got != "Jerry" {
		t.Errorf("expected mapped location to slice %q from the original, got %q", "Jerry", got)
	}
	if got := orig[locations[1].Start:locations[1].End]; got != "Tom" {
		t.Errorf("expected mapped location to slice %q from the original, got %q", "Tom", got)
	}

	// a nil mapping leaves locations untouched
	locations = highlight.TermLocations{
		&highlight.TermLocation{Term: "jerry", Start: 6, End: 11},
	}
	locations.MapOffsets(nil)
	if locations[0].Start != 6 || locations[0].End != 11 {
		t.Errorf("expected nil mapping to leave locations unchanged, got [%d, %d)",
			locations[0].Start, locations[0].End)
	}
}
