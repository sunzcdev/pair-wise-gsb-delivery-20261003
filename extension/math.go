package extension

import (
	"io"

	gast "github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

type mathBlockParser struct{}

var defaultMathBlockParser = &mathBlockParser{}

// NewMathBlockParser returns a BlockParser that parses display math blocks.
func NewMathBlockParser() parser.BlockParser {
	return defaultMathBlockParser
}

func (b *mathBlockParser) Trigger() []byte {
	return []byte{'$'}
}

func isDollarFence(line []byte, pos int) bool {
	return pos+1 < len(line) && line[pos] == '$' && line[pos+1] == '$' && util.IsBlank(line[pos+2:])
}

func (b *mathBlockParser) Open(_ gast.Node, reader text.Reader, pc parser.Context) (gast.Node, parser.State) {
	line, _ := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 || !isDollarFence(line, pos) {
		return nil, parser.NoChildren
	}
	reader.AdvanceToEOL()
	return ast.NewMathBlock(text.Lines{}), parser.NoChildren
}

func (b *mathBlockParser) Continue(node gast.Node, reader text.Reader, _ parser.Context) parser.State {
	line, segment := reader.PeekLine()
	width, pos := util.IndentWidth(line, reader.LineOffset())
	if width < 4 && isDollarFence(line, pos) {
		reader.AdvanceToEOL()
		return parser.Close
	}

	node.(*ast.MathBlock).Value.AppendSegment(segment)
	reader.AdvanceToEOL()
	return parser.Continue | parser.NoChildren
}

func (b *mathBlockParser) Close(_ gast.Node, _ text.Reader, _ parser.Context) {}

func (b *mathBlockParser) CanInterruptParagraph() bool {
	return true
}

func (b *mathBlockParser) CanAcceptIndentedLine() bool {
	return false
}

type mathInlineParser struct{}

var defaultMathInlineParser = &mathInlineParser{}

// NewMathInlineParser returns an InlineParser that parses inline math.
func NewMathInlineParser() parser.InlineParser {
	return defaultMathInlineParser
}

func (p *mathInlineParser) Trigger() []byte {
	return []byte{'$'}
}

func (p *mathInlineParser) Parse(_ gast.Node, block text.Reader, _ parser.Context) gast.Node {
	line, startSegment := block.PeekLine()
	if len(line) < 2 || line[1] == '$' || util.IsSpace(line[1]) || line[1] == '\r' || line[1] == '\n' {
		return nil
	}

	startLine, startPosition := block.Position()
	block.Advance(1)

	var indices []text.Index
	firstLine := true
	for {
		line, segment := block.PeekLine()
		if line == nil || line[0] == '\n' {
			block.SetPosition(startLine, startPosition)
			return nil
		}

		for i := 0; i < len(line); i++ {
			if line[i] == '\\' {
				if i+1 < len(line) {
					i++
				}
				continue
			}

			if line[i] != '$' {
				continue
			}
			backslashes := 0
			for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
				backslashes++
			}
			if backslashes%2 == 1 {
				continue
			}
			if i == 0 || util.IsSpace(line[i-1]) || line[i-1] == '\r' {
				block.SetPosition(startLine, startPosition)
				return nil
			}
			if i+1 < len(line) && util.IsNumeric(line[i+1]) {
				block.SetPosition(startLine, startPosition)
				return nil
			}

			contentStop := segment.Start + i - segment.Padding
			if firstLine {
				indices = append(indices, text.NewIndex(startSegment.Start+1, contentStop))
			} else {
				indices = append(indices, text.NewIndex(segment.Start-segment.Padding, contentStop))
			}
			block.Advance(i + 1)
			return ast.NewInlineMath(text.NewMultiLineValueFromIndices(indices, text.IdentityDecoder))
		}

		if firstLine {
			indices = append(indices, text.NewIndex(startSegment.Start+1, segment.Stop))
		} else {
			indices = append(indices, text.NewIndex(segment.Start-segment.Padding, segment.Stop))
		}
		firstLine = false
		block.AdvanceLine()
	}
}

type mathParserExtension struct{}

// NewMathParser returns a parser.Extension that parses math expressions.
func NewMathParser() parser.Extension {
	return &mathParserExtension{}
}

func (e *mathParserExtension) ParserOptions(_ *parser.Config) []parser.Option {
	return []parser.Option{
		parser.WithBlockParsers(util.Prioritized(NewMathBlockParser(), 650)),
		parser.WithInlineParsers(util.Prioritized(NewMathInlineParser(), 150)),
	}
}

// MathParser is the default parser.Extension for math expressions.
var MathParser = NewMathParser()

type mathHTMLRendererConfig struct {
	inlineTag string
	blockTag  string
}

// MathHTMLRendererOption configures math HTML rendering.
type MathHTMLRendererOption interface {
	applyMathHTMLRendererOption(*mathHTMLRendererConfig)
}

type withMathInlineTag string

func (o withMathInlineTag) applyMathHTMLRendererOption(c *mathHTMLRendererConfig) {
	c.inlineTag = string(o)
}

// WithMathInlineTag sets the HTML tag used for inline math.
func WithMathInlineTag[T string | []byte](tag T) MathHTMLRendererOption {
	switch value := any(tag).(type) {
	case string:
		return withMathInlineTag(value)
	case []byte:
		return withMathInlineTag(util.BytesToReadOnlyString(value))
	default:
		panic("unsupported tag type")
	}
}

type withMathBlockTag string

func (o withMathBlockTag) applyMathHTMLRendererOption(c *mathHTMLRendererConfig) {
	c.blockTag = string(o)
}

// WithMathBlockTag sets the HTML tag used inside pre for display math.
func WithMathBlockTag[T string | []byte](tag T) MathHTMLRendererOption {
	switch value := any(tag).(type) {
	case string:
		return withMathBlockTag(value)
	case []byte:
		return withMathBlockTag(util.BytesToReadOnlyString(value))
	default:
		panic("unsupported tag type")
	}
}

type mathHTMLRendererExtension struct {
	config mathHTMLRendererConfig
}

// NewMathHTMLRenderer returns an html.Extension that renders math nodes.
func NewMathHTMLRenderer(opts ...MathHTMLRendererOption) html.Extension {
	config := mathHTMLRendererConfig{inlineTag: "code", blockTag: "code"}
	for _, opt := range opts {
		opt.applyMathHTMLRendererOption(&config)
	}
	return &mathHTMLRendererExtension{config: config}
}

func (e *mathHTMLRendererExtension) RendererOptions(_ *html.Config) []html.Option {
	return []html.Option{
		html.WithNodeRenderers(map[gast.NodeKind]html.NodeRenderer{
			ast.KindInlineMath: html.NodeRendererFunc(e.renderInlineMath),
			ast.KindMathBlock:  html.NodeRendererFunc(e.renderMathBlock),
		}),
	}
}

func (e *mathHTMLRendererExtension) renderInlineMath(
	writer io.Writer, source []byte, node gast.Node, entering bool, rc renderer.Context,
) (gast.WalkStatus, error) {
	w := writer.(util.BufWriter)
	if entering {
		_ = w.WriteByte('<')
		_, _ = w.WriteString(e.config.inlineTag)
		_ = w.WriteByte('>')
		_, _ = node.(*ast.InlineMath).Value.WriteTo(html.ContextTextWriter(rc), source)
	} else {
		_, _ = w.WriteString("</")
		_, _ = w.WriteString(e.config.inlineTag)
		_ = w.WriteByte('>')
	}
	return gast.WalkContinue, nil
}

func (e *mathHTMLRendererExtension) renderMathBlock(
	writer io.Writer, source []byte, node gast.Node, entering bool, rc renderer.Context,
) (gast.WalkStatus, error) {
	w := writer.(util.BufWriter)
	if entering {
		_, _ = w.WriteString("<pre><")
		_, _ = w.WriteString(e.config.blockTag)
		_ = w.WriteByte('>')
		_, _ = node.(*ast.MathBlock).Value.WriteTo(html.ContextTextWriter(rc), source)
	} else {
		_, _ = w.WriteString("</")
		_, _ = w.WriteString(e.config.blockTag)
		_, _ = w.WriteString("></pre>\n")
	}
	return gast.WalkContinue, nil
}

// MathHTMLRenderer is the default html.Extension for math nodes.
var MathHTMLRenderer = NewMathHTMLRenderer()
