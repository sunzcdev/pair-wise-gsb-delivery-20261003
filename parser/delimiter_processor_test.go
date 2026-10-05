package parser_test

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/util"
)

// markNode is a minimal inline span node produced by the demo processors.
type markNode struct {
	ast.BaseInline
	tag string
}

var kindMarkA = ast.NewNodeKind("MarkA")
var kindMarkB = ast.NewNodeKind("MarkB")
var kindMarkDouble = ast.NewNodeKind("MarkDouble")

func (n *markNode) Kind() ast.NodeKind {
	switch n.tag {
	case "b":
		return kindMarkB
	case "double":
		return kindMarkDouble
	default:
		return kindMarkA
	}
}

func (n *markNode) Dump(_ []byte) *ast.NodeDump {
	return ast.NewNodeDump(n, map[string]any{"Tag": n.tag})
}

// markProcessor is a minimal delimiter processor that wraps matched content in
// markNode spans.
type markProcessor struct {
	name string
	tag  string
	char byte
}

func (p *markProcessor) IsDelimiter(b byte) bool {
	return b == p.char
}

func (p *markProcessor) CanOpenCloser(opener, closer *parser.Delimiter) bool {
	return opener.Char == closer.Char
}

func (p *markProcessor) OnMatch(_ int) ast.Node {
	return &markNode{tag: p.tag}
}

func newMarkParser(name, tag string, char byte, opts ...parser.DelimiterProcessorOption) parser.Option {
	r := parser.NewDelimiterProcessorRegistration(
		name,
		&markProcessor{name: name, tag: tag, char: char},
		[]byte{char},
		opts...,
	)
	return parser.WithDelimiterProcessors(r)
}

// unnamedProcessor does not implement DelimiterProcessorNamer, exercising the
// compatibility default name for extensions that predate named processors.
type unnamedProcessor struct {
	char byte
}

func (p *unnamedProcessor) IsDelimiter(b byte) bool { return b == p.char }
func (p *unnamedProcessor) CanOpenCloser(_, _ *parser.Delimiter) bool {
	return true
}
func (p *unnamedProcessor) OnMatch(_ int) ast.Node { return &markNode{tag: "a"} }

type unnamedProcessorTwo struct {
	char byte
}

func (p *unnamedProcessorTwo) IsDelimiter(b byte) bool { return b == p.char }
func (p *unnamedProcessorTwo) CanOpenCloser(_, _ *parser.Delimiter) bool {
	return true
}
func (p *unnamedProcessorTwo) OnMatch(_ int) ast.Node { return &markNode{tag: "b"} }

// TestDelimiterProcessorCompatibilityDefaultName verifies a registration
// without an explicit name and a processor without DelimiterProcessorNamer
// still gets a stable, type-derived default name in diagnostics.
func TestDelimiterProcessorCompatibilityDefaultName(t *testing.T) {
	var diagnostics []parser.DelimiterDiagnostic
	_ = renderWithMarks(t, "!x!",
		parser.WithDelimiterDiagnostics(func(d parser.DelimiterDiagnostic) {
			diagnostics = append(diagnostics, d)
		}),
		parser.WithDelimiterProcessors(
			parser.NewDelimiterProcessorRegistration("", &unnamedProcessor{char: '!'},
				[]byte{'!'}, parser.WithDelimiterProcessorPriority(400)),
			parser.NewDelimiterProcessorRegistration("", &unnamedProcessorTwo{char: '!'},
				[]byte{'!'}, parser.WithDelimiterProcessorPriority(600)),
		),
	)
	if len(diagnostics) != 1 {
		t.Fatalf("expected one diagnostic, got %v", diagnostics)
	}
	name := diagnostics[0].Registration.Name()
	if !strings.HasSuffix(name, ".unnamedProcessorTwo") {
		t.Fatalf("expected type-derived default name ending with .unnamedProcessorTwo, got %q", name)
	}
	shadower := diagnostics[0].ShadowedBy.Name()
	if !strings.HasSuffix(shadower, ".unnamedProcessor") {
		t.Fatalf("expected shadower default name ending with .unnamedProcessor, got %q", shadower)
	}
}

func renderWithMarks(t *testing.T, source string, opts ...parser.Option) string {
	t.Helper()
	allOpts := append([]parser.Option{}, opts...)
	p := parser.New(allOpts...)
	r := html.New(html.WithUnsafe(), html.WithNodeRenderers(
		map[ast.NodeKind]html.NodeRenderer{
			kindMarkA:      markRenderer("mark-a"),
			kindMarkB:      markRenderer("mark-b"),
			kindMarkDouble: markRenderer("mark-double"),
		},
	))
	var buf strings.Builder
	src := util.StringToReadOnlyBytes(source)
	if err := r.Render(&buf, src, p.Parse(src)); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func markRenderer(tag string) html.NodeRenderer {
	return html.NodeRendererFunc(func(w io.Writer, _ []byte,
		_ ast.Node, entering bool, _ renderer.Context) (ast.WalkStatus, error) {
		if entering {
			_, err := io.WriteString(w, "<"+tag+">")
			return ast.WalkContinue, err
		}
		_, err := io.WriteString(w, "</"+tag+">")
		return ast.WalkContinue, err
	})
}

// TestDelimiterProcessorPriority shows that when two minimal extensions
// contend for the same character, the lower-priority value wins.
func TestDelimiterProcessorPriority(t *testing.T) {
	// b has the lower priority value, so it wins over a.
	got := renderWithMarks(t, ":x:",
		newMarkParser("colon-a", "a", ':', parser.WithDelimiterProcessorPriority(600)),
		newMarkParser("colon-b", "b", ':', parser.WithDelimiterProcessorPriority(400)),
	)
	if want := "<p><mark-b>x</mark-b></p>\n"; got != want {
		t.Fatalf("priority: expected %q, got %q", want, got)
	}
}

// TestDelimiterProcessorTie shows the tie-break rule: equal priorities keep
// registration order, so the first registration wins.
func TestDelimiterProcessorTie(t *testing.T) {
	got := renderWithMarks(t, ":x:",
		newMarkParser("colon-first", "a", ':', parser.WithDelimiterProcessorPriority(500)),
		newMarkParser("colon-second", "b", ':', parser.WithDelimiterProcessorPriority(500)),
	)
	if want := "<p><mark-a>x</mark-a></p>\n"; got != want {
		t.Fatalf("tie: expected first registration to win, got %q (want %q)", got, want)
	}
}

// TestDelimiterProcessorTieAcrossTriggers verifies the tie-break survives
// registry indexing: two multi-trigger registrations with equal priority keep
// registration order regardless of trigger byte values.
func TestDelimiterProcessorTieAcrossTriggers(t *testing.T) {
	makeMulti := func(name, tag string) parser.Option {
		r := parser.NewDelimiterProcessorRegistration(
			name,
			&markProcessor{name: name, tag: tag, char: '='},
			[]byte{'=', ':'},
			parser.WithDelimiterProcessorPriority(500),
		)
		return parser.WithDelimiterProcessors(r)
	}
	got := renderWithMarks(t, "=x=",
		makeMulti("multi-first", "a"),
		makeMulti("multi-second", "b"),
	)
	if want := "<p><mark-a>x</mark-a></p>\n"; got != want {
		t.Fatalf("cross-trigger tie: expected first registration to win, got %q", got)
	}
}

// TestDelimiterProcessorLengthCoexistence shows that processors sharing a
// character but accepting different run lengths coexist: the general
// processor handles single runs, the length-restricted processor handles
// double runs, and neither shadows the other.
func TestDelimiterProcessorLengthCoexistence(t *testing.T) {
	var diagnostics []parser.DelimiterDiagnostic
	got := renderWithMarks(t, ":a: and ::b::",
		parser.WithDelimiterDiagnostics(func(d parser.DelimiterDiagnostic) {
			diagnostics = append(diagnostics, d)
		}),
		// Single-run specialist and double-run specialist: neither length
		// window covers the other, so they coexist on ':'.
		newMarkParser("colon-single", "a", ':',
			parser.WithDelimiterProcessorPriority(500),
			parser.WithDelimiterProcessorLengths(1, 1)),
		newMarkParser("colon-double", "double", ':',
			parser.WithDelimiterProcessorPriority(501),
			parser.WithDelimiterProcessorLengths(2, 2)),
	)
	if want := "<p><mark-a>a</mark-a> and <mark-double>b</mark-double></p>\n"; got != want {
		t.Fatalf("coexistence: expected %q, got %q", want, got)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("length-distinct processors must coexist without diagnostics, got %v", diagnostics)
	}
}

// TestDelimiterProcessorShadowDiagnostic verifies the build-time diagnostic
// for a fully shadowed duplicate configuration: it names both processors and
// locates the shadowed registration.
func TestDelimiterProcessorShadowDiagnostic(t *testing.T) {
	var diagnostics []parser.DelimiterDiagnostic
	_ = renderWithMarks(t, ":x:",
		parser.WithDelimiterDiagnostics(func(d parser.DelimiterDiagnostic) {
			diagnostics = append(diagnostics, d)
		}),
		newMarkParser("colon-winner", "a", ':', parser.WithDelimiterProcessorPriority(400)),
		newMarkParser("colon-shadowed", "b", ':', parser.WithDelimiterProcessorPriority(600)),
	)
	if len(diagnostics) != 1 {
		t.Fatalf("expected exactly one shadow diagnostic, got %d: %v", len(diagnostics), diagnostics)
	}
	d := diagnostics[0]
	if d.Severity != parser.DelimiterDiagnosticWarning {
		t.Fatalf("expected warning severity, got %s", d.Severity)
	}
	if d.Registration == nil || d.Registration.Name() != "colon-shadowed" {
		t.Fatalf("diagnostic should target colon-shadowed, got %+v", d.Registration)
	}
	if d.ShadowedBy == nil || d.ShadowedBy.Name() != "colon-winner" {
		t.Fatalf("diagnostic should name colon-winner as shadower, got %+v", d.ShadowedBy)
	}
	if d.Trigger != ':' {
		t.Fatalf("expected trigger ':', got %q", string(d.Trigger))
	}
	dir, file := filepath.Split(d.Location())
	if filepath.Base(dir) != "parser" ||
		!strings.HasPrefix(file, "delimiter_processor_test.go:") {
		t.Fatalf("expected locatable parser/delimiter_processor_test.go:line, got %q", d.Location())
	}
	parts := strings.Split(d.Location(), ":")
	if parts[len(parts)-1] == "" || parts[len(parts)-1] == "0" {
		t.Fatalf("expected locatable file:line, got %q", d.Location())
	}
	if !strings.Contains(d.Message, "colon-shadowed") ||
		!strings.Contains(d.Message, "colon-winner") {
		t.Fatalf("message should name both processors, got %q", d.Message)
	}
}

// TestDelimiterProcessorTraceDoesNotChangeAST verifies tracing is
// observation-only: enabling it cannot change the parsed AST.
func TestDelimiterProcessorTraceDoesNotChangeAST(t *testing.T) {
	source := []byte(":a: **b** _c_ ::d::")
	opts := []parser.Option{
		newMarkParser("colon-general", "a", ':', parser.WithDelimiterProcessorPriority(500)),
		newMarkParser("colon-double", "double", ':',
			parser.WithDelimiterProcessorPriority(501),
			parser.WithDelimiterProcessorLengths(2, 2)),
	}
	withoutTrace := parser.New(opts...).Parse(append([]byte(nil), source...))
	var events []parser.DelimiterTraceEvent
	withTrace := parser.New(opts...).Parse(
		append([]byte(nil), source...),
		parser.WithDelimiterTrace(func(e parser.DelimiterTraceEvent) {
			events = append(events, e)
		}),
	)
	if astToText(withoutTrace, source) != astToText(withTrace, source) {
		t.Fatalf("tracing changed the AST")
	}
	if len(events) == 0 {
		t.Fatal("expected trace events")
	}
	for _, e := range events {
		if strings.Contains(e.String(), string(source)) {
			t.Fatalf("trace event must not embed source text: %s", e)
		}
	}
}

func astToText(n ast.Node, source []byte) string {
	var sb strings.Builder
	var walk func(ast.Node)
	walk = func(node ast.Node) {
		sb.WriteString(node.Kind().String())
		sb.WriteString("{")
		if t, ok := node.(*ast.Text); ok {
			sb.WriteString(t.Value.Value(source))
		}
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			walk(c)
		}
		sb.WriteString("}")
	}
	walk(n)
	return sb.String()
}
