package etree

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
)

// Namespace URIs with special meaning to the canonicalization algorithm.
const (
	xmlNamespaceURI   = "http://www.w3.org/XML/1998/namespace"
	xmlnsNamespaceURI = "http://www.w3.org/2000/xmlns/"
)

// ErrC14NRelativeNamespace is returned by the ExcC14N functions when a
// namespace declaration uses a relative URI reference. The Exclusive XML
// Canonicalization specification forbids relative namespace URIs in its
// input.
var ErrC14NRelativeNamespace = errors.New("etree: exclusive canonicalization forbids relative namespace URIs")

// ErrC14NForbiddenBinding is returned by the ExcC14N functions when a
// namespace declaration binds a prefix or URI reserved by the XML namespaces
// specification (e.g., the "xml" or "xmlns" prefixes, or an empty URI bound
// to a non-default prefix).
var ErrC14NForbiddenBinding = errors.New("etree: namespace declaration binds a reserved prefix or URI")

// ErrC14NUnboundPrefix is returned by the ExcC14N functions when an element
// or attribute uses a namespace prefix that is not bound to a URI.
var ErrC14NUnboundPrefix = errors.New("etree: namespace prefix is not bound to a URI")

// ExcC14NSettings determine the behavior of the ExcC14N functions, which
// produce the Exclusive XML Canonicalization 1.0 form of an XML document or
// element subtree. See: https://www.w3.org/TR/xml-exc-c14n/
type ExcC14NSettings struct {
	// WithComments causes comments to be preserved in the canonical
	// output. When false, comments are omitted. Default: false.
	WithComments bool

	// InclusivePrefixes lists namespace prefixes to be treated as
	// "inclusive" during canonicalization, corresponding to the
	// InclusiveNamespaces PrefixList parameter described by the Exclusive
	// XML Canonicalization specification. A prefix on this list is rendered
	// whenever it is in scope, even if it is not visibly utilized by the
	// canonicalized nodes. The special value "#default" refers to the
	// default namespace. Default: nil.
	InclusivePrefixes []string
}

// ExcC14N returns the Exclusive XML Canonicalization 1.0 form of this
// document, as described by https://www.w3.org/TR/xml-exc-c14n/. The
// document's XML declaration, directives, and character data outside the
// root element are not part of the canonical form. Processing instructions
// and comments (if preserved) appearing before the root element are each
// followed by a newline character, and those appearing after the root
// element are each preceded by one. The document is not modified by the
// canonicalization process.
func (d *Document) ExcC14N(settings *ExcC14NSettings) ([]byte, error) {
	c := newExcC14N(settings)
	rootSeen := false
	for _, t := range d.Child {
		switch t := t.(type) {
		case *Element:
			if err := c.element(t, nil); err != nil {
				return nil, err
			}
			rootSeen = true
		case *Comment:
			if !c.comments {
				continue
			}
			if rootSeen {
				c.buf.WriteByte('\n')
			}
			c.comment(t)
			if !rootSeen {
				c.buf.WriteByte('\n')
			}
		case *ProcInst:
			if strings.EqualFold(t.Target, "xml") {
				continue // the XML declaration is not canonicalized
			}
			if rootSeen {
				c.buf.WriteByte('\n')
			}
			c.procInst(t)
			if !rootSeen {
				c.buf.WriteByte('\n')
			}
		}
	}
	return c.buf.Bytes(), nil
}

// ExcC14N returns the Exclusive XML Canonicalization 1.0 form of the subtree
// rooted at this element, as described by https://www.w3.org/TR/xml-exc-c14n/.
// Namespace declarations in scope at the element, including those declared
// by its ancestors, are taken into account: declarations for prefixes that
// are visibly utilized within the subtree (or listed in the settings'
// InclusivePrefixes) are re-emitted within the canonical output, so the
// result remains valid even after the subtree is detached from its
// ancestors. The element tree is not modified by the canonicalization
// process.
func (e *Element) ExcC14N(settings *ExcC14NSettings) ([]byte, error) {
	c := newExcC14N(settings)
	if err := c.element(e, nil); err != nil {
		return nil, err
	}
	return c.buf.Bytes(), nil
}

// excC14N holds the state of an exclusive canonicalization operation.
type excC14N struct {
	buf       bytes.Buffer
	comments  bool
	inclusive map[string]bool
}

// newExcC14N creates an exclusive canonicalization state object configured
// by the provided settings.
func newExcC14N(s *ExcC14NSettings) *excC14N {
	c := &excC14N{}
	if s != nil {
		c.comments = s.WithComments
		if len(s.InclusivePrefixes) > 0 {
			c.inclusive = make(map[string]bool, len(s.InclusivePrefixes))
			for _, p := range s.InclusivePrefixes {
				c.inclusive[p] = true
			}
		}
	}
	return c
}

// element writes the canonical form of the element 'e' and its descendants
// to the canonicalization buffer. The 'rendered' map records the namespace
// prefix-to-URI bindings already rendered by ancestors of 'e' within the
// canonical output.
func (c *excC14N) element(e *Element, rendered map[string]string) error {
	scope, err := excC14NScope(e)
	if err != nil {
		return err
	}

	// Determine the namespace prefixes visibly utilized by this element
	// and its attributes. An unprefixed element visibly utilizes the
	// default namespace; unprefixed attributes utilize no namespace.
	utilized := make(map[string]bool)
	utilized[e.Space] = true
	for _, a := range e.Attr {
		if a.Space != "" && a.Space != "xmlns" {
			utilized[a.Space] = true
		}
	}

	// out records the bindings rendered within the canonical output so
	// far, starting from the bindings rendered by output ancestors.
	out := maps.Clone(rendered)
	if out == nil {
		out = make(map[string]string)
	}

	var decls []string
	render := func(prefix string) {
		if prefix == "xml" {
			return // the xml prefix is implicitly bound and never rendered
		}
		if uri := scope[prefix]; out[prefix] != uri {
			decls = append(decls, prefix)
			out[prefix] = uri
		}
	}

	// Render declarations for visibly utilized prefixes whose bindings
	// differ from those already rendered by output ancestors.
	for prefix := range utilized {
		if prefix != "" && prefix != "xml" && scope[prefix] == "" {
			return ErrC14NUnboundPrefix
		}
		render(prefix)
	}

	// Render declarations for inclusive prefixes that are in scope.
	for prefix := range c.inclusive {
		if prefix == "#default" {
			render("")
		} else if scope[prefix] != "" {
			render(prefix)
		}
	}

	// Sort namespace declarations lexicographically by prefix. The default
	// namespace declaration (empty prefix) sorts first.
	slices.Sort(decls)

	// Collect the element's non-namespace attributes, validating that all
	// attribute prefixes are bound.
	attrs := make([]Attr, 0, len(e.Attr))
	for _, a := range e.Attr {
		if a.Space == "xmlns" || (a.Space == "" && a.Key == "xmlns") {
			continue
		}
		if a.Space != "" && a.Space != "xml" && scope[a.Space] == "" {
			return ErrC14NUnboundPrefix
		}
		attrs = append(attrs, a)
	}

	// Sort attributes lexicographically by namespace URI, then by local
	// name. Attributes without a namespace sort first.
	attrNS := func(a Attr) string {
		switch a.Space {
		case "":
			return ""
		case "xml":
			return xmlNamespaceURI
		default:
			return scope[a.Space]
		}
	}
	slices.SortFunc(attrs, func(a, b Attr) int {
		if v := strings.Compare(attrNS(a), attrNS(b)); v != 0 {
			return v
		}
		return strings.Compare(a.Key, b.Key)
	})

	c.buf.WriteByte('<')
	c.buf.WriteString(e.FullTag())
	for _, prefix := range decls {
		if prefix == "" {
			c.buf.WriteString(` xmlns="`)
		} else {
			c.buf.WriteString(` xmlns:`)
			c.buf.WriteString(prefix)
			c.buf.WriteString(`="`)
		}
		excC14NAttrEscaper.WriteString(&c.buf, scope[prefix])
		c.buf.WriteByte('"')
	}
	for _, a := range attrs {
		c.buf.WriteByte(' ')
		c.buf.WriteString(a.FullKey())
		c.buf.WriteString(`="`)
		excC14NAttrEscaper.WriteString(&c.buf, a.Value)
		c.buf.WriteByte('"')
	}
	c.buf.WriteByte('>')

	for _, t := range e.Child {
		switch t := t.(type) {
		case *Element:
			if err := c.element(t, out); err != nil {
				return err
			}
		case *CharData:
			excC14NTextEscaper.WriteString(&c.buf, t.Data)
		case *Comment:
			if c.comments {
				c.comment(t)
			}
		case *ProcInst:
			c.procInst(t)
		}
	}

	c.buf.WriteString("</")
	c.buf.WriteString(e.FullTag())
	c.buf.WriteByte('>')
	return nil
}

// comment writes the canonical form of a comment token.
func (c *excC14N) comment(t *Comment) {
	c.buf.WriteString("<!--")
	excC14NMiscEscaper.WriteString(&c.buf, t.Data)
	c.buf.WriteString("-->")
}

// procInst writes the canonical form of a processing instruction token. The
// XML declaration is not part of the canonical form and is skipped.
func (c *excC14N) procInst(p *ProcInst) {
	if strings.EqualFold(p.Target, "xml") {
		return
	}
	inst := strings.TrimLeft(p.Inst, " \t\r\n")
	c.buf.WriteString("<?")
	c.buf.WriteString(p.Target)
	if inst != "" {
		c.buf.WriteByte(' ')
		excC14NMiscEscaper.WriteString(&c.buf, inst)
	}
	c.buf.WriteString("?>")
}

// excC14NScope collects the namespace prefix bindings in scope at the
// element 'e', including bindings declared by its ancestors. Bindings are
// validated against the rules of the canonicalization specification.
func excC14NScope(e *Element) (map[string]string, error) {
	var chain []*Element
	for p := e; p != nil; p = p.parent {
		chain = append(chain, p)
	}
	scope := make(map[string]string)
	for i := len(chain) - 1; i >= 0; i-- {
		for _, a := range chain[i].Attr {
			var prefix string
			switch {
			case a.Space == "xmlns":
				prefix = a.Key
			case a.Space == "" && a.Key == "xmlns":
				prefix = ""
			default:
				continue
			}
			if err := checkExcC14NBinding(prefix, a.Value); err != nil {
				return nil, err
			}
			scope[prefix] = a.Value
		}
	}
	return scope, nil
}

// checkExcC14NBinding validates a single namespace declaration against the
// rules of the XML namespaces and canonicalization specifications.
func checkExcC14NBinding(prefix, uri string) error {
	switch {
	case uri == xmlnsNamespaceURI:
		return ErrC14NForbiddenBinding
	case prefix == "xml" && uri != xmlNamespaceURI:
		return ErrC14NForbiddenBinding
	case prefix != "xml" && uri == xmlNamespaceURI:
		return ErrC14NForbiddenBinding
	case prefix != "" && uri == "":
		return ErrC14NForbiddenBinding
	case uri != "" && !isAbsURI(uri):
		return ErrC14NRelativeNamespace
	}
	return nil
}

// isAbsURI returns true if the string 's' is an absolute URI reference as
// defined by RFC 3986 (i.e., it begins with a scheme followed by a colon).
func isAbsURI(s string) bool {
	i := strings.IndexByte(s, ':')
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := s[j]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case j > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}

// Escapers used to produce canonical character content. In addition to the
// markup-significant characters, carriage returns are escaped in all
// content, and tabs and newlines are escaped within attribute values.
var (
	excC14NTextEscaper = strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\r", "&#xD;",
	)
	excC14NAttrEscaper = strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		`"`, "&quot;",
		"\t", "&#x9;",
		"\n", "&#xA;",
		"\r", "&#xD;",
	)
	excC14NMiscEscaper = strings.NewReplacer(
		"\r", "&#xD;",
	)
)
