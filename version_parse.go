// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// versionParseBufSize is the rune scanner's internal buffer size. It is kept
// deliberately small so that multi-byte characters routinely straddle Read
// boundaries; the scanner must reassemble them correctly.
const versionParseBufSize = 64

// syntaxError is returned by the versioned parser for malformed input. Its
// position fields identify the source location.
type syntaxError struct {
	version XMLVersion
	msg     string
	line    int
	col     int
	offset  int64
}

func (e *syntaxError) Error() string {
	return fmt.Sprintf("etree: XML %s syntax error on line %d, column %d: %s",
		e.version, e.line, e.col, e.msg)
}

// Is allows errors.Is(err, ErrXML) to succeed for every versioned syntax
// error, keeping error handling consistent with the legacy parser.
func (e *syntaxError) Is(target error) bool {
	return target == ErrXML
}

// runeEntry is one normalized rune consumed from the input, together with
// the source position of its first raw byte.
type runeEntry struct {
	r      rune
	width  int
	line   int
	col    int
	offset int64
}

// byteCountReader counts every byte pulled from the underlying reader.
type byteCountReader struct {
	r io.Reader
	n int64
}

func (c *byteCountReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// versionParser is a strict XML tokenizer and tree builder that implements
// the selected XML version's character, reference, name and line-ending
// rules. It works directly over a byte reader with a small internal buffer,
// so multi-byte UTF-8 sequences spanning Read calls are reassembled rather
// than mangled.
type versionParser struct {
	cr      *byteCountReader
	br      *bufio.Reader
	version XMLVersion

	settings ReadSettings
	maxDepth int

	depth int

	// queue holds entries pushed back via unread.
	queue []runeEntry

	line   int
	col    int
	offset int64
}

func newVersionParser(r io.Reader, settings ReadSettings) *versionParser {
	cr := &byteCountReader{r: r}
	maxDepth := settings.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	return &versionParser{
		cr:       cr,
		br:       bufio.NewReaderSize(cr, versionParseBufSize),
		version:  settings.Version,
		settings: settings,
		maxDepth: maxDepth,
		line:     1,
		col:      1,
	}
}

// syntaxf builds a syntax error at ent's position, or the current position
// when ent is nil.
func (p *versionParser) syntaxf(ent *runeEntry, format string, args ...any) error {
	e := &syntaxError{version: p.version, msg: fmt.Sprintf(format, args...)}
	if ent != nil {
		e.line, e.col, e.offset = ent.line, ent.col, ent.offset
	} else {
		e.line, e.col, e.offset = p.line, p.col, p.offset
	}
	return e
}

// raw reads one raw rune from the input and folds the line-ending sequences
// CRLF and CR (plus NEL and LS for XML 1.1) into a single U+000A entry.
func (p *versionParser) raw() (runeEntry, error) {
	ent := runeEntry{line: p.line, col: p.col, offset: p.offset}
	r, width, err := p.br.ReadRune()
	if err != nil {
		return runeEntry{}, err
	}

	switch {
	case r == '\r':
		r2, w2, e2 := p.br.ReadRune()
		switch {
		case e2 == io.EOF:
		case e2 != nil:
			return runeEntry{}, e2
		case r2 == '\n':
			width += w2
		default:
			if err := p.br.UnreadRune(); err != nil {
				return runeEntry{}, err
			}
		}
		ent.r, ent.width = '\n', width
		p.line++
		p.col = 1
		p.offset += int64(width)

	case r == '\n':
		ent.r, ent.width = '\n', width
		p.line++
		p.col = 1
		p.offset += int64(width)

	case p.version == Version11 && (r == 0x85 || r == 0x2028):
		ent.r, ent.width = '\n', width
		p.line++
		p.col = 1
		p.offset += int64(width)

	default:
		ent.r, ent.width = r, width
		p.col++
		p.offset += int64(width)
	}
	return ent, nil
}

// read returns the next normalized rune, honoring pushback.
func (p *versionParser) read() (runeEntry, error) {
	if len(p.queue) > 0 {
		e := p.queue[0]
		p.queue = p.queue[1:]
		return e, nil
	}
	return p.raw()
}

// unread pushes one entry back for the next read to return.
func (p *versionParser) unread(e runeEntry) {
	p.queue = append(p.queue, e)
}

// expect reads the next rune and fails unless it equals want.
func (p *versionParser) expect(want rune, context string) (runeEntry, error) {
	e, err := p.read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return e, p.syntaxf(&e, "unexpected end of document while reading %s", context)
		}
		return e, err
	}
	if e.r != want {
		return e, p.syntaxf(&e, "expected %q in %s, found %q", want, context, e.r)
	}
	return e, nil
}

// skipSpace consumes normalized S characters and returns the first
// non-space entry without consuming it.
func (p *versionParser) skipSpace() (runeEntry, error) {
	for {
		e, err := p.read()
		if err != nil {
			return e, err
		}
		if !versionIsSpace(e.r) {
			return e, nil
		}
	}
}

// readName reads an XML Name using the selected version's productions.
func (p *versionParser) readName(context string) (string, runeEntry, error) {
	first, err := p.read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", first, p.syntaxf(&first, "unexpected end of document while reading %s", context)
		}
		return "", first, err
	}
	if first.r == utf8.RuneError || !versionIsNameStartChar(first.r) {
		return "", first, p.syntaxf(&first, "invalid name-start character %q in %s", first.r, context)
	}
	var b strings.Builder
	b.WriteRune(first.r)
	for {
		e, err := p.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", first, err
		}
		if e.r == utf8.RuneError || !versionIsNameChar(p.version, e.r) {
			p.unread(e)
			break
		}
		b.WriteRune(e.r)
	}
	return b.String(), first, nil
}

// parse builds the document tree into the embedded document element.
func (p *versionParser) parse(docRoot *Element) error {
	// A single U+FEFF BOM is legal only at the very beginning and is
	// discarded.
	first, err := p.read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return p.syntaxf(nil, "empty document")
		}
		return err
	}
	if first.r == 0xFEFF {
		first, err = p.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return p.syntaxf(nil, "empty document")
			}
			return err
		}
	}

	var rootCount int
	current := first
	var miscSpace strings.Builder
	flushMiscSpace := func(parent *Element) {
		if miscSpace.Len() > 0 {
			newCharData(miscSpace.String(), whitespaceFlag, parent)
			miscSpace.Reset()
		}
	}
	for {
		switch {
		case versionIsSpace(current.r):
			miscSpace.WriteRune(current.r)
		case current.r == '<':
			flushMiscSpace(docRoot)
			nxt, err := p.read()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return p.syntaxf(&current, "unexpected end of document")
				}
				return err
			}
			switch nxt.r {
			case '?':
				if err := p.parseProcInst(docRoot, &rootCount); err != nil {
					return err
				}
			case '!':
				if err := p.parseMarkupDecl(docRoot, false); err != nil {
					return err
				}
			default:
				if rootCount > 0 {
					return p.syntaxf(&current, "document may contain only one root element")
				}
				if err := p.parseElement(docRoot, nxt); err != nil {
					return err
				}
				rootCount++
			}
		default:
			flushMiscSpace(docRoot)
			return p.syntaxf(&current, "unexpected character %q outside the document element", current.r)
		}

		current, err = p.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}

	if rootCount == 0 {
		return p.syntaxf(nil, "document contains no root element")
	}
	flushMiscSpace(docRoot)
	return nil
}

// parseMarkupDecl handles input beginning with "<!". Comments are accepted;
// CDATA sections are legal only within element content; every other kind of
// declaration (DOCTYPE or an internal DTD subset) is rejected.
func (p *versionParser) parseMarkupDecl(parent *Element, inElement bool) error {
	first, err := p.read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return p.syntaxf(nil, "unexpected end of document after \"<!\"")
		}
		return err
	}
	switch first.r {
	case '-':
		if _, err := p.expect('-', "comment"); err != nil {
			return err
		}
		data, err := p.parseCommentBody()
		if err != nil {
			return err
		}
		newComment(data, parent)
		return nil
	case '[':
		if inElement {
			data, err := p.parseCDataBody(first)
			if err != nil {
				return err
			}
			newCharData(data, cdataFlag, parent)
			return nil
		}
		return p.syntaxf(&first, "CDATA section outside of element content")
	default:
		p.unread(first)
		name, at, err := p.readName("declaration")
		if err != nil {
			return err
		}
		for {
			e, err := p.read()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return p.syntaxf(&at, "unterminated declaration")
				}
				return err
			}
			if e.r == '>' {
				break
			}
		}
		if strings.EqualFold(name, "DOCTYPE") {
			return ErrDTD
		}
		return p.syntaxf(&at, "declaration %q is not supported; only UTF-8 DTD-free documents are supported", name)
	}
}

// parseCommentBody reads a comment body after the opening "<!--" (which has
// already been consumed) and up to the closing "-->". It enforces the "--"
// prohibition, the rule that the body cannot end with "-", and the
// version's literal character rules.
func (p *versionParser) parseCommentBody() (string, error) {
	var b strings.Builder
	for {
		e, err := p.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", p.syntaxf(nil, "unterminated comment")
			}
			return "", err
		}
		if e.r != '-' {
			if e.r == utf8.RuneError || !versionIsChar(p.version, e.r) {
				return "", p.syntaxf(&e, "illegal character U+%04X in comment", e.r)
			}
			b.WriteRune(e.r)
			continue
		}

		// A hyphen in the body. Because the closing "-->" still needs both
		// of its hyphens, the next rune must be another hyphen.
		second, err := p.read()
		if err != nil {
			return "", err
		}
		if second.r != '-' {
			// A single hyphen is ordinary comment text.
			if second.r == utf8.RuneError || !versionIsChar(p.version, second.r) {
				return "", p.syntaxf(&second, "illegal character U+%04X in comment", second.r)
			}
			b.WriteRune(e.r)
			b.WriteRune(second.r)
			continue
		}

		// Two consecutive hyphens ("--") must be followed by '>' to close
		// the comment; anything else is the forbidden "--" sequence. The
		// body cannot end with a hyphen either: that happens when the rune
		// immediately before 'e' (already in b) is itself a hyphen.
		beforeBody := b.String()
		closer, err := p.read()
		if err != nil {
			return "", err
		}
		if closer.r != '>' {
			return "", p.syntaxf(&closer, "comment may not contain \"--\"")
		}
		if strings.HasSuffix(beforeBody, "-") {
			return "", p.syntaxf(&closer, "comment may not end with \"-\"")
		}
		return b.String(), nil
	}
}

// parseElement parses a start tag (whose first name rune has already been
// read as 'first'), its content and its matching end tag, attaching the new
// element to parent.
func (p *versionParser) parseElement(parent *Element, first runeEntry) error {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > p.maxDepth {
		return ErrMaxDepth
	}

	name, err := p.readRestOfName(first, "element name")
	if err != nil {
		return err
	}

	space, local := spaceDecompose(name)
	el := newElement(space, local, parent)

	// Attributes.
	seenAttr := make(map[[2]string]bool)
	for {
		e, err := p.skipSpace()
		if err != nil {
			return err
		}
		switch e.r {
		case '/':
			if _, err := p.expect('>', "empty element tag"); err != nil {
				return err
			}
			return nil
		case '>':
			return p.parseContent(el)
		default:
			p.unread(e)
			keySpace, keyLocal, _, key, err := p.parseAttribute(el)
			if err != nil {
				return err
			}
			id := [2]string{keySpace, keyLocal}
			if seenAttr[id] && !p.settings.PreserveDuplicateAttrs {
				return p.syntaxf(&key, "duplicate attribute %q", keySpace+":"+keyLocal)
			}
			seenAttr[id] = true
		}
	}
}

// readRestOfName continues a Name whose first rune was already consumed.
func (p *versionParser) readRestOfName(first runeEntry, context string) (string, error) {
	if first.r == utf8.RuneError || !versionIsNameStartChar(first.r) {
		return "", p.syntaxf(&first, "invalid name-start character %q in %s", first.r, context)
	}
	var b strings.Builder
	b.WriteRune(first.r)
	for {
		e, err := p.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", p.syntaxf(&first, "unexpected end of document while reading %s", context)
			}
			return "", err
		}
		if e.r == utf8.RuneError || !versionIsNameChar(p.version, e.r) {
			p.unread(e)
			return b.String(), nil
		}
		b.WriteRune(e.r)
	}
}

// parseAttribute parses one attribute (name, equals, quoted value) and adds
// it directly to element el. It returns the decomposed name, the value and
// the name's first source entry.
func (p *versionParser) parseAttribute(el *Element) (space, key, value string, first runeEntry, err error) {
	name, first, err := p.readName("attribute name")
	if err != nil {
		return "", "", "", first, err
	}

	eq, err := p.skipSpace()
	if err != nil {
		return "", "", "", first, err
	}
	if eq.r != '=' {
		return "", "", "", first, p.syntaxf(&eq, "expected \"=\" after attribute %q", name)
	}

	quote, err := p.skipSpace()
	if err != nil {
		return "", "", "", first, err
	}
	if quote.r != '"' && quote.r != '\'' {
		return "", "", "", first, p.syntaxf(&quote, "attribute value must be quoted")
	}

	value, err = p.parseAttrValue(quote.r)
	if err != nil {
		return "", "", "", first, err
	}

	space, local := spaceDecompose(name)
	el.addAttr(space, local, value)
	return space, local, value, first, nil
}

// parseAttrValue reads a quoted attribute value and applies the selected
// version's reference and literal-character rules. Line endings have
// already been normalized; tabs and line feeds are preserved literally.
func (p *versionParser) parseAttrValue(quote rune) (string, error) {
	var b strings.Builder
	for {
		e, err := p.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", p.syntaxf(nil, "unterminated attribute value")
			}
			return "", err
		}
		switch e.r {
		case quote:
			return b.String(), nil
		case '<':
			return "", p.syntaxf(&e, "literal \"<\" is not allowed in an attribute value")
		case '&':
			out, err := p.parseReference()
			if err != nil {
				return "", err
			}
			b.WriteString(out)
		default:
			if e.r == utf8.RuneError || !versionIsChar(p.version, e.r) {
				return "", p.syntaxf(&e, "illegal character U+%04X in attribute value", e.r)
			}
			if p.version == Version11 && versionIsRestricted(e.r) {
				return "", p.syntaxf(&e, "restricted character U+%04X in attribute value must be a character reference", e.r)
			}
			b.WriteRune(e.r)
		}
	}
}

// parseContent parses everything between a start tag and its matching end
// tag, creating child tokens on el.
func (p *versionParser) parseContent(el *Element) error {
	var text strings.Builder
	textOnlySpace := true
	flush := func() {
		if text.Len() == 0 {
			return
		}
		flags := charDataFlags(0)
		if textOnlySpace {
			flags = whitespaceFlag
		}
		newCharData(text.String(), flags, el)
		text.Reset()
		textOnlySpace = true
	}

	for {
		e, err := p.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return p.syntaxf(nil, "unexpected end of document inside element <%s>", el.FullTag())
			}
			return err
		}
		switch e.r {
		case '<':
			nxt, err := p.read()
			if err != nil {
				return err
			}
			switch nxt.r {
			case '/':
				flush()
				return p.parseEndTag(el)
			case '?':
				flush()
				if err := p.parseProcInst(el, nil); err != nil {
					return err
				}
			case '!':
				marker, err := p.read()
				if err != nil {
					return err
				}
				switch marker.r {
				case '-':
					flush()
					if _, err := p.expect('-', "comment"); err != nil {
						return err
					}
					data, err := p.parseCommentBody()
					if err != nil {
						return err
					}
					newComment(data, el)
				case '[':
					// CDATA: flush surrounding text and preserve the
					// section separately.
					flush()
					data, err := p.parseCDataBody(marker)
					if err != nil {
						return err
					}
					flags := charDataFlags(cdataFlag)
					if isWhitespace(data) {
						flags |= whitespaceFlag
					}
					newCharData(data, flags, el)
				default:
					flush()
					p.unread(marker)
					if err := p.parseMarkupDecl(el, true); err != nil {
						return err
					}
				}
			default:
				flush()
				if err := p.parseElement(el, nxt); err != nil {
					return err
				}
			}
		case '&':
			out, err := p.parseReference()
			if err != nil {
				return err
			}
			for _, r := range out {
				if !versionIsSpace(r) {
					textOnlySpace = false
				}
			}
			text.WriteString(out)
		default:
			if e.r == ']' {
				second, err := p.read()
				if err != nil {
					return err
				}
				if second.r == ']' {
					third, err := p.read()
					if err != nil {
						return err
					}
					if third.r == '>' {
						return p.syntaxf(&e, "element content may not contain the sequence \"]]>\"")
					}
					if !versionIsSpace(third.r) {
						textOnlySpace = false
					}
					text.WriteString("]]")
					text.WriteRune(third.r)
					continue
				}
				if !versionIsSpace(second.r) {
					textOnlySpace = false
				}
				text.WriteRune(e.r)
				text.WriteRune(second.r)
				continue
			}
			if e.r == utf8.RuneError || !versionIsChar(p.version, e.r) {
				return p.syntaxf(&e, "illegal character U+%04X in element content", e.r)
			}
			if p.version == Version11 && versionIsRestricted(e.r) {
				return p.syntaxf(&e, "restricted character U+%04X in element content must be a character reference", e.r)
			}
			if !versionIsSpace(e.r) {
				textOnlySpace = false
			}
			text.WriteRune(e.r)
		}
	}
}

// parseEndTag reads an end tag after "</" and verifies it matches el.
func (p *versionParser) parseEndTag(el *Element) error {
	name, first, err := p.readName("end tag")
	if err != nil {
		return err
	}

	e, err := p.skipSpace()
	if err != nil {
		return err
	}
	if e.r != '>' {
		return p.syntaxf(&e, "expected \">\" at end of element %q", name)
	}

	space, local := spaceDecompose(name)
	if space != el.Space || local != el.Tag {
		return p.syntaxf(&first, "end tag %q does not match start tag %q", name, el.FullTag())
	}
	return nil
}

// parseReference reads an entity or character reference after '&' and
// returns its replacement text. Entity references resolve against the
// ReadSettings entity map; the five predefined entities are always known.
func (p *versionParser) parseReference() (string, error) {
	first, err := p.read()
	if err != nil {
		return "", err
	}
	if first.r == '#' {
		return p.parseCharRef()
	}

	// Named entity.
	p.unread(first)
	name, at, err := p.readName("entity reference")
	if err != nil {
		return "", err
	}
	if _, err := p.expect(';', "entity reference"); err != nil {
		return "", err
	}

	switch name {
	case "lt":
		return "<", nil
	case "gt":
		return ">", nil
	case "amp":
		return "&", nil
	case "apos":
		return "'", nil
	case "quot":
		return "\"", nil
	}
	if p.settings.Entity != nil {
		if value, ok := p.settings.Entity[name]; ok {
			if r, _ := validateChars(p.version, value, true); r != 0 {
				return "", p.syntaxf(&at, "entity %q expands to an illegal character U+%04X", name, r)
			}
			return value, nil
		}
	}
	return "", p.syntaxf(&at, "unknown entity reference &%s;", name)
}

// parseCharRef reads a numeric character reference after "&#".
func (p *versionParser) parseCharRef() (string, error) {
	first, err := p.read()
	if err != nil {
		return "", err
	}

	hex := first.r == 'x' || first.r == 'X'
	validDigit := func(r rune) bool { return r >= '0' && r <= '9' }
	if hex {
		validDigit = func(r rune) bool {
			return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		}
	}

	var digits strings.Builder
	if !hex {
		if first.r == utf8.RuneError || !validDigit(first.r) {
			return "", p.syntaxf(&first, "malformed numeric character reference")
		}
		digits.WriteRune(first.r)
	}
	for {
		e, err := p.read()
		if err != nil {
			return "", err
		}
		if e.r == ';' {
			break
		}
		if e.r == utf8.RuneError || !validDigit(e.r) {
			return "", p.syntaxf(&first, "malformed numeric character reference")
		}
		digits.WriteRune(e.r)
	}
	if digits.Len() == 0 {
		return "", p.syntaxf(&first, "malformed numeric character reference")
	}

	base := 10
	if hex {
		base = 16
	}
	value, err := strconv.ParseUint(digits.String(), base, 32)
	if err != nil {
		return "", p.syntaxf(&first, "malformed numeric character reference")
	}
	r := rune(value)
	if r == utf8.RuneError || !versionIsCharRef(p.version, r) {
		return "", p.syntaxf(&first, "character reference U+%04X is not legal in XML %s", r, p.version)
	}
	return string(r), nil
}

// parseProcInst reads a processing instruction after "<?". When rootCount is
// non-nil the PI occurs at document level; the reserved "xml" target is
// treated as an XML declaration only when it is the very first child token.
func (p *versionParser) parseProcInst(parent *Element, rootCount *int) error {
	target, first, err := p.readName("processing instruction target")
	if err != nil {
		return err
	}

	var inst strings.Builder
	for {
		e, err := p.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return p.syntaxf(&first, "unterminated processing instruction")
			}
			return err
		}
		if e.r == '?' {
			end, err := p.read()
			if err != nil {
				return err
			}
			if end.r == '>' {
				break
			}
			inst.WriteRune('?')
			inst.WriteRune(end.r)
			continue
		}
		if e.r == utf8.RuneError || !versionIsChar(p.version, e.r) {
			return p.syntaxf(&e, "illegal character U+%04X in processing instruction", e.r)
		}
		inst.WriteRune(e.r)
	}

	data := inst.String()
	if strings.EqualFold(target, "xml") {
		// The reserved target is spelled exactly "xml"; case variants are
		// forbidden.
		if target != "xml" {
			return p.syntaxf(&first, "processing instruction target %q is reserved", target)
		}
		// The XML declaration may appear only at the start of the document.
		if rootCount == nil || len(parent.Child) != 0 {
			return p.syntaxf(&first, "processing instruction target \"xml\" is reserved")
		}
		if err := p.parseXMLDecl(data, first); err != nil {
			return err
		}
	}

	// Match encoding/xml: strip a single separating space between the
	// target and the instruction text.
	data = strings.TrimPrefix(data, " ")
	newProcInst(target, data, parent)
	return nil
}

// parseXMLDecl validates the content of the reserved <?xml ...?> processing
// instruction against the selected version.
func (p *versionParser) parseXMLDecl(inst string, at runeEntry) error {
	attrs, err := parsePseudoAttributes(inst)
	if err != nil {
		return p.syntaxf(&at, "%s", err.Error())
	}

	var sawVersion, sawStandalone bool
	for i, a := range attrs {
		switch a.key {
		case "version":
			if i != 0 {
				return p.syntaxf(&at, "\"version\" must be the first attribute in the XML declaration")
			}
			sawVersion = true
			if a.value != p.version.String() {
				return p.syntaxf(&at, "XML declaration version %q does not match selected version %q", a.value, p.version)
			}
		case "encoding":
			if i == 0 || sawStandalone {
				return p.syntaxf(&at, "\"encoding\" must follow \"version\" in the XML declaration")
			}
			if !strings.EqualFold(a.value, "UTF-8") {
				return p.syntaxf(&at, "encoding %q is not supported; only UTF-8 documents are supported", a.value)
			}
		case "standalone":
			if i == 0 {
				return p.syntaxf(&at, "\"standalone\" must follow \"version\" in the XML declaration")
			}
			sawStandalone = true
			if a.value != "yes" && a.value != "no" {
				return p.syntaxf(&at, "invalid \"standalone\" value %q", a.value)
			}
		default:
			return p.syntaxf(&at, "unknown attribute %q in XML declaration", a.key)
		}
	}
	if !sawVersion {
		return p.syntaxf(&at, "XML declaration requires a \"version\" attribute")
	}
	return nil
}

// pseudoAttr is one name="value" pair parsed from an XML declaration.
type pseudoAttr struct {
	key, value string
}

// parsePseudoAttributes parses the S* pseudo-attributes permitted in an
// XML declaration without relying on encoding/xml.
func parsePseudoAttributes(inst string) ([]pseudoAttr, error) {
	rs := []rune(inst)
	i := 0
	skipS := func() {
		for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t' || rs[i] == '\n') {
			i++
		}
	}

	var attrs []pseudoAttr
	skipS()
	for i < len(rs) {
		start := i
		for i < len(rs) && rs[i] != '=' && rs[i] != ' ' && rs[i] != '\t' && rs[i] != '\n' {
			i++
		}
		key := string(rs[start:i])
		if key == "" {
			return nil, errBadXMLDecl("invalid XML declaration")
		}
		if _, _, ok := validateNamePart(Version10, key); !ok {
			return nil, errBadXMLDecl("invalid attribute name in XML declaration")
		}
		skipS()
		if i >= len(rs) || rs[i] != '=' {
			return nil, errBadXMLDecl("missing \"=\" in XML declaration")
		}
		i++
		skipS()
		if i < len(rs) && rs[i] == '?' {
			return nil, errBadXMLDecl("attribute in XML declaration must be quoted")
		}
		if i >= len(rs) || (rs[i] != '"' && rs[i] != '\'') {
			return nil, errBadXMLDecl("attribute in XML declaration must be quoted")
		}
		quote := rs[i]
		i++
		valStart := i
		for i < len(rs) && rs[i] != quote {
			if rs[i] == '<' || rs[i] == '&' {
				return nil, errBadXMLDecl("illegal character in XML declaration attribute value")
			}
			i++
		}
		if i >= len(rs) {
			return nil, errBadXMLDecl("unterminated attribute in XML declaration")
		}
		value := string(rs[valStart:i])
		i++ // closing quote
		attrs = append(attrs, pseudoAttr{key, value})
		skipS()
	}
	return attrs, nil
}

type declError string

func (e declError) Error() string { return string(e) }

func errBadXMLDecl(msg string) error { return declError(msg) }

// parseCDataBody reads a CDATA section after the leading "<![". 'first' is
// the '[' rune, so the remaining literal "CDATA[" must follow.
func (p *versionParser) parseCDataBody(first runeEntry) (string, error) {
	for _, want := range "CDATA[" {
		if _, err := p.expect(want, "CDATA section"); err != nil {
			return "", err
		}
	}
	var b strings.Builder
	for {
		e, err := p.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", p.syntaxf(&first, "unterminated CDATA section")
			}
			return "", err
		}
		if e.r == ']' {
			second, err := p.read()
			if err != nil {
				return "", err
			}
			if second.r == ']' {
				closer, err := p.read()
				if err != nil {
					return "", err
				}
				if closer.r == '>' {
					return b.String(), nil
				}
				// "]]" not followed by ">" is ordinary CDATA text.
				if closer.r == utf8.RuneError || !versionIsChar(p.version, closer.r) {
					return "", p.syntaxf(&closer, "illegal character U+%04X in CDATA section", closer.r)
				}
				b.WriteString("]]")
				b.WriteRune(closer.r)
				continue
			}
			b.WriteRune(e.r)
			b.WriteRune(second.r)
			continue
		}
		if e.r == utf8.RuneError || !versionIsChar(p.version, e.r) {
			return "", p.syntaxf(&e, "illegal character U+%04X in CDATA section", e.r)
		}
		b.WriteRune(e.r)
	}
}
