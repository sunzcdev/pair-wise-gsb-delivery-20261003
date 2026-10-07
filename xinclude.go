package etree

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	xincludeNamespace = "http://www.w3.org/2001/XInclude"
	xmlNamespace      = "http://www.w3.org/1998/namespace"

	defaultXIncludeDepth      = 64
	defaultXIncludeXMLDepth   = 1024
	defaultXIncludeResource   = 10 << 20
	defaultXIncludeTotalBytes = 100 << 20
)

var (
	// ErrXInclude is returned when an XInclude document cannot be expanded.
	ErrXInclude = errors.New("etree: XInclude expansion failed")

	// ErrXIncludeResourceNotFound is returned by a resolver when an included
	// resource does not exist. It permits an xi:fallback element to be used.
	ErrXIncludeResourceNotFound = errors.New("etree: XInclude resource not found")

	// ErrXIncludeDepth is returned when the XInclude depth budget is exceeded.
	ErrXIncludeDepth = errors.New("etree: XInclude depth budget exceeded")

	// ErrXIncludeSize is returned when an XInclude resource size budget is
	// exceeded.
	ErrXIncludeSize = errors.New("etree: XInclude size budget exceeded")
)

// XIncludeResolver resolves an XInclude href through a caller-controlled
// resource store. The returned reader must be non-nil when the error is nil.
type XIncludeResolver func(href string) (io.ReadCloser, error)

// XIncludeSettings control XInclude expansion.
type XIncludeSettings struct {
	// BaseURL is the base URI of the document being expanded. It may be
	// absolute or relative.
	BaseURL string

	// Resolver obtains every included resource. It must not be nil.
	Resolver XIncludeResolver

	// MaxDepth is the maximum number of nested XInclude references. If zero,
	// a default of 64 is used.
	MaxDepth int

	// MaxXMLDepth is passed to ReadSettings.MaxDepth when parsing XML
	// resources. If zero, a default of 1024 is used.
	MaxXMLDepth int

	// MaxResourceBytes limits the encoded size of one resource. If zero, a
	// default of 10 MiB is used.
	MaxResourceBytes int64

	// MaxTotalBytes limits the encoded bytes read from all included
	// resources. If zero, a default of 100 MiB is used.
	MaxTotalBytes int64
}

type xincludeProcessor struct {
	settings     XIncludeSettings
	readSettings ReadSettings

	totalRead int64
	depth     int
	resources []string

	// elements holds the selected same-document elements for active
	// includes. It prevents a fragment from indirectly including itself.
	elements []activeXIncludeElement
}

type xincludeContext struct {
	root *Element
	key  string
}

type activeXIncludeElement struct {
	key     string
	element *Element
}

// ExpandXIncludes expands a controlled subset of XInclude 1.0 in the
// document. parse="xml", parse="text", xi:fallback, and element() XPointer
// references are supported. If expansion fails, the document is left
// unchanged.
func (d *Document) ExpandXIncludes(settings XIncludeSettings) (err error) {
	if settings.Resolver == nil {
		return xincludeError("XInclude resolver is nil")
	}
	if settings.MaxDepth < 0 || settings.MaxXMLDepth < 0 ||
		settings.MaxResourceBytes < 0 || settings.MaxTotalBytes < 0 {
		return xincludeError("XInclude budgets must not be negative")
	}

	if settings.MaxDepth == 0 {
		settings.MaxDepth = defaultXIncludeDepth
	}
	if settings.MaxXMLDepth == 0 {
		settings.MaxXMLDepth = defaultXIncludeXMLDepth
	}
	if settings.MaxResourceBytes == 0 {
		settings.MaxResourceBytes = defaultXIncludeResource
	}
	if settings.MaxTotalBytes == 0 {
		settings.MaxTotalBytes = defaultXIncludeTotalBytes
	}

	backup := d.Copy()
	defer func() {
		if recovered := recover(); recovered != nil {
			d.Element = backup.Element
			d.ReadSettings = backup.ReadSettings
			d.WriteSettings = backup.WriteSettings
			err = xincludeError(fmt.Sprintf("XInclude expansion panicked: %v", recovered))
		}
	}()

	documentKey := resourceKey(settings.BaseURL)
	p := &xincludeProcessor{
		settings:     settings,
		readSettings: d.ReadSettings,
		resources:    []string{documentKey},
	}
	ctx := xincludeContext{root: &d.Element, key: documentKey}
	if err = p.processChildren(&d.Element, nil, settings.BaseURL, "", ctx); err != nil {
		d.Element = backup.Element
		d.ReadSettings = backup.ReadSettings
		d.WriteSettings = backup.WriteSettings
		return err
	}
	return nil
}

func xincludeError(message string) error {
	return fmt.Errorf("%w: %s", ErrXInclude, message)
}

func isXIncludeElement(e *Element, local string) bool {
	return e != nil && e.Tag == local && e.NamespaceURI() == xincludeNamespace
}

func documentElement(e *Element) *Element {
	for _, child := range e.Child {
		if root, ok := child.(*Element); ok {
			return root
		}
	}
	return nil
}

func xmlAttr(e *Element, key string) *Attr {
	if e == nil {
		return nil
	}
	for i := range e.Attr {
		a := &e.Attr[i]
		if a.Key == key && (a.Space == "xml" || a.Space == xmlNamespace) {
			return a
		}
	}
	return nil
}

func plainAttr(e *Element, key string) *Attr {
	if e == nil {
		return nil
	}
	for i := range e.Attr {
		a := &e.Attr[i]
		if a.Key == key && a.Space == "" {
			return a
		}
	}
	return nil
}

func (p *xincludeProcessor) processChildren(parent *Element, ancestors []*Element, base, lang string, ctx xincludeContext) error {
	for i := 0; i < len(parent.Child); i++ {
		e, ok := parent.Child[i].(*Element)
		if !ok {
			continue
		}

		if isXIncludeElement(e, "include") {
			if p.depth >= p.settings.MaxDepth {
				return fmt.Errorf("%w: include depth %d", ErrXIncludeDepth, p.depth+1)
			}

			tokens, err := p.processInclude(e, ancestors, base, lang, ctx)
			if err != nil {
				return err
			}

			parent.RemoveChildAt(i)
			if len(tokens) == 0 {
				i--
				continue
			}

			for j := len(tokens) - 1; j >= 0; j-- {
				parent.InsertChildAt(i, tokens[j])
			}
			i += len(tokens) - 2
			continue
		}

		childBase, childLang := base, lang
		if a := xmlAttr(e, "base"); a != nil {
			var err error
			childBase, err = resolveRelativeURI(base, a.Value)
			if err != nil {
				return xincludeError(fmt.Sprintf("invalid xml:base %q: %v", a.Value, err))
			}
		}
		if a := xmlAttr(e, "lang"); a != nil && a.Value != "" {
			childLang = a.Value
		}

		if err := p.processChildren(e, appendAncestor(ancestors, e), childBase, childLang, ctx); err != nil {
			return err
		}
	}
	return nil
}

func appendAncestor(ancestors []*Element, e *Element) []*Element {
	result := make([]*Element, 0, len(ancestors)+1)
	result = append(result, ancestors...)
	result = append(result, e)
	return result
}

func (p *xincludeProcessor) processInclude(e *Element, ancestors []*Element, parentBase, parentLang string, ctx xincludeContext) ([]Token, error) {
	includeBase, includeLang := parentBase, parentLang
	if a := xmlAttr(e, "base"); a != nil {
		var err error
		includeBase, err = resolveRelativeURI(parentBase, a.Value)
		if err != nil {
			return nil, xincludeError(fmt.Sprintf("invalid xml:base %q: %v", a.Value, err))
		}
	}
	if a := xmlAttr(e, "lang"); a != nil && a.Value != "" {
		includeLang = a.Value
	}

	parseMode := "xml"
	var href, xpointer, encoding string
	seen := make(map[string]bool)
	for _, a := range e.Attr {
		key := a.Key
		if a.Space != "" {
			if a.Space == "xmlns" || key == "xmlns" || a.NamespaceURI() == xmlNamespace {
				continue
			}
			if a.NamespaceURI() == xincludeNamespace {
				return nil, xincludeError(fmt.Sprintf("unsupported XInclude attribute %q", a.FullKey()))
			}
			continue
		}

		switch key {
		case "href", "parse", "xpointer", "encoding":
			if seen[key] {
				return nil, xincludeError(fmt.Sprintf("duplicate xi:include attribute %q", key))
			}
			seen[key] = true
			switch key {
			case "href":
				href = a.Value
			case "parse":
				parseMode = a.Value
			case "xpointer":
				xpointer = a.Value
			case "encoding":
				encoding = a.Value
			}
		case "xmlns":
		default:
			return nil, xincludeError(fmt.Sprintf("unsupported xi:include attribute %q", key))
		}
	}

	fallback, err := includeFallback(e)
	if err != nil {
		return nil, err
	}

	switch parseMode {
	case "xml":
		if encoding != "" {
			return nil, xincludeError(`encoding attribute is not allowed with parse="xml"`)
		}
	case "text":
		if xpointer != "" {
			return nil, xincludeError(`xpointer attribute is not allowed with parse="text"`)
		}
	default:
		return nil, xincludeError(fmt.Sprintf("unsupported xi:include parse mode %q", parseMode))
	}

	resolved, err := resolveRelativeURI(includeBase, href)
	if err != nil {
		return nil, xincludeError(fmt.Sprintf("invalid xi:include href %q: %v", href, err))
	}
	u, err := url.Parse(resolved)
	if err != nil {
		return nil, xincludeError(fmt.Sprintf("invalid xi:include href %q: %v", href, err))
	}
	fragment := u.Fragment
	if xpointer != "" {
		if fragment != "" {
			return nil, xincludeError("xi:include cannot specify both an xpointer attribute and an href fragment")
		}
		fragment = xpointer
	}

	if parseMode == "text" {
		data, resourceErr := p.readResource(resourceKey(resolved))
		if resourceErr != nil {
			if isMissingResourceError(resourceErr) && fallback != nil {
				return p.useFallback(fallback, e.Parent(), parentBase, includeBase, parentLang, includeLang)
			}
			return nil, resourceErr
		}
		text, resourceErr := decodeTextResource(data, encoding)
		if resourceErr != nil {
			return nil, resourceErr
		}
		return []Token{NewText(text)}, nil
	}

	if fragment != "" && !strings.HasPrefix(fragment, "element(") {
		return nil, xincludeError(fmt.Sprintf("unsupported XPointer %q: only element() is supported", fragment))
	}
	if fragment != "" {
		if _, _, err := parseElementPointer(fragment); err != nil {
			return nil, err
		}
	}
	if !seen["href"] {
		return nil, xincludeError("xi:include requires an href attribute")
	}

	key := resourceKey(resolved)
	if key == ctx.key {
		if fragment == "" {
			return nil, xincludeError("same-document xi:include requires an element() XPointer")
		}
		return p.includeSameDocument(fragment, e.Parent(), ancestors, ctx, parentBase, includeBase, parentLang, includeLang, fallback)
	}
	return p.includeExternal(key, fragment, e.Parent(), parentBase, includeBase, parentLang, includeLang, fallback)
}

func (p *xincludeProcessor) includeExternal(key, fragment string, parent *Element, parentBase, includeBase, parentLang, includeLang string, fallback *Element) ([]Token, error) {
	for _, active := range p.resources {
		if active == key {
			return nil, xincludeError(fmt.Sprintf("recursive XInclude reference to %q", key))
		}
	}
	data, err := p.readResource(key)
	if err != nil {
		if isMissingResourceError(err) && fallback != nil {
			return p.useFallback(fallback, parent, parentBase, includeBase, parentLang, includeLang)
		}
		return nil, err
	}

	xmlData, err := normalizeXMLResource(data)
	if err != nil {
		return nil, err
	}

	source := NewDocument()
	source.ReadSettings = p.readSettings
	source.ReadSettings.MaxDepth = p.settings.MaxXMLDepth
	if err = source.ReadFromBytes(xmlData); err != nil {
		return nil, xincludeError(fmt.Sprintf("XML resource %q is invalid: %v", key, err))
	}
	rawTarget := source.Root()
	if rawTarget == nil {
		return nil, xincludeError(fmt.Sprintf("XML resource %q has no document element", key))
	}

	if fragment != "" {
		rawTarget, err = selectElementPointer(&source.Element, nil, fragment)
		if err != nil {
			if isMissingResourceError(err) && fallback != nil {
				return p.useFallback(fallback, parent, parentBase, includeBase, parentLang, includeLang)
			}
			return nil, err
		}
	}

	if p.depth >= p.settings.MaxDepth {
		return nil, fmt.Errorf("%w: include depth %d", ErrXIncludeDepth, p.depth+1)
	}

	sourceBase, sourceLang := inheritedContext(&source.Element, rawTarget, key, "")
	target := rawTarget.Copy()
	fixElementNamespaces(parent, target)
	fixElementBase(target, sourceBase, parentBase)
	fixElementLang(target, sourceLang, parentLang)

	p.depth++
	p.resources = append(p.resources, key)
	err = p.processChildren(target, nil, sourceBase, sourceLang, xincludeContext{root: &source.Element, key: key})
	p.resources = p.resources[:len(p.resources)-1]
	p.depth--
	if err != nil {
		return nil, err
	}
	return []Token{target}, nil
}

func (p *xincludeProcessor) includeSameDocument(fragment string, parent *Element, ancestors []*Element, ctx xincludeContext, parentBase, includeBase, parentLang, includeLang string, fallback *Element) ([]Token, error) {
	target, err := selectElementPointer(ctx.root, documentElement(ctx.root), fragment)
	if err != nil {
		if isMissingResourceError(err) && fallback != nil {
			return p.useFallback(fallback, parent, parentBase, includeBase, parentLang, includeLang)
		}
		return nil, err
	}

	for _, active := range p.elements {
		if active.key == ctx.key && active.element == target {
			return nil, xincludeError(fmt.Sprintf("recursive same-document XInclude reference %q", fragment))
		}
	}
	for _, ancestor := range ancestors {
		if ancestor == target {
			return nil, xincludeError(fmt.Sprintf("recursive same-document XInclude reference %q", fragment))
		}
	}
	if p.depth >= p.settings.MaxDepth {
		return nil, fmt.Errorf("%w: include depth %d", ErrXIncludeDepth, p.depth+1)
	}

	sourceBase, sourceLang := inheritedContext(ctx.root, target, ctx.key, "")
	copyTarget := target.Copy()
	fixElementNamespaces(parent, copyTarget)
	fixElementBase(copyTarget, sourceBase, parentBase)
	fixElementLang(copyTarget, sourceLang, parentLang)

	p.depth++
	p.elements = append(p.elements, activeXIncludeElement{key: ctx.key, element: target})
	err = p.processChildren(copyTarget, nil, sourceBase, sourceLang, ctx)
	p.elements = p.elements[:len(p.elements)-1]
	p.depth--
	if err != nil {
		return nil, err
	}
	return []Token{copyTarget}, nil
}

func includeFallback(include *Element) (*Element, error) {
	var fallback *Element
	for _, t := range include.Child {
		switch c := t.(type) {
		case *Element:
			if isXIncludeElement(c, "fallback") {
				if fallback != nil {
					return nil, xincludeError("xi:include contains multiple xi:fallback elements")
				}
				fallback = c
				continue
			}
			return nil, xincludeError("xi:include contains an unsupported child element")
		case *CharData:
			if !c.IsWhitespace() {
				return nil, xincludeError("xi:include contains non-whitespace text")
			}
		case *Comment, *ProcInst, *Directive:
		default:
			return nil, xincludeError("xi:include contains an unsupported child token")
		}
	}
	return fallback, nil
}

func (p *xincludeProcessor) useFallback(fallback, parent *Element, parentBase, includeBase, parentLang, includeLang string) ([]Token, error) {
	tokens := make([]Token, 0, len(fallback.Child))
	tokens = append(tokens, fallback.Child...)
	for _, t := range tokens {
		e, ok := t.(*Element)
		if !ok {
			continue
		}

		base := includeBase
		if a := xmlAttr(e, "base"); a != nil {
			var err error
			base, err = resolveRelativeURI(includeBase, a.Value)
			if err != nil {
				return nil, xincludeError(fmt.Sprintf("invalid xml:base %q: %v", a.Value, err))
			}
		}
		lang := includeLang
		if a := xmlAttr(e, "lang"); a != nil && a.Value != "" {
			lang = a.Value
		}
		fixElementNamespaces(parent, e)
		fixElementBase(e, base, parentBase)
		fixElementLang(e, lang, parentLang)
	}
	return tokens, nil
}

func isMissingResourceError(err error) bool {
	return errors.Is(err, ErrXIncludeResourceNotFound) || errors.Is(err, fs.ErrNotExist)
}

func (p *xincludeProcessor) readResource(key string) ([]byte, error) {
	rc, err := p.settings.Resolver(key)
	if err != nil {
		if isMissingResourceError(err) {
			return nil, err
		}
		return nil, xincludeError(fmt.Sprintf("unable to resolve resource %q: %v", key, err))
	}
	defer rc.Close()
	if rc == nil {
		return nil, xincludeError(fmt.Sprintf("resolver returned a nil reader for resource %q", key))
	}

	limit := p.settings.MaxResourceBytes
	if remaining := p.settings.MaxTotalBytes - p.totalRead; remaining < limit {
		limit = remaining
	}
	if limit < 0 {
		limit = 0
	}

	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, xincludeError(fmt.Sprintf("unable to read resource %q: %v", key, err))
	}
	if int64(len(data)) > p.settings.MaxResourceBytes {
		return nil, fmt.Errorf("%w: resource %q exceeds %d bytes", ErrXIncludeSize, key, p.settings.MaxResourceBytes)
	}
	if int64(len(data)) > limit || p.totalRead > p.settings.MaxTotalBytes {
		return nil, fmt.Errorf("%w: total included resource bytes exceed %d", ErrXIncludeSize, p.settings.MaxTotalBytes)
	}
	p.totalRead += int64(len(data))
	return data, nil
}

func resourceKey(resolved string) string {
	u, err := url.Parse(resolved)
	if err != nil {
		return resolved
	}
	u.Fragment = ""
	return u.String()
}

func resolveRelativeURI(base, reference string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	refURL, err := url.Parse(reference)
	if err != nil {
		return "", err
	}
	if refURL.Scheme != "" {
		return refURL.String(), nil
	}
	if baseURL.Scheme != "" {
		return baseURL.ResolveReference(refURL).String(), nil
	}
	if reference == "" {
		return base, nil
	}
	if refURL.Host != "" || refURL.Opaque != "" {
		return "", fmt.Errorf("relative reference %q cannot use an authority", reference)
	}
	if refURL.Path == "" {
		refURL.Path = baseURL.Path
		return refURL.String(), nil
	}
	if strings.HasPrefix(refURL.Path, "/") {
		return joinRelativeURI(refURL), nil
	}

	baseDir := ""
	if index := strings.LastIndexByte(baseURL.Path, '/'); index >= 0 {
		baseDir = baseURL.Path[:index+1]
	}
	merged := pathClean(baseDir + refURL.Path)
	if strings.HasSuffix(refURL.Path, "/") && !strings.HasSuffix(merged, "/") {
		merged += "/"
	}
	refURL.Path = merged
	return refURL.String(), nil
}

func joinRelativeURI(u *url.URL) string {
	return u.String()
}

func pathClean(path string) string {
	parts := make([]string, 0, strings.Count(path, "/")+1)
	leadingSlash := strings.HasPrefix(path, "/")
	for _, part := range strings.Split(path, "/") {
		switch part {
		case "", ".":
		case "..":
			if len(parts) > 0 && parts[len(parts)-1] != ".." {
				parts = parts[:len(parts)-1]
			} else if !leadingSlash {
				parts = append(parts, "..")
			}
		default:
			parts = append(parts, part)
		}
	}
	clean := strings.Join(parts, "/")
	if leadingSlash {
		clean = "/" + clean
	}
	if clean == "" {
		return "."
	}
	return clean
}

func inheritedContext(root, target *Element, baseURI, lang string) (string, string) {
	chain := make([]*Element, 0)
	for e := target; e != nil && e != root; e = e.Parent() {
		chain = append(chain, e)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		e := chain[i]
		if a := xmlAttr(e, "base"); a != nil {
			resolved, err := resolveRelativeURI(baseURI, a.Value)
			if err == nil {
				baseURI = resolved
			}
		}
		if a := xmlAttr(e, "lang"); a != nil && a.Value != "" {
			lang = a.Value
		}
	}
	return baseURI, lang
}

func fixElementBase(e *Element, sourceBase, targetBase string) {
	if sourceBase == "" || sourceBase == targetBase {
		return
	}
	if a := xmlAttr(e, "base"); a != nil {
		a.Value = sourceBase
	} else {
		e.CreateAttr("xml:base", sourceBase)
	}
}

func fixElementLang(e *Element, sourceLang, targetLang string) {
	if sourceLang == "" || sourceLang == targetLang {
		return
	}
	if a := xmlAttr(e, "lang"); a != nil {
		a.Value = sourceLang
	} else {
		e.CreateAttr("xml:lang", sourceLang)
	}
}

func fixElementNamespaces(destinationParent, sourceRoot *Element) {
	sourceNamespaces, sourceHasDefault := inScopeNamespaces(sourceRoot)
	destinationNamespaces, _ := inScopeNamespaces(destinationParent)
	required := requiredNamespacePrefixes(sourceRoot)
	for prefix := range required {
		sourceURI, ok := sourceNamespaces[prefix]
		if !ok || sourceURI == destinationNamespaces[prefix] || hasNamespaceDeclaration(sourceRoot, prefix) {
			continue
		}
		if prefix == "" {
			sourceRoot.CreateAttr("xmlns", sourceURI)
		} else {
			sourceRoot.CreateAttr("xmlns:"+prefix, sourceURI)
		}
	}
	if sourceHasDefault {
		sourceURI := sourceNamespaces[""]
		if sourceURI != destinationNamespaces[""] && !hasNamespaceDeclaration(sourceRoot, "") {
			sourceRoot.CreateAttr("xmlns", sourceURI)
		}
	} else if destinationNamespaces[""] != "" && !hasNamespaceDeclaration(sourceRoot, "") {
		sourceRoot.CreateAttr("xmlns", "")
	}
}

func inScopeNamespaces(e *Element) (map[string]string, bool) {
	chain := make([]*Element, 0)
	for current := e; current != nil; current = current.Parent() {
		chain = append(chain, current)
	}
	namespaces := map[string]string{"": ""}
	hasDefault := false
	for i := len(chain) - 1; i >= 0; i-- {
		for _, a := range chain[i].Attr {
			if a.Space == "xmlns" && a.Key != "xml" {
				namespaces[a.Key] = a.Value
			} else if a.Space == "" && a.Key == "xmlns" {
				namespaces[""] = a.Value
				hasDefault = true
			}
		}
	}
	return namespaces, hasDefault
}

func requiredNamespacePrefixes(e *Element) map[string]struct{} {
	prefixes := make(map[string]struct{})
	var walk func(*Element)
	walk = func(current *Element) {
		if current.Space != "" && current.Space != "xml" {
			prefixes[current.Space] = struct{}{}
		}
		for _, a := range current.Attr {
			if a.Space != "" && a.Space != "xmlns" && a.Space != "xml" {
				prefixes[a.Space] = struct{}{}
			}
		}
		for _, child := range current.Child {
			if childElement, ok := child.(*Element); ok {
				walk(childElement)
			}
		}
	}
	walk(e)
	return prefixes
}

func hasNamespaceDeclaration(e *Element, prefix string) bool {
	for _, a := range e.Attr {
		if prefix == "" && a.Space == "" && a.Key == "xmlns" {
			return true
		}
		if prefix != "" && a.Space == "xmlns" && a.Key == prefix {
			return true
		}
	}
	return false
}

func selectElementPointer(root, documentRoot *Element, pointer string) (*Element, error) {
	id, indexes, err := parseElementPointer(pointer)
	if err != nil {
		return nil, err
	}
	target := root
	if id != "" {
		target = documentRoot
		target = findElementByID(root, id)
		if target == nil {
			return nil, fmt.Errorf("%w: XPointer element id %q was not found", ErrXIncludeResourceNotFound, id)
		}
	}
	for _, index := range indexes {
		elements := target.ChildElements()
		if index < 1 || index > len(elements) {
			return nil, fmt.Errorf("%w: XPointer child sequence %q is out of range", ErrXIncludeResourceNotFound, pointer)
		}
		target = elements[index-1]
	}
	return target, nil
}

func parseElementPointer(pointer string) (string, []int, error) {
	const prefix = "element("
	if !strings.HasPrefix(pointer, prefix) || !strings.HasSuffix(pointer, ")") {
		return "", nil, xincludeError(fmt.Sprintf("unsupported XPointer %q: only element() is supported", pointer))
	}
	body := strings.TrimSpace(pointer[len(prefix) : len(pointer)-1])
	if body == "" {
		return "", nil, xincludeError("element() XPointer must contain an id or child sequence")
	}

	if strings.HasPrefix(body, "/") {
		indexes, err := parseChildSequence(body)
		if err != nil {
			return "", nil, err
		}
		return "", indexes, nil
	}

	id := body
	var sequence string
	if index := strings.IndexAny(body, "/ \t\n\r"); index >= 0 {
		id = body[:index]
		sequence = body[index:]
	}
	if id == "" || !isNCName(id) {
		return "", nil, xincludeError(fmt.Sprintf("invalid element() XPointer id %q", id))
	}
	var indexes []int
	if sequence != "" {
		var err error
		indexes, err = parseChildSequence(sequence)
		if err != nil {
			return "", nil, err
		}
	}
	return id, indexes, nil
}

func parseChildSequence(sequence string) ([]int, error) {
	parts := strings.Split(sequence, "/")
	if parts[0] != "" {
		return nil, xincludeError(fmt.Sprintf("invalid element() child sequence %q", sequence))
	}
	if len(parts) == 1 {
		return nil, xincludeError(fmt.Sprintf("invalid element() child sequence %q", sequence))
	}
	indexes := make([]int, 0, len(parts)-1)
	for _, part := range parts[1:] {
		if part == "" || strings.ContainsAny(part, " \t") {
			return nil, xincludeError(fmt.Sprintf("invalid element() child sequence %q", sequence))
		}
		index := 0
		if part[0] == '0' {
			return nil, xincludeError(fmt.Sprintf("invalid element() child sequence %q", sequence))
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return nil, xincludeError(fmt.Sprintf("invalid element() child sequence %q", sequence))
			}
			index = index*10 + int(r-'0')
		}
		indexes = append(indexes, index)
	}
	return indexes, nil
}

func isNCName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if r == '-' || r == '_' || r == '.' || i > 0 && unicode.IsDigit(r) {
			continue
		}
		if unicode.IsLetter(r) || i > 0 && unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return true
}

func findElementByID(root *Element, id string) *Element {
	var found *Element
	var walk func(*Element) bool
	walk = func(e *Element) bool {
		if a := xmlAttr(e, "id"); a != nil && a.Value == id {
			found = e
			return true
		}
		if a := plainAttr(e, "id"); a != nil && a.Value == id {
			found = e
			return true
		}
		for _, child := range e.Child {
			if childElement, ok := child.(*Element); ok && walk(childElement) {
				return true
			}
		}
		return false
	}
	for _, child := range root.Child {
		if e, ok := child.(*Element); ok && walk(e) {
			return found
		}
	}
	return nil
}

func decodeTextResource(data []byte, requested string) (string, error) {
	encodingName := canonicalEncodingName(requested)
	if encodingName == "" {
		encodingName = "UTF-8"
	}

	switch encodingName {
	case "UTF-8":
		data = stripBOM(data, utf8BOM)
		if !utf8.Valid(data) {
			return "", xincludeError("text resource is not valid UTF-8")
		}
		return string(data), nil
	case "US-ASCII":
		for _, b := range data {
			if b > 0x7F {
				return "", xincludeError("text resource is not valid US-ASCII")
			}
		}
		return string(data), nil
	case "ISO-8859-1":
		runes := make([]rune, len(data))
		for i, b := range data {
			runes[i] = rune(b)
		}
		return string(runes), nil
	case "UTF-16":
		runes, _, err := decodeUTF16(data, true)
		if err != nil {
			return "", err
		}
		return string(runes), nil
	default:
		return "", xincludeError(fmt.Sprintf("unsupported text encoding %q", requested))
	}
}

func normalizeXMLResource(data []byte) ([]byte, error) {
	decoded, endianBOM := stripKnownBOM(data)
	if decoded != nil {
		if endianBOM != 0 {
			runes, endian, err := decodeUTF16(data, false)
			if err != nil {
				return nil, err
			}
			data = []byte(string(runes))
			endianBOM = endian
		} else {
			data = decoded
		}
	} else if isLikelyUTF16(data) {
		runes, endian, err := decodeUTF16(data, false)
		if err != nil {
			return nil, err
		}
		data = []byte(string(runes))
		endianBOM = endian
	} else if hasBOM(data, utf32BOMLE[:]) || hasBOM(data, utf32BOMBE[:]) {
		return nil, xincludeError("UTF-32 XML resources are not supported")
	} else {
		data = stripBOM(data, utf8BOM)
	}

	_, encodingValue, start, end, ok := findXMLDeclarationEncoding(data)
	if !ok {
		if !utf8.Valid(data) {
			return nil, xincludeError("XML resource is not valid UTF-8")
		}
		return data, nil
	}

	encodingName := canonicalEncodingName(encodingValue)
	switch encodingName {
	case "UTF-8":
	case "US-ASCII":
		for _, b := range data {
			if b > 0x7F {
				return nil, xincludeError("XML resource is not valid US-ASCII")
			}
		}
	case "ISO-8859-1":
		runes := make([]rune, len(data))
		for i, b := range data {
			runes[i] = rune(b)
		}
		data = []byte(string(runes))
	case "UTF-16":
		if endianBOM == 0 {
			runes, _, err := decodeUTF16(data, false)
			if err != nil {
				return nil, err
			}
			data = []byte(string(runes))
		}
	default:
		return nil, xincludeError(fmt.Sprintf("unsupported XML encoding %q", encodingValue))
	}

	if !utf8.Valid(data) {
		return nil, xincludeError(fmt.Sprintf("XML resource declared as %q is not valid after decoding", encodingValue))
	}
	return rewriteXMLDeclarationEncoding(data, start, end, encodingName), nil
}

var (
	utf8BOM    = []byte{0xEF, 0xBB, 0xBF}
	utf16BOMLE = []byte{0xFF, 0xFE}
	utf16BOMBE = []byte{0xFE, 0xFF}
	utf32BOMLE = []byte{0xFF, 0xFE, 0x00, 0x00}
	utf32BOMBE = []byte{0x00, 0x00, 0xFE, 0xFF}
)

type utf16Endian int

const (
	utf16UnknownEndian utf16Endian = iota
	utf16LittleEndian
	utf16BigEndian
)

func stripKnownBOM(data []byte) ([]byte, utf16Endian) {
	if hasBOM(data, utf8BOM) {
		return data[len(utf8BOM):], 0
	}
	if hasBOM(data, utf16BOMLE) {
		return data[len(utf16BOMLE):], utf16LittleEndian
	}
	if hasBOM(data, utf16BOMBE) {
		return data[len(utf16BOMBE):], utf16BigEndian
	}
	return nil, 0
}

func stripBOM(data, bom []byte) []byte {
	if hasBOM(data, bom) {
		return data[len(bom):]
	}
	return data
}

func hasBOM(data, bom []byte) bool {
	return len(data) >= len(bom) && bytes.Equal(data[:len(bom)], bom)
}

func isLikelyUTF16(data []byte) bool {
	return len(data) >= 2 && ((data[0] == '<' && data[1] == 0) || (data[0] == 0 && data[1] == '<'))
}

func decodeUTF16(data []byte, requireBOM bool) ([]rune, utf16Endian, error) {
	endian := utf16UnknownEndian
	if hasBOM(data, utf16BOMLE) {
		endian = utf16LittleEndian
		data = data[len(utf16BOMLE):]
	} else if hasBOM(data, utf16BOMBE) {
		endian = utf16BigEndian
		data = data[len(utf16BOMBE):]
	} else if isLikelyUTF16(data) {
		if data[0] == '<' {
			endian = utf16LittleEndian
		} else {
			endian = utf16BigEndian
		}
	} else if requireBOM {
		return nil, 0, xincludeError("UTF-16 text resource requires a byte order mark")
	} else {
		return nil, 0, xincludeError("UTF-16 resource requires a byte order mark or XML declaration")
	}

	if len(data)%2 != 0 {
		return nil, 0, xincludeError("UTF-16 resource has an odd byte length")
	}
	values := make([]uint16, len(data)/2)
	for i := range values {
		hi, lo := data[i*2], data[i*2+1]
		if endian == utf16LittleEndian {
			values[i] = uint16(hi) | uint16(lo)<<8
		} else {
			values[i] = uint16(lo) | uint16(hi)<<8
		}
	}
	runes := utf16.Decode(values)
	for _, r := range runes {
		if r == 0xFFFD {
			return nil, 0, xincludeError("UTF-16 resource contains an invalid code unit")
		}
	}
	return runes, endian, nil
}

func canonicalEncodingName(value string) string {
	name := strings.ToUpper(strings.TrimSpace(strings.Trim(value, "\"'")))
	name = strings.ReplaceAll(name, "_", "-")
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	name = strings.TrimPrefix(name, "X-")
	switch name {
	case "UTF8", "UTF-8":
		return "UTF-8"
	case "US-ASCII", "ASCII", "ISO646-US", "ISO-IR-6":
		return "US-ASCII"
	case "ISO-8859-1", "ISO8859-1", "LATIN1", "LATIN-1", "ISO-LATIN-1":
		return "ISO-8859-1"
	case "UTF-16", "UTF16", "ISO-10646-UCS-2":
		return "UTF-16"
	default:
		return name
	}
}

func findXMLDeclarationEncoding(data []byte) (raw, value string, start, end int, ok bool) {
	declStart := bytes.Index(data, []byte("<?xml"))
	if declStart < 0 {
		return "", "", 0, 0, false
	}
	if declStart+5 < len(data) && !isXMLSpace(data[declStart+5]) {
		return "", "", 0, 0, false
	}

	declEndRelative := bytes.Index(data[declStart:], []byte("?>"))
	if declEndRelative < 0 {
		return "", "", 0, 0, false
	}
	declEnd := declStart + declEndRelative
	declaration := data[declStart : declEnd+2]
	lowerDeclaration := bytes.ToLower(declaration)
	encodingIndex := bytes.Index(lowerDeclaration, []byte("encoding"))
	if encodingIndex < 0 {
		return "", "", 0, 0, false
	}

	position := encodingIndex + len("encoding")
	for position < len(declaration) && isXMLSpace(declaration[position]) {
		position++
	}
	if position >= len(declaration) || declaration[position] != '=' {
		return "", "", 0, 0, false
	}
	position++
	for position < len(declaration) && isXMLSpace(declaration[position]) {
		position++
	}
	if position >= len(declaration) || (declaration[position] != '"' && declaration[position] != '\'') {
		return "", "", 0, 0, false
	}

	quote := declaration[position]
	valueStart := position + 1
	valueEndRelative := bytes.IndexByte(declaration[valueStart:], quote)
	if valueEndRelative < 0 {
		return "", "", 0, 0, false
	}
	valueEnd := valueStart + valueEndRelative
	return string(declaration), string(declaration[valueStart:valueEnd]), declStart, declEnd + 2, true
}

func isXMLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

func rewriteXMLDeclarationEncoding(data []byte, start, end int, encoding string) []byte {
	declaration := data[start:end]
	updated := replaceEncodingInDeclaration(declaration, encoding)
	result := make([]byte, 0, len(data)-len(declaration)+len(updated))
	result = append(result, data[:start]...)
	result = append(result, updated...)
	result = append(result, data[end:]...)
	return result
}

func replaceEncodingInDeclaration(declaration []byte, encoding string) []byte {
	lowerDeclaration := bytes.ToLower(declaration)
	encodingIndex := bytes.Index(lowerDeclaration, []byte("encoding"))
	if encodingIndex < 0 {
		return declaration
	}
	position := encodingIndex + len("encoding")
	for position < len(declaration) && isXMLSpace(declaration[position]) {
		position++
	}
	if position >= len(declaration) || declaration[position] != '=' {
		return declaration
	}
	position++
	for position < len(declaration) && isXMLSpace(declaration[position]) {
		position++
	}
	if position >= len(declaration) {
		return declaration
	}

	quote := declaration[position]
	valueStart := position + 1
	valueEnd := bytes.IndexByte(declaration[valueStart:], quote)
	if valueEnd < 0 {
		return declaration
	}
	valueEnd += valueStart

	result := make([]byte, 0, len(declaration)+len(encoding))
	result = append(result, declaration[:valueStart]...)
	result = append(result, encoding...)
	result = append(result, declaration[valueEnd:]...)
	return result
}
