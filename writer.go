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

// writeToStrict serializes the document using strict XML 1.0 or 1.1 rules.
// The complete tree is validated before any output is produced.
func (d *Document) writeToStrict(w io.Writer) (int64, error) {
	version := d.WriteSettings.Version
	if !version.valid() {
		return 0, &TreeError{Version: version, Path: "(document)", Msg: "unknown XML version"}
	}

	if err := validateTree(version, &d.Element); err != nil {
		return 0, err
	}

	xw := newXmlWriter(w)
	b := bufio.NewWriter(xw)

	decl, declIndex, err := xmlDeclaration(version, d.Child)
	if err != nil {
		return 0, err
	}

	if _, err := b.WriteString(decl); err != nil {
		return 0, err
	}

	for i, c := range d.Child {
		if i == declIndex {
			continue
		}
		writeTokenStrict(b, c, &d.WriteSettings)
	}

	if err := b.Flush(); err != nil {
		return xw.bytes, err
	}
	return xw.bytes, nil
}

// validateTree walks the element tree and returns the first illegal value
// found for the selected XML version.
func validateTree(version XMLVersion, doc *Element) error {
	for i, c := range doc.Child {
		path := "(document)"
		switch t := c.(type) {
		case *Element:
			if err := validateElement(version, t, "/"); err != nil {
				return err
			}
		case *CharData:
			if err := validateText(version, t, path); err != nil {
				return err
			}
		case *Comment:
			if err := validateComment(version, t.Data, path); err != nil {
				return err
			}
		case *ProcInst:
			if strings.EqualFold(t.Target, "xml") {
				if i != 0 {
					return treeErr(version, path, "the XML declaration must be the first node of the document")
				}
				continue // handled by xmlDeclaration()
			}
			if err := validateProcInst(version, t, path); err != nil {
				return err
			}
		case *Directive:
			if err := validateDirective(version, t.Data, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateElement validates an element's name, attributes and descendants.
func validateElement(version XMLVersion, e *Element, parentPath string) error {
	path := parentPath + e.Tag
	if e.Space != "" {
		path = parentPath + e.Space + ":" + e.Tag
	}

	if err := validateQName(version, e.FullTag(), path); err != nil {
		return err
	}

	seen := make(map[string]bool, len(e.Attr))
	for _, a := range e.Attr {
		apath := path + "/@" + a.FullKey()
		if err := validateQName(version, a.FullKey(), apath); err != nil {
			return err
		}
		if seen[a.FullKey()] {
			return treeErr(version, apath, "duplicate attribute %q", a.FullKey())
		}
		seen[a.FullKey()] = true
		if err := validateAttrValue(version, a.Value, apath); err != nil {
			return err
		}
	}

	for _, c := range e.Child {
		switch t := c.(type) {
		case *Element:
			if err := validateElement(version, t, path+"/"); err != nil {
				return err
			}
		case *CharData:
			if err := validateText(version, t, path); err != nil {
				return err
			}
		case *Comment:
			if err := validateComment(version, t.Data, path); err != nil {
				return err
			}
		case *ProcInst:
			if err := validateProcInst(version, t, path); err != nil {
				return err
			}
		case *Directive:
			if err := validateDirective(version, t.Data, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateQName validates an XML qualified name with at most one colon.
func validateQName(version XMLVersion, name, path string) error {
	if name == "" {
		return treeErr(version, path, "empty XML name")
	}
	r, size := utf8.DecodeRuneInString(name)
	if r == utf8.RuneError && size == 1 {
		return treeErr(version, path, "invalid UTF-8 in XML name")
	}
	if !isNCNameStartChar(r) {
		return treeErr(version, path, "illegal name character U+%04X", r)
	}
	rest := name[size:]
	colons := 0
	for len(rest) > 0 {
		r, size = utf8.DecodeRuneInString(rest)
		rest = rest[size:]
		if r == ':' {
			colons++
			if colons > 1 {
				return treeErr(version, path, "invalid XML name %q: too many colons", name)
			}
			if len(rest) == 0 {
				return treeErr(version, path, "invalid XML name %q", name)
			}
			nr, ns := utf8.DecodeRuneInString(rest)
			if !isNCNameStartChar(nr) {
				return treeErr(version, path, "illegal name character U+%04X", nr)
			}
			rest = rest[ns:]
			continue
		}
		if !isNCNameChar(r) {
			return treeErr(version, path, "illegal name character U+%04X", r)
		}
	}
	return nil
}

// validateText validates character data (plain text or CDATA content).
func validateText(version XMLVersion, c *CharData, path string) error {
	tpath := path + "/text()"
	if c.IsCData() {
		tpath = path + "/cdata()"
		for _, r := range c.Data {
			if r == '\r' || (version == Version11 && (r == 0x85 || r == 0x2028)) {
				return treeErr(version, tpath,
					"line ending character U+%04X cannot be preserved in a CDATA section", r)
			}
			if err := validateContentChar(version, r, tpath, true); err != nil {
				return err
			}
		}
		return nil
	}

	for _, r := range c.Data {
		if err := validateContentChar(version, r, tpath, false); err != nil {
			return err
		}
	}
	return nil
}

// validateContentChar validates a single rune within character data.
func validateContentChar(version XMLVersion, r rune, path string, cdata bool) error {
	if !isXMLChar(version, r) {
		return treeErr(version, path, "illegal character U+%04X", r)
	}
	if version == Version11 && isRestrictedChar11(r) {
		if cdata {
			return treeErr(version, path,
				"XML 1.1 restricted character U+%04X cannot be a reference in a CDATA section", r)
		}
		return nil
	}
	return nil
}

// validateAttrValue validates an attribute value.
func validateAttrValue(version XMLVersion, val, path string) error {
	for _, r := range val {
		if r == '\r' || r == '\n' || r == '\t' || r == 0x85 || r == 0x2028 {
			continue
		}
		if !isXMLChar(version, r) {
			return treeErr(version, path, "illegal character U+%04X", r)
		}
		if version == Version11 && isRestrictedChar11(r) {
			continue
		}
	}
	return nil
}

// validateComment validates a comment's data.
func validateComment(version XMLVersion, data, path string) error {
	cpath := path + "/comment()"
	if strings.Contains(data, "--") {
		return treeErr(version, cpath, "comment may not contain '--'")
	}
	if strings.HasSuffix(data, "-") {
		return treeErr(version, cpath, "comment may not end with '-'")
	}
	for _, r := range data {
		if r == '\r' {
			return treeErr(version, cpath, "carriage return cannot be preserved in a comment")
		}
		if !isXMLChar(version, r) {
			return treeErr(version, cpath, "illegal character U+%04X", r)
		}
		if version == Version11 && isRestrictedChar11(r) {
			return treeErr(version, cpath,
				"XML 1.1 restricted character U+%04X cannot be a reference in a comment", r)
		}
	}
	return nil
}

// validateProcInst validates a processing instruction.
func validateProcInst(version XMLVersion, p *ProcInst, path string) error {
	ppath := path + "/pi(" + p.Target + ")"
	if strings.EqualFold(p.Target, "xml") {
		return treeErr(version, ppath, "the XML declaration must be validated as a document declaration")
	}
	if err := validateQName(version, p.Target, ppath); err != nil {
		return err
	}
	if strings.Contains(p.Inst, "?>") {
		return treeErr(version, ppath, "processing instruction may not contain '?>'")
	}
	for _, r := range p.Inst {
		if r == '\r' {
			return treeErr(version, ppath, "carriage return cannot be preserved in a processing instruction")
		}
		if !isXMLChar(version, r) {
			return treeErr(version, ppath, "illegal character U+%04X", r)
		}
		if version == Version11 && isRestrictedChar11(r) {
			return treeErr(version, ppath,
				"XML 1.1 restricted character U+%04X cannot be a reference in a processing instruction", r)
		}
	}
	return nil
}

// validateDirective validates DTD-level directives encountered in the tree.
// Directives other than DOCTYPE are tolerated for tree data produced by
// legacy readers, but anything beginning with "DOCTYPE" is rejected.
func validateDirective(version XMLVersion, data, path string) error {
	trimmed := strings.TrimLeft(data, " \t\r\n")
	if strings.HasPrefix(strings.ToUpper(trimmed), "DOCTYPE") {
		return &TreeError{
			Version: version,
			Path:    path,
			Msg:     "document type declarations (DTDs) are not supported in strict XML mode",
			Err:     ErrUnsupportedDTD,
		}
	}
	return nil
}

func treeErr(version XMLVersion, path, format string, args ...any) error {
	return &TreeError{Version: version, Path: path, Msg: fmt.Sprintf(format, args...)}
}

// xmlDeclaration returns the XML declaration to emit and the index of the
// existing "<?xml ...?>" processing instruction (or -1 when none exists).
// When an existing declaration is present, it must be the first child and
// must agree with the selected version and UTF-8 encoding.
func xmlDeclaration(version XMLVersion, children []Token) (string, int, error) {
	defaultDecl := `<?xml version="` + version.String() + `" encoding="UTF-8"?>`

	declIndex := -1
	for i, c := range children {
		pi, ok := c.(*ProcInst)
		if !ok {
			if _, isEl := c.(*Element); isEl {
				break
			}
			continue
		}
		if strings.EqualFold(pi.Target, "xml") {
			declIndex = i
			break
		}
	}

	if declIndex == -1 {
		return defaultDecl, -1, nil
	}
	if declIndex != 0 {
		return "", -1, &TreeError{
			Version: version,
			Path:    "(document)",
			Msg:     "the XML declaration must be the first node of the document",
		}
	}

	pi := children[declIndex].(*ProcInst)
	v, encoding, err := parseDeclPI(pi.Inst)
	if err != nil {
		return "", -1, &TreeError{Version: version, Path: "(document)", Msg: err.Error()}
	}
	if v != version.String() {
		return "", -1, &TreeError{
			Version: version,
			Path:    "(document)",
			Msg:     fmt.Sprintf("XML declaration version %q does not match write version %q", v, version),
		}
	}
	if encoding != "" && !strings.EqualFold(encoding, "UTF-8") {
		return "", -1, &TreeError{
			Version: version,
			Path:    "(document)",
			Msg:     fmt.Sprintf("unsupported XML encoding %q: only UTF-8 is supported", encoding),
		}
	}

	decl := `<?xml ` + strings.TrimSpace(pi.Inst) + `?>`
	return decl, declIndex, nil
}

// parseDeclPI parses the body of an XML declaration processing instruction
// and returns its version and encoding values.
func parseDeclPI(inst string) (version, encoding string, err error) {
	s := strings.TrimSpace(inst)
	seen := map[string]bool{}

	for s != "" {
		s = strings.TrimLeft(s, " \t\r\n")
		if s == "" {
			break
		}

		key, rest, ok := cutName(s)
		if !ok {
			return "", "", fmt.Errorf("invalid XML declaration")
		}
		rest = strings.TrimLeft(rest, " \t\r\n")
		if !strings.HasPrefix(rest, "=") {
			return "", "", fmt.Errorf("invalid XML declaration")
		}
		rest = strings.TrimLeft(rest[1:], " \t\r\n")
		if rest == "" || (rest[0] != '"' && rest[0] != '\'') {
			return "", "", fmt.Errorf("invalid XML declaration")
		}
		quote := rest[0]
		end := strings.IndexByte(rest[1:], quote)
		if end < 0 {
			return "", "", fmt.Errorf("invalid XML declaration")
		}
		val := rest[1 : 1+end]
		s = rest[1+end+1:]

		switch key {
		case "version":
			if len(seen) != 0 {
				return "", "", fmt.Errorf("version must be the first attribute in the XML declaration")
			}
		case "encoding":
			if seen["standalone"] {
				return "", "", fmt.Errorf("encoding must precede standalone in the XML declaration")
			}
		}
		if seen[key] {
			return "", "", fmt.Errorf("duplicate %q attribute in XML declaration", key)
		}
		seen[key] = true

		switch key {
		case "version":
			version = val
		case "encoding":
			encoding = val
		case "standalone":
			if val != "yes" && val != "no" {
				return "", "", fmt.Errorf("invalid standalone value %q", val)
			}
		default:
			return "", "", fmt.Errorf("unknown attribute %q in XML declaration", key)
		}
	}

	if !seen["version"] {
		return "", "", fmt.Errorf("XML declaration missing version attribute")
	}
	return version, encoding, nil
}

// cutName splits s into a leading XML name and the remainder.
func cutName(s string) (name, rest string, ok bool) {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if i == 0 && !isNCNameStartChar(r) {
			return "", "", false
		}
		if i > 0 && !isNCNameChar(r) && r != ':' {
			break
		}
		i += size
	}
	if i == 0 {
		return "", "", false
	}
	return s[:i], s[i:], true
}

// writeTokenStrict serializes a single token using strict version rules.
func writeTokenStrict(w Writer, t Token, s *WriteSettings) {
	switch t := t.(type) {
	case *Element:
		writeElementStrict(w, t, s)
	case *CharData:
		writeCharDataStrict(w, t, s)
	case *Comment:
		w.WriteString("<!--")
		sanitizeComment(w, t.Data)
		w.WriteString("-->")
	case *ProcInst:
		w.WriteString("<?")
		sanitizeProcInst(w, t.Target)
		if t.Inst != "" {
			w.WriteByte(' ')
			sanitizeProcInst(w, t.Inst)
		}
		w.WriteString("?>")
	case *Directive:
		w.WriteString("<!")
		sanitizeDirective(w, t.Data)
		w.WriteString(">")
	}
}

// writeElementStrict serializes an element using strict version rules.
func writeElementStrict(w Writer, e *Element, s *WriteSettings) {
	w.WriteByte('<')
	w.WriteString(e.FullTag())
	for i := range e.Attr {
		a := &e.Attr[i]
		w.WriteByte(' ')
		writeAttrStrict(w, a, s)
	}
	if len(e.Child) > 0 {
		w.WriteByte('>')
		for _, c := range e.Child {
			writeTokenStrict(w, c, s)
		}
		w.WriteString("</")
		w.WriteString(e.FullTag())
		w.WriteByte('>')
	} else {
		if s.CanonicalEndTags {
			w.WriteString("></")
			w.WriteString(e.FullTag())
			w.WriteByte('>')
		} else {
			w.WriteString("/>")
		}
	}
}

// writeAttrStrict serializes an attribute using strict version rules.
func writeAttrStrict(w Writer, a *Attr, s *WriteSettings) {
	w.WriteString(a.FullKey())
	if s.AttrSingleQuote {
		w.WriteString(`='`)
	} else {
		w.WriteString(`="`)
	}
	m := escapeNormal
	if s.CanonicalAttrVal && !s.AttrSingleQuote {
		m = escapeCanonicalAttr
	}
	escapeAttrStrict(w, a.Value, s.Version, m, s.AttrSingleQuote)
	if s.AttrSingleQuote {
		w.WriteByte('\'')
	} else {
		w.WriteByte('"')
	}
}

// writeCharDataStrict serializes character data using strict version rules.
func writeCharDataStrict(w Writer, c *CharData, s *WriteSettings) {
	if c.IsCData() {
		w.WriteString(`<![CDATA[`)
		sanitizeCData(w, c.Data)
		w.WriteString(`]]>`)
		return
	}
	m := escapeNormal
	if s.CanonicalText {
		m = escapeCanonicalText
	}
	escapeTextStrict(w, c.Data, s.Version, m)
}

// escapeTextStrict writes element text using strict XML version escaping.
func escapeTextStrict(w Writer, s string, version XMLVersion, m escapeMode) {
	var esc []byte
	last := 0
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		i += width
		switch {
		case r == '&':
			esc = []byte("&amp;")
		case r == '<':
			esc = []byte("&lt;")
		case r == '>':
			if m == escapeCanonicalAttr {
				continue
			}
			esc = []byte("&gt;")
		case r == '\'':
			if m != escapeNormal {
				continue
			}
			esc = []byte("&apos;")
		case r == '"':
			if m == escapeCanonicalText {
				continue
			}
			esc = []byte("&quot;")
		case r == '\r':
			// CR must always be a character reference so it survives line
			// ending normalization.
			esc = []byte("&#xD;")
		case r == 0x85:
			// NEL is normalized to LF by an XML 1.1 parser, so escape it to
			// preserve it; in XML 1.0 escape for consistent round-tripping.
			esc = []byte("&#x85;")
		case r == 0x2028 && version == Version11:
			esc = []byte("&#x2028;")
		case version == Version11 && isRestrictedChar11(r):
			esc = charRef(r)
		default:
			continue
		}
		w.WriteString(s[last : i-width])
		w.Write(esc)
		last = i
	}
	w.WriteString(s[last:])
}

// escapeAttrStrict writes an attribute value using strict XML version
// escaping.
func escapeAttrStrict(w Writer, s string, version XMLVersion, m escapeMode, singleQuote bool) {
	var esc []byte
	last := 0
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		i += width
		switch {
		case r == '&':
			esc = []byte("&amp;")
		case r == '<':
			esc = []byte("&lt;")
		case r == '"':
			if singleQuote {
				continue
			}
			esc = []byte("&quot;")
		case r == '\'':
			if singleQuote {
				esc = []byte("&apos;")
			} else if m == escapeCanonicalAttr {
				continue
			} else {
				esc = []byte("&apos;")
			}
		case r == '\t':
			if m != escapeCanonicalAttr {
				continue
			}
			esc = []byte("&#x9;")
		case r == '\n':
			esc = []byte("&#xA;")
		case r == '\r':
			esc = []byte("&#xD;")
		case r == 0x85:
			esc = []byte("&#x85;")
		case r == 0x2028:
			esc = []byte("&#x2028;")
		case version == Version11 && isRestrictedChar11(r):
			esc = charRef(r)
		default:
			continue
		}
		w.WriteString(s[last : i-width])
		w.Write(esc)
		last = i
	}
	w.WriteString(s[last:])
}

// charRef renders r as a lowercase hexadecimal character reference.
func charRef(r rune) []byte {
	const hex = "0123456789abcdef"
	return []byte(fmt.Sprintf("&#x%x;", r))
}
