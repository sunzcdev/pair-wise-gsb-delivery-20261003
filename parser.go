// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// pos is a position within the input byte stream.
type pos struct {
	byte int64 // 1-based byte offset
	line int   // 1-based line number
	col  int   // 1-based byte column on the line
}

// scanned is a rune returned by the scanner. r is the normalized rune. rawLF
// is true when the rune is a line ending that appeared raw in the input (CR,
// CR/LF or LF; NEL and LS too in XML 1.1 mode), allowing the parser to
// distinguish raw line-ending characters from character references.
type scanned struct {
	r     rune
	pos   pos
	rawLF bool
}

// scanner performs UTF-8 decoding and XML line ending normalization on top of
// a byte reader. It normalizes unescaped CR and CR/LF pairs to LF. In XML
// 1.1 mode, unescaped NEL (U+0085) and line separator (U+2028) characters
// are also normalized to LF.
type scanner struct {
	r       *bufio.Reader
	version XMLVersion

	pos pos // position of the next rune to read

	// bomDone is true once the leading bytes of the input have been checked
	// for a byte order mark.
	bomDone bool

	// push holds runes pushed back by the parser, most recent first.
	push []scanned

	bytes int64
}

func newScanner(ri io.Reader, version XMLVersion) *scanner {
	return &scanner{
		r:       bufio.NewReader(ri),
		version: version,
		pos:     pos{byte: 1, line: 1, col: 1},
	}
}

// advance moves the stream position forward over width bytes representing
// rune r, where r == '\n' indicates a line break.
func (s *scanner) advance(r rune, width int) {
	s.pos.byte += int64(width)
	if r == '\n' {
		s.pos.line++
		s.pos.col = 1
	} else {
		s.pos.col += width
	}
	s.bytes += int64(width)
}

// next returns the next normalized rune. At EOF it returns io.EOF.
func (s *scanner) next() (scanned, error) {
	if len(s.push) > 0 {
		n := len(s.push)
		sc := s.push[n-1]
		s.push = s.push[:n-1]
		return sc, nil
	}
	for {
		r, w, err := s.r.ReadRune()
		if err != nil {
			return scanned{}, err
		}
		start := s.pos

		// Collapse CR and CR/LF pairs to LF.
		if r == '\r' {
			nr, _, nerr := s.r.ReadRune()
			if nerr == nil && nr == '\n' {
				s.advance('\n', 2)
				return scanned{r: '\n', pos: start, rawLF: true}, nil
			}
			if nerr == nil {
				if uerr := s.r.UnreadRune(); uerr != nil {
					return scanned{}, uerr
				}
			}
			s.advance('\n', 1)
			return scanned{r: '\n', pos: start, rawLF: true}, nil
		}

		s.advance(r, w)

		// Skip a leading byte order mark once.
		if !s.bomDone {
			s.bomDone = true
			if start.byte == 1 && r == 0xFEFF {
				continue
			}
		}

		if r == utf8.RuneError && w == 1 {
			return scanned{}, syntaxErrorAt(start, "invalid UTF-8 encoding")
		}

		if s.version == Version11 && (r == 0x85 || r == 0x2028) {
			return scanned{r: '\n', pos: start, rawLF: true}, nil
		}
		return scanned{r: r, pos: start}, nil
	}
}

// pushBack returns sc so that it will be delivered by the next call to next.
func (s *scanner) pushBack(sc scanned) {
	s.push = append(s.push, sc)
	s.pos = sc.pos
}

// isSpace reports whether r is an XML white space character.
func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func syntaxErrorAt(p pos, msg string) error {
	return &SyntaxError{Msg: msg, Byte: p.byte, Line: p.line, Col: p.col}
}

// strictParser is a hand-written XML 1.0/1.1 parser supporting UTF-8
// documents without DTDs.
type strictParser struct {
	s        *scanner
	version  XMLVersion
	root     *Element
	maxDepth int

	// declContainer receives the XML declaration processing-instruction
	// token when one is present.
	declContainer *Element
}

// readFromStrict parses an XML 1.0/1.1 document from ri into e, which must be
// a document's embedded element.
func (e *Element) readFromStrict(ri io.Reader, settings ReadSettings) (int64, error) {
	maxDepth := settings.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}

	p := &strictParser{
		s:             newScanner(ri, settings.Version),
		version:       settings.Version,
		root:          e,
		maxDepth:      maxDepth,
		declContainer: e,
	}

	if err := p.parseDocument(); err != nil {
		return p.s.bytes, err
	}
	return p.s.bytes, nil
}

// errorf creates a syntax error at the scanner's current position.
func (p *strictParser) errorf(format string, args ...any) error {
	pos := p.s.pos
	return &SyntaxError{Msg: fmt.Sprintf(format, args...), Byte: pos.byte, Line: pos.line, Col: pos.col}
}

// errorAt creates a syntax error at a captured position.
func (p *strictParser) errorAt(pos pos, format string, args ...any) error {
	return &SyntaxError{Msg: fmt.Sprintf(format, args...), Byte: pos.byte, Line: pos.line, Col: pos.col}
}

// parseDocument parses document ::= prolog element Misc*.
func (p *strictParser) parseDocument() error {
	container := p.root

	// prolog ::= XMLDecl? Misc*
	if err := p.parseProlog(container); err != nil {
		return err
	}

	// Misc*
	for {
		sc, err := p.s.next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case isSpace(sc.r):
			p.consumeWhitespace(container, sc)
		case sc.r == '<':
			if err := p.parseMarkupEpilog(container); err != nil {
				return err
			}
		default:
			return p.errorAt(sc.pos, "content after root element")
		}
	}
}

// consumeWhitespace accumulates the run of white space beginning with first
// into a single whitespace CharData token.
func (p *strictParser) consumeWhitespace(container *Element, first scanned) {
	var b strings.Builder
	b.WriteRune(first.r)
	for {
		sc, err := p.s.next()
		if err != nil {
			break
		}
		if !isSpace(sc.r) {
			p.s.pushBack(sc)
			break
		}
		b.WriteRune(sc.r)
	}
	newCharData(b.String(), whitespaceFlag, container)
}

// parseProlog parses the optional XML declaration, optional misc and
// doctypedecl, and then the root element. On return the root element has been
// fully parsed.
func (p *strictParser) parseProlog(container *Element) error {
	declared := false

	// The XML declaration, if present, must begin at the very first
	// character of the document.
	sc, err := p.s.next()
	if err != nil {
		return err
	}
	if sc.r == '<' {
		ok, err := p.parseDeclOrRoot(container, &declared, true)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	} else if isSpace(sc.r) {
		p.consumeWhitespace(container, sc)
	} else {
		return p.errorAt(sc.pos, "content outside of root element")
	}

	for {
		sc, err := p.s.next()
		if err == io.EOF {
			return p.errorf("missing root element")
		}
		if err != nil {
			return err
		}
		switch {
		case isSpace(sc.r):
			p.consumeWhitespace(container, sc)
		case sc.r == '<':
			ok, err := p.parseDeclOrRoot(container, &declared, false)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
		default:
			return p.errorAt(sc.pos, "content outside of root element")
		}
	}
}

// parseDeclOrRoot handles a '<' encountered in the prolog. It returns true
// when the '<' introduced the root element, which has then been fully
// parsed. The first parameter reports whether the '<' is the first
// construct in the document (the only legal position of the XML
// declaration).
func (p *strictParser) parseDeclOrRoot(container *Element, declared *bool, first bool) (bool, error) {
	sc, err := p.s.next()
	if err != nil {
		return false, err
	}

	switch {
	case sc.r == '?':
		if err := p.parseProcInstProlog(container, first, declared); err != nil {
			return false, err
		}
		return false, nil
	case sc.r == '!':
		d, err := p.s.next()
		if err != nil {
			return false, err
		}
		if d.r != '-' {
			return false, p.unsupportedDecl(d.pos)
		}
		d2, err := p.s.next()
		if err != nil {
			return false, err
		}
		if d2.r != '-' {
			return false, p.unsupportedDecl(d2.pos)
		}
		if err := p.parseComment(container); err != nil {
			return false, err
		}
		return false, nil
	case isNCNameStartChar(sc.r):
		if err := p.parseElementOpen(container, sc); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, p.errorAt(sc.pos, "invalid markup declaration")
	}
}

// parseProcInstProlog parses the processing instruction immediately after
// '<?'. The reserved "xml" target is treated as the XML declaration when it
// appears as the document's first construct; it is illegal anywhere else.
func (p *strictParser) parseProcInstProlog(container *Element, first bool, declared *bool) error {
	namePos := p.s.pos
	target, _, err := p.readQName()
	if err != nil {
		return err
	}

	if strings.EqualFold(target, "xml") {
		if !first || *declared {
			return p.errorAt(namePos, "the XML declaration must appear at the start of the document")
		}
		if target != "xml" {
			return p.errorAt(namePos, "processing instruction target %q is reserved", target)
		}
		*declared = true
		return p.parseXMLDecl()
	}

	return p.parseProcInstRest(container, target, namePos)
}

// parseXMLDecl parses the body of an XML declaration after '<?xml'.
//
//	XMLDecl ::= '<?xml' S Version (S Encoding)? (S standalone)? S? '?>'
func (p *strictParser) parseXMLDecl() error {
	seen := map[string]bool{}
	var attrs []string
	needSpace := true
	for {
		if needSpace {
			if err := p.skipSpace(); err != nil {
				return err
			}
		} else {
			if err := p.skipSpaceOpt(); err != nil {
				return err
			}
		}

		sc, err := p.s.next()
		if err != nil {
			return err
		}
		if sc.r == '?' {
			gt, err := p.s.next()
			if err != nil {
				return err
			}
			if gt.r != '>' {
				return p.errorAt(gt.pos, "invalid XML declaration")
			}
			if !seen["version"] {
				return p.errorAt(gt.pos, "XML declaration missing version attribute")
			}
			newProcInst("xml", strings.Join(attrs, " "), p.declContainer)
			return nil
		}

		needSpace = false
		p.s.pushBack(sc)
		keyPos := p.s.pos
		key, _, err := p.readQName()
		if err != nil {
			return err
		}
		eq, err := p.s.next()
		if err != nil {
			return err
		}
		if eq.r != '=' {
			return p.errorAt(eq.pos, "expected '=' in XML declaration")
		}

		val, vpos, err := p.readDeclAttrValue(keyPos)
		if err != nil {
			return err
		}
		if seen[key] {
			return p.errorAt(keyPos, "duplicate %q attribute in XML declaration", key)
		}
		switch key {
		case "version":
			if len(seen) != 0 {
				return p.errorAt(keyPos, "version must be the first attribute in the XML declaration")
			}
		case "encoding":
			if _, hasStandalone := seen["standalone"]; hasStandalone {
				return p.errorAt(keyPos, "encoding must precede standalone in the XML declaration")
			}
		}
		seen[key] = true
		attrs = append(attrs, key+`="`+val+`"`)

		switch key {
		case "version":
			if val != p.version.String() {
				return p.errorAt(vpos, "XML declaration version %q does not match selected version %q",
					val, p.version)
			}
		case "encoding":
			if !strings.EqualFold(val, "UTF-8") {
				return p.errorAt(vpos, "unsupported XML encoding %q: only UTF-8 is supported", val)
			}
		case "standalone":
			if val != "yes" && val != "no" {
				return p.errorAt(vpos, "invalid standalone value %q", val)
			}
		default:
			return p.errorAt(keyPos, "unknown attribute %q in XML declaration", key)
		}
	}
}

// readDeclAttrValue reads a quoted XML declaration pseudo-attribute value.
// References and '<' are not permitted.
func (p *strictParser) readDeclAttrValue(start pos) (string, pos, error) {
	sc, err := p.s.next()
	if err != nil {
		return "", start, err
	}
	if sc.r != '"' && sc.r != '\'' {
		return "", start, p.errorAt(sc.pos, "expected quoted value in XML declaration")
	}
	quote := sc.r
	var b strings.Builder
	for {
		sc, err := p.s.next()
		if err != nil {
			return "", start, err
		}
		switch {
		case sc.r == quote:
			return b.String(), start, nil
		case sc.r == '<' || sc.r == '&':
			return "", start, p.errorAt(sc.pos, "invalid character in XML declaration value")
		case !isXMLChar(p.version, sc.r):
			return "", start, p.errorAt(sc.pos, "illegal character U+%04X in XML declaration value", sc.r)
		default:
			b.WriteRune(sc.r)
		}
	}
}

// parseComment parses a comment after '<!--'.
func (p *strictParser) parseComment(container *Element) error {
	start := p.s.pos
	var b strings.Builder
	for {
		sc, err := p.s.next()
		if err != nil {
			return p.errorAt(start, "unterminated comment")
		}
		if sc.r == '-' {
			d2, err := p.s.next()
			if err != nil {
				return p.errorAt(start, "unterminated comment")
			}
			if d2.r == '-' {
				gt, err := p.s.next()
				if err != nil {
					return p.errorAt(start, "unterminated comment")
				}
				if gt.r != '>' {
					return p.errorAt(gt.pos, "comment may not contain '--'")
				}
				newComment(b.String(), container)
				return nil
			}
			b.WriteRune('-')
			b.WriteRune(d2.r)
			continue
		}
		if !isXMLChar(p.version, sc.r) {
			return p.errorAt(sc.pos, "illegal character U+%04X in comment", sc.r)
		}
		if p.version == Version11 && isRestrictedChar11(sc.r) {
			return p.errorAt(sc.pos, "XML 1.1 restricted character U+%04X must be a character reference", sc.r)
		}
		b.WriteRune(sc.r)
	}
}

// parseProcInstRest parses the remainder of a processing instruction after
// its target name has been read.
func (p *strictParser) parseProcInstRest(container *Element, target string, namePos pos) error {
	if strings.EqualFold(target, "xml") {
		return p.errorAt(namePos, "the XML declaration must appear at the start of the document")
	}
	var b strings.Builder

	// A PI must either close immediately or begin with white space.
	sc, err := p.s.next()
	if err != nil {
		return err
	}
	if sc.r == '?' {
		gt, err := p.s.next()
		if err != nil {
			return err
		}
		if gt.r != '>' {
			return p.errorAt(gt.pos, "invalid processing instruction")
		}
		newProcInst(target, "", container)
		return nil
	}
	if !isSpace(sc.r) {
		return p.errorAt(sc.pos, "processing instruction target must be followed by white space or '?>'")
	}

	for {
		sc, err := p.s.next()
		if err != nil {
			return p.errorAt(namePos, "unterminated processing instruction")
		}
		if !isXMLChar(p.version, sc.r) {
			return p.errorAt(sc.pos, "illegal character U+%04X in processing instruction", sc.r)
		}
		if p.version == Version11 && isRestrictedChar11(sc.r) {
			return p.errorAt(sc.pos, "XML 1.1 restricted character U+%04X must be a character reference", sc.r)
		}
		if sc.r == '?' {
			gt, err := p.s.next()
			if err != nil {
				return p.errorAt(namePos, "unterminated processing instruction")
			}
			if gt.r == '>' {
				newProcInst(target, strings.TrimLeftFunc(b.String(), isSpace), container)
				return nil
			}
			b.WriteRune('?')
			b.WriteRune(gt.r)
			continue
		}
		b.WriteRune(sc.r)
	}
}

// parseMarkupEpilog handles a '<' in the epilog; only comments and
// processing instructions are permitted there.
func (p *strictParser) parseMarkupEpilog(container *Element) error {
	sc, err := p.s.next()
	if err != nil {
		return err
	}
	switch sc.r {
	case '?':
		namePos := p.s.pos
		target, _, err := p.readQName()
		if err != nil {
			return err
		}
		if strings.EqualFold(target, "xml") {
			return p.errorAt(namePos, "the XML declaration must appear at the start of the document")
		}
		return p.parseProcInstRest(container, target, namePos)
	case '!':
		d, err := p.s.next()
		if err != nil {
			return err
		}
		if d.r != '-' {
			return p.unsupportedDecl(d.pos)
		}
		d2, err := p.s.next()
		if err != nil {
			return err
		}
		if d2.r != '-' {
			return p.unsupportedDecl(d2.pos)
		}
		return p.parseComment(container)
	default:
		return p.errorAt(sc.pos, "only comments and processing instructions may follow the root element")
	}
}

// unsupportedDecl rejects DTDs and other markup declarations.
func (p *strictParser) unsupportedDecl(at pos) error {
	return &SyntaxError{
		Msg:  "document type declarations (DTDs) are not supported in strict XML mode",
		Byte: at.byte, Line: at.line, Col: at.col,
		Err: ErrUnsupportedDTD,
	}
}

// skipSpace consumes a sequence of XML white space characters and pushes
// back the first non-white space rune. At least one white space rune must be
// present.
func (p *strictParser) skipSpace() error {
	sc, err := p.s.next()
	if err != nil {
		return err
	}
	if !isSpace(sc.r) {
		return p.errorAt(sc.pos, "expected white space")
	}
	for {
		sc, err := p.s.next()
		if err != nil {
			return err
		}
		if !isSpace(sc.r) {
			p.s.pushBack(sc)
			return nil
		}
	}
}

// readQName reads an XML qualified name, permitting at most one colon. The
// first name rune has not yet been read.
func (p *strictParser) readQName() (string, pos, error) {
	start := p.s.pos
	sc, err := p.s.next()
	if err != nil {
		return "", start, err
	}
	if !isNCNameStartChar(sc.r) {
		return "", start, p.errorAt(start, "invalid XML name")
	}
	name, err := p.readNameRest(sc)
	return name, start, err
}

// readQNameFrom reads an XML qualified name whose first rune is first.
func (p *strictParser) readQNameFrom(first scanned) (string, pos, error) {
	start := first.pos
	if !isNCNameStartChar(first.r) {
		return "", start, p.errorAt(start, "invalid XML name")
	}
	name, err := p.readNameRest(first)
	return name, start, err
}

func (p *strictParser) readNameRest(first scanned) (string, error) {
	var b strings.Builder
	b.WriteRune(first.r)

	colons := 0
	for {
		sc, err := p.s.next()
		if err != nil {
			if err == io.EOF {
				return b.String(), nil
			}
			return b.String(), err
		}
		switch {
		case isNCNameChar(sc.r):
			b.WriteRune(sc.r)
		case sc.r == ':':
			colons++
			if colons > 1 {
				return "", p.errorAt(sc.pos, "invalid XML name: too many colons")
			}
			next, err := p.s.next()
			if err != nil {
				return b.String(), err
			}
			if !isNCNameStartChar(next.r) {
				return b.String(), p.errorAt(next.pos, "invalid XML name")
			}
			b.WriteRune(':')
			b.WriteRune(next.r)
		default:
			p.s.pushBack(sc)
			return b.String(), nil
		}
	}
}

// parseElementOpen parses an element start tag after the '<' has been
// consumed. first is the first rune of the element name. It pushes the
// element onto parent and leaves the element's content to be parsed.
func (p *strictParser) parseElementOpen(parent *Element, first scanned) error {
	name, namePos, err := p.readQNameFrom(first)
	if err != nil {
		return err
	}

	if depth := parentDepth(parent, p.root); depth >= p.maxDepth {
		return &SyntaxError{
			Msg:  "XML tree exceeds maximum depth of " + fmt.Sprint(p.maxDepth),
			Byte: namePos.byte, Line: namePos.line, Col: namePos.col,
			Err: ErrMaxDepth,
		}
	}

	space, tag := spaceDecompose(name)
	e := newElement(space, tag, parent)

	attrs := make(map[string]bool)
	for {
		if err := p.skipSpaceOpt(); err != nil {
			return err
		}
		sc, err := p.s.next()
		if err != nil {
			return err
		}
		switch sc.r {
		case '/':
			gt, err := p.s.next()
			if err != nil {
				return err
			}
			if gt.r != '>' {
				return p.errorAt(gt.pos, "invalid self-closing element tag")
			}
			return nil
		case '>':
			return p.parseElementContent(e)
		default:
			p.s.pushBack(sc)
			aname, apos, err := p.readQName()
			if err != nil {
				return err
			}
			if attrs[aname] {
				return p.errorAt(apos, "duplicate attribute %q", aname)
			}
			attrs[aname] = true

			if err := p.skipSpaceOpt(); err != nil {
				return err
			}
			eq, err := p.s.next()
			if err != nil || eq.r != '=' {
				if err != nil {
					return err
				}
				return p.errorAt(eq.pos, "expected '=' after attribute name")
			}
			if err := p.skipSpaceOpt(); err != nil {
				return err
			}
			val, _, err := p.readAttrValue(apos)
			if err != nil {
				return err
			}
			aspace, akey := spaceDecompose(aname)
			e.addAttr(aspace, akey, val)
		}
	}
}

// parseElementContent is invoked on an element whose start tag has been
// fully parsed and which is already bound in the tree.
func (p *strictParser) parseElementContent(e *Element) error {
	for {
		closed, err := p.parseContent(e)
		if err != nil {
			return err
		}
		if closed {
			return nil
		}
	}
}

// parseContent parses one unit of element content: character data, a
// comment, a CDATA section, a processing instruction, a child element start
// tag, or the element's own end tag. It returns closed=true when the
// element's end tag was consumed.
func (p *strictParser) parseContent(e *Element) (closed bool, err error) {
	var b strings.Builder
	allSpace := true

	flush := func() {
		if b.Len() == 0 {
			return
		}
		var flags charDataFlags
		if allSpace {
			flags = whitespaceFlag
		}
		newCharData(b.String(), flags, e)
		b.Reset()
	}

	for {
		sc, err := p.s.next()
		if err != nil {
			flush()
			if err == io.EOF {
				return false, p.errorf("unterminated element <%s>", e.FullTag())
			}
			return false, err
		}

		switch {
		case sc.r == '<':
			flush()
			return p.parseAfterLTInContent(e)
		case sc.r == '&':
			val, err := p.parseReference(sc.pos)
			if err != nil {
				return false, err
			}
			b.WriteString(val)
			if allSpace && !isSpaceString(val) {
				allSpace = false
			}
		case sc.r == ']':
			s2, err := p.s.next()
			if err != nil {
				flush()
				return false, err
			}
			if s2.r != ']' {
				p.s.pushBack(s2)
				b.WriteRune(']')
				allSpace = false
				continue
			}
			gt, err := p.s.next()
			if err != nil {
				flush()
				return false, err
			}
			if gt.r == '>' {
				flush()
				return false, p.errorAt(sc.pos,
					"element content may not contain the sequence \"]]>\"")
			}
			b.WriteString("]]")
			p.s.pushBack(s2)
			p.s.pushBack(gt)
			allSpace = false
		default:
			if !isXMLChar(p.version, sc.r) {
				return false, p.errorAt(sc.pos, "illegal character U+%04X in element content", sc.r)
			}
			if p.version == Version11 && isRestrictedChar11(sc.r) {
				return false, p.errorAt(sc.pos, "XML 1.1 restricted character U+%04X must be a character reference", sc.r)
			}
			b.WriteRune(sc.r)
			if allSpace && !isSpace(sc.r) {
				allSpace = false
			}
		}
	}
}

// parseAfterLTInContent handles the rune following a '<' within an element.
func (p *strictParser) parseAfterLTInContent(e *Element) (bool, error) {
	sc, err := p.s.next()
	if err != nil {
		return false, err
	}
	switch {
	case sc.r == '/':
		return true, p.parseEndTag(e)
	case sc.r == '!':
		d, err := p.s.next()
		if err != nil {
			return false, err
		}
		if d.r == '-' {
			d2, err := p.s.next()
			if err != nil {
				return false, err
			}
			if d2.r != '-' {
				return false, p.unsupportedDecl(d2.pos)
			}
			return false, p.parseComment(e)
		}

		// Only CDATA sections and, when rejected, DTD/markup declarations
		// begin with '<!'.
		p.s.pushBack(d)
		if isCDataOpen(p) {
			return false, p.parseCData(e)
		}
		return false, p.unsupportedDecl(d.pos)
	case sc.r == '?':
		namePos := p.s.pos
		target, _, err := p.readQName()
		if err != nil {
			return false, err
		}
		return false, p.parseProcInstRest(e, target, namePos)
	case isNCNameStartChar(sc.r):
		return false, p.parseElementOpen(e, sc)
	default:
		return false, p.errorAt(sc.pos, "invalid element content")
	}
}

// parseEndTag parses an end tag after '</'.
func (p *strictParser) parseEndTag(e *Element) error {
	name, _, err := p.readQName()
	if err != nil {
		return err
	}
	if err := p.skipSpaceOpt(); err != nil {
		return err
	}
	gt, err := p.s.next()
	if err != nil {
		return err
	}
	if gt.r != '>' {
		return p.errorAt(gt.pos, "invalid element end tag")
	}
	if name != e.FullTag() {
		return p.errorAt(gt.pos, "element end tag </%s> does not match start tag <%s>", name, e.FullTag())
	}
	return nil
}

// isCDataOpen assumes the rune immediately after '<!' has been pushed back.
// It consumes the "[CDATA[" keyword when present.
func isCDataOpen(p *strictParser) bool {
	keyword := "[CDATA["
	consumed := make([]scanned, 0, len(keyword))
	for i := 0; i < len(keyword); i++ {
		sc, err := p.s.next()
		if err != nil {
			for j := 0; j < len(consumed); j++ {
				p.s.pushBack(consumed[len(consumed)-1-j])
			}
			return false
		}
		if sc.r != rune(keyword[i]) {
			for j := 0; j < len(consumed); j++ {
				p.s.pushBack(consumed[len(consumed)-1-j])
			}
			p.s.pushBack(sc)
			return false
		}
		consumed = append(consumed, sc)
	}
	return true
}

// parseCData parses a CDATA section after the opening "<![CDATA[".
func (p *strictParser) parseCData(e *Element) error {
	start := p.s.pos
	var b strings.Builder
	for {
		sc, err := p.s.next()
		if err != nil {
			return p.errorAt(start, "unterminated CDATA section")
		}
		if !isXMLChar(p.version, sc.r) {
			return p.errorAt(sc.pos, "illegal character U+%04X in CDATA section", sc.r)
		}
		if p.version == Version11 && isRestrictedChar11(sc.r) {
			return p.errorAt(sc.pos, "XML 1.1 restricted character U+%04X must be a character reference", sc.r)
		}
		if sc.r == ']' {
			s2, err := p.s.next()
			if err != nil {
				return p.errorAt(start, "unterminated CDATA section")
			}
			if s2.r != ']' {
				b.WriteRune(']')
				p.s.pushBack(s2)
				continue
			}
			gt, err := p.s.next()
			if err != nil {
				return p.errorAt(start, "unterminated CDATA section")
			}
			if gt.r == '>' {
				newCharData(b.String(), cdataFlag, e)
				return nil
			}
			// The sequence is "]]" followed by a non-'>' rune. Keep both
			// brackets in the content and reprocess gt, then s2.
			b.WriteString("]]")
			p.s.pushBack(s2)
			p.s.pushBack(gt)
			continue
		}
		b.WriteRune(sc.r)
	}
}

// skipSpaceOpt consumes zero or more white space runes, pushing back the
// first non-white space rune.
func (p *strictParser) skipSpaceOpt() error {
	for {
		sc, err := p.s.next()
		if err != nil {
			return err
		}
		if !isSpace(sc.r) {
			p.s.pushBack(sc)
			return nil
		}
	}
}

// parentDepth returns the depth of parent relative to the document root:
// zero when parent is the document, one for its children, etc.
func parentDepth(parent, doc *Element) int {
	depth := 0
	for cur := parent; cur != doc && cur != nil; cur = cur.parent {
		depth++
	}
	return depth
}

// readAttrValue reads a quoted attribute value starting with its opening
// quote. Raw CR/LF and, in XML 1.1 mode, NEL and LS are converted to spaces
// unless they are produced by character references.
func (p *strictParser) readAttrValue(start pos) (string, pos, error) {
	sc, err := p.s.next()
	if err != nil {
		return "", start, err
	}
	if sc.r != '"' && sc.r != '\'' {
		return "", start, p.errorAt(sc.pos, "expected quoted attribute value")
	}
	quote := sc.r

	var b strings.Builder
	for {
		sc, err := p.s.next()
		if err != nil {
			return "", start, err
		}
		switch {
		case sc.r == quote:
			return b.String(), start, nil
		case sc.r == '<':
			return "", start, p.errorAt(sc.pos, "'<' is not permitted in attribute values")
		case sc.r == '&':
			val, err := p.parseReference(sc.pos)
			if err != nil {
				return "", start, err
			}
			b.WriteString(val)
		case sc.rawLF:
			// Raw line endings (CR, CR/LF, LF and, in 1.1, NEL/LS) are
			// normalized to spaces in attribute values.
			b.WriteByte(' ')
		case !isXMLChar(p.version, sc.r):
			return "", start, p.errorAt(sc.pos, "illegal character U+%04X in attribute value", sc.r)
		case p.version == Version11 && isRestrictedChar11(sc.r):
			return "", start, p.errorAt(sc.pos, "XML 1.1 restricted character U+%04X must be a character reference", sc.r)
		default:
			b.WriteRune(sc.r)
		}
	}
}

// parseReference parses a character or entity reference after the '&' has
// been consumed and returns its expansion. Raw line-ending characters are
// distinguished from referenced ones by the scanner's rawLF flag, not by
// this function.
func (p *strictParser) parseReference(amp pos) (string, error) {
	sc, err := p.s.next()
	if err != nil {
		return "", err
	}

	if sc.r == '#' {
		return p.parseCharRef(amp)
	}

	p.s.pushBack(sc)
	name, _, err := p.readQName()
	if err != nil {
		return "", err
	}
	semi, err := p.s.next()
	if err != nil {
		return "", err
	}
	if semi.r != ';' {
		return "", p.errorAt(semi.pos, "entity reference must end with ';'")
	}
	val, ok := predefinedEntities[name]
	if !ok {
		return "", p.errorAt(amp, "undefined entity %q: only the predefined XML entities are supported without a DTD", name)
	}
	return val, nil
}

// parseCharRef parses a character reference after '&#'.
func (p *strictParser) parseCharRef(amp pos) (string, error) {
	sc, err := p.s.next()
	if err != nil {
		return "", err
	}

	var (
		r      int64
		hex    bool
		ref    strings.Builder
		digits int
	)
	ref.WriteString("&#")
	if sc.r == 'x' || sc.r == 'X' {
		hex = true
		ref.WriteRune(sc.r)
	} else {
		p.s.pushBack(sc)
	}

	for {
		sc, err := p.s.next()
		if err != nil {
			return "", err
		}
		if sc.r == ';' {
			break
		}
		digits++
		var d int64
		switch {
		case sc.r >= '0' && sc.r <= '9':
			d = int64(sc.r - '0')
		case hex && sc.r >= 'a' && sc.r <= 'f':
			d = int64(sc.r - 'a' + 10)
		case hex && sc.r >= 'A' && sc.r <= 'F':
			d = int64(sc.r - 'A' + 10)
		default:
			return "", p.errorAt(sc.pos, "invalid character reference")
		}
		if hex {
			r = r*16 + d
		} else {
			r = r*10 + d
		}
		if r > 0x10FFFF {
			r = 0x10FFFF + 1
		}
		ref.WriteRune(sc.r)
	}
	ref.WriteByte(';')

	if digits == 0 {
		return "", p.errorAt(amp, "empty character reference")
	}
	if r > 0x10FFFF {
		return "", p.errorAt(amp, "character reference %s exceeds U+10FFFF", ref.String())
	}
	rr := rune(r)
	if !isXMLChar(p.version, rr) || utf16Surrogate(rr) {
		return "", p.errorAt(amp, "illegal character reference %s", ref.String())
	}
	if rr == 0xFFFE || rr == 0xFFFF {
		return "", p.errorAt(amp, "illegal character reference %s", ref.String())
	}
	if rr == 0x00 {
		return "", p.errorAt(amp, "illegal character reference %s", ref.String())
	}
	return string(rr), nil
}

// utf16Surrogate reports whether r is a UTF-16 surrogate code point.
func utf16Surrogate(r rune) bool {
	return r >= 0xD800 && r <= 0xDFFF
}

// isSpaceString reports whether s consists solely of XML white space.
func isSpaceString(s string) bool {
	for _, r := range s {
		if !isSpace(r) {
			return false
		}
	}
	return s != ""
}
