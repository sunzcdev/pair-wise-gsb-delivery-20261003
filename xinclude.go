package etree

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// XIncludeNamespace is the XML namespace recognized for xi:include and
// xi:fallback elements.
const XIncludeNamespace = "http://www.w3.org/2001/XInclude"

// Default processing budgets applied when the corresponding XIncludeOptions
// field is left at its zero value.
const (
	defaultXIncludeMaxDepth    = 32
	defaultXIncludeMaxIncludes = 256
	defaultXIncludeMaxBytes    = 16 << 20
)

// XIncludeResolver fetches the resource identified by an absolute or
// document-relative URI. All resource access performed during XInclude
// processing goes through this callback; the library never touches the
// network or the file system on its own. The returned reader is closed by
// the processor.
type XIncludeResolver func(uri string) (io.ReadCloser, error)

// XIncludeOptions controls how ProcessXIncludes expands xi:include elements.
type XIncludeOptions struct {
	// Resolver fetches included resources. Required.
	Resolver XIncludeResolver

	// BaseURI is the base URI of the document being processed. Relative
	// include hrefs are resolved against it (and against any xml:base
	// attributes found along the way). May be empty.
	BaseURI string

	// CharsetReader converts non-UTF-8 content to UTF-8. It is used for
	// parse="text" includes carrying an "encoding" attribute and for XML
	// resources declaring a non-UTF-8 charset. If nil, non-UTF-8 content is
	// rejected. Optional.
	CharsetReader func(charset string, input io.Reader) (io.Reader, error)

	// MaxDepth limits nested include expansion. Zero selects the default
	// (32).
	MaxDepth int

	// MaxIncludes limits the total number of expanded xi:include elements.
	// Zero selects the default (256).
	MaxIncludes int

	// MaxBytes limits the cumulative number of resource bytes fetched from
	// the resolver. Zero selects the default (16 MiB).
	MaxBytes int64
}

func (o XIncludeOptions) withDefaults() XIncludeOptions {
	if o.MaxDepth <= 0 {
		o.MaxDepth = defaultXIncludeMaxDepth
	}
	if o.MaxIncludes <= 0 {
		o.MaxIncludes = defaultXIncludeMaxIncludes
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = defaultXIncludeMaxBytes
	}
	return o
}

// ProcessXIncludes expands every xi:include element found in the document,
// replacing it with the content of the resource it references. It implements
// a controlled subset of XML Inclusions 1.0:
//
//   - parse="xml" (default) and parse="text" includes
//   - xi:fallback for resource errors (missing/unfetchable resources and
//     xpointer evaluation failures)
//   - element() scheme xpointer values, e.g. element(/1/2) or element(id/1)
//
// Relative hrefs are resolved against xml:base attributes and BaseURI.
// Included top-level elements receive xml:base / xml:lang fixup attributes
// so that relative links and language inheritance keep the semantics they
// had in the source resource. Unsupported xpointer forms are reported as
// errors rather than silently including the whole document.
//
// On any error the document is left unmodified.
func (d *Document) ProcessXIncludes(opts XIncludeOptions) error {
	if opts.Resolver == nil {
		return errors.New("xinclude: a Resolver is required")
	}
	p := &xincludeProcessor{opts: opts.withDefaults()}
	work := d.Copy()
	if err := p.processChildren(&work.Element, p.opts.BaseURI, "", 0); err != nil {
		return err
	}
	d.Element.Child = work.Element.Child
	for i, c := range d.Element.Child {
		c.setParent(&d.Element)
		c.setIndex(i)
	}
	return nil
}

type xincludeProcessor struct {
	opts     XIncludeOptions
	includes int
	bytes    int64
	stack    []string // URIs of resources currently being expanded
}

// processChildren expands xi:include elements among the children of e. base
// and lang are the in-scope base URI and language at e.
func (p *xincludeProcessor) processChildren(e *Element, base, lang string, depth int) error {
	for i := 0; i < len(e.Child); i++ {
		ce, ok := e.Child[i].(*Element)
		if !ok {
			continue
		}
		childBase := resolveXMLBase(base, ce)
		childLang := lang
		if l := ce.SelectAttrValue("xml:lang", ""); l != "" {
			childLang = l
		}
		if isXIncludeElement(ce, "include") {
			nodes, err := p.expand(ce, childBase, childLang, depth)
			if err != nil {
				return err
			}
			replaceChild(ce.parent, ce.index, nodes)
			i += len(nodes) - 1
			continue
		}
		if err := p.processChildren(ce, childBase, childLang, depth); err != nil {
			return err
		}
	}
	return nil
}

// expand resolves a single xi:include element and returns the tokens that
// should replace it.
func (p *xincludeProcessor) expand(inc *Element, base, lang string, depth int) ([]Token, error) {
	if depth >= p.opts.MaxDepth {
		return nil, fmt.Errorf("xinclude: maximum include depth %d exceeded", p.opts.MaxDepth)
	}
	p.includes++
	if p.includes > p.opts.MaxIncludes {
		return nil, fmt.Errorf("xinclude: maximum include count %d exceeded", p.opts.MaxIncludes)
	}

	href := inc.SelectAttrValue("href", "")
	if href == "" {
		return nil, errors.New("xinclude: xi:include is missing the required href attribute")
	}
	if strings.Contains(href, "#") {
		return nil, fmt.Errorf("xinclude: fragment identifiers are not allowed in href %q; use the xpointer attribute", href)
	}

	parse := inc.SelectAttrValue("parse", "xml")
	if parse != "xml" && parse != "text" {
		return nil, fmt.Errorf("xinclude: unsupported parse value %q", parse)
	}

	var pointer *elementPointer
	if xptr := inc.SelectAttrValue("xpointer", ""); xptr != "" {
		if parse == "text" {
			return nil, errors.New("xinclude: xpointer is not allowed with parse=\"text\"")
		}
		ep, err := parseElementXPointer(xptr)
		if err != nil {
			return nil, err
		}
		pointer = ep
	}

	uri := resolveURI(base, href)
	for _, u := range p.stack {
		if u == uri {
			return nil, fmt.Errorf("xinclude: recursive inclusion of resource %q", uri)
		}
	}

	data, err := p.fetch(uri)
	if err != nil {
		return p.fallback(inc, base, lang, depth, err)
	}

	if parse == "text" {
		text, err := p.decodeText(data, inc.SelectAttrValue("encoding", ""))
		if err != nil {
			return nil, err
		}
		return []Token{NewText(text)}, nil
	}

	doc := NewDocument()
	doc.ReadSettings.CharsetReader = p.opts.CharsetReader
	if err := doc.ReadFromBytes(data); err != nil {
		return nil, fmt.Errorf("xinclude: parsing %q: %w", uri, err)
	}

	p.stack = append(p.stack, uri)
	err = p.processChildren(&doc.Element, uri, lang, depth+1)
	p.stack = p.stack[:len(p.stack)-1]
	if err != nil {
		return nil, err
	}

	var tops []*Element
	if pointer != nil {
		el, err := pointer.eval(doc)
		if err != nil {
			return p.fallback(inc, base, lang, depth, fmt.Errorf("xinclude: %q: %w", uri, err))
		}
		tops = []*Element{el}
	} else {
		root := doc.Root()
		if root == nil {
			return p.fallback(inc, base, lang, depth, fmt.Errorf("xinclude: %q has no root element", uri))
		}
		tops = []*Element{root}
	}

	nodes := make([]Token, 0, len(tops))
	for _, t := range tops {
		fixupXMLBase(t, uri, base)
		fixupXMLLang(t, lang)
		nodes = append(nodes, t.Copy())
	}
	return nodes, nil
}

// fallback returns the processed children of the include element's
// xi:fallback child, or the original error when no fallback is present.
func (p *xincludeProcessor) fallback(inc *Element, base, lang string, depth int, cause error) ([]Token, error) {
	var fb *Element
	for _, ce := range inc.ChildElements() {
		if isXIncludeElement(ce, "fallback") {
			fb = ce
			break
		}
	}
	if fb == nil {
		return nil, cause
	}
	if err := p.processChildren(fb, base, lang, depth+1); err != nil {
		return nil, err
	}
	nodes := make([]Token, 0, len(fb.Child))
	for _, c := range fb.Child {
		nodes = append(nodes, c.dup(nil))
	}
	return nodes, nil
}

// fetch retrieves a resource through the resolver while enforcing the
// cumulative byte budget.
func (p *xincludeProcessor) fetch(uri string) ([]byte, error) {
	rc, err := p.opts.Resolver(uri)
	if err != nil {
		return nil, fmt.Errorf("xinclude: fetching %q: %w", uri, err)
	}
	defer rc.Close()

	remaining := p.opts.MaxBytes - p.bytes
	data, err := io.ReadAll(io.LimitReader(rc, remaining+1))
	if err != nil {
		return nil, fmt.Errorf("xinclude: reading %q: %w", uri, err)
	}
	p.bytes += int64(len(data))
	if p.bytes > p.opts.MaxBytes {
		return nil, fmt.Errorf("xinclude: maximum byte budget %d exceeded", p.opts.MaxBytes)
	}
	return data, nil
}

// decodeText converts parse="text" content to UTF-8 text.
func (p *xincludeProcessor) decodeText(data []byte, encoding string) (string, error) {
	enc := strings.ToLower(strings.TrimSpace(encoding))
	switch enc {
	case "", "utf-8", "utf8":
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	default:
		if p.opts.CharsetReader == nil {
			return "", fmt.Errorf("xinclude: unsupported text encoding %q (no CharsetReader configured)", encoding)
		}
		r, err := p.opts.CharsetReader(encoding, bytes.NewReader(data))
		if err != nil {
			return "", fmt.Errorf("xinclude: decoding %q text: %w", encoding, err)
		}
		data, err = io.ReadAll(r)
		if err != nil {
			return "", fmt.Errorf("xinclude: decoding %q text: %w", encoding, err)
		}
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("xinclude: text content is not valid UTF-8 (encoding %q)", encoding)
	}
	return string(data), nil
}

// isXIncludeElement reports whether e is an xi:include/xi:fallback element
// from the XInclude namespace.
func isXIncludeElement(e *Element, local string) bool {
	if e.Tag != local {
		return false
	}
	return e.Space == "xi" || e.NamespaceURI() == XIncludeNamespace
}

// resolveXMLBase computes the in-scope base URI for element e given its
// parent's base URI.
func resolveXMLBase(parentBase string, e *Element) string {
	if b := e.SelectAttrValue("xml:base", ""); b != "" {
		return resolveURI(parentBase, b)
	}
	return parentBase
}

// resolveURI resolves ref against base. If base is empty, ref is returned
// unchanged.
func resolveURI(base, ref string) string {
	if base == "" {
		return ref
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// fixupXMLBase ensures the included top-level element carries an absolute
// xml:base whenever its in-scope base URI would otherwise change, so that
// relative links inside the included content keep referring to the source
// resource.
func fixupXMLBase(t *Element, resourceURI, includeBase string) {
	if b := t.SelectAttrValue("xml:base", ""); b != "" {
		t.CreateAttr("xml:base", resolveURI(resourceURI, b))
		return
	}
	if resourceURI != includeBase {
		t.CreateAttr("xml:base", resourceURI)
	}
}

// fixupXMLLang applies language fixup: an included top-level element without
// its own xml:lang inherits the language in scope at the include location.
func fixupXMLLang(t *Element, lang string) {
	if lang != "" && t.SelectAttrValue("xml:lang", "") == "" {
		t.CreateAttr("xml:lang", lang)
	}
}

// replaceChild splices nodes into parent's child list, replacing the child
// at index.
func replaceChild(parent *Element, index int, nodes []Token) {
	child := make([]Token, 0, len(parent.Child)-1+len(nodes))
	child = append(child, parent.Child[:index]...)
	child = append(child, nodes...)
	child = append(child, parent.Child[index+1:]...)
	parent.Child = child
	for i, c := range child {
		c.setParent(parent)
		c.setIndex(i)
	}
}

// elementPointer is a parsed element() scheme xpointer: an optional ID
// followed by a 1-based child element sequence (XPointer element() scheme).
type elementPointer struct {
	id    string
	steps []int
}

// parseElementXPointer parses xpointer values of the form element(/1/2),
// element(name) or element(name/1/2). Any other xpointer form is rejected
// with an explicit error.
func parseElementXPointer(xptr string) (*elementPointer, error) {
	s := strings.TrimSpace(xptr)
	if !strings.HasPrefix(s, "element(") || !strings.HasSuffix(s, ")") {
		return nil, fmt.Errorf("xinclude: unsupported xpointer %q: only the element() scheme is supported", xptr)
	}
	body := s[len("element(") : len(s)-1]
	p := &elementPointer{}
	var rest string
	if strings.HasPrefix(body, "/") {
		rest = body
	} else {
		slash := strings.IndexByte(body, '/')
		name := body
		if slash >= 0 {
			name, rest = body[:slash], body[slash:]
		}
		if name == "" || !isNCName(name) {
			return nil, fmt.Errorf("xinclude: unsupported xpointer %q: invalid element() scheme", xptr)
		}
		p.id = name
	}
	for rest != "" {
		rest = rest[1:] // strip leading '/'
		n := len(rest)
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			n = i
		}
		step, err := strconv.Atoi(rest[:n])
		if err != nil || step < 1 {
			return nil, fmt.Errorf("xinclude: unsupported xpointer %q: invalid child sequence", xptr)
		}
		p.steps = append(p.steps, step)
		rest = rest[n:]
	}
	if p.id == "" && len(p.steps) == 0 {
		return nil, fmt.Errorf("xinclude: unsupported xpointer %q: empty element() scheme", xptr)
	}
	return p, nil
}

// eval locates the element addressed by the pointer within doc.
func (p *elementPointer) eval(doc *Document) (*Element, error) {
	var cur *Element
	steps := p.steps
	if p.id != "" {
		cur = findElementByID(&doc.Element, p.id)
		if cur == nil {
			return nil, fmt.Errorf("xpointer: no element with id %q", p.id)
		}
	} else {
		if len(steps) == 0 {
			return nil, errors.New("xpointer: empty element() scheme")
		}
		children := doc.Element.ChildElements()
		if steps[0] > len(children) {
			return nil, fmt.Errorf("xpointer: child step /%d out of range", steps[0])
		}
		cur = children[steps[0]-1]
		steps = steps[1:]
	}
	for _, step := range steps {
		children := cur.ChildElements()
		if step > len(children) {
			return nil, fmt.Errorf("xpointer: child step /%d out of range", step)
		}
		cur = children[step-1]
	}
	return cur, nil
}

// findElementByID returns the first element (depth-first) carrying an "id"
// or "xml:id" attribute equal to id.
func findElementByID(e *Element, id string) *Element {
	if e.SelectAttrValue("id", "") == id || e.SelectAttrValue("xml:id", "") == id {
		return e
	}
	for _, c := range e.ChildElements() {
		if found := findElementByID(c, id); found != nil {
			return found
		}
	}
	return nil
}

func isNCName(s string) bool {
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && (r >= '0' && r <= '9' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return len(s) > 0
}
