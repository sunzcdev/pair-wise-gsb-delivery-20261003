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

package analysis

import "sort"

// OffsetAnchor records that the byte offset Filtered in a char filter's
// output corresponds to the byte offset Original in that filter's input.
type OffsetAnchor struct {
	Filtered int
	Original int
}

// OffsetMapping maps byte offsets in filtered text back to byte offsets in
// the original, unfiltered text. It is built from a set of anchors; offsets
// between anchors are interpolated linearly, which is exact for regions a
// filter copied through unchanged.
//
// Because a filter may delete bytes entirely, a single filtered offset can
// correspond to two original offsets (the end of the text before a deleted
// region and the start of the text after it). MapStart resolves such
// boundaries towards the following text, MapEnd towards the preceding text,
// so a token spanning [start, end) maps to the original span it came from.
//
// Mappings are composable: when several char filters run in a chain, their
// individual mappings can be combined with Compose so offsets in the final
// output still map all the way back to the original input.
//
// The zero value (and nil) is the identity mapping: offsets pass through
// unchanged.
type OffsetMapping struct {
	// anchors describes a single filter's mapping, sorted by Filtered.
	anchors []OffsetAnchor
	// layers, when non-nil, composes several mappings; they are applied
	// from the last layer back to the first.
	layers []*OffsetMapping
}

// NewOffsetMapping builds a leaf mapping from the given anchors. Anchors
// are sorted by their filtered offset; the sort is stable so that both
// anchors of a fully collapsed (deleted) region keep their relative order.
func NewOffsetMapping(anchors []OffsetAnchor) *OffsetMapping {
	if len(anchors) == 0 {
		return nil
	}
	sort.Stable(offsetAnchorSortable(anchors))
	return &OffsetMapping{anchors: anchors}
}

type offsetAnchorSortable []OffsetAnchor

func (o offsetAnchorSortable) Len() int           { return len(o) }
func (o offsetAnchorSortable) Swap(i, j int)      { o[i], o[j] = o[j], o[i] }
func (o offsetAnchorSortable) Less(i, j int) bool { return o[i].Filtered < o[j].Filtered }

// IsIdentity reports whether the mapping leaves every offset unchanged.
func (m *OffsetMapping) IsIdentity() bool {
	return m == nil || (len(m.anchors) == 0 && len(m.layers) == 0)
}

// Compose returns a mapping that first applies the inner mapping and then
// this one. If m maps filter1's output back to the original text and inner
// maps filter2's output back to filter1's output, the result maps filter2's
// output all the way back to the original text.
func (m *OffsetMapping) Compose(inner *OffsetMapping) *OffsetMapping {
	if m.IsIdentity() {
		return inner
	}
	if inner.IsIdentity() {
		return m
	}
	layers := make([]*OffsetMapping, 0, 4)
	if len(m.layers) > 0 {
		layers = append(layers, m.layers...)
	} else {
		layers = append(layers, m)
	}
	if len(inner.layers) > 0 {
		layers = append(layers, inner.layers...)
	} else {
		layers = append(layers, inner)
	}
	return &OffsetMapping{layers: layers}
}

// MapStart maps the start offset of a span in filtered text to the
// corresponding start offset in the original text.
func (m *OffsetMapping) MapStart(offset int) int {
	return m.mapOffset(offset, true)
}

// MapEnd maps the end offset of a span in filtered text to the
// corresponding end offset in the original text.
func (m *OffsetMapping) MapEnd(offset int) int {
	return m.mapOffset(offset, false)
}

func (m *OffsetMapping) mapOffset(offset int, start bool) int {
	if m == nil {
		return offset
	}
	if len(m.layers) > 0 {
		for i := len(m.layers) - 1; i >= 0; i-- {
			offset = m.layers[i].mapOffset(offset, start)
		}
		return offset
	}
	n := len(m.anchors)
	if n == 0 {
		return offset
	}
	first := m.anchors[0]
	last := m.anchors[n-1]
	// outside the anchored range, shift by the nearest anchor's delta,
	// which is exact for head/tail regions a filter copied unchanged
	if offset < first.Filtered {
		return offset + first.Original - first.Filtered
	}
	if offset > last.Filtered {
		return offset + last.Original - last.Filtered
	}
	if start {
		// last anchor with Filtered <= offset
		i := sort.Search(n, func(i int) bool { return m.anchors[i].Filtered > offset }) - 1
		if m.anchors[i].Filtered == offset {
			return m.anchors[i].Original
		}
		return interpolateOffset(m.anchors[i], m.anchors[i+1], offset)
	}
	// first anchor with Filtered >= offset
	j := sort.Search(n, func(i int) bool { return m.anchors[i].Filtered >= offset })
	if m.anchors[j].Filtered == offset {
		return m.anchors[j].Original
	}
	return interpolateOffset(m.anchors[j-1], m.anchors[j], offset)
}

// interpolateOffset maps offset linearly between anchors a and b, where
// a.Filtered < offset < b.Filtered.
func interpolateOffset(a, b OffsetAnchor, offset int) int {
	return a.Original + (offset-a.Filtered)*(b.Original-a.Original)/(b.Filtered-a.Filtered)
}

// OffsetMappingCharFilter is an optional interface a CharFilter can
// implement to describe how byte offsets in its output relate to byte
// offsets in its input. Char filters that only implement CharFilter are
// treated as identity mappings: offsets pass through unchanged, preserving
// the behavior that existed before offset mappings were introduced.
type OffsetMappingCharFilter interface {
	CharFilter
	FilterWithOffsetMapping(input []byte) ([]byte, *OffsetMapping)
}

// ApplyCharFilters runs the given char filters over the input in order and
// returns the filtered bytes along with the composed offset mapping from
// the final output back to the original input. Filters that do not
// implement OffsetMappingCharFilter contribute an identity mapping, so the
// returned mapping is exact as long as every length-changing filter in the
// chain provides one. The returned mapping is nil when no filter provided
// any mapping information.
func ApplyCharFilters(filters []CharFilter, input []byte) ([]byte, *OffsetMapping) {
	var mapping *OffsetMapping
	for _, cf := range filters {
		if omcf, ok := cf.(OffsetMappingCharFilter); ok {
			var m *OffsetMapping
			input, m = omcf.FilterWithOffsetMapping(input)
			mapping = mapping.Compose(m)
		} else {
			input = cf.Filter(input)
		}
	}
	return input, mapping
}
