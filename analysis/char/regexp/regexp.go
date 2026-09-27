//  Copyright (c) 2014 Couchbase, Inc.
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

package regexp

import (
	"fmt"
	"regexp"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/registry"
)

const Name = "regexp"

type CharFilter struct {
	r           *regexp.Regexp
	replacement []byte
}

func New(r *regexp.Regexp, replacement []byte) *CharFilter {
	return &CharFilter{
		r:           r,
		replacement: replacement,
	}
}

func (s *CharFilter) Filter(input []byte) []byte {
	return s.r.ReplaceAll(input, s.replacement)
}

// FilterWithOffsetMapping filters the input and records how the offsets of
// the output relate to the offsets of the input, so that token offsets can
// be mapped back onto the original text. Each match of the regexp produces
// a pair of anchors delimiting the replaced region; the bytes between
// matches are copied through unchanged and map one-to-one.
func (s *CharFilter) FilterWithOffsetMapping(input []byte) ([]byte, *analysis.OffsetMapping) {
	matches := s.r.FindAllSubmatchIndex(input, -1)
	if len(matches) == 0 {
		return input, nil
	}
	out := make([]byte, 0, len(input))
	anchors := make([]analysis.OffsetAnchor, 0, 2*len(matches)+2)
	anchors = append(anchors, analysis.OffsetAnchor{Filtered: 0, Original: 0})
	last := 0
	for _, match := range matches {
		out = append(out, input[last:match[0]]...)
		anchors = append(anchors, analysis.OffsetAnchor{Filtered: len(out), Original: match[0]})
		out = s.r.Expand(out, s.replacement, input, match)
		anchors = append(anchors, analysis.OffsetAnchor{Filtered: len(out), Original: match[1]})
		last = match[1]
	}
	out = append(out, input[last:]...)
	anchors = append(anchors, analysis.OffsetAnchor{Filtered: len(out), Original: len(input)})
	return out, analysis.NewOffsetMapping(anchors)
}

func CharFilterConstructor(config map[string]interface{}, cache *registry.Cache) (analysis.CharFilter, error) {
	regexpStr, ok := config["regexp"].(string)
	if !ok {
		return nil, fmt.Errorf("must specify regexp")
	}
	r, err := regexp.Compile(regexpStr)
	if err != nil {
		return nil, fmt.Errorf("unable to build regexp char filter: %v", err)
	}
	replaceBytes := []byte(" ")
	replaceStr, ok := config["replace"].(string)
	if ok {
		replaceBytes = []byte(replaceStr)
	}
	return New(r, replaceBytes), nil
}

func init() {
	err := registry.RegisterCharFilter(Name, CharFilterConstructor)
	if err != nil {
		panic(err)
	}
}
