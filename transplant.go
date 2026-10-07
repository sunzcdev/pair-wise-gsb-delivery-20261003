package etree

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// ErrUnboundPrefix is returned by TransplantChild when the source subtree
// contains a namespace prefix that is not bound to a namespace URI.
var ErrUnboundPrefix = errors.New("etree: unbound namespace prefix")

// ErrTransplantCycle is returned by TransplantChild when the requested
// transplant would make an element a child of itself or of one of its
// descendants.
var ErrTransplantCycle = errors.New("etree: transplant would create an element cycle")

// xmlNamespaceURI is the namespace URI implicitly bound to the "xml" prefix.
const xmlNamespaceURI = "http://www.w3.org/XML/1998/namespace"

// TransplantMode selects whether a transplant copies or moves the source
// element.
type TransplantMode int

const (
	// TransplantCopy inserts a deep copy of the source element into the
	// destination, leaving the source element unchanged.
	TransplantCopy TransplantMode = iota

	// TransplantMove detaches the source element from its current position
	// and inserts it into the destination.
	TransplantMove
)

// QNameAttrMode describes how the value of a registered attribute is
// interpreted during a transplant.
type QNameAttrMode int

const (
	// QNameValue indicates the attribute value holds a single QName.
	QNameValue QNameAttrMode = iota

	// QNameListValue indicates the attribute value holds a
	// whitespace-separated list of QNames.
	QNameListValue
)

// TransplantSettings configure the behavior of the TransplantChild function.
type TransplantSettings struct {
	// QNameAttrs registers attributes whose values contain a QName or a
	// whitespace-separated list of QNames (for example, "xsi:type"). Keys
	// are attribute names as returned by Attr.FullKey (e.g., "xsi:type", or
	// "type" for an unprefixed attribute). The QNames held by registered
	// attributes are rewritten along with element and attribute names so
	// that they resolve to the same namespace URIs after the transplant.
	// Values of unregistered attributes are never modified.
	QNameAttrs map[string]QNameAttrMode
}

// qnameAttrMode returns the QName mode registered for the attribute 'a'.
func (s *TransplantSettings) qnameAttrMode(a *Attr) (QNameAttrMode, bool) {
	if s == nil || len(s.QNameAttrs) == 0 {
		return 0, false
	}
	mode, ok := s.QNameAttrs[a.FullKey()]
	return mode, ok
}

// TransplantChild inserts the element 'src' into this element's list of
// child tokens while preserving the meaning of every namespace-qualified
// name within the source subtree. It returns the inserted element.
//
// Unlike AddChild, which performs no namespace adjustment, TransplantChild
// first collects all namespace bindings used by the source subtree and
// compares them with the bindings in scope at the destination. It then adds
// the minimum set of namespace declarations required on the transplanted
// subtree's root so that every element name, attribute name, and registered
// QName attribute value resolves to the same namespace URI (its "expanded
// name") as it did in the source tree. Prefixes that conflict with
// destination bindings are renamed, and default namespace shadowing is
// compensated with an explicit "xmlns" declaration on the transplanted
// root.
//
// The 'mode' parameter selects whether the source element is copied
// (TransplantCopy) or moved (TransplantMove) into the destination.
//
// If an error is returned (for example, because the source subtree uses an
// unbound namespace prefix, or because the transplant would create an
// element cycle), neither the source nor the destination tree is modified.
func (e *Element) TransplantChild(src *Element, mode TransplantMode, settings *TransplantSettings) (*Element, error) {
	if src == nil {
		return nil, errors.New("etree: cannot transplant a nil element")
	}
	if mode != TransplantCopy && mode != TransplantMove {
		return nil, errors.New("etree: invalid transplant mode")
	}

	// The destination must not be the source element or one of its
	// descendants.
	for p := e; p != nil; p = p.parent {
		if p == src {
			return nil, ErrTransplantCycle
		}
	}

	if settings == nil {
		settings = &TransplantSettings{}
	}

	// Pass 1: collect the namespace requirements of the source subtree and
	// validate that every prefix it uses is bound. The tree is not
	// modified during this pass.
	srcScope := newNsScope(src.parent)
	usage := newNsUsage()
	if err := usage.collect(src, srcScope, settings); err != nil {
		return nil, err
	}

	// Pass 2: plan the namespace declarations to add to the transplanted
	// subtree's root element.
	dstScope := newNsScope(e)
	gen := newNsPrefixGen(usage, dstScope)
	plan := planNsDecls(src, usage, srcScope, dstScope, gen)

	// Create the working element. Nothing below can fail, so from this
	// point on both trees remain consistent.
	work := src
	if mode == TransplantCopy {
		work = src.Copy()
	}

	// Add the planned declarations to the root of the transplanted
	// subtree.
	if plan.defaultDecl != nil {
		work.CreateAttr("xmlns", *plan.defaultDecl)
	}
	for _, d := range plan.prefixDecls {
		work.CreateAttr("xmlns:"+d.prefix, d.uri)
	}

	// Pass 3: rewrite names and registered QName values whose prefixes do
	// not resolve correctly in the destination scope.
	rewriteNs(work, srcScope, dstScope.clone(), settings, gen)

	e.AddChild(work)
	return work, nil
}

// nsScope is a stack of namespace declaration frames used to resolve
// namespace prefixes within a scope. Each frame maps a prefix to a
// namespace URI; the empty prefix maps to the default namespace.
type nsScope struct {
	frames []map[string]string
}

// newNsScope builds the in-scope namespace bindings visible to a child of
// the element 'e'. If 'e' is nil, the returned scope is empty.
func newNsScope(e *Element) *nsScope {
	var chain []*Element
	for p := e; p != nil; p = p.parent {
		chain = append(chain, p)
	}
	s := &nsScope{}
	for i := len(chain) - 1; i >= 0; i-- {
		s.push(nsDeclsOf(chain[i]))
	}
	return s
}

// nsDeclsOf returns the namespace declarations local to the element 'e',
// mapping prefix to namespace URI. The empty prefix maps to the default
// namespace declared by an "xmlns" attribute.
func nsDeclsOf(e *Element) map[string]string {
	var decls map[string]string
	for i := range e.Attr {
		a := &e.Attr[i]
		switch {
		case a.Space == "xmlns":
			if decls == nil {
				decls = make(map[string]string)
			}
			decls[a.Key] = a.Value
		case a.Space == "" && a.Key == "xmlns":
			if decls == nil {
				decls = make(map[string]string)
			}
			decls[""] = a.Value
		}
	}
	return decls
}

// isNsDecl returns true if the attribute 'a' is a namespace declaration.
func isNsDecl(a *Attr) bool {
	return a.Space == "xmlns" || (a.Space == "" && a.Key == "xmlns")
}

// clone returns a shallow copy of the scope's frame stack.
func (s *nsScope) clone() *nsScope {
	c := &nsScope{frames: make([]map[string]string, len(s.frames))}
	copy(c.frames, s.frames)
	return c
}

// push adds a frame of local declarations to the top of the scope.
func (s *nsScope) push(frame map[string]string) {
	s.frames = append(s.frames, frame)
}

// pop removes the innermost frame from the scope.
func (s *nsScope) pop() {
	s.frames = s.frames[:len(s.frames)-1]
}

// lookup returns the namespace URI bound to 'prefix' by the innermost
// frame declaring it.
func (s *nsScope) lookup(prefix string) (string, bool) {
	uri, _, ok := s.lookupFrame(prefix)
	return uri, ok
}

// lookupFrame behaves like lookup but also returns the index of the frame
// containing the binding, or -1 for an implicit binding.
func (s *nsScope) lookupFrame(prefix string) (string, int, bool) {
	if prefix == "xml" {
		return xmlNamespaceURI, -1, true
	}
	for i := len(s.frames) - 1; i >= 0; i-- {
		if uri, ok := s.frames[i][prefix]; ok {
			return uri, i, true
		}
	}
	return "", -1, false
}

// defaultURI returns the default namespace URI in the innermost scope.
func (s *nsScope) defaultURI() string {
	uri, _ := s.lookup("")
	return uri
}

// prefixFor returns a non-empty prefix effectively bound to 'uri' in the
// innermost scope, honoring shadowing. It returns false if no prefix is
// bound to the URI.
func (s *nsScope) prefixFor(uri string) (string, bool) {
	if uri == xmlNamespaceURI {
		return "xml", true
	}
	seen := make(map[string]bool)
	match := ""
	for i := len(s.frames) - 1; i >= 0; i-- {
		for p, u := range s.frames[i] {
			if seen[p] {
				continue
			}
			seen[p] = true
			if p != "" && u == uri && (match == "" || p < match) {
				match = p
			}
		}
	}
	return match, match != ""
}

// bind records an additional binding in the innermost frame of the scope.
func (s *nsScope) bind(prefix, uri string) {
	i := len(s.frames) - 1
	if i < 0 {
		s.push(nil)
		i = 0
	}
	if s.frames[i] == nil {
		s.frames[i] = make(map[string]string)
	}
	s.frames[i][prefix] = uri
}

// nsUsage records the namespace requirements of a source subtree, gathered
// before a transplant.
type nsUsage struct {
	// preferredPrefix maps each namespace URI used with a prefix to the
	// first source prefix found bound to it.
	preferredPrefix map[string]string

	// needsDecl contains each namespace URI whose use depends at least
	// once on a binding declared outside the subtree.
	needsDecl map[string]bool

	// resolved maps each prefix used by the subtree to the URI it resolved
	// to. A prefix resolving to more than one URI is marked in conflict.
	resolved map[string]string
	conflict map[string]bool

	// declared contains all prefixes declared within the subtree.
	declared map[string]bool

	// used contains all prefixes used or declared within the subtree.
	used map[string]bool
}

// newNsUsage creates an empty nsUsage record.
func newNsUsage() *nsUsage {
	return &nsUsage{
		preferredPrefix: make(map[string]string),
		needsDecl:       make(map[string]bool),
		resolved:        make(map[string]string),
		conflict:        make(map[string]bool),
		declared:        make(map[string]bool),
		used:            make(map[string]bool),
	}
}

// collect walks the subtree rooted at 'e', recording the namespace
// requirements of every element name, attribute name, and registered QName
// attribute value. The scope 'scope' must contain the bindings in scope at
// the subtree's root; it is restored before collect returns.
func (u *nsUsage) collect(e *Element, scope *nsScope, settings *TransplantSettings) error {
	return u.walk(e, scope, len(scope.frames), settings)
}

// walk is the recursive implementation of collect. The 'base' parameter is
// the number of scope frames belonging to ancestors of the subtree.
func (u *nsUsage) walk(e *Element, scope *nsScope, base int, settings *TransplantSettings) error {
	decls := nsDeclsOf(e)
	for p := range decls {
		u.declared[p] = true
		u.used[p] = true
	}
	scope.push(decls)
	defer scope.pop()

	record := func(prefix string) error {
		uri, frame, ok := scope.lookupFrame(prefix)
		if !ok {
			return fmt.Errorf("%w: %q", ErrUnboundPrefix, prefix)
		}
		u.used[prefix] = true
		if cur, ok := u.resolved[prefix]; !ok {
			u.resolved[prefix] = uri
		} else if cur != uri {
			u.conflict[prefix] = true
		}
		if uri == "" {
			return nil
		}
		if frame < base {
			u.needsDecl[uri] = true
		}
		if _, ok := u.preferredPrefix[uri]; !ok {
			u.preferredPrefix[uri] = prefix
		}
		return nil
	}

	if e.Space != "" {
		if err := record(e.Space); err != nil {
			return err
		}
	}

	for i := range e.Attr {
		a := &e.Attr[i]
		if isNsDecl(a) {
			continue
		}
		if a.Space != "" {
			if err := record(a.Space); err != nil {
				return err
			}
		}
		if _, ok := settings.qnameAttrMode(a); ok {
			for _, token := range strings.Fields(a.Value) {
				if prefix, _ := spaceDecompose(token); prefix != "" {
					if err := record(prefix); err != nil {
						return err
					}
				}
			}
		}
	}

	for _, c := range e.Child {
		if ce, ok := c.(*Element); ok {
			if err := u.walk(ce, scope, base, settings); err != nil {
				return err
			}
		}
	}
	return nil
}

// nsDecl describes a namespace declaration to add to an element.
type nsDecl struct {
	prefix, uri string
}

// nsPlan describes the namespace declarations to add to the root of a
// transplanted subtree.
type nsPlan struct {
	defaultDecl *string
	prefixDecls []nsDecl
}

// planNsDecls computes the minimum set of namespace declarations that must
// be added to the source subtree's root so that all names used by the
// subtree resolve to the same namespace URIs once the subtree is inserted
// into the destination scope 'dstScope'.
func planNsDecls(src *Element, usage *nsUsage, srcScope, dstScope *nsScope, gen *nsPrefixGen) *nsPlan {
	plan := &nsPlan{}

	// Compensate for a difference in the default namespace unless the
	// source root already carries its own default declaration (which
	// travels with the subtree).
	srcDecls := nsDeclsOf(src)
	if _, ok := srcDecls[""]; !ok {
		if d := srcScope.defaultURI(); d != dstScope.defaultURI() {
			plan.defaultDecl = &d
		}
	}

	// Bindings visible at the transplanted root before planning: the
	// destination scope plus the source root's own declarations.
	planScope := dstScope.clone()
	planScope.push(maps.Clone(srcDecls))

	assigned := make(map[string]bool)
	for _, uri := range slices.Sorted(maps.Keys(usage.needsDecl)) {
		if _, ok := planScope.prefixFor(uri); ok {
			// The destination scope or the source root already binds a
			// prefix to this URI.
			continue
		}

		// Prefer reusing the source prefix, but only when doing so cannot
		// alter the meaning of any name in the subtree: the prefix must
		// not be declared within the subtree, and every use of it must
		// have resolved to this same URI.
		prefix := usage.preferredPrefix[uri]
		if !usablePrefix(prefix) || assigned[prefix] ||
			usage.declared[prefix] || usage.conflict[prefix] ||
			usage.resolved[prefix] != uri {
			prefix = gen.next()
		}
		assigned[prefix] = true
		gen.used[prefix] = true
		plan.prefixDecls = append(plan.prefixDecls, nsDecl{prefix, uri})
		planScope.bind(prefix, uri)
	}
	return plan
}

// usablePrefix returns true if 'prefix' may be used in a namespace
// declaration.
func usablePrefix(prefix string) bool {
	return prefix != "" && prefix != "xml" && prefix != "xmlns"
}

// nsPrefixGen generates namespace prefixes that collide with neither the
// source subtree nor the destination scope.
type nsPrefixGen struct {
	used map[string]bool
	n    int
}

// newNsPrefixGen creates a prefix generator avoiding all prefixes used or
// declared by the source subtree and all prefixes bound in the destination
// scope.
func newNsPrefixGen(usage *nsUsage, dstScope *nsScope) *nsPrefixGen {
	g := &nsPrefixGen{
		used: map[string]bool{"xml": true, "xmlns": true},
	}
	for p := range usage.used {
		g.used[p] = true
	}
	for _, frame := range dstScope.frames {
		for p := range frame {
			g.used[p] = true
		}
	}
	return g
}

// next returns a fresh, unused prefix.
func (g *nsPrefixGen) next() string {
	for {
		g.n++
		p := "ns" + strconv.Itoa(g.n)
		if !g.used[p] {
			g.used[p] = true
			return p
		}
	}
}

// bindFor returns a prefix bound to 'uri' in the scope, reusing an existing
// binding if possible. Otherwise it declares a fresh prefix on the element
// 'e' and records the binding in the scope.
func (g *nsPrefixGen) bindFor(e *Element, scope *nsScope, uri string) string {
	if p, ok := scope.prefixFor(uri); ok {
		return p
	}
	p := g.next()
	e.CreateAttr("xmlns:"+p, uri)
	scope.bind(p, uri)
	return p
}

// rewriteNs rewrites the subtree rooted at 'e' so that every element name,
// attribute name, and registered QName attribute value resolves to the same
// namespace URI in the target scope as it did in the source scope. The
// scopes 'srcScope' and 'tgtScope' must contain the source and destination
// bindings visible above the subtree's root; both are restored before
// rewriteNs returns.
func rewriteNs(e *Element, srcScope, tgtScope *nsScope, settings *TransplantSettings, gen *nsPrefixGen) {
	decls := nsDeclsOf(e)
	srcScope.push(decls)
	tgtScope.push(maps.Clone(decls))
	defer srcScope.pop()
	defer tgtScope.pop()

	if e.Space != "" {
		if uri, ok := srcScope.lookup(e.Space); ok && uri != "" {
			if bound, _ := tgtScope.lookup(e.Space); bound != uri {
				e.Space = gen.bindFor(e, tgtScope, uri)
			}
		}
	}

	for i := range e.Attr {
		a := &e.Attr[i]
		if isNsDecl(a) {
			continue
		}
		if a.Space != "" {
			if uri, ok := srcScope.lookup(a.Space); ok && uri != "" {
				if bound, _ := tgtScope.lookup(a.Space); bound != uri {
					a.Space = gen.bindFor(e, tgtScope, uri)
				}
			}
		}
		if _, ok := settings.qnameAttrMode(a); ok {
			rewriteQNameAttr(a, e, srcScope, tgtScope, gen)
		}
	}

	for _, c := range e.Child {
		if ce, ok := c.(*Element); ok {
			rewriteNs(ce, srcScope, tgtScope, settings, gen)
		}
	}
}

// rewriteQNameAttr rewrites the prefixes of the QNames held by the
// attribute 'a' so that they resolve to the same namespace URIs in the
// target scope. Unprefixed QNames are left untouched; they resolve through
// the default namespace, which the transplant preserves at the subtree's
// root.
func rewriteQNameAttr(a *Attr, e *Element, srcScope, tgtScope *nsScope, gen *nsPrefixGen) {
	tokens := strings.Fields(a.Value)
	changed := false
	for i, token := range tokens {
		prefix, local := spaceDecompose(token)
		if prefix == "" {
			continue
		}
		uri, ok := srcScope.lookup(prefix)
		if !ok || uri == "" {
			continue
		}
		if bound, _ := tgtScope.lookup(prefix); bound == uri {
			continue
		}
		tokens[i] = gen.bindFor(e, tgtScope, uri) + ":" + local
		changed = true
	}
	if changed {
		a.Value = strings.Join(tokens, " ")
	}
}
