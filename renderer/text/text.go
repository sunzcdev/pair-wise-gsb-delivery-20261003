// Package text implements a renderer that outputs the visible plain text of a
// Markdown document.
//
// Unlike rendering the document to HTML and then stripping tags, this renderer
// walks the AST directly, so block boundaries are preserved:
//
//   - Paragraphs and other top level blocks are separated by a blank line.
//   - Adjacent items of tight lists are separated by a single newline.
//   - Code blocks keep their internal whitespace verbatim.
//   - Links render their label and, optionally, the destination.
//   - Images render their alternative text and, optionally, the destination.
//   - Raw HTML is never parsed; it is handled by an explicit [RawHTMLMode].
//
// Entity references and backslash escapes are decoded exactly once, using the
// decoder bound to each AST value by the parser.
//
// Node kinds that are not handled by the renderer (e.g. extension nodes such
// as tables or footnotes) are walked through transparently, so the text of
// their children is emitted instead of dropping the whole subtree. Custom
// renderers for such node kinds can be registered with [WithNodeRenderer].
package text

import (
	"io"
	"strconv"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/renderer"
	"github.com/yuin/goldmark/v2/util"
)

// Renderer is a renderer that renders AST nodes as plain text.
type Renderer = renderer.Renderer[io.Writer]

// NodeRenderer renders a single AST node kind as plain text.
type NodeRenderer = renderer.NodeRenderer[io.Writer]

// NodeRendererDecorator decorates a NodeRenderer.
type NodeRendererDecorator = renderer.NodeRendererDecorator[io.Writer]

// NodeRendererFunc adapts f into a NodeRenderer.
func NodeRendererFunc(f func(w io.Writer, source []byte,
	n ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error)) NodeRenderer {
	return renderer.NodeRendererFunc(f)
}

// Extension extends plain text renderers.
type Extension = renderer.Extension[Config]

// Option is a functional option for plain text renderers.
type Option = renderer.Option[Config]

// WithNodeRenderers sets node renderers for the given node kinds.
func WithNodeRenderers(nodeRenderers map[ast.NodeKind]NodeRenderer) Option {
	return renderer.WithNodeRenderers[io.Writer, Config](nodeRenderers)
}

// WithNodeRenderer sets a node renderer for the given node kind.
func WithNodeRenderer(kind ast.NodeKind, nodeRenderer NodeRenderer) Option {
	return renderer.WithNodeRenderer[io.Writer, Config](kind, nodeRenderer)
}

// WithNodeRendererDecorators sets node renderer decorators.
func WithNodeRendererDecorators(decorators map[ast.NodeKind]NodeRendererDecorator) Option {
	return renderer.WithNodeRendererDecorators[io.Writer, Config](decorators)
}

// WithExtensions adds extensions.
func WithExtensions(extensions ...Extension) Option {
	return renderer.WithExtensions[io.Writer](extensions...)
}

// SoftLineBreakMode determines how soft line breaks inside a paragraph are
// rendered.
type SoftLineBreakMode int

const (
	// SoftLineBreakSpace renders soft line breaks as a single space.
	// This is the default and matches how the source is displayed as text.
	SoftLineBreakSpace SoftLineBreakMode = iota

	// SoftLineBreakNewline renders soft line breaks as a newline.
	SoftLineBreakNewline
)

// RawHTMLMode determines how raw HTML (both block and inline) is handled.
// The renderer never attempts to parse or interpret the HTML.
type RawHTMLMode int

const (
	// RawHTMLOmit drops raw HTML nodes entirely.
	// This is the default.
	RawHTMLOmit RawHTMLMode = iota

	// RawHTMLPassthrough writes raw HTML verbatim without parsing it.
	RawHTMLPassthrough
)

// Config holds configuration for plain text renderers.
type Config struct {
	renderer.Config[io.Writer, Config]

	// SoftLineBreak controls how soft line breaks are rendered.
	SoftLineBreak SoftLineBreakMode

	// RawHTML controls how raw HTML nodes are handled.
	RawHTML RawHTMLMode

	// WithLinkURL appends the destination of links after their label
	// (e.g. "label (https://example.com)").
	WithLinkURL bool

	// WithImageURL appends the destination of images after their alternative
	// text (e.g. "alt (image.png)").
	WithImageURL bool

	// ListIndent is the number of spaces used to indent nested list contents.
	// Negative values are treated as 0.
	ListIndent int
}

// Default returns a Config with default values.
func (c Config) Default() Config {
	return Config{
		SoftLineBreak: SoftLineBreakSpace,
		RawHTML:       RawHTMLOmit,
		WithLinkURL:   false,
		WithImageURL:  false,
		ListIndent:    2,
	}
}

// WithSoftLineBreakNewline renders soft line breaks as newlines instead of
// spaces.
func WithSoftLineBreakNewline() Option {
	return renderer.NewOptionFunc(func(c *Config) {
		c.SoftLineBreak = SoftLineBreakNewline
	})
}

// WithLinkURL appends link destinations after their labels.
func WithLinkURL() Option {
	return renderer.NewOptionFunc(func(c *Config) {
		c.WithLinkURL = true
	})
}

// WithImageURL appends image destinations after their alternative text.
func WithImageURL() Option {
	return renderer.NewOptionFunc(func(c *Config) {
		c.WithImageURL = true
	})
}

// WithRawHTMLPassthrough writes raw HTML verbatim instead of omitting it.
// The HTML is not parsed.
func WithRawHTMLPassthrough() Option {
	return renderer.NewOptionFunc(func(c *Config) {
		c.RawHTML = RawHTMLPassthrough
	})
}

// WithListIndent sets the number of spaces used to indent nested list
// contents. Values smaller than 2 make nested list markers overlap their
// parent content.
func WithListIndent(width int) Option {
	return renderer.NewOptionFunc(func(c *Config) {
		c.ListIndent = width
	})
}

// New returns a new plain text Renderer with the given options.
func New(opts ...Option) Renderer {
	opts = append([]Option{WithExtensions(NewCommonMark())}, opts...)
	var hb renderer.HelperBuilder[io.Writer, Config]
	helper := hb.Options(opts...).OnBeforeRender(
	func(w io.Writer, _ []byte, _ ast.Node, rc renderer.Context) error {
			rc.Set(stateKey, newWriterState(w.(util.BufWriter)))
			return nil
		}).Build()
	return &textRenderer{Helper: helper}
}

type textRenderer struct {
	*renderer.Helper[io.Writer, Config]
}

// Render renders the given AST node to the given writer.
func (r *textRenderer) Render(w io.Writer, source []byte, n ast.Node, opts ...renderer.RenderOption) error {
	if ew, ok := w.(util.ErrorBufWriter); ok {
		return r.Helper.Render(ew, source, n, opts...)
	}
	return r.Helper.Render(util.NewErrorBufWriterSize(w, len(source)*2), source, n, opts...)
}

// RenderStringSource renders the given AST node to the given writer with a string source.
func (r *textRenderer) RenderStringSource(w io.Writer, source string, n ast.Node,
	opts ...renderer.RenderOption) error {
	return r.Render(w, util.StringToReadOnlyBytes(source), n, opts...)
}

var stateKey = renderer.NewContextKey()

// ContextWriter returns the stateful writer stored in the rendering context.
//
// Custom node renderers should write through the returned [*Writer] so that
// indentation, blank lines and list numbering stay consistent.
func ContextWriter(rc renderer.Context) *Writer {
	v := rc.Get(stateKey)
	if v == nil {
		panic("text renderer writer state not found in context")
	}
	return v.(*Writer)
}

// Writer is the stateful writer used while rendering plain text.
// It tracks line starts, list indentation and ordered list counters.
type Writer struct {
	out      util.BufWriter
	err      error
	indent   string
	indented bool
	lastNL   int
	lists    []*listFrame
}

type listFrame struct {
	ordered bool
	number  int
}

func newWriterState(w util.BufWriter) *Writer {
	return &Writer{
		out:      w,
		lastNL:   -1,
		indent:   "",
		indented: true,
	}
}

// Err returns the first error encountered while writing, if any.
func (w *Writer) Err() error {
	return w.err
}

func (w *Writer) remember(err error) {
	if err != nil && w.err == nil {
		w.err = err
	}
}

func (w *Writer) writeIndent() {
	if w.indented || w.indent == "" {
		return
	}
	_, err := w.out.WriteString(w.indent)
	w.remember(err)
	w.indented = true
}

// Write writes visible text. Indentation is inserted at the start of each
// physical line.
func (w *Writer) Write(p []byte) (int, error) {
	for len(p) > 0 {
		if i := indexByte(p, '\n'); i >= 0 {
			w.writeIndent()
			_, err := w.out.Write(p[:i+1])
			w.remember(err)
			w.indented = false
			p = p[i+1:]
			continue
		}
		w.writeIndent()
		n, err := w.out.Write(p)
		w.remember(err)
		w.lastNL = trailingNewlines(p)
		return n, err
	}
	w.lastNL = 0
	return 0, nil
}

// WriteString writes visible text.
func (w *Writer) WriteString(s string) {
	_, _ = w.Write(util.StringToReadOnlyBytes(s))
}

// Raw writes content verbatim without inserting indentation at line starts.
// It is used for code blocks and passthrough raw HTML.
func (w *Writer) Raw(p []byte) {
	if len(p) == 0 {
		return
	}
	w.writeIndent()
	_, err := w.out.Write(p)
	w.remember(err)
	w.lastNL = trailingNewlines(p)
	w.indented = false
}

// Newline ends the current line.
func (w *Writer) Newline() {
	w.writeIndent()
	w.remember(w.out.WriteByte('\n'))
	w.indented = false
	if w.lastNL >= 0 {
		w.lastNL++
	}
}

// EnsureBlankLine inserts an empty line unless the output already ends with
// one (or is empty).
func (w *Writer) EnsureBlankLine() {
	switch {
	case w.lastNL < 0:
		return
	case w.lastNL < 2:
		w.Newline()
	}
}

// EnsureNewline ends the current line unless the output already ends with a
// newline (or is empty).
func (w *Writer) EnsureNewline() {
	if w.lastNL < 0 || w.lastNL > 0 {
		return
	}
	w.Newline()
}

func (w *Writer) pushList(ordered bool) {
	w.lists = append(w.lists, &listFrame{ordered: ordered, number: 0})
}

func (w *Writer) popList() {
	if len(w.lists) > 0 {
		w.lists = w.lists[:len(w.lists)-1]
	}
}

func (w *Writer) currentList() *listFrame {
	if len(w.lists) == 0 {
		return nil
	}
	return w.lists[len(w.lists)-1]
}

// nextItemMarker returns the bullet or ordered marker for the next item of
// the current list and increments the ordered counter.
func (w *Writer) nextItemMarker() string {
	list := w.currentList()
	if list == nil || !list.ordered {
		return "- "
	}
	list.number++
	return strconv.Itoa(list.number) + ". "
}

func indexByte(p []byte, c byte) int {
	for i, b := range p {
		if b == c {
			return i
		}
	}
	return -1
}

func trailingNewlines(p []byte) int {
	n := 0
	for i := len(p) - 1; i >= 0 && p[i] == '\n'; i-- {
		n++
	}
	return n
}
