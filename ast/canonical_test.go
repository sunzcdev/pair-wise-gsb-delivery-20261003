package ast_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
)

var updateCanonicalGolden = flag.Bool("update", false, "update canonical golden files")

type canonicalCase struct {
	name       string
	source     string
	extensions []parser.Extension
	opts       []ast.CanonicalOption
}

func canonicalCases() []canonicalCase {
	return []canonicalCase{
		{
			name:   "code-blocks",
			source: "    indented line\n\n```go\nfenced line 1\nfenced line 2\n```\n",
		},
		{
			name:   "entities",
			source: "a &amp; b &#35; &AElig;\n",
		},
		{
			name:       "generated-text",
			source:     "hello \"world\" -- x...\n",
			extensions: []parser.Extension{extension.NewTypographerParser()},
		},
		{
			name:       "table",
			source:     "| left | center | right |\n|:-----|:------:|------:|\n| a    | b      | c     |\n",
			extensions: []parser.Extension{extension.NewTableParser()},
		},
		{
			name:   "gfm-mix",
			source: "# H\n\n- [x] done\n- [ ] todo\n\n~~strike~~\n\n[^1]\n\n[^1]: note\n",
			extensions: []parser.Extension{
				extension.NewStrikethroughParser(),
				extension.NewTaskListItemParser(),
				extension.NewFootnoteParser(),
			},
		},
		{
			name: "links",
			source: "3. [one][r]\n4. two\n\n" +
				"[r]: https://example.com \"title\"\n\n" +
				"<https://auto.link> ![img](https://x/y.png)\n",
		},
		{
			name:   "custom-extension",
			source: "plain paragraph\n",
			opts: []ast.CanonicalOption{
				ast.WithCanonicalNodeAdapters(map[ast.NodeKind]ast.CanonicalNodeAdapter{
					customCanonicalKind: func(node ast.Node, _ []byte) ([]ast.CanonicalProperty, error) {
						n := node.(*customCanonicalNode)
						return []ast.CanonicalProperty{
							{Name: "marker", Value: n.marker},
							{Name: "count", Value: n.count},
						}, nil
					},
				}),
			},
		},
		{
			name:   "with-source",
			source: "# heading\n\npara *em*\n",
			opts:   []ast.CanonicalOption{ast.WithCanonicalSource()},
		},
	}
}

func marshalCanonical(t *testing.T, tc canonicalCase) []byte {
	t.Helper()
	doc := parser.New(parser.WithExtensions(tc.extensions...)).Parse([]byte(tc.source))
	if tc.name == "custom-extension" {
		doc.AppendChild(newCustomCanonicalNode("z", 7))
	}
	out, err := ast.MarshalCanonicalToBytes(doc, []byte(tc.source), tc.opts...)
	if err != nil {
		t.Fatalf("MarshalCanonicalToBytes: %v", err)
	}
	return out
}

// TestCanonicalGolden snapshots the canonical output for code blocks,
// entities, generated text, tables and custom extension nodes.
func TestCanonicalGolden(t *testing.T) {
	dir := filepath.Join("testdata", "canonical")
	for _, tc := range canonicalCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := marshalCanonical(t, tc)

			// Stdlib must be able to read the output.
			var decoded any
			if err := json.Unmarshal(got, &decoded); err != nil {
				t.Fatalf("output is not readable by encoding/json: %v\n%s", err, got)
			}

			path := filepath.Join(dir, tc.name+".golden.json")
			if *updateCanonicalGolden {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("golden mismatch (-want +got):\n%s\n-- got --\n%s", string(want), string(got))
			}
		})
	}
}

// TestCanonicalDeterministic checks repeated runs produce identical bytes.
func TestCanonicalDeterministic(t *testing.T) {
	for _, tc := range canonicalCases() {
		t.Run(tc.name, func(t *testing.T) {
			var first []byte
			for range 10 {
				got := marshalCanonical(t, tc)
				if first == nil {
					first = got
					continue
				}
				if !bytes.Equal(first, got) {
					t.Fatalf("non-deterministic output:\n%s\nvs\n%s", first, got)
				}
			}
		})
	}
}

// TestCanonicalConcurrent exercises independent exports from multiple
// goroutines; the canonical writer is per-export and must not share buffers.
func TestCanonicalConcurrent(t *testing.T) {
	source := []byte("# H\n\n`a\nb`\n\n- [x] y\n")
	p := parser.New(
		parser.WithExtensions(
			extension.NewStrikethroughParser(),
			extension.NewTaskListItemParser(),
			extension.NewTableParser(),
		),
	)
	doc := p.Parse(source)
	done := make(chan []byte, 8)
	for range 8 {
		go func() {
			out, err := ast.MarshalCanonicalToBytes(doc, source, ast.WithCanonicalSource())
			if err != nil {
				done <- []byte("ERR:" + err.Error())
				return
			}
			done <- out
		}()
	}
	var first []byte
	for range 8 {
		got := <-done
		if bytes.HasPrefix(got, []byte("ERR:")) {
			t.Fatalf("%s", got)
		}
		if first == nil {
			first = got
		} else if !bytes.Equal(first, got) {
			t.Fatal("concurrent exports diverged")
		}
	}
}

// TestCanonicalUnknownNode ensures extension nodes cannot be silently
// exported as empty objects.
func TestCanonicalUnknownNode(t *testing.T) {
	source := []byte("plain paragraph\n")
	doc := parser.New().Parse(source)
	doc.AppendChild(newCustomCanonicalNode("m", 1))

	_, err := ast.ExportCanonical(doc, source)
	if err == nil {
		t.Fatal("expected error for node without adapter")
	}
	if !errors.Is(err, ast.ErrUnsupportedCanonicalNode) {
		t.Fatalf("expected ErrUnsupportedCanonicalNode, got %v", err)
	}
	var exportErr *ast.CanonicalExportError
	if !errors.As(err, &exportErr) {
		t.Fatalf("expected *CanonicalExportError, got %T", err)
	}
	if exportErr.Kind != "CustomCanonical" || len(exportErr.Path) != 1 || exportErr.Path[0] != "Document" {
		t.Fatalf("unexpected error context: %+v", exportErr)
	}

	// A deeply nested unknown node must report the full kind chain.
	deepSource := []byte("> - x\n")
	deep := parser.New().Parse(deepSource)
	blockquote := deep.FirstChild()
	list := blockquote.FirstChild()
	listItem := list.FirstChild()
	listItem.AppendChild(newCustomCanonicalNode("deep", 1))
	_, err = ast.ExportCanonical(deep, deepSource)
	if err == nil {
		t.Fatal("expected error for deep unknown node")
	}
	if !errors.Is(err, ast.ErrUnsupportedCanonicalNode) {
		t.Fatalf("expected ErrUnsupportedCanonicalNode, got %v", err)
	}
	var deepErr *ast.CanonicalExportError
	if !errors.As(err, &deepErr) {
		t.Fatalf("expected *CanonicalExportError, got %T", err)
	}
	wantPath := []string{"Document", "Blockquote", "List", "ListItem"}
	if !sliceEqual(deepErr.Path, wantPath) {
		t.Fatalf("unexpected deep path: %+v", deepErr.Path)
	}

	out, err := ast.MarshalCanonicalToBytes(doc, source, ast.WithCanonicalNodeAdapters(map[ast.NodeKind]ast.CanonicalNodeAdapter{
		customCanonicalKind: func(node ast.Node, _ []byte) ([]ast.CanonicalProperty, error) {
			n := node.(*customCanonicalNode)
			return []ast.CanonicalProperty{
				{Name: "marker", Value: n.marker},
				{Name: "count", Value: n.count},
			}, nil
		},
	}))
	if err != nil {
		t.Fatalf("export with adapter: %v", err)
	}
	var decoded struct {
		Root struct {
			Children []map[string]any `json:"children"`
		} `json:"root"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, child := range decoded.Root.Children {
		if child["type"] == "CustomCanonical" {
			found = true
			if child["marker"] != "m" {
				t.Fatalf("marker = %v", child["marker"])
			}
		}
	}
	if !found {
		t.Fatalf("custom node missing from output:\n%s", out)
	}
}

// TestCanonicalAdapterErrors checks that faulty adapters surface concrete
// errors instead of empty output.
func TestCanonicalAdapterErrors(t *testing.T) {
	source := []byte("plain paragraph\n")

	build := func(adapter ast.CanonicalNodeAdapter) error {
		doc := parser.New().Parse(source)
		doc.AppendChild(newCustomCanonicalNode("m", 1))
		_, err := ast.ExportCanonical(doc, source, ast.WithCanonicalNodeAdapters(map[ast.NodeKind]ast.CanonicalNodeAdapter{
			customCanonicalKind: adapter,
		}))
		return err
	}

	if err := build(func(ast.Node, []byte) ([]ast.CanonicalProperty, error) {
		return []ast.CanonicalProperty{{Name: "start", Value: 1}}, nil
	}); err == nil {
		t.Fatal("expected reserved-key error")
	}

	if err := build(func(ast.Node, []byte) ([]ast.CanonicalProperty, error) {
		return []ast.CanonicalProperty{{Name: "ch", Value: make(chan int)}}, nil
	}); err == nil {
		t.Fatal("expected unsupported-value error")
	}

	if err := build(func(ast.Node, []byte) ([]ast.CanonicalProperty, error) {
		return nil, errors.New("boom")
	}); err == nil || !contains(err.Error(), "boom") {
		t.Fatalf("expected adapter error to be wrapped, got %v", err)
	}
}

// TestCanonicalSourceOffWrapping verifies that, without source ranges,
// inputs that differ only in line-wrapping positions export the same
// semantic tree whenever their visible content is identical (code spans
// normalize newlines to spaces).
func TestCanonicalSourceOffWrapping(t *testing.T) {
	oneLine := []byte("`foo bar baz`\n")
	wrapped := []byte("`foo bar\nbaz`\n")

	out1 := mustCanonical(t, oneLine)
	out2 := mustCanonical(t, wrapped)
	if !bytes.Equal(out1, out2) {
		t.Fatalf("expected equal canonical output without source ranges:\n%s\nvs\n%s", out1, out2)
	}

	// The multiline value keeps two ordered source segments internally;
	// export its tree to confirm the segments are covered.
	docWrapped := parser.New().Parse(wrapped)
	exportedWrapped, err := ast.ExportCanonical(docWrapped, wrapped, ast.WithCanonicalSource())
	if err != nil {
		t.Fatal(err)
	}
	csWrapped := exportedWrapped.Root.Children[0].Children[0]
	if csWrapped.Kind != "CodeSpan" || csWrapped.Start != 1 || csWrapped.Stop != 12 {
		t.Fatalf("unexpected multiline code span range: %+v", csWrapped)
	}

	// With source ranges, genuinely different inputs remain distinguishable.
	withRange1 := mustCanonical(t, oneLine, ast.WithCanonicalSource())
	withRange3 := mustCanonical(t, []byte("xx `foo bar baz`\n"), ast.WithCanonicalSource())
	if bytes.Equal(withRange1, withRange3) {
		t.Fatal("expected different canonical output with source ranges")
	}

	// Soft line breaks are a real semantic distinction (visible as a line
	// break in wrapping renderers), so emphasis split over two lines must
	// not be folded into the single-line form.
	emSoft := mustCanonical(t, []byte("*foo\nbar*\n"))
	emOne := mustCanonical(t, []byte("*foo bar*\n"))
	if bytes.Equal(emSoft, emOne) {
		t.Fatal("soft line break inside emphasis must remain visible")
	}
}

// TestCanonicalSourceRanges checks that ranges are present only when asked.
func TestCanonicalSourceRanges(t *testing.T) {
	source := []byte("# heading\n\npara\n")
	off := mustCanonical(t, source)
	if bytes.Contains(off, []byte(`"start"`)) || bytes.Contains(off, []byte(`"stop"`)) {
		t.Fatalf("source ranges must be omitted by default:\n%s", off)
	}

	on := mustCanonical(t, source, ast.WithCanonicalSource())
	if !bytes.Contains(on, []byte(`"start"`)) || !bytes.Contains(on, []byte(`"stop"`)) {
		t.Fatalf("source ranges must be present when enabled:\n%s", on)
	}

	doc := parser.New().Parse(source)
	canonicalDoc, err := ast.ExportCanonical(doc, source, ast.WithCanonicalSource())
	if err != nil {
		t.Fatal(err)
	}
	heading := canonicalDoc.Root.Children[0]
	if heading.Kind != "Heading" || heading.Start != 2 || !heading.HasStop {
		t.Fatalf("unexpected heading range: %+v", heading)
	}
	paragraph := canonicalDoc.Root.Children[1]
	if paragraph.Kind != "Paragraph" || paragraph.Start != 11 || !paragraph.HasStop {
		t.Fatalf("unexpected paragraph range: %+v", paragraph)
	}
}

// TestCanonicalAttributeOrder verifies attributes are emitted in document
// order, not map iteration order.
func TestCanonicalAttributeOrder(t *testing.T) {
	doc := parser.New().Parse([]byte("text\n"))
	p := doc.FirstChild()
	p.SetAttribute("zebra", mustOwnedValue("1"))
	p.SetAttribute("apple", mustOwnedValue("2"))
	p.SetAttribute("mango", mustOwnedValue("3"))

	for range 10 {
		out, err := ast.MarshalCanonicalToBytes(doc, []byte("text\n"))
		if err != nil {
			t.Fatal(err)
		}
		z := bytes.Index(out, []byte(`"name": "zebra"`))
		a := bytes.Index(out, []byte(`"name": "apple"`))
		m := bytes.Index(out, []byte(`"name": "mango"`))
		if z < 0 || a < 0 || m < 0 || !(z < a && a < m) {
			t.Fatalf("attributes not in document order:\n%s", out)
		}
	}
}

// TestCanonicalVersionAndText verifies the version envelope and that text
// fields are materialized to actual content (entities decoded).
func TestCanonicalVersionAndText(t *testing.T) {
	out := mustCanonical(t, []byte("&AElig;\n"))
	var envelope struct {
		Version int            `json:"version"`
		Root    map[string]any `json:"root"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != ast.CanonicalFormatVersion {
		t.Fatalf("version = %d", envelope.Version)
	}
	textNode := envelope.Root["children"].([]any)[0].(map[string]any)["children"].([]any)[0].(map[string]any)
	if textNode["value"] != "\u00c6" {
		t.Fatalf("entity not materialized: %v", textNode["value"])
	}
	if bytes.Contains(out, []byte("0x")) || bytes.Contains(out, []byte("parent")) {
		t.Fatalf("output leaks pointers/addresses:\n%s", out)
	}
}

func mustCanonical(t *testing.T, source []byte, opts ...ast.CanonicalOption) []byte {
	t.Helper()
	doc := parser.New().Parse(source)
	out, err := ast.MarshalCanonicalToBytes(doc, source, opts...)
	if err != nil {
		t.Fatalf("MarshalCanonicalToBytes: %v", err)
	}
	return out
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
