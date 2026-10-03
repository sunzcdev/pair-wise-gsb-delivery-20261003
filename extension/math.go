package extension

import (
	"bytes"
	"io"

	gast "github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

// isMathSpace reports whether the given byte is an ASCII whitespace
// character that may not appear directly inside the opening or closing
// dollar delimiters of an inline math expression.
func isMathSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// mathInlineParser parses inline math expressions delimited by single
// dollar signs ($...$).
type mathInlineParser struct{}

// NewMathInlineParser returns a new [parser.InlineParser] that parses inline
// math expressions delimited by single dollar signs.
func NewMathInlineParser() parser.InlineParser {
	return &mathInlineParser{}
}

func (p *mathInlineParser) Trigger() []byte {
	return []byte{'$'}
}

func (p *mathInlineParser) Parse(_ gast.Node, block text.Reader, _ parser.Context) gast.Node {
	line, _ := block.PeekLine()
	opener := 0
	for opener < len(line) && line[opener] == '$' {
		opener++
	}
	// Only a single dollar sign opens an inline math expression. A run of
	// two or more dollar signs (e.g. "$$") is left as literal content;
	// display math is handled exclusively at the block level.
	if opener != 1 {
		return nil
	}
	// The character immediately after the opening delimiter must not be
	// whitespace.
	if opener >= len(line) || isMathSpace(line[opener]) {
		return nil
	}

	startLine, startSegment := block.Position()
	block.Advance(opener)

	var content []byte
	first := true
	var prevByte byte
	for {
		line, _ := block.PeekLine()
		if line == nil {
			block.SetPosition(startLine, startSegment)
			return nil
		}
		// Inline math never spans a blank line. An unclosed expression is
		// therefore rendered as its literal source text.
		if !first && util.IsBlank(line) {
			block.SetPosition(startLine, startSegment)
			return nil
		}
		first = false

		// Line content excluding the trailing newline sequence.
		lineEnd := len(line)
		if lineEnd > 0 && line[lineEnd-1] == '\n' {
			lineEnd--
			if lineEnd > 0 && line[lineEnd-1] == '\r' {
				lineEnd--
			}
		}

		from := 0
		for {
			rel := bytes.IndexByte(line[from:lineEnd], '$')
			if rel < 0 {
				break
			}
			j := from + rel

			// The character directly before the delimiter determines
			// whitespace and escaping checks. On the opener line it comes
			// from the same line; on continuation lines a delimiter at the
			// line head is checked against the previous line's last byte.
			var before byte
			if j > 0 {
				before = line[j-1]
			} else if !first {
				before = prevByte
			}
			// An odd number of preceding backslashes (on the same line)
			// means the dollar sign is escaped and stays ordinary content.
			if j > 0 {
				slashes := 0
				for k := j - 1; k >= 0 && line[k] == '\\'; k-- {
					slashes++
				}
				if slashes%2 == 1 {
					from = j + 1
					continue
				}
			}
			// The delimiter must not be preceded by whitespace or sit at
			// the start of the expression, and it must not be followed by
			// a digit (so prices such as "$5 and $10" stay literal). A
			// dollar sign inside a run (e.g. "$a$$b$") still closes the
			// preceding expression, allowing adjacent math nodes.
			if before == 0 || isMathSpace(before) {
				from = j + 1
				continue
			}
			if j+1 < len(line) && util.IsNumeric(line[j+1]) {
				from = j + 1
				continue
			}

			content = append(content, line[:j]...)
			block.Advance(j + 1)
			value := text.NewMultiLineValueFromString(
				util.BytesToReadOnlyString(content),
				text.IdentityDecoder,
			)
			return ast.NewInlineMath(value)
		}

		// No closer on this line: retain the line and its newline verbatim
		// and continue scanning the next line.
		content = append(content, line[:lineEnd]...)
		if len(line) > lineEnd {
			content = append(content, '\n')
		}
		if lineEnd > 0 {
			prevByte = line[lineEnd-1]
		}
		block.AdvanceLine()
	}
}

func (p *mathInlineParser) CloseBlock(_ gast.Node, _ text.Reader, _ parser.Context) {
	// nothing to do
}

// mathFenceData holds the state of a block math fence while it is open.
type mathFenceData struct {
	indent int
	node   *ast.BlockMath
	raw    []text.Segment
	closed bool
}

var mathFenceKey = parser.NewContextKey()

// mathBlockParser parses display math blocks delimited by double dollar
// sign fences ($$).
type mathBlockParser struct{}

// NewMathBlockParser returns a new [parser.BlockParser] that parses display
// math blocks delimited by lines of double dollar signs.
func NewMathBlockParser() parser.BlockParser {
	return &mathBlockParser{}
}

func (b *mathBlockParser) Trigger() []byte {
	return []byte{'$'}
}

func (b *mathBlockParser) Open(_ gast.Node, reader text.Reader, pc parser.Context) (gast.Node, parser.State) {
	line, segment := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 || line[pos] != '$' {
		return nil, parser.NoChildren
	}
	i := pos
	for i < len(line) && line[i] == '$' {
		i++
	}
	if i-pos != 2 {
		return nil, parser.NoChildren
	}
	// The opening fence line may contain indentation and trailing
	// whitespace only; an info string is not allowed.
	if !util.IsBlank(line[i:]) {
		return nil, parser.NoChildren
	}

	node := ast.NewBlockMath(text.Lines{})
	pc.Set(mathFenceKey, &mathFenceData{
		indent: pos,
		node:   node,
		raw:    []text.Segment{segment},
	})
	reader.AdvanceToEOL()
	return node, parser.NoChildren
}

func (b *mathBlockParser) Continue(node gast.Node, reader text.Reader, pc parser.Context) parser.State {
	line, segment := reader.PeekLine()
	v := pc.Get(mathFenceKey)
	if v == nil {
		return parser.Close
	}
	fdata := v.(*mathFenceData)

	w, pos := util.IndentWidth(line, reader.LineOffset())
	if w < 4 {
		i := pos
		for i < len(line) && line[i] == '$' {
			i++
		}
		if i-pos == 2 && util.IsBlank(line[i:]) {
			fdata.closed = true
			reader.AdvanceToEOL()
			return parser.Close
		}
	}

	// Preserve the body as raw bytes; only the fence-level indentation is
	// treated as block indentation.
	bodyPos, padding := util.IndentPositionPadding(line, reader.LineOffset(), segment.Padding, fdata.indent)
	if bodyPos < 0 {
		bodyPos = max(0, util.FirstNonSpacePosition(line)) - segment.Padding
		padding = 0
	}
	seg := text.NewSegmentPadding(segment.Start+bodyPos, segment.Stop, padding)
	seg.ForceNewline = true // EOF as newline
	fdata.node.Value.AppendSegment(seg)

	// Keep the untouched source segment so an unclosed fence can be
	// restored as a paragraph.
	fdata.raw = append(fdata.raw, segment)

	reader.AdvanceToEOL()
	return parser.Continue | parser.NoChildren
}

func (b *mathBlockParser) Close(node gast.Node, reader text.Reader, pc parser.Context) {
	v := pc.Get(mathFenceKey)
	if v == nil {
		return
	}
	fdata := v.(*mathFenceData)
	if fdata.node != node {
		return
	}
	pc.Set(mathFenceKey, nil)
	// If the block reached EOF without a closing fence, restore its literal
	// source as a paragraph so CommonMark output is preserved.
	if !fdata.closed {
		parent := node.Parent()
		if parent != nil {
			paragraph := gast.NewParagraph()
			paragraph.SetPos(node.Pos())
			for _, seg := range fdata.raw {
				paragraph.AppendSource(seg)
			}
			if lines := paragraph.Source(); len(lines) != 0 {
				last := len(lines) - 1
				lines[last] = lines[last].TrimRightSpace(reader.Source())
			}
			parent.ReplaceChild(node, paragraph)
		}
	}
}

func (b *mathBlockParser) CanInterruptParagraph() bool {
	return true
}

func (b *mathBlockParser) CanAcceptIndentedLine() bool {
	return false
}

// mathHTMLRendererConfig holds configuration for the math HTML renderer.
type mathHTMLRendererConfig struct {
	// InlineTag is the HTML element name used for inline math.
	InlineTag string
	// BlockTag is the HTML element name used for display math blocks.
	BlockTag string
}

// MathHTMLRendererOption is an option for the math HTML renderer extension.
type MathHTMLRendererOption interface {
	applyMathHTMLRendererOption(*mathHTMLRendererConfig)
}

type withMathInlineTag struct {
	value string
}

func (o *withMathInlineTag) applyMathHTMLRendererOption(c *mathHTMLRendererConfig) {
	c.InlineTag = o.value
}

// WithInlineMathTag sets the HTML element name used to wrap inline math
// expressions. The default is "code". The value must be a valid, simple
// HTML tag name.
func WithInlineMathTag(tag string) MathHTMLRendererOption {
	return &withMathInlineTag{value: tag}
}

type withMathBlockTag struct {
	value string
}

func (o *withMathBlockTag) applyMathHTMLRendererOption(c *mathHTMLRendererConfig) {
	c.BlockTag = o.value
}

// WithBlockMathTag sets the HTML element name used to wrap display math
// blocks. The default is "pre". The value must be a valid, simple HTML tag
// name.
func WithBlockMathTag(tag string) MathHTMLRendererOption {
	return &withMathBlockTag{value: tag}
}

type mathHTMLRendererExtension struct {
	config mathHTMLRendererConfig
}

// NewMathHTMLRenderer returns a new html.Extension for rendering math nodes.
func NewMathHTMLRenderer(opts ...MathHTMLRendererOption) html.Extension {
	cfg := mathHTMLRendererConfig{
		InlineTag: "code",
		BlockTag:  "pre",
	}
	for _, opt := range opts {
		opt.applyMathHTMLRendererOption(&cfg)
	}
	return &mathHTMLRendererExtension{config: cfg}
}

func (e *mathHTMLRendererExtension) RendererOptions(_ *html.Config) []html.Option {
	return []html.Option{
		html.WithNodeRenderers(map[gast.NodeKind]html.NodeRenderer{
			ast.KindInlineMath: html.NodeRendererFunc(e.renderInlineMath),
			ast.KindBlockMath:  html.NodeRendererFunc(e.renderBlockMath),
		}),
	}
}

func (e *mathHTMLRendererExtension) renderInlineMath(
	writer io.Writer, source []byte, n gast.Node, entering bool, rc renderer.Context,
) (gast.WalkStatus, error) {
	w := writer.(util.BufWriter)
	node := n.(*ast.InlineMath)
	if entering {
		_, _ = w.WriteString("<" + e.config.InlineTag + ">")
		_, _ = node.Value.WriteTo(html.ContextTextWriter(rc), source)
		_, _ = w.WriteString("</" + e.config.InlineTag + ">")
		return gast.WalkSkipChildren, nil
	}
	return gast.WalkContinue, nil
}

func (e *mathHTMLRendererExtension) renderBlockMath(
	writer io.Writer, source []byte, n gast.Node, entering bool, rc renderer.Context,
) (gast.WalkStatus, error) {
	w := writer.(util.BufWriter)
	node := n.(*ast.BlockMath)
	if entering {
		_, _ = w.WriteString("<" + e.config.BlockTag + ">")
		_, _ = node.Value.WriteTo(html.ContextTextWriter(rc), source)
		_, _ = w.WriteString("</" + e.config.BlockTag + ">\n")
		return gast.WalkSkipChildren, nil
	}
	return gast.WalkContinue, nil
}

type mathParserExtension struct{}

// NewMathParser returns a new parser.Extension that parses inline ($...$)
// and display ($$) math expressions.
func NewMathParser() parser.Extension {
	return &mathParserExtension{}
}

func (e *mathParserExtension) ParserOptions(_ *parser.Config) []parser.Option {
	return []parser.Option{
		// Code spans (100) must win over math; links (200) must be able to
		// contain math in their labels.
		parser.WithBlockParsers(
			util.Prioritized(NewMathBlockParser(), 650),
		),
		parser.WithInlineParsers(
			util.Prioritized(NewMathInlineParser(), 150),
		),
	}
}

// MathParser is a default [parser.Extension] for parsing math expressions.
// It is disabled by default; pass it to [parser.WithExtensions] to enable it.
var MathParser = NewMathParser()

// MathHTMLRenderer is a default [html.Extension] for rendering math nodes.
var MathHTMLRenderer = NewMathHTMLRenderer()
