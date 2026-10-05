package astjson_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/astjson"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/text"
)

func textMultiLine(s string) text.MultiLineValue {
	return text.NewMultiLineValueFromString(s, text.IdentityDecoder)
}

func export(t *testing.T, source string, opts ...astjson.Option) []byte {
	t.Helper()
	doc := parser.New().Parse([]byte(source))
	b, err := astjson.NewExporter(opts...).Marshal(doc, []byte(source))
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	return b
}

func TestFormatEnvelope(t *testing.T) {
	out := export(t, "hello\n")
	var envelope struct {
		Version int             `json:"version"`
		Tree    json.RawMessage `json:"tree"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("output is not readable by encoding/json: %v\n%s", err, out)
	}
	if envelope.Version != astjson.FormatVersion || astjson.FormatVersion != 1 {
		t.Fatalf("unexpected format version: %d", envelope.Version)
	}
	if !bytes.Contains(out, []byte(`"type": "Document"`)) {
		t.Fatalf("missing document type: %s", out)
	}
}

func TestExportNodeAndRegistryOption(t *testing.T) {
	doc := parser.New().Parse([]byte("x\n"))

	registry := astjson.NewRegistry()
	exp := astjson.NewExporter(astjson.WithRegistry(registry))
	if _, err := exp.ExportNode(doc, []byte("x\n")); !errors.Is(err, astjson.ErrUnknownNode) {
		t.Fatalf("empty registry must report unknown nodes, got %v", err)
	}

	raw, err := astjson.NewExporter().ExportNode(doc, []byte("x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("ExportNode result is not valid JSON: %s", raw)
	}
	if bytes.Contains(raw, []byte("\n")) {
		t.Fatalf("ExportNode must return compact JSON, got: %s", raw)
	}
}

func TestContextAccessorsAndFailingWriter(t *testing.T) {
	doc := parser.New().Parse([]byte("x\n"))
	exp := astjson.NewExporter(astjson.WithSourceRange(true))

	var buf bytes.Buffer
	if err := exp.WriteTo(&buf, doc, []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"position"`)) {
		t.Fatalf("source-range output must include positions: %s", buf.Bytes())
	}

	fw := &failingWriter{}
	if err := exp.WriteTo(fw, doc, []byte("x\n")); err == nil {
		t.Fatal("expected WriteTo to propagate writer errors")
	}
}

type failingWriter struct{}

func (failingWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestSnapshotCodeSpansEntitiesAndGeneratedText(t *testing.T) {
	// Covers fixed code spans (multiline, normalized), entities (decoded into
	// actual content), generated text (owned mailto: destination), code
	// blocks, raw HTML and link reference definitions.
	source := strings.Join([]string{
		"[ref]: /url \"the title\"",
		"",
		"```go",
		"a := 1 < 2 && 3",
		"```",
		"",
		"`a",
		"b` &amp; &#65; \\*x*",
		"",
		"<user@example.com>",
		"",
		"see [ref]",
		"",
	}, "\n")
	withSource := export(t, source, astjson.WithSourceRange(true))
	withoutSource := export(t, source)

	goldenCheck(t, "snapshot_code_span_entity.golden.json", withSource)
	goldenCheck(t, "snapshot_code_span_entity_nosource.golden.json", withoutSource)
}

func TestOutputIsByteStableAcrossRuns(t *testing.T) {
	source := "# Title\n\n- a\n- b\n\n```\nx\n```\n"
	doc := parser.New().Parse([]byte(source))
	exp := astjson.NewExporter(astjson.WithSourceRange(true))

	first, err := exp.Marshal(doc, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		again, err := exp.Marshal(doc, []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("export is not byte-stable on run %d", i)
		}
	}
	// WriteTo must end with exactly one trailing newline.
	var buf bytes.Buffer
	if err := exp.WriteTo(&buf, doc, []byte(source)); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("}\n")) || bytes.HasSuffix(buf.Bytes(), []byte("\n\n")) {
		t.Fatalf("WriteTo must end with a single newline, got: %q", buf.Bytes()[len(buf.Bytes())-4:])
	}
}

func TestEquivalentWrappingProducesEqualOutputWithoutSource(t *testing.T) {
	// A multiline code span normalizes the newline to a space, and text
	// entities decode to literal characters. Both pairs parse to identical
	// materialized content even though their source byte offsets differ.
	cases := []struct {
		name string
		a    string
		b    string
	}{
		{
			name: "multiline code span vs space",
			a:    "`alpha\nbeta`\n",
			b:    "`alpha beta`\n",
		},
		{
			name: "backslash escape vs named entity",
			a:    "a\\&b\n",
			b:    "a&amp;b\n",
		},
		{
			name: "different entity encodings of the same text",
			a:    "&amp;&#65;\n",
			b:    "&#38;A\n",
		},
		{
			name: "entity vs literal",
			a:    "&amp;\n",
			b:    "&\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aOut := export(t, tc.a)
			bOut := export(t, tc.b)
			if !bytes.Equal(aOut, bOut) {
				t.Fatalf("expected byte-equal output without source ranges\na:\n%s\nb:\n%s", aOut, bOut)
			}

			// With source ranges the differing input layout must remain
			// visible so cache debugging can distinguish them.
			aSrc := export(t, tc.a, astjson.WithSourceRange(true))
			bSrc := export(t, tc.b, astjson.WithSourceRange(true))
			if bytes.Equal(aSrc, bSrc) {
				t.Fatalf("source-range output unexpectedly identical for:\n%s\nand\n%s", tc.a, tc.b)
			}
		})
	}
}

func TestDistinctNodeTypesAreNotCollapsed(t *testing.T) {
	// *x* and **x** share the visible text "x" but must remain different node
	// types.
	emphasis := export(t, "*x*\n")
	strong := export(t, "**x**\n")
	if bytes.Equal(emphasis, strong) {
		t.Fatalf("Emphasis and Strong must not collapse to the same node type")
	}
	if !bytes.Contains(emphasis, []byte(`"type": "Emphasis"`)) {
		t.Fatalf("missing Emphasis node: %s", emphasis)
	}
	if !bytes.Contains(strong, []byte(`"type": "Strong"`)) {
		t.Fatalf("missing Strong node: %s", strong)
	}
}

type unknownNode struct {
	ast.BaseInline
}

var unknownNodeKind = ast.NewNodeKind("astjson_test.UnknownNode")

func (n *unknownNode) Kind() ast.NodeKind { return unknownNodeKind }
func (n *unknownNode) Dump(_ []byte) *ast.NodeDump {
	return ast.NewNodeDump(n, nil)
}

func TestUnknownNodeReturnsConcreteError(t *testing.T) {
	doc := ast.NewDocument()
	para := ast.NewParagraph()
	para.AppendChild(&unknownNode{})
	doc.AppendChild(para)

	_, err := astjson.NewExporter().Marshal(doc, nil)
	if err == nil {
		t.Fatal("expected an error for an unknown node kind")
	}
	if !errors.Is(err, astjson.ErrUnknownNode) {
		t.Fatalf("expected errors.Is(err, ErrUnknownNode), got %v", err)
	}
	var concrete *astjson.UnknownNodeError
	if !errors.As(err, &concrete) {
		t.Fatalf("expected *UnknownNodeError, got %T (%v)", err, err)
	}
	if !strings.Contains(err.Error(), "astjson_test.UnknownNode") {
		t.Fatalf("error must name the unknown kind: %v", err)
	}

	// Registering an adapter resolves the error, and an adapter returning an
	// error propagates it as-is rather than emitting an empty object.
	customErr := errors.New("boom")
	exp := astjson.NewExporter()
	kind := doc.FirstChild().FirstChild().Kind()
	exp.Register(kind, astjson.AdapterFunc(func(_ ast.Node, _ *astjson.Context) ([]astjson.Property, error) {
		return nil, customErr
	}))
	if _, err := exp.Marshal(doc, nil); !errors.Is(err, customErr) {
		t.Fatalf("expected adapter error to propagate, got %v", err)
	}

	exp.Register(kind, astjson.AdapterFunc(func(_ ast.Node, c *astjson.Context) ([]astjson.Property, error) {
		return []astjson.Property{astjson.Prop("custom", c.String("ok"))}, nil
	}))
	b, err := exp.Marshal(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"type": "astjson_test.UnknownNode"`)) ||
		!bytes.Contains(b, []byte(`"custom": "ok"`)) {
		t.Fatalf("registered adapter output missing: %s", b)
	}
}

func TestAttributesKeepOrderAndValues(t *testing.T) {
	// Attributes are emitted in attachment order with materialized values.
	doc := ast.NewDocument()
	para := ast.NewParagraph()
	doc.AppendChild(para)
	para.SetAttribute("z-id", textMultiLine("zzz"))
	para.SetAttribute("a-class", textMultiLine("alpha"))

	b, err := astjson.NewExporter().Marshal(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	z := bytes.Index(b, []byte(`"z-id"`))
	a := bytes.Index(b, []byte(`"a-class"`))
	if z < 0 || a < 0 || z > a {
		t.Fatalf("attributes must keep attachment order, got:\n%s", b)
	}
	if !json.Valid(b) {
		t.Fatal("invalid json")
	}
}

func goldenCheck(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with UPDATE_GOLDEN=1 to create)", name, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", name, want, got)
	}
}
