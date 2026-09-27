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

package analysis

import (
	"fmt"
	"time"
)

// CharFilter transforms the raw input bytes before tokenization. Filters
// that change the length of the input (expanding HTML entities, removing
// tags, folding multi-byte characters, ...) shift the offsets of all
// following text; such filters should also implement
// OffsetMappingCharFilter so token offsets can be mapped back onto the
// original text (e.g. for accurate highlighting). Filters that do not
// implement OffsetMappingCharFilter are treated as identity mappings:
// offsets pass through unchanged, exactly as they did before offset
// mappings existed.
type CharFilter interface {
	Filter([]byte) []byte
}

type TokenType int

const (
	AlphaNumeric TokenType = iota
	Ideographic
	Numeric
	DateTime
	Shingle
	Single
	Double
	Boolean
	IP
)

// Token represents one occurrence of a term at a particular location in a
// field.
type Token struct {
	// Start specifies the byte offset of the beginning of the term in the
	// field.
	Start int `json:"start"`

	// End specifies the byte offset of the end of the term in the field.
	End  int    `json:"end"`
	Term []byte `json:"term"`

	// Position specifies the 1-based index of the token in the sequence of
	// occurrences of its term in the field.
	Position int       `json:"position"`
	Type     TokenType `json:"type"`
	KeyWord  bool      `json:"keyword"`
}

func (t *Token) String() string {
	return fmt.Sprintf("Start: %d  End: %d  Position: %d  Token: %s  Type: %d", t.Start, t.End, t.Position, string(t.Term), t.Type)
}

type TokenStream []*Token

// A Tokenizer splits an input string into tokens, the usual behaviour being to
// map words to tokens.
type Tokenizer interface {
	Tokenize([]byte) TokenStream
}

// A TokenFilter adds, transforms or removes tokens from a token stream.
type TokenFilter interface {
	Filter(TokenStream) TokenStream
}

type Analyzer interface {
	Analyze([]byte) TokenStream
}

type DefaultAnalyzer struct {
	CharFilters  []CharFilter
	Tokenizer    Tokenizer
	TokenFilters []TokenFilter
}

func (a *DefaultAnalyzer) Analyze(input []byte) TokenStream {
	tokens, _ := a.AnalyzeWithOffsetMapping(input)
	return tokens
}

// AnalyzeWithOffsetMapping analyzes the input like Analyze, and also
// returns the offset mapping composed from the char filter chain, which
// maps offsets in the filtered (tokenized) text back to offsets in the
// original input. The returned tokens already have their Start/End offsets
// expressed in original input coordinates, so highlights land on the text
// the user actually stored. The mapping is nil when none of the char
// filters provide offset information, in which case token offsets are
// passed through unchanged (the historical behavior).
func (a *DefaultAnalyzer) AnalyzeWithOffsetMapping(input []byte) (TokenStream, *OffsetMapping) {
	var mapping *OffsetMapping
	if a.CharFilters != nil {
		input, mapping = ApplyCharFilters(a.CharFilters, input)
	}
	tokens := a.Tokenizer.Tokenize(input)
	if a.TokenFilters != nil {
		for _, tf := range a.TokenFilters {
			tokens = tf.Filter(tokens)
		}
	}
	if !mapping.IsIdentity() {
		for _, token := range tokens {
			token.Start = mapping.MapStart(token.Start)
			token.End = mapping.MapEnd(token.End)
		}
	}
	return tokens, mapping
}

var ErrInvalidDateTime = fmt.Errorf("unable to parse datetime with any of the layouts")

var ErrInvalidTimestampString = fmt.Errorf("unable to parse timestamp string")
var ErrInvalidTimestampRange = fmt.Errorf("timestamp out of range")

type DateTimeParser interface {
	ParseDateTime(string) (time.Time, string, error)
}

const SynonymSourceType = "synonym"

type SynonymSourceVisitor func(name string, item SynonymSource) error

type SynonymSource interface {
	Analyzer() string
	Collection() string
}

type ByteArrayConverter interface {
	Convert([]byte) (interface{}, error)
}
