// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode"
)

// The namespace URI bound to the "xml" prefix by the XML recommendation.
const xmlNamespaceURI = "http://www.w3.org/XML/1998/namespace"

// Errors returned by the Exclusive XML Canonicalization functions.
var (
	// ErrC14NUndeclaredPrefix is returned when an element or attribute uses a
	// namespace prefix that cannot be resolved within the element tree.
	ErrC14NUndeclaredPrefix = errors.New("etree: exclusive canonicalization: undeclared namespace prefix")

	// ErrC14NRelativeURI is returned when a namespace is declared with a
	// relative namespace URI, which is forbidden by canonical XML.
	ErrC14NRelativeURI = errors.New("etree: exclusive canonicalization: relative namespace URI")

	// ErrC14NInvalidNamespace is returned when an element carries an illegal
	// namespace declaration, such as a rebound "xmlns" or "xml" prefix, an
	// empty prefixed namespace, or duplicate declarations.
	ErrC14NInvalidNamespace = errors.New("etree: exclusive canonicalization: invalid namespace declaration")

	// ErrC14NInvalidPIName is returned when a processing instruction uses a
	// target name reserved by or forbidden under XML.
	ErrC14NInvalidPIName = errors.New("etree: exclusive canonicalization: invalid processing instruction target")
)

// ExclusiveC14N implements the Exclusive XML Canonicalization version 1.0
// algorithms defined by the W3C Recommendation
// "http://www.w3.org/2001/10/xml-exc-c14n".
//
// Unlike the WriteSettings.CanonicalText and WriteSettings.CanonicalAttrVal
// options, which only adjust character escaping during ordinary
// serialization, ExclusiveC14N performs the complete canonicalization
// algorithm: exclusive namespace context propagation, InclusiveNamespaces
// prefix lists, namespace-node and attribute sorting, character reference
// rules, and optional comment retention.
//
// An ExclusiveC14N value may be reused for multiple canonicalization calls.
type ExclusiveC14N struct {
	// WithComments selects the
	// "http://www.w3.org/2001/10/xml-exc-c14n#WithComments" algorithm when
	// true. When false (the default), comments are omitted from the output.
	WithComments bool

	// InclusivePrefixes holds the prefixes carried by an InclusiveNamespaces
	// PrefixList. The token "#default" refers to the default namespace.
	// Prefixes not in scope within the canonicalized node set are ignored.
	InclusivePrefixes []string
}

// NewExclusiveC14N creates an ExclusiveC14N using the
// "http://www.w3.org/2001/10/xml-exc-c14n#" algorithm (comments omitted).
func NewExclusiveC14N() *ExclusiveC14N {
	return &ExclusiveC14N{}
}

// NewExclusiveC14NWithComments creates an ExclusiveC14N using the
// "http://www.w3.org/2001/10/xml-exc-c14n#WithComments" algorithm.
func NewExclusiveC14NWithComments() *ExclusiveC14N {
	return &ExclusiveC14N{WithComments: true}
}

// SetInclusivePrefixes sets the namespace prefixes carried by an
// InclusiveNamespaces PrefixList. The token "#default" refers to the default
// namespace. The receiver is returned to allow chaining.
func (c *ExclusiveC14N) SetInclusivePrefixes(prefixes ...string) *ExclusiveC14N {
	c.InclusivePrefixes = prefixes
	return c
}

// CanonicalizeDocument canonicalizes the complete document. The input tree
// is never modified.
func (c *ExclusiveC14N) CanonicalizeDocument(d *Document) ([]byte, error) {
	var buf bytes.Buffer
	cw := c.newWriter(&buf)
	cw.renderDocument(d)
	if cw.err != nil {
		return nil, cw.err
	}
	return buf.Bytes(), nil
}

// CanonicalizeElement canonicalizes the subtree rooted at e. Ancestor
// context outside the subtree is read when resolving namespace bindings, but
// the input tree is never modified.
func (c *ExclusiveC14N) CanonicalizeElement(e *Element) ([]byte, error) {
	var buf bytes.Buffer
	cw := c.newWriter(&buf)
	if err := cw.validateSubtree(e); err != nil {
		return nil, err
	}
	cw.writeElementNode(e)
	if cw.err != nil {
		return nil, cw.err
	}
	return buf.Bytes(), nil
}

// WriteDocumentTo canonicalizes the complete document and writes the result
// to w. It returns the number of bytes written.
func (c *ExclusiveC14N) WriteDocumentTo(w io.Writer, d *Document) (int64, error) {
	xw := newXmlWriter(w)
	b := bufio.NewWriter(xw)
	cw := c.newWriter(b)
	cw.renderDocument(d)
	if cw.err != nil {
		return 0, cw.err
	}
	if err := b.Flush(); err != nil {
		return 0, err
	}
	return xw.bytes, nil
}

// WriteElementTo canonicalizes the subtree rooted at e and writes the result
// to w. It returns the number of bytes written.
func (c *ExclusiveC14N) WriteElementTo(w io.Writer, e *Element) (int64, error) {
	xw := newXmlWriter(w)
	b := bufio.NewWriter(xw)
	cw := c.newWriter(b)
	if err := cw.validateSubtree(e); err != nil {
		return 0, err
	}
	cw.writeElementNode(e)
	if cw.err != nil {
		return 0, cw.err
	}
	if err := b.Flush(); err != nil {
		return 0, err
	}
	return xw.bytes, nil
}

// docNodeKind identifies the type of the most recently emitted node at the
// document level, allowing the renderer to insert the line breaks required
// between adjacent processing instructions, comments, and the document
// element.
type docNodeKind int

const (
	docNodeNone docNodeKind = iota
	docNodeElement
	docNodePI
	docNodeComment
)

// c14nWriter carries the state of a single canonicalization pass.
type c14nWriter struct {
	w            Writer
	withComments bool
	inclusive    map[string]bool

	// rendered maps each prefix to the URI of the namespace declaration most
	// recently rendered on the current traversal path. The empty prefix ("")
	// represents the default namespace. A missing entry means no declaration
	// for the prefix is visible in the rendered output so far.
	rendered map[string]string

	err error

	// Document-level state.
	docPrev docNodeKind
}

func (c *ExclusiveC14N) newWriter(w Writer) *c14nWriter {
	cw := &c14nWriter{
		w:            w,
		withComments: c.WithComments,
		rendered:     make(map[string]string),
	}
	if len(c.InclusivePrefixes) > 0 {
		cw.inclusive = make(map[string]bool, len(c.InclusivePrefixes))
		for _, p := range c.InclusivePrefixes {
			cw.inclusive[p] = true
		}
	}
	return cw
}

// renderDocument canonicalizes all of the document's top-level tokens.
func (cw *c14nWriter) renderDocument(d *Document) {
	// Validate namespace declarations before producing any output.
	for _, t := range d.Child {
		if e, ok := t.(*Element); ok {
			if err := cw.validateElementNS(e); err != nil {
				cw.err = err
				return
			}
		}
	}

	for _, t := range d.Child {
		switch t := t.(type) {
		case *Element:
			cw.emitDocNewline()
			cw.writeElementNode(t)
			cw.docPrev = docNodeElement

		case *ProcInst:
			// The XML declaration is never part of the canonical form.
			if strings.EqualFold(t.Target, "xml") {
				continue
			}
			if err := validatePITarget(t.Target); err != nil {
				cw.err = err
				return
			}
			cw.emitDocNewline()
			cw.writeProcInst(t)
			cw.docPrev = docNodePI

		case *Comment:
			if !cw.withComments {
				continue
			}
			cw.emitDocNewline()
			cw.writeComment(t)
			cw.docPrev = docNodeComment

		default:
			// Character data and directives outside the document element are
			// not part of the canonical form.
		}
		if cw.err != nil {
			return
		}
	}
}

// emitDocNewline writes the separating newline required between adjacent
// document-level processing instructions, comments, and the document
// element.
func (cw *c14nWriter) emitDocNewline() {
	if cw.docPrev != docNodeNone {
		cw.w.WriteByte('\n')
	}
}

// renderedNS is a namespace declaration selected for emission on an
// element.
type renderedNS struct {
	prefix string // "" represents the default namespace
	uri    string
}

// outAttr is a non-namespace attribute prepared for emission.
type outAttr struct {
	uri    string
	local  string
	prefix string
	value  string
}

// writeElementNode writes the canonical form of e and its descendants.
func (cw *c14nWriter) writeElementNode(e *Element) {
	if cw.err != nil {
		return
	}

	// Snapshot the rendered-namespace state so it can be restored after the
	// subtree is processed.
	snapshot := make(map[string]string, len(cw.rendered))
	for k, v := range cw.rendered {
		snapshot[k] = v
	}
	defer func() { cw.rendered = snapshot }()

	// Determine the namespace declarations rendered by this element.
	nsDecls, err := cw.namespaceDecls(e)
	if err != nil {
		cw.err = err
		return
	}
	for _, ns := range nsDecls {
		cw.rendered[ns.prefix] = ns.uri
	}

	// Prepare and sort the non-namespace attributes.
	attrs, err := cw.elementAttrs(e)
	if err != nil {
		cw.err = err
		return
	}

	cw.w.WriteByte('<')
	cw.w.WriteString(e.FullTag())

	for _, ns := range nsDecls {
		cw.w.WriteByte(' ')
		if ns.prefix == "" {
			cw.w.WriteString("xmlns")
		} else {
			cw.w.WriteString("xmlns:")
			cw.w.WriteString(ns.prefix)
		}
		cw.w.WriteString(`="`)
		escapeString(cw.w, ns.uri, escapeCanonicalAttr)
		cw.w.WriteByte('"')
	}

	for _, a := range attrs {
		cw.w.WriteByte(' ')
		if a.prefix != "" {
			cw.w.WriteString(a.prefix)
			cw.w.WriteByte(':')
		}
		cw.w.WriteString(a.local)
		cw.w.WriteString(`="`)
		escapeString(cw.w, a.value, escapeCanonicalAttr)
		cw.w.WriteByte('"')
	}

	cw.w.WriteByte('>')

	for _, t := range e.Child {
		cw.writeToken(t)
		if cw.err != nil {
			return
		}
	}

	cw.w.WriteString("</")
	cw.w.WriteString(e.FullTag())
	cw.w.WriteByte('>')
}

// writeToken writes a single child token.
func (cw *c14nWriter) writeToken(t Token) {
	switch t := t.(type) {
	case *Element:
		cw.writeElementNode(t)

	case *CharData:
		// CDATA sections are emitted as ordinary character data.
		escapeString(cw.w, t.Data, escapeCanonicalText)

	case *Comment:
		if cw.withComments {
			cw.writeComment(t)
		}

	case *ProcInst:
		if err := validatePITarget(t.Target); err != nil {
			cw.err = err
			return
		}
		cw.writeProcInst(t)

	case *Directive:
		// Directives (e.g. the document type declaration) are omitted.
	}
}

// writeComment writes a canonical comment. Line endings are normalized
// first; any remaining carriage return (one originating from a character
// reference, which Go's decoder preserves verbatim) is escaped.
func (cw *c14nWriter) writeComment(c *Comment) {
	cw.w.WriteString("<!--")
	writeCommentPIBody(cw.w, normalizeLineBreaks(c.Data))
	cw.w.WriteString("-->")
}

// writeProcInst writes a canonical processing instruction. Line endings are
// normalized first; any remaining carriage return (one originating from a
// character reference) is escaped.
func (cw *c14nWriter) writeProcInst(p *ProcInst) {
	cw.w.WriteString("<?")
	cw.w.WriteString(p.Target)
	if inst := normalizeLineBreaks(p.Inst); inst != "" {
		cw.w.WriteByte(' ')
		writeCommentPIBody(cw.w, inst)
	}
	cw.w.WriteString("?>")
}

// namespaceDecls computes the namespace nodes rendered on element e under
// the exclusive canonicalization rules, in their canonical output order.
func (cw *c14nWriter) namespaceDecls(e *Element) ([]renderedNS, error) {
	candidates := make(map[string]bool)

	// The element's own prefix.
	if e.Space != "" && e.Space != "xml" {
		candidates[e.Space] = true
	}

	// Prefixes visibly utilized by the element's non-namespace attributes.
	for _, a := range e.Attr {
		if isNamespaceAttr(a) {
			continue
		}
		if a.Space != "" && a.Space != "xml" {
			candidates[a.Space] = true
		}
	}

	// Prefixes forced into scope by the InclusiveNamespaces PrefixList.
	for prefix := range cw.inclusive {
		if prefix == "#default" || prefix == "xml" {
			continue
		}
		if _, found := resolvePrefixURI(e, prefix); found {
			candidates[prefix] = true
		}
	}

	// An unprefixed element visibly utilizes the default namespace when the
	// in-scope default namespace URI is non-empty. An empty in-scope default
	// (xmlns="") only participates when a non-empty default declaration was
	// emitted earlier on the traversal path; rendering it otherwise would
	// introduce a redundant namespace node.
	defaultURI, defaultDeclared := resolveDefaultURI(e)
	if e.Space == "" && (defaultURI != "" ||
		(defaultDeclared && cw.rendered[""] != "")) {
		candidates[""] = true
	}

	prefixes := make([]string, 0, len(candidates))
	for prefix := range candidates {
		prefixes = append(prefixes, prefix)
	}
	slices.Sort(prefixes)

	decls := make([]renderedNS, 0, len(prefixes))
	for _, prefix := range prefixes {
		var uri string
		var found bool
		if prefix == "" {
			uri, found = defaultURI, defaultDeclared
		} else {
			uri, found = resolvePrefixURI(e, prefix)
		}
		if !found {
			return nil, ErrC14NUndeclaredPrefix
		}
		// Emit only when the binding differs from the nearest declaration
		// already rendered on the traversal path with the same binding.
		if rendered, ok := cw.rendered[prefix]; ok && rendered == uri {
			continue
		}
		decls = append(decls, renderedNS{prefix: prefix, uri: uri})
	}
	return decls, nil
}

// elementAttrs resolves and sorts the non-namespace attributes of e.
func (cw *c14nWriter) elementAttrs(e *Element) ([]outAttr, error) {
	attrs := make([]outAttr, 0, len(e.Attr))
	for _, a := range e.Attr {
		if isNamespaceAttr(a) {
			continue
		}
		uri, err := cw.attrNamespaceURI(e, a)
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, outAttr{
			uri:    uri,
			local:  a.Key,
			prefix: a.Space,
			value:  a.Value,
		})
	}

	slices.SortFunc(attrs, func(a, b outAttr) int {
		if a.uri != b.uri {
			return strings.Compare(a.uri, b.uri)
		}
		return strings.Compare(a.local, b.local)
	})
	return attrs, nil
}

// attrNamespaceURI resolves the namespace URI of a non-namespace
// attribute. Unprefixed attributes are always in no namespace.
func (cw *c14nWriter) attrNamespaceURI(e *Element, a Attr) (string, error) {
	switch a.Space {
	case "":
		return "", nil
	case "xml":
		return xmlNamespaceURI, nil
	default:
		uri, found := resolvePrefixURI(e, a.Space)
		if !found {
			return "", ErrC14NUndeclaredPrefix
		}
		return uri, nil
	}
}

// isNamespaceAttr reports whether a represents a namespace declaration
// attribute ("xmlns" or "xmlns:prefix").
func isNamespaceAttr(a Attr) bool {
	return a.Space == "xmlns" || (a.Space == "" && a.Key == "xmlns")
}

// resolvePrefixURI returns the namespace URI bound to prefix in the scope of
// e. found is false when the prefix is not declared.
func resolvePrefixURI(e *Element, prefix string) (uri string, found bool) {
	if prefix == "xml" {
		return xmlNamespaceURI, true
	}
	for el := e; el != nil; el = el.parent {
		for _, a := range el.Attr {
			if a.Space == "xmlns" && a.Key == prefix {
				return a.Value, true
			}
		}
	}
	return "", false
}

// resolveDefaultURI returns the default namespace URI in scope at e. An
// explicit xmlns="" is reported as the empty string with found true.
func resolveDefaultURI(e *Element) (uri string, found bool) {
	for el := e; el != nil; el = el.parent {
		for _, a := range el.Attr {
			if a.Space == "" && a.Key == "xmlns" {
				return a.Value, true
			}
		}
	}
	return "", false
}

// validateSubtree validates all namespace declarations that can influence
// the canonical form of the subtree rooted at apex, including declarations
// on ancestor elements that remain in scope. The element tree itself is not
// modified.
func (cw *c14nWriter) validateSubtree(apex *Element) error {
	// Ancestor declarations may be pulled into the subtree by visibly
	// utilized prefixes or the InclusiveNamespaces PrefixList.
	for el := apex.parent; el != nil; el = el.parent {
		if err := validateNamespaceAttrs(el); err != nil {
			return err
		}
	}
	return cw.validateElementNS(apex)
}

// validateElementNS validates e's own namespace declarations and recurses
// through its descendants.
func (cw *c14nWriter) validateElementNS(e *Element) error {
	if err := validateNamespaceAttrs(e); err != nil {
		return err
	}
	for _, t := range e.Child {
		if child, ok := t.(*Element); ok {
			if err := cw.validateElementNS(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateNamespaceAttrs validates the namespace declaration attributes
// carried by e.
func validateNamespaceAttrs(e *Element) error {
	seenPrefixes := make(map[string]bool)
	defaultSeen := false

	for _, a := range e.Attr {
		switch {
		case a.Space == "" && a.Key == "xmlns":
			if defaultSeen {
				return ErrC14NInvalidNamespace
			}
			defaultSeen = true
			if a.Value != "" && !isAbsoluteNamespaceURI(a.Value) {
				return ErrC14NRelativeURI
			}

		case a.Space == "xmlns":
			if seenPrefixes[a.Key] {
				return ErrC14NInvalidNamespace
			}
			seenPrefixes[a.Key] = true

			switch {
			case a.Key == "xmlns":
				// The xmlns prefix is itself reserved.
				return ErrC14NInvalidNamespace
			case a.Key == "xml":
				if a.Value != xmlNamespaceURI {
					return ErrC14NInvalidNamespace
				}
			case a.Value == "":
				// An empty prefixed namespace declaration is illegal.
				return ErrC14NInvalidNamespace
			case !isAbsoluteNamespaceURI(a.Value):
				return ErrC14NRelativeURI
			}
		}
	}
	return nil
}

// isAbsoluteNamespaceURI reports whether uri is an absolute URI reference
// per RFC 3986. A namespace URI must be absolute; the empty string is only
// legal for an unbinding default namespace declaration (which is handled
// separately by the caller).
func isAbsoluteNamespaceURI(uri string) bool {
	colon := strings.IndexByte(uri, ':')
	if colon <= 0 {
		return false
	}
	c := uri[0]
	if !isAlpha(c) {
		return false
	}
	for i := 1; i < colon; i++ {
		c := uri[i]
		if !isAlpha(c) && !isDigit(c) && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func isAlpha(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// validatePITarget validates a processing instruction target name. The
// target must be an XML Name without a colon and may not be the reserved
// "xml" name (the XML declaration is handled separately).
func validatePITarget(target string) error {
	if target == "" || strings.EqualFold(target, "xml") {
		return ErrC14NInvalidPIName
	}
	if strings.IndexByte(target, ':') >= 0 {
		return ErrC14NInvalidPIName
	}
	for i, r := range target {
		var ok bool
		if i == 0 {
			ok = unicode.IsLetter(r) || r == '_'
		} else {
			ok = unicode.IsLetter(r) || unicode.IsDigit(r) ||
				r == '.' || r == '-' || r == '_'
		}
		if !ok {
			return ErrC14NInvalidPIName
		}
	}
	return nil
}

// normalizeLineBreaks converts CRLF and CR sequences to LF, matching the XML
// line-ending normalization applied to character data but required manually
// for comments and processing instructions.
func normalizeLineBreaks(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' {
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			b.WriteByte('\n')
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// writeCommentPIBody writes the body of a comment or processing
// instruction. Go's XML decoder preserves the raw content of these
// constructs, including already-serialized entity and character references
// such as "&amp;" or "&#x9;", so ampersands, angle brackets, line feeds,
// and tabs must be passed through untouched. The only character that may
// not appear literally is a carriage return, which is escaped.
func writeCommentPIBody(w Writer, s string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' {
			w.WriteString("&#xD;")
		} else {
			w.WriteByte(s[i])
		}
	}
}
