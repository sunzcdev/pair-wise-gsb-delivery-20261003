package text

import (
	"io"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/renderer"
)

type commonMark struct {
	config *Config
}

// NewCommonMark returns an [Extension] that renders CommonMark nodes as plain
// text.
func NewCommonMark(opts ...Option) Extension {
	return &commonMark{}
}

func (e *commonMark) RendererOptions(cfg *Config) []Option {
	// RendererOptions is called with the fully resolved configuration, so keep
	// a reference to it for the node renderer functions.
	e.config = cfg
	return []Option{
		WithNodeRenderers(map[ast.NodeKind]NodeRenderer{
			ast.KindDocument:                NodeRendererFunc(e.renderDocument),
			ast.KindHeading:                 NodeRendererFunc(e.renderBlock),
			ast.KindParagraph:               NodeRendererFunc(e.renderParagraph),
			ast.KindBlockquote:              NodeRendererFunc(e.renderBlock),
			ast.KindCodeBlock:               NodeRendererFunc(e.renderCodeBlock),
			ast.KindHTMLBlock:               NodeRendererFunc(e.renderHTMLBlock),
			ast.KindList:                    NodeRendererFunc(e.renderList),
			ast.KindListItem:                NodeRendererFunc(e.renderListItem),
			ast.KindThematicBreak:           NodeRendererFunc(e.renderThematicBreak),
			ast.KindLinkReferenceDefinition: NodeRendererFunc(e.renderNoop),
			ast.KindText:                    NodeRendererFunc(e.renderText),
			ast.KindCodeSpan:                NodeRendererFunc(e.renderCodeSpan),
			ast.KindEmphasis:                NodeRendererFunc(e.renderContainer),
			ast.KindStrong:                  NodeRendererFunc(e.renderContainer),
			ast.KindLink:                    NodeRendererFunc(e.renderLink),
			ast.KindImage:                   NodeRendererFunc(e.renderImage),
			ast.KindAutoLink:                NodeRendererFunc(e.renderAutoLink),
			ast.KindRawHTML:                 NodeRendererFunc(e.renderRawHTML),
		}),
	}
}

// isInTightBlock reports whether n is a paragraph that is the direct child of
// an item of a tight list.
func isInTightBlock(n ast.Node) bool {
	item := n.Parent()
	if item == nil || item.Kind() != ast.KindListItem {
		return false
	}
	list := item.Parent()
	if list == nil || list.Kind() != ast.KindList {
		return false
	}
	return list.(*ast.List).IsTight
}

// separateBlock inserts blank line separation before a block level node.
// Tight list blocks are kept on adjacent lines.
func separateBlock(w *Writer, n ast.Node) {
	if n.Parent() == nil || n.PreviousSibling() == nil {
		return
	}
	if isInTightBlock(n) {
		w.EnsureNewline()
		return
	}
	w.EnsureBlankLine()
}

func (e *commonMark) renderDocument(
	_ io.Writer, _ []byte, _ ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	w := ContextWriter(rc)
	if !entering {
		w.EnsureNewline()
	}
	return ast.WalkContinue, nil
}

// renderBlock handles blocks that contribute nothing themselves: headings and
// block quotes simply emit their child text, separated from surrounding
// blocks.
func (e *commonMark) renderBlock(
	_ io.Writer, _ []byte, n ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	w := ContextWriter(rc)
	if entering {
		separateBlock(w, n)
	} else if n.Kind() == ast.KindHeading {
		w.EnsureNewline()
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderParagraph(
	_ io.Writer, _ []byte, n ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	w := ContextWriter(rc)
	if entering {
		separateBlock(w, n)
	} else if !isInTightBlock(n) {
		w.EnsureNewline()
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderCodeBlock(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	w := ContextWriter(rc)
	if !entering {
		return ast.WalkContinue, nil
	}
	separateBlock(w, node)
	n := node.(*ast.CodeBlock)
	// Code block content is emitted verbatim; it is not decoded or indented.
	_, _ = n.Value.WriteTo(&rawAdapter{w: w}, source)
	w.EnsureNewline()
	return ast.WalkSkipChildren, nil
}

func (e *commonMark) renderHTMLBlock(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if !entering || e.config.RawHTML != RawHTMLPassthrough {
		return ast.WalkContinue, nil
	}
	w := ContextWriter(rc)
	separateBlock(w, node)
	n := node.(*ast.HTMLBlock)
	_, _ = n.Value.WriteTo(&rawAdapter{w: w}, source)
	w.EnsureNewline()
	return ast.WalkContinue, nil
}

func (e *commonMark) renderList(
	_ io.Writer, _ []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	w := ContextWriter(rc)
	n := node.(*ast.List)
	if entering {
		separateBlock(w, node)
		w.pushList(n.IsOrdered())
	} else {
		w.popList()
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderListItem(
	_ io.Writer, _ []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	w := ContextWriter(rc)
	indentWidth := e.config.ListIndent
	if indentWidth < 0 {
		indentWidth = 0
	}
	if entering {
		if node.PreviousSibling() != nil {
			w.EnsureNewline()
		}
		marker := w.nextItemMarker()
		w.WriteString(marker)
		w.indent += spaces(indentWidth) + spaces(len(marker))
	} else {
		// Remove the marker width ("- " is 2) and the configured indent.
		cut := indentWidth + 2
		if len(w.indent) >= cut {
			w.indent = w.indent[:len(w.indent)-cut]
		}
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderThematicBreak(
	_ io.Writer, _ []byte, _ ast.Node, _ bool, _ renderer.Context) (ast.WalkStatus, error) {
	// Thematic breaks carry no searchable text; omit them.
	return ast.WalkSkipChildren, nil
}

func (e *commonMark) renderNoop(
	_ io.Writer, _ []byte, _ ast.Node, _ bool, _ renderer.Context) (ast.WalkStatus, error) {
	return ast.WalkSkipChildren, nil
}

func (e *commonMark) renderText(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	w := ContextWriter(rc)
	n := node.(*ast.Text)
	_, _ = n.Value.WriteTo(w, source)
	switch {
	case n.HardLineBreak():
		w.Newline()
	case n.SoftLineBreak() && e.config.SoftLineBreak == SoftLineBreakNewline:
		w.Newline()
	case n.SoftLineBreak():
		w.WriteString(" ")
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderCodeSpan(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	w := ContextWriter(rc)
	// Code span content is not entity/escape decoded.
	_, _ = node.(*ast.CodeSpan).Value.WriteTo(w, source)
	return ast.WalkSkipChildren, nil
}

func (e *commonMark) renderContainer(
	_ io.Writer, _ []byte, _ ast.Node, _ bool, _ renderer.Context) (ast.WalkStatus, error) {
	return ast.WalkContinue, nil
}

func (e *commonMark) renderLink(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	w := ContextWriter(rc)
	if entering && e.config.WithLinkURL {
		n := node.(*ast.Link)
		dest := n.Destination.Value(source)
		if dest != "" {
			defer func() {
				w.WriteString(" (" + dest + ")")
			}()
		}
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderImage(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	w := ContextWriter(rc)
	if entering {
		n := node.(*ast.Image)
		if e.config.WithImageURL {
			dest := n.Destination.Value(source)
			if dest != "" {
				defer func() {
					w.WriteString(" (" + dest + ")")
				}()
			}
		}
		// Alternative text is the child nodes.
		return ast.WalkContinue, nil
	}
	return ast.WalkContinue, nil
}

func (e *commonMark) renderAutoLink(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	w := ContextWriter(rc)
	n := node.(*ast.AutoLink)
	label := n.Label.Value(source)
	w.WriteString(label)
	if e.config.WithLinkURL {
		dest := n.Destination.Value(source)
		if dest != "" && dest != label {
			w.WriteString(" (" + dest + ")")
		}
	}
	return ast.WalkSkipChildren, nil
}

func (e *commonMark) renderRawHTML(
	_ io.Writer, source []byte, node ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
	if !entering || e.config.RawHTML != RawHTMLPassthrough {
		return ast.WalkSkipChildren, nil
	}
	w := ContextWriter(rc)
	_, _ = node.(*ast.RawHTML).Value.WriteTo(w, source)
	return ast.WalkSkipChildren, nil
}

// rawAdapter lets [text.Lines].WriteTo write through [Writer.Raw].
type rawAdapter struct {
	w *Writer
}

func (a *rawAdapter) Write(p []byte) (int, error) {
	a.w.Raw(p)
	return len(p), nil
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}
