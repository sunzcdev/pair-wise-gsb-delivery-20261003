package extension

import (
	"unicode"
	"unicode/utf8"

	gast "github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

// SmartQuoteStyle selects the typographic quotation marks produced by the
// paragraph scoped smart quotes extension.
type SmartQuoteStyle int

const (
	// EnglishSmartQuotes replaces straight quotes with English curly quotes:
	// “ and ” for double quotes and ‘ and ’ for single quotes. The typographic
	// glyph always matches the straight quote character, so double and single
	// quotes pair independently. Word internal apostrophes become ’.
	EnglishSmartQuotes SmartQuoteStyle = iota + 1

	// ChineseSmartQuotes replaces straight quotes with Chinese corner
	// brackets. The outer quotation level uses 「 and 」 and every nested
	// level alternates with 『 and 』. English word internal apostrophes still
	// become ’.
	ChineseSmartQuotes
)

// smartQuoteGlyphs is the typographic alphabet of one smart quote style.
type smartQuoteGlyphs struct {
	leftDouble  rune
	rightDouble rune
	leftSingle  rune
	rightSingle rune
	apostrophe  rune

	// alternateByLevel is set for Chinese style: the glyph is chosen by
	// nesting level instead of by the straight quote character.
	alternateByLevel bool

	// elisions enables English leading elisions such as '90s and 'net.
	elisions bool
}

var englishSmartQuoteGlyphs = smartQuoteGlyphs{
	leftDouble:  '“',
	rightDouble: '”',
	leftSingle:  '‘',
	rightSingle: '’',
	apostrophe:  '’',
	elisions:    true,
}

var chineseSmartQuoteGlyphs = smartQuoteGlyphs{
	leftDouble:       '「',
	rightDouble:      '」',
	leftSingle:       '『',
	rightSingle:      '』',
	apostrophe:       '’',
	alternateByLevel: true,
}

// SmartQuotesParserOption is a functional option for the paragraph scoped
// smart quotes extension.
type SmartQuotesParserOption func(*smartQuotesConfig)

type smartQuotesConfig struct {
	glyphs smartQuoteGlyphs
}

// WithSmartQuoteStyle is a functional option that selects the quotation marks
// produced by the smart quotes extension. The default style is
// [EnglishSmartQuotes].
func WithSmartQuoteStyle(style SmartQuoteStyle) SmartQuotesParserOption {
	return func(c *smartQuotesConfig) {
		switch style {
		case ChineseSmartQuotes:
			c.glyphs = chineseSmartQuoteGlyphs
		default:
			c.glyphs = englishSmartQuoteGlyphs
		}
	}
}

// NewSmartQuotesParser returns a new [parser.Extension] that pairs straight
// quotation marks within the scope of a single paragraph and replaces them
// with typographic quotation marks.
//
// Unlike [NewTypographerParser], pairing state flows through emphasis, strong,
// strikethrough, link and image nodes, but never across paragraphs, code
// spans, auto links or inline raw HTML. Only text nodes are rewritten, so link
// destinations, code and node attributes are left untouched.
//
// All pairing state is created inside [parser.ASTTransformer.Transform] and
// never stored on the parser, so a single parser instance can parse multiple
// documents concurrently.
func NewSmartQuotesParser(opts ...SmartQuotesParserOption) parser.Extension {
	cfg := smartQuotesConfig{glyphs: englishSmartQuoteGlyphs}
	for _, o := range opts {
		o(&cfg)
	}
	t := &smartQuotesTransformer{glyphs: cfg.glyphs}
	return &smartQuotesParserExtension{transformer: t}
}

// SmartQuotesParser is a default [parser.Extension] that pairs straight
// quotation marks within paragraphs and replaces them with English curly
// quotes.
var SmartQuotesParser = NewSmartQuotesParser()

type smartQuotesParserExtension struct {
	transformer parser.ASTTransformer
}

func (e *smartQuotesParserExtension) ParserOptions(_ *parser.Config) []parser.Option {
	return []parser.Option{
		parser.WithASTTransformers(
			util.Prioritized(e.transformer, 100),
		),
	}
}

type smartQuotesTransformer struct {
	glyphs smartQuoteGlyphs
}

func (a *smartQuotesTransformer) Transform(doc *gast.Document, reader text.Reader, _ parser.Context) {
	source := reader.Source()
	var roots []gast.Node
	_ = gast.Walk(doc, func(n gast.Node, entering bool) (gast.WalkStatus, error) {
		if !entering {
			return gast.WalkContinue, nil
		}
		switch n.Kind() {
		case gast.KindParagraph, gast.KindHeading:
			roots = append(roots, n)
			return gast.WalkSkipChildren, nil
		}
		return gast.WalkContinue, nil
	})
	for _, root := range roots {
		newQuoteProcessor(root, a.glyphs, reader, source).run()
	}
}

// segmentKind distinguishes rewritable text nodes from pairing barriers.
type segmentKind int

const (
	textKind segmentKind = iota
	barrierKind
)

type quoteSegment struct {
	kind segmentKind
	node *gast.Text
	raw  []byte

	// lineBreak marks a text node that ends a source line.
	lineBreak bool
}

// quoteFrame records one open quotation mark.
type quoteFrame struct {
	double bool
	level  int
}

// quoteProcessor pairs and replaces the quotes of one paragraph or heading.
// Every instance and all of its fields are local to a single run and never
// escape it, which keeps paragraphs independent and concurrent parses safe.
type quoteProcessor struct {
	glyphs smartQuoteGlyphs
	reader text.Reader

	segments []quoteSegment

	prev         rune
	prevStraight bool
	stack        []quoteFrame
}

func newQuoteProcessor(root gast.Node, glyphs smartQuoteGlyphs, reader text.Reader, source []byte) *quoteProcessor {
	p := &quoteProcessor{
		glyphs: glyphs,
		reader: reader,
		prev:   ' ',
	}
	collectSegments(root, &p.segments, source)
	return p
}

func collectSegments(n gast.Node, segments *[]quoteSegment, source []byte) {
	switch n.Kind() {
	case gast.KindCodeSpan, gast.KindRawHTML, gast.KindAutoLink:
		// Pairing cannot cross code spans, raw HTML tags or autolinks.
		*segments = append(*segments, quoteSegment{kind: barrierKind})
		return
	}
	if t, ok := n.(*gast.Text); ok {
		*segments = append(*segments, quoteSegment{
			kind:      textKind,
			node:      t,
			raw:       t.Value.Bytes(source),
			lineBreak: t.SoftLineBreak() || t.HardLineBreak(),
		})
		return
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		collectSegments(c, segments, source)
	}
}

func (p *quoteProcessor) run() {
	for si := range p.segments {
		seg := &p.segments[si]
		if seg.kind == barrierKind {
			p.prev = ' '
			p.prevStraight = false
			p.stack = p.stack[:0]
			continue
		}
		p.processText(si, seg)
		if seg.lineBreak {
			// A folded source newline behaves as whitespace.
			p.prev = ' '
			p.prevStraight = false
		}
	}
}

func (p *quoteProcessor) processText(segIndex int, seg *quoteSegment) {
	raw := seg.raw
	var out []byte
	copied := 0
	changed := false
	flush := func(stop int) {
		out = append(out, raw[copied:stop]...)
		copied = stop
	}

	for i := 0; i < len(raw); {
		b := raw[i]
		if b == '\\' {
			// Count the run of backslashes. Only an odd run escapes the byte
			// that follows it; the copy is emitted verbatim because the
			// decoder resolves backslashes at render time.
			j := i
			for j < len(raw) && raw[j] == '\\' {
				j++
			}
			run := j - i
			if j < len(raw) {
				p.prev = '\\'
				p.prevStraight = false
				if run%2 == 1 {
					// Escaped byte: advance over it without treating it as a
					// quote; the decoder emits the literal character.
					escaped := raw[j]
					if escaped != '\'' && escaped != '"' {
						p.prev = util.ToRune(raw, j)
					}
					j++
				}
			}
			i = j
			continue
		}
		if b != '\'' && b != '"' {
			r, size := utf8.DecodeRune(raw[i:])
			p.prev = r
			p.prevStraight = false
			i += size
			continue
		}

		double := b == '"'
		after := p.peekAfter(segIndex, i)
		if replacement, ok := p.classify(double, after); ok {
			flush(i)
			var buf [utf8.UTFMax]byte
			n := utf8.EncodeRune(buf[:], replacement)
			out = append(out, buf[:n]...)
			copied = i + 1
			changed = true
			p.prevStraight = false
		} else {
			p.prevStraight = true
		}
		p.prev = rune(b)
		i++
	}

	if changed {
		flush(len(raw))
		seg.node.Value = text.NewSingleLineValueFromString(
			util.BytesToReadOnlyString(out), p.reader.Decoder())
	}
}

// classify returns the typographic replacement for the quote at the current
// position. The boolean result is false when the character must stay
// straight, such as a foot or inch prime.
func (p *quoteProcessor) classify(double bool, after rune) (rune, bool) {
	before := p.prev
	canOpen := parser.IsLeftFlankingDelimiterRun(before, after)
	canClose := parser.IsRightFlankingDelimiterRun(before, after)

	// Chinese style nests by level: any closing flanking quote closes the
	// innermost open frame of the same straight quote character (CJK runs are
	// both flanking, so character type disambiguates), and other quotes open a
	// new nested level.
	if p.glyphs.alternateByLevel {
		if !double && p.isApostrophe(before, after) {
			return p.glyphs.apostrophe, true
		}
		// Measurement primes and doubled inch marks after digits stay straight.
		if p.prevStraight {
			return 0, false
		}
		if unicode.IsDigit(before) {
			if double || !canClose {
				return 0, false
			}
			if idx := p.matchingFrame(false); idx >= 0 {
				return p.closeAt(idx)
			}
			return 0, false
		}
		idx := p.matchingFrame(double)
		switch {
		case idx >= 0 && canClose:
			return p.closeAt(idx)
		case idx < 0 && canClose && !canOpen:
			// A close only mark after a barrier renders as a closer without
			// changing pairing state.
			return p.glyphs.right(1, double), true
		case canOpen:
			return p.open(double)
		}
		return 0, false
	}

	if !double && p.isApostrophe(before, after) {
		return p.glyphs.apostrophe, true
	}

	// Measurement primes follow digits: 5'6" and the doubled inch mark in
	// 21"" are not quotation marks.
	if unicode.IsDigit(before) {
		// Double quotes after a digit are treated as inch marks even when a
		// pair happens to be open; foot marks close a quoted measurement only
		// when they genuinely can.
		idx := -1
		if !double && canClose {
			idx = p.closable(false)
		}
		if double || idx < 0 {
			return 0, false
		}
		return p.closeAt(idx)
	}
	// A quote immediately following a straight prime or inch mark never opens
	// a pair, so doubled marks such as 21"" remain straight together.
	if p.prevStraight {
		if idx := p.closable(double); canClose && idx >= 0 {
			return p.closeAt(idx)
		}
		return 0, false
	}

	switch {
	case canOpen && !canClose:
		return p.open(double)
	case canClose && !canOpen:
		return p.closeOrFallback(double)
	case canOpen && canClose:
		// Punctuation adjacent quotes close a pending pair when one exists;
		// otherwise they open a new pair.
		if idx := p.closable(double); idx >= 0 {
			return p.closeAt(idx)
		}
		return p.open(double)
	default:
		return 0, false
	}
}

// isApostrophe recognizes English word internal apostrophes (contractions and
// possessives) and, in English mode, leading elisions such as '90s and 'net.
// A pending nested quote wins over a trailing mark so it never steals CJK
// nested single quotes.
func (p *quoteProcessor) isApostrophe(before, after rune) bool {
	// Word internal apostrophe: ASCII letters or digits on both sides so it
	// never steals CJK nested single quotes.
	if (isASCIILetter(before) || unicode.IsDigit(before)) && isASCIILetter(after) {
		return true
	}
	if p.closable(false) >= 0 && !isASCIILetter(before) {
		return false
	}
	// Trailing apostrophes: plural possessives (the Smiths' dog) and slang
	// (doin' that). ASCII only, so CJK text keeps relying on pairing.
	if isASCIILetter(before) && (unicode.IsSpace(after) || unicode.IsPunct(after)) {
		return true
	}
	if p.glyphs.elisions && (unicode.IsSpace(before) || unicode.IsPunct(before)) {
		if unicode.IsDigit(after) {
			return true
		}
		switch after {
		case 't', 'e', 'n', 'l':
			return true
		}
	}
	return false
}

func (p *quoteProcessor) open(double bool) (rune, bool) {
	level := len(p.stack) + 1
	p.stack = append(p.stack, quoteFrame{double: double, level: level})
	return p.glyphs.left(level, double), true
}

// closeOrFallback closes a pending pair of the given type. Without an opener
// it emits a closing mark without changing pairing state, which gives
// unmatched quotes a deterministic result.
func (p *quoteProcessor) closeOrFallback(double bool) (rune, bool) {
	idx := p.closable(double)
	if idx < 0 {
		return p.glyphs.right(len(p.stack)+1, double), true
	}
	return p.closeAt(idx)
}

// closable returns the stack index of the frame that a quote of the given type
// may close, or -1. English pairs match by the straight quote character.
// Chinese style nests by level, so any quote closes the innermost open frame.
func (p *quoteProcessor) closable(double bool) int {
	for i := len(p.stack) - 1; i >= 0; i-- {
		if p.glyphs.alternateByLevel || p.stack[i].double == double {
			return i
		}
	}
	return -1
}

// matchingFrame returns the stack index of the innermost open frame opened by
// the same straight quote character, or -1.
func (p *quoteProcessor) matchingFrame(double bool) int {
	for i := len(p.stack) - 1; i >= 0; i-- {
		if p.stack[i].double == double {
			return i
		}
	}
	return -1
}

// closeAt pops the frame at idx and returns the closing glyph for its nesting
// level.
func (p *quoteProcessor) closeAt(idx int) (rune, bool) {
	openerDouble := p.stack[idx].double
	level := p.stack[idx].level
	p.stack = p.stack[:idx+copy(p.stack[idx:], p.stack[idx+1:])]
	return p.glyphs.right(level, openerDouble), true
}

// peekAfter returns the rune following the quote at byte offset i of segment
// segIndex, looking through transparent text segments but not barriers.
func (p *quoteProcessor) peekAfter(segIndex, i int) rune {
	for si := segIndex; si < len(p.segments); si++ {
		seg := &p.segments[si]
		if seg.kind == barrierKind {
			return ' '
		}
		start := i + 1
		if si != segIndex {
			start = 0
		}
		if start >= len(seg.raw) {
			continue
		}
		if seg.raw[start] == '\\' && start+1 < len(seg.raw) {
			start++
		}
		return util.ToRune(seg.raw, start)
	}
	return ' '
}

func (g smartQuoteGlyphs) left(level int, double bool) rune {
	if !g.alternateByLevel {
		if double {
			return g.leftDouble
		}
		return g.leftSingle
	}
	if level%2 == 1 {
		return g.leftDouble
	}
	return g.leftSingle
}

func (g smartQuoteGlyphs) right(level int, double bool) rune {
	if !g.alternateByLevel {
		if double {
			return g.rightDouble
		}
		return g.rightSingle
	}
	if level%2 == 1 {
		return g.rightDouble
	}
	return g.rightSingle
}

func isASCIILetter(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}
