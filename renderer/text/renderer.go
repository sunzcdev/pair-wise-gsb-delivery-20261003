package text

import (
	"io"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/renderer"
	"github.com/yuin/goldmark/v2/util"
)

var plainWriterKey = renderer.NewContextKey()

// ContextPlainWriter returns the stateful plain text writer associated with
// the rendering context. It is intended for custom node renderers registered
// via WithNodeRenderer(s).
func ContextPlainWriter(rc renderer.Context) *plainWriter {
	v := rc.Get(plainWriterKey)
	if v == nil {
		panic("plain text writer not found in context")
	}
	return v.(*plainWriter)
}

type plainTextRenderer struct {
	*renderer.Helper[io.Writer, Config]
}

// New returns a new plain text Renderer with the given options.
func New(opts ...Option) Renderer {
	opts = append([]Option{WithExtensions(NewCommonMark())}, opts...)
	var hb renderer.HelperBuilder[io.Writer, Config]
	helper := hb.Options(opts...).OnBeforeRender(
		func(w io.Writer, _ []byte, _ ast.Node, rc renderer.Context) error {
			pw := newPlainWriter(w.(util.BufWriter))
			rc.Set(plainWriterKey, pw)
			return nil
		}).Build()
	return &plainTextRenderer{Helper: helper}
}

// Render renders the given AST node to the given writer.
func (r *plainTextRenderer) Render(w io.Writer, source []byte, n ast.Node, opts ...renderer.RenderOption) error {
	if ew, ok := w.(util.ErrorBufWriter); ok {
		return r.Helper.Render(ew, source, n, opts...)
	}
	return r.Helper.Render(util.NewErrorBufWriterSize(w, len(source)*2), source, n, opts...)
}

// RenderStringSource renders the given AST node to the given writer.
func (r *plainTextRenderer) RenderStringSource(w io.Writer, source string, n ast.Node, opts ...renderer.RenderOption) error {
	return r.Render(w, util.StringToReadOnlyBytes(source), n, opts...)
}

type commonMark struct {
	opts   []Option
	config *Config
}

// NewCommonMark returns an Extension that renders the CommonMark node kinds as
// plain text.
func NewCommonMark(opts ...Option) Extension {
	return &commonMark{opts: opts}
}

func (e *commonMark) RendererOptions(cfg *Config) []Option {
	if len(e.opts) != 0 {
		thisConfig := *cfg
		for _, opt := range e.opts {
			opt.SetFormatOption(&thisConfig)
		}
		e.config = &thisConfig
	} else {
		e.config = cfg
	}

	noop := NodeRendererFunc(func(
		_ io.Writer, _ []byte, _ ast.Node, _ bool, _ renderer.Context) (ast.WalkStatus, error) {
		return ast.WalkContinue, nil
	})
	skipChildren := NodeRendererFunc(func(
		_ io.Writer, _ []byte, _ ast.Node, _ bool, _ renderer.Context) (ast.WalkStatus, error) {
		return ast.WalkSkipChildren, nil
	})

	return []Option{
		WithNodeRenderers(map[ast.NodeKind]NodeRenderer{
			ast.KindDocument:                NodeRendererFunc(e.renderDocument),
			ast.KindHeading:                 NodeRendererFunc(e.renderHeading),
			ast.KindBlockquote:              noop,
			ast.KindCodeBlock:               NodeRendererFunc(e.renderCodeBlock),
			ast.KindHTMLBlock:               NodeRendererFunc(e.renderHTMLBlock),
			ast.KindList:                    NodeRendererFunc(e.renderList),
			ast.KindListItem:                NodeRendererFunc(e.renderListItem),
			ast.KindParagraph:               NodeRendererFunc(e.renderParagraph),
			ast.KindThematicBreak:           NodeRendererFunc(e.renderThematicBreak),
			ast.KindLinkReferenceDefinition: skipChildren,
			ast.KindAutoLink:                NodeRendererFunc(e.renderAutoLink),
			ast.KindCodeSpan:                NodeRendererFunc(e.renderCodeSpan),
			ast.KindEmphasis:                noop,
			ast.KindStrong:                  noop,
			ast.KindImage:                   NodeRendererFunc(e.renderImage),
			ast.KindLink:                    NodeRendererFunc(e.renderLink),
			ast.KindRawHTML:                 NodeRendererFunc(e.renderRawHTML),
			ast.KindText:                    NodeRendererFunc(e.renderText),
		}),
	}
}

func (e *commonMark) renderDocument(
	_ io.Writer, _ []byte, n ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if !entering {
		if err := ContextPlainWriter(rc).finalize(); err != nil {
			return ast.WalkStop, err
		}
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderHeading(
	_ io.Writer, _ []byte, n ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	pw := ContextPlainWriter(rc)
	if entering {
		if err := pw.beforeBlock(); err != nil {
			return ast.WalkStop, err
		}
	} else if err := pw.nl(); err != nil {
		return ast.WalkStop, err
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderCodeBlock(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	pw := ContextPlainWriter(rc)
	n := node.(*ast.CodeBlock)
	if entering {
		// The first block of a list item starts right after the marker;
		// subsequent blocks are separated by a blank line.
		if err := pw.beforeBlock(); err != nil {
			return ast.WalkStop, err
		}
		// Emit the code block verbatim: no list indentation is added so the
		// internal whitespace is preserved exactly.
		if err := pw.raw(n.Value.Bytes(source)); err != nil {
			return ast.WalkStop, err
		}
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderHTMLBlock(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if e.config.RawHTML != RawHTMLKeep || !entering {
		return ast.WalkSkipChildren, nil
	}
	pw := ContextPlainWriter(rc)
	if err := pw.beforeBlock(); err != nil {
		return ast.WalkStop, err
	}
	if _, err := node.(*ast.HTMLBlock).Value.WriteTo(pw, source); err != nil {
		return ast.WalkStop, err
	}
	return ast.WalkSkipChildren, nil
}

func (e *commonMark) renderList(
	_ io.Writer, _ []byte, n ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if entering {
		pw := ContextPlainWriter(rc)
		// A list nested in a tight list item follows the item's lead
		// paragraph with only a newline (tight layout); every other list
		// starts after a blank line.
		if pw.isInTightListItem() {
			pw.consumeFirstBlock()
		} else {
			if err := pw.beforeBlock(); err != nil {
				return ast.WalkStop, err
			}
		}
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderListItem(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	pw := ContextPlainWriter(rc)
	n := node.(*ast.ListItem)
	if entering {
		list, ok := n.Parent().(*ast.List)
		if !ok {
			return ast.WalkContinue, nil
		}
		if err := pw.beforeListItem(list, n); err != nil {
			return ast.WalkStop, err
		}
	} else {
		if err := pw.afterListItem(); err != nil {
			return ast.WalkStop, err
		}
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderParagraph(
	_ io.Writer, _ []byte, n ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	pw := ContextPlainWriter(rc)
	if entering {
		// The first paragraph of a list item follows its marker; separation
		// from preceding blocks is handled when the list/item starts. Every
		// later paragraph is preceded by a blank line.
		if _, first, err := pw.beginFirstParagraph(); err != nil {
			return ast.WalkStop, err
		} else if !first {
			if err := pw.blank(); err != nil {
				return ast.WalkStop, err
			}
		}
	} else {
		if err := pw.nl(); err != nil {
			return ast.WalkStop, err
		}
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderThematicBreak(
	_ io.Writer, _ []byte, _ ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	pw := ContextPlainWriter(rc)
	if err := pw.beforeBlock(); err != nil {
		return ast.WalkStop, err
	}
	if err := pw.text("***"); err != nil {
		return ast.WalkStop, err
	}
	if err := pw.nl(); err != nil {
		return ast.WalkStop, err
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderAutoLink(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	pw := ContextPlainWriter(rc)
	n := node.(*ast.AutoLink)
	if _, err := n.Label.WriteTo(pw, source); err != nil {
		return ast.WalkStop, err
	}
	if e.config.LinkURLs {
		if err := pw.text(" (" + n.Destination.Value(source) + ")"); err != nil {
			return ast.WalkStop, err
		}
	}
	return ast.WalkSkipChildren, nil
}

func (e *commonMark) renderCodeSpan(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	pw := ContextPlainWriter(rc)
	// The CodeSpan node stores only the normalized inner content; wrap it
	// with a single backtick pair so inline code stays distinguishable.
	if err := pw.writeByte('`'); err != nil {
		return ast.WalkStop, err
	}
	if _, err := node.(*ast.CodeSpan).Value.WriteTo(pw, source); err != nil {
		return ast.WalkStop, err
	}
	if err := pw.writeByte('`'); err != nil {
		return ast.WalkStop, err
	}
	return ast.WalkSkipChildren, nil
}

func (e *commonMark) renderLink(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	// The link label is rendered from the child nodes; on exit the
	// destination may be appended.
	if entering {
		return ast.WalkContinue, nil
	}
	if !e.config.LinkURLs {
		return ast.WalkContinue, nil
	}
	pw := ContextPlainWriter(rc)
	dest := node.(*ast.Link).Destination.Value(source)
	if err := pw.text(" (" + dest + ")"); err != nil {
		return ast.WalkStop, err
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderImage(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	pw := ContextPlainWriter(rc)
	n := node.(*ast.Image)
	// Render only the inline text children as the alternative text.
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if err := renderImageAlt(pw, source, c); err != nil {
			return ast.WalkStop, err
		}
	}
	if e.config.ImageURLs {
		if err := pw.text(" (" + n.Destination.Value(source) + ")"); err != nil {
			return ast.WalkStop, err
		}
	}
	return ast.WalkSkipChildren, nil
}

// renderImageAlt renders image alternative text: plain text nodes and code
// spans are emitted, nested links are unwrapped, and other inlines contribute
// their text children. Soft line breaks are always rendered as spaces since
// the alt text is an inline label; hard line breaks are rendered as newlines.
func renderImageAlt(pw *plainWriter, source []byte, n ast.Node) error {
	switch c := n.(type) {
	case *ast.Text:
		if _, err := c.Value.WriteTo(pw, source); err != nil {
			return err
		}
		if c.HardLineBreak() {
			return pw.nl()
		}
		if c.SoftLineBreak() {
			return pw.text(" ")
		}
		return nil
	case *ast.CodeSpan:
		_, err := c.Value.WriteTo(pw, source)
		return err
	}
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		if err := renderImageAlt(pw, source, child); err != nil {
			return err
		}
	}
	return nil
}

func (e *commonMark) renderRawHTML(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if e.config.RawHTML != RawHTMLKeep || !entering {
		return ast.WalkSkipChildren, nil
	}
	pw := ContextPlainWriter(rc)
	if _, err := node.(*ast.RawHTML).Value.WriteTo(pw, source); err != nil {
		return ast.WalkStop, err
	}
	return ast.WalkSkipChildren, nil
}

func (e *commonMark) renderText(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	pw := ContextPlainWriter(rc)
	n := node.(*ast.Text)
	if _, err := n.Value.WriteTo(pw, source); err != nil {
		return ast.WalkStop, err
	}
	switch {
	case n.HardLineBreak():
		if err := pw.nl(); err != nil {
			return ast.WalkStop, err
		}
	case n.SoftLineBreak():
		if e.config.SoftLineBreak == SoftLineBreakNewline {
			if err := pw.nl(); err != nil {
				return ast.WalkStop, err
			}
		} else {
			if err := pw.text(" "); err != nil {
				return ast.WalkStop, err
			}
		}
	}
	return ast.WalkContinue, nil
}
