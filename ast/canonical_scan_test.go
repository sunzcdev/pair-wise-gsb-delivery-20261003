package ast_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
)

type specCase struct {
	Markdown string `json:"markdown"`
	Example  int    `json:"example"`
}

func loadSpecCases(t *testing.T) []specCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []specCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// TestCanonicalScanSpec exports every CommonMark spec example with both the
// default and GFM parsers: every built-in/extension node must have a
// canonical representation, and the output must round-trip through the
// standard library JSON decoder.
func TestCanonicalScanSpec(t *testing.T) {
	parsers := map[string]parser.Parser{
		"default": parser.New(),
		"gfm": parser.New(
			parser.WithExtensions(
				extension.NewGFMParser(),
				extension.NewFootnoteParser(),
				extension.NewDefinitionListParser(),
				extension.NewTypographerParser(),
			),
		),
	}
	for _, tc := range loadSpecCases(t) {
		source := []byte(tc.Markdown)
		for parserName, p := range parsers {
			doc := p.Parse(source)
			out, err := ast.MarshalCanonicalToBytes(doc, source, ast.WithCanonicalSource())
			if err != nil {
				t.Fatalf("example %d (%s): %v", tc.Example, parserName, err)
			}
			var decoded any
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("example %d (%s): invalid JSON: %v", tc.Example, parserName, err)
			}

			// Source-on and source-off must both be deterministic and
			// source-off never carries offsets.
			outNoSource, err := ast.MarshalCanonicalToBytes(doc, source)
			if err != nil {
				t.Fatalf("example %d (%s): %v", tc.Example, parserName, err)
			}
			if bytes.Contains(outNoSource, []byte(`"start"`)) {
				t.Fatalf("example %d (%s): source offsets leaked", tc.Example, parserName)
			}
		}
	}
}
