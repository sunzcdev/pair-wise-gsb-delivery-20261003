package extension

import (
	"sort"
	"unicode"

	gast "github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/text"
)

// SmartQuotesMode selects the output style of paragraph scoped smart quotes.
// The zero value, SmartQuotesDisabled, keeps the legacy token-wise typographer
// behaviour untouched.
type SmartQuotesMode int

const (
	// SmartQuotesDisabled preserves the original token-wise substitutions of
	// the Typographer extension.
	SmartQuotesDisabled SmartQuotesMode = iota

	// SmartQuotesEnglish outputs English curly quotes:
	// double “ ”, single ‘ ’ and apostrophe ’.
	SmartQuotesEnglish

	// SmartQuotesChinese outputs Chinese corner quotes:
	// double 「 」, single 『 』 and apostrophe ’.
	SmartQuotesChinese
)

type smartQuoteSet struct {
	leftDouble  string
	rightDouble string
	leftSingle  string
	rightSingle string
	apostrophe  string
}

var (
	englishQuoteSet = smartQuoteSet{
		leftDouble:  "\u201c", // “
		rightDouble: "\u201d", // ”
		leftSingle:  "\u2018", // ‘
		rightSingle: "\u2019", // ’
		apostrophe:  "\u2019", // ’
	}
	chineseQuoteSet = smartQuoteSet{
		leftDouble:  "\u300c", // 「
		rightDouble: "\u300d", // 」
		leftSingle:  "\u300e", // 『
		rightSingle: "\u300f", // 』
		apostrophe:  "\u2019", // ’
	}
)

func (m SmartQuotesMode) quoteSet() (smartQuoteSet, bool) {
	switch m {
	case SmartQuotesEnglish:
		return englishQuoteSet, true
	case SmartQuotesChinese:
		return chineseQuoteSet, true
	default:
		return smartQuoteSet{}, false
	}
}

// WithSmartQuotes enables paragraph scoped quote pairing. Pairing state flows
// through emphasis and link labels but never crosses paragraphs, code spans or
// raw HTML. Only rendered text nodes are rewritten; URLs, code and node
// attributes stay unchanged.
func WithSmartQuotes(mode SmartQuotesMode) TypographerParserOption {
	return func(c *typographerConfig) {
		c.SmartQuotes = mode
	}
}

// NewSmartQuotesParser returns a parser.Extension that performs paragraph
// scoped smart quote substitution in the given output style.
func NewSmartQuotesParser(mode SmartQuotesMode) parser.Extension {
	return NewTypographerParser(WithSmartQuotes(mode))
}

type quoteKind uint8

const (
	doubleQuote quoteKind = iota
	singleQuote
)

// runeRef addresses one rune of a text node. Only ASCII quote and apostrophe
// runes can carry a substitution glyph.
type runeRef struct {
	node  *gast.Text
	start int    // byte offset inside the original node value
	r     rune   // the addressed rune
	glyph string // substitution glyph; empty keeps the ASCII byte
	k     quoteKind
}

// section is a barrier-delimited run of inline text. Code spans, autolinks,
// images and raw HTML each start a fresh section, so pairing cannot cross
// them.
type section struct {
	refs []runeRef
}

type smartQuotesTransformer struct {
	mode SmartQuotesMode
}

func (t *smartQuotesTransformer) Transform(doc *gast.Document, reader text.Reader, _ parser.Context) {
	quotes, ok := t.mode.quoteSet()
	if !ok {
		return
	}
	source := reader.Source()
	decoder := reader.Decoder()
	for child := doc.FirstChild(); child != nil; child = child.NextSibling() {
		transformBlock(child, source, decoder, quotes)
	}
}

// transformBlock collects and rewrites one block subtree. Every block starts
// with fresh sections and pairing stacks, so a quote opened in one paragraph
// can never close inside another.
func transformBlock(node gast.Node, source []byte, decoder text.Decoder, quotes smartQuoteSet) {
	if block, isBlock := node.(gast.BlockNode); isBlock {
		// Indented/fenced code blocks and raw HTML blocks stay untouched.
		if block.Kind() == gast.KindCodeBlock || block.Kind() == gast.KindHTMLBlock {
			return
		}
	}

	sections := []*section{{}}

	var walk func(gast.Node)
	walk = func(n gast.Node) {
		barrier := isBarrier(n)

		if barrier {
			// A barrier splits the run and its subtree is never rewritten:
			// open quotes on its left cannot be closed by text on the right.
			// The first fresh section stands for the skipped barrier itself.
			sections = append(sections, &section{})
			sections = append(sections, &section{})
			return
		} else if tn, ok := n.(*gast.Text); ok {
			appendTextRefs(tn, tn.Value.Value(source), sections)
		}

		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			if _, isBlock := child.(gast.BlockNode); isBlock {
				transformBlock(child, source, decoder, quotes)
				continue
			}
			walk(child)
		}
	}

	walk(node)

	var edited []*runeRef
	for _, sec := range sections {
		edited = append(edited, pairSection(sec, quotes)...)
	}
	applyChanges(edited, source, decoder)
}

// isBarrier reports nodes whose contents cannot participate in quote pairing
// and which sever pairing between the surrounding text.
func isBarrier(n gast.Node) bool {
	switch n.Kind() {
	case gast.KindCodeSpan, gast.KindRawHTML, gast.KindAutoLink, gast.KindImage:
		return true
	}
	return false
}

func appendTextRefs(node *gast.Text, value string, sections []*section) {
	sec := sections[len(sections)-1]
	for offset, r := range value {
		ref := runeRef{node: node, start: offset, r: r}
		switch r {
		case '"':
			ref.k = doubleQuote
		case '\'':
			ref.k = singleQuote
		}
		sec.refs = append(sec.refs, ref)
	}
}

// pairSection assigns glyphs to the markers of one barrier run using local
// open-quote stacks, and returns the substituted markers. It performs no
// shared or global mutation.
func pairSection(sec *section, q smartQuoteSet) []*runeRef {
	var open [2][]int // marker indices per quote kind
	var edited []*runeRef

	for i := range sec.refs {
		ref := &sec.refs[i]
		if ref.r != '"' && ref.r != '\'' {
			continue
		}

		prev, next := neighbors(sec.refs, i)
		k := int(ref.k)

		switch decideMarker(ref, sec.refs, i, prev, next, len(open[k]) > 0, q) {
		case roleOpen:
			open[k] = append(open[k], i)
			ref.glyph = openingGlyph(ref.k, q)
			edited = append(edited, ref)
		case roleClose:
			open[k] = open[k][:len(open[k])-1]
			ref.glyph = closingGlyph(ref.k, q)
			edited = append(edited, ref)
		case roleApostrophe:
			ref.glyph = q.apostrophe
			edited = append(edited, ref)
		}
	}
	return edited
}

type markerRole int

const (
	rolePlain markerRole = iota
	roleOpen
	roleClose
	roleApostrophe
)

// decideMarker fixes the nesting, unclosed and consecutive quote rules:
//   - an open quote always matches the nearest still-open quote of the same
//     kind, enabling “...‘...’...”;
//   - a quote that cannot open or close remains ASCII;
//   - consecutive openers simply stack and are closed LIFO.
func decideMarker(ref *runeRef, refs []runeRef, i int, prev, next rune, hasOpen bool, q smartQuoteSet) markerRole {
	prime := isPrime(refs, i)
	canOpen := isOpeningContext(prev, next) && !prime
	canClose := isClosingContext(prev, next)

	if ref.k == singleQuote {
		// An apostrophe is attached to a word and therefore takes precedence
		// over a possible nested quote close: Alice's inside quotes.
		if isInternalApostrophe(prev, next) || isLeadingElision(refs, i) {
			return roleApostrophe
		}
		// A matching open single quote then closes as nested punctuation.
		if hasOpen && canClose {
			return roleClose
		}
		// A trailing single quote after a word with no opener is an
		// apostrophe (doin', Smiths'); primes stay ASCII.
		if !prime && isTrailingApostrophe(prev, next) {
			return roleApostrophe
		}
	}

	// A prime can never open; the first of a doubled prime may still close.
	if prime && !canClose {
		return rolePlain
	}
	if hasOpen && canClose {
		return roleClose
	}
	if canOpen {
		return roleOpen
	}
	return rolePlain
}

func neighbors(refs []runeRef, i int) (prev, next rune) {
	if i > 0 {
		prev = refs[i-1].r
	}
	if i+1 < len(refs) {
		next = refs[i+1].r
	}
	return prev, next
}

func openingGlyph(k quoteKind, q smartQuoteSet) string {
	if k == doubleQuote {
		return q.leftDouble
	}
	return q.leftSingle
}

func closingGlyph(k quoteKind, q smartQuoteSet) string {
	if k == doubleQuote {
		return q.rightDouble
	}
	return q.rightSingle
}

// isInternalApostrophe recognises apostrophes inside or ending a word:
// Alice's, don't, 90's, Smiths' and doin' style elisions.
func isInternalApostrophe(prev, next rune) bool {
	if isWordLetter(prev) {
		return isWordLetter(next) || next == 0 || isBoundary(next)
	}
	return unicode.IsDigit(prev) && isWordLetter(next)
}

// isTrailingApostrophe covers a bare right-flanking single quote with no
// opener, e.g. rock 'n' roll handled by isLeadingElision instead.
func isTrailingApostrophe(prev, next rune) bool {
	return isWordLetter(prev) && (next == 0 || isBoundary(next))
}

// isLeadingElision recognises fixed leading-elision shapes: a decade ('90s)
// or one of the conventional elided words ('twas, 'tis, 'em, 'net, 'n').
func isLeadingElision(refs []runeRef, i int) bool {
	prev, _ := neighbors(refs, i)
	if prev != 0 && !isBreak(prev) {
		return false
	}
	word, ok := readWordAfter(refs, i)
	if !ok {
		return false
	}
	// Decades such as '90s.
	if len(word) >= 3 && isAllDigits(word[:2]) && word[2] == 's' {
		return true
	}
	switch word {
	case "twas", "tis", "em", "net", "n":
		return true
	}
	return false
}

// readWordAfter returns the maximal letter/digit run directly following the
// marker, stopping at a boundary or another quote marker.
func readWordAfter(refs []runeRef, i int) (string, bool) {
	var word []rune
	for j := i + 1; j < len(refs); j++ {
		r := refs[j].r
		if r == '"' || r == '\'' || isBreak(r) {
			break
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			break
		}
		word = append(word, r)
	}
	if len(word) == 0 {
		return "", false
	}
	return string(word), true
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
}

// isPrime reports whether the marker is a feet/inches mark embedded in a
// measurement, which must never become an opening quote (5'6", 21"). The
// second mark of a doubled prime (21"") is also a prime.
func isPrime(refs []runeRef, i int) bool {
	prev, next := neighbors(refs, i)
	if !unicode.IsDigit(prev) {
		// Doubled terminating prime: the quote right after another prime.
		return i >= 2 && (prev == '"' || prev == '\'') && unicode.IsDigit(refs[i-2].r)
	}
	// 5'6": the mark stands between digits, so it cannot open.
	if unicode.IsDigit(next) {
		return true
	}
	// The first of a doubled terminating prime: digit + quote + quote.
	return (next == '"' || next == '\'') && i+2 < len(refs)
}

// isOpeningContext is the CommonMark left-flanking rule with CJK ideographs
// treated as boundary characters.
func isOpeningContext(prev, next rune) bool {
	if next == 0 || unicode.IsSpace(next) {
		return false
	}
	if prev == 0 || unicode.IsSpace(prev) {
		return true
	}
	if isBoundary(prev) {
		return true
	}
	// Prev is a word rune: open only before a boundary or CJK rune.
	return isBoundary(next) || isCJK(next) || isCJK(prev)
}

// isClosingContext is the CommonMark right-flanking rule with CJK ideographs
// treated as boundary characters.
func isClosingContext(prev, next rune) bool {
	if prev == 0 || unicode.IsSpace(prev) {
		return false
	}
	if next == 0 || unicode.IsSpace(next) {
		return true
	}
	if isBoundary(next) {
		return true
	}
	// Next is a word rune: close only after a boundary or CJK rune.
	return isBoundary(prev) || isCJK(prev) || isCJK(next)
}

// isBoundary reports punctuation or CJK typographic/ideographic punctuation.
func isBoundary(r rune) bool {
	if r == 0 {
		return true
	}
	return unicode.IsPunct(r)
}

// isBreak reports whitespace or punctuation, the word boundary used by the
// elision rules.
func isBreak(r rune) bool {
	return r == 0 || unicode.IsSpace(r) || unicode.IsPunct(r)
}

// isWordLetter reports a Latin-style word letter. CJK ideographs and kana act
// as flanking boundaries instead.
func isWordLetter(r rune) bool {
	return r != 0 && unicode.IsLetter(r) && !isCJK(r)
}

// isCJK reports whether the rune is an East Asian ideograph, kana or
// typographic punctuation.
func isCJK(r rune) bool {
	switch {
	case r >= 0x2e80 && r <= 0x2fff:
		return true
	case r >= 0x3000 && r <= 0x303f:
		return true
	case r >= 0x3040 && r <= 0x30ff:
		return true
	case r >= 0x3400 && r <= 0x4dbf:
		return true
	case r >= 0x4e00 && r <= 0x9fff:
		return true
	case r >= 0xf900 && r <= 0xfaff:
		return true
	case r >= 0xff00 && r <= 0xffef:
		return true
	}
	return false
}

// nodeEdit is one substitution inside a text node, recorded against the
// original byte offset so multiple edits can be applied in one rebuild.
type nodeEdit struct {
	start int
	glyph string
}

// applyChanges groups substitutions by text node and rebuilds each affected
// value once, using original byte offsets. The source slice is never
// mutated and node flags are preserved.
func applyChanges(edited []*runeRef, source []byte, decoder text.Decoder) {
	byNode := map[*gast.Text][]nodeEdit{}
	var order []*gast.Text
	for _, ref := range edited {
		if _, seen := byNode[ref.node]; !seen {
			order = append(order, ref.node)
		}
		byNode[ref.node] = append(byNode[ref.node], nodeEdit{start: ref.start, glyph: ref.glyph})
	}
	for _, node := range order {
		edits := byNode[node]
		sort.Slice(edits, func(a, b int) bool { return edits[a].start < edits[b].start })
		original := node.Value.Value(source)
		var rebuilt []byte
		cursor := 0
		for _, e := range edits {
			if e.start < cursor || e.start >= len(original) {
				continue
			}
			if original[e.start] != '\'' && original[e.start] != '"' {
				continue
			}
			rebuilt = append(rebuilt, original[cursor:e.start]...)
			rebuilt = append(rebuilt, e.glyph...)
			cursor = e.start + 1
		}
		if cursor == 0 {
			continue
		}
		rebuilt = append(rebuilt, original[cursor:]...)
		node.Value = text.NewSingleLineValue(string(rebuilt), decoder)
	}
}
