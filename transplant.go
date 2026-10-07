// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const xmlNamespaceURI = "http://www.w3.org/XML/1998/namespace"

// Namespace transplantation errors.
var (
	// ErrTransplantNilElement is returned when a transplant function is
	// called with a nil source or target element.
	ErrTransplantNilElement = errors.New("etree: transplant source and target elements must not be nil")

	// ErrTransplantSelf is returned when a transplant function is asked to
	// insert a subtree into itself.
	ErrTransplantSelf = errors.New("etree: cannot transplant an element into its own subtree")

	// ErrTransplantAncestor is returned when a move transplant would insert
	// an element into its own descendant.
	ErrTransplantAncestor = errors.New("etree: cannot move an element into its own descendant")

	// ErrUndefinedPrefix is returned when a prefix used by the source
	// subtree cannot be resolved to a namespace URI.
	ErrUndefinedPrefix = errors.New("etree: transplant encountered an undefined namespace prefix")
)

// QNameAttrs registers attributes whose values are QNames or whitespace-
// separated lists of QNames. Ordinary attribute values are never interpreted
// as QNames; only attributes explicitly registered here have their values
// rewritten when prefixes change during a namespace transplant.
//
// Attributes are identified by their expanded name: the namespace URI
// associated with the attribute's prefix and the attribute's local name.
// For example, to register xsi:type attributes:
//
//	qna := etree.NewQNameAttrs()
//	qna.Register("http://www.w3.org/2001/XMLSchema-instance", "type")
type QNameAttrs struct {
	attrs map[qnameAttrKey]bool
}

type qnameAttrKey struct {
	uri string
	key string
}

// NewQNameAttrs creates an empty QName attribute registry.
func NewQNameAttrs() *QNameAttrs {
	return &QNameAttrs{attrs: make(map[qnameAttrKey]bool)}
}

// Register marks the attribute with the requested namespace URI and local
// name as carrying a QName or whitespace-separated list of QNames.
func (q *QNameAttrs) Register(uri, localName string) *QNameAttrs {
	if q.attrs == nil {
		q.attrs = make(map[qnameAttrKey]bool)
	}
	q.attrs[qnameAttrKey{uri, localName}] = true
	return q
}

// TransplantSettings configure a namespace-safe subtree transplant.
type TransplantSettings struct {
	// Move selects the transplant mode. When false (the default), the
	// source subtree is copied and the original tree is left in place.
	// When true, the source subtree is removed from its original parent
	// after the transplanted copy has been built.
	Move bool

	// QNameAttrs identifies attributes whose values contain QNames. When
	// nil, no attribute values are interpreted or rewritten.
	QNameAttrs *QNameAttrs
}

// Transplant builds a namespace-safe copy of the subtree rooted at src and
// appends it as the last child of target. Namespace prefix bindings used by
// the source subtree are reconciled with those in effect around target, so
// that element names, attribute names, and registered QName values keep the
// same expanded meaning after the move. New namespace declarations are added
// only where required. If settings is nil, default (copy-mode) settings are
// used.
//
// Neither the source nor the target tree is modified unless the transplant
// succeeds; on error both trees are left unchanged.
func Transplant(target, src *Element, settings *TransplantSettings) (*Element, error) {
	return transplantAt(target, len(target.Child), src, settings)
}

// TransplantAt behaves like Transplant but inserts the copied subtree into
// target's child list just before the requested index. If the index is
// greater than or equal to the length of the child list, the copy is
// appended. If settings is nil, default (copy-mode) settings are used.
//
// Neither the source nor the target tree is modified unless the transplant
// succeeds; on error both trees are left unchanged.
func TransplantAt(target *Element, index int, src *Element, settings *TransplantSettings) (*Element, error) {
	return transplantAt(target, index, src, settings)
}

func transplantAt(target *Element, index int, src *Element, settings *TransplantSettings) (*Element, error) {
	if target == nil || src == nil {
		return nil, ErrTransplantNilElement
	}

	var s TransplantSettings
	if settings != nil {
		s = *settings
	}

	for e := target; e != nil; e = e.parent {
		if e == src {
			if s.Move {
				return nil, ErrTransplantAncestor
			}
			return nil, ErrTransplantSelf
		}
	}

	analysis, err := analyzeSubtree(src, ancestorScopes(src), s.QNameAttrs)
	if err != nil {
		return nil, err
	}

	// The new subtree becomes a child of target, so target's own namespace
	// declarations are in effect for it as well.
	dstFrames := ancestorFrames(target)
	if f := elementFrame(target); f != nil {
		dstFrames = append(dstFrames, f)
	}

	tx := &transplant{
		root:         src,
		taken:        map[string]bool{"xml": true, "xmlns": true},
		plans:        analysis.plans,
		binds:        analysis.binds,
		defaultNeeds: analysis.defaultNeeds,
		assign:       make(map[bindKey]string),
	}
	for _, f := range dstFrames {
		for p := range f {
			tx.taken[p] = true
		}
	}

	root, err := tx.emit(src, dstFrames)
	if err != nil {
		return nil, err
	}

	if s.Move && src.parent != nil {
		movedIndex := src.index
		movedFrom := src.parent
		movedFrom.RemoveChild(src)
		if movedFrom == target && movedIndex < index {
			index--
		}
	}

	if index < 0 {
		index = 0
	}
	target.InsertChildAt(index, root)
	return root, nil
}

// nsFrame maps prefixes in effect at one element to namespace URIs. The
// empty prefix key holds the default namespace URI.
type nsFrame map[string]string

// nsScope pairs a namespace frame with the element whose start tag declares
// it. Frames inherited from ancestors have a nil owner.
type nsScope struct {
	frame nsFrame
	owner *Element
}

// nsRef describes the resolution of one prefix used by a subtree node.
type nsRef struct {
	prefix  string
	uri     string
	owner   *Element // element declaring the binding, or nil when inherited
	builtin bool     // the predefined "xml" prefix
}

// bindKey identifies a namespace binding: the declaring element (nil for
// inherited bindings) and the source prefix.
type bindKey struct {
	owner  *Element
	prefix string
}

// bindInfo records a binding the subtree depends on.
type bindInfo struct {
	key bindKey
	uri string
}

// nsDecl is one namespace declaration made on an element.
type nsDecl struct {
	prefix string
	uri    string
}

// qnamePart is one token of a QName-list value.
type qnamePart struct {
	prefix string
	text   string
	ref    nsRef
}

// qnameRef locates a registered QName-valued attribute within its element.
type qnameRef struct {
	attrIndex int
	parts     []qnamePart
}

// elemPlan holds the analyzed namespace information for one source element.
type elemPlan struct {
	ownDecls   []nsDecl // unique declarations in source order
	ownUsed    map[string]bool
	tagRef     nsRef
	attrRefs   map[int]nsRef // bindings used by prefixed non-xmlns attributes
	qnameAttrs []qnameRef
}

// declURI returns the URI of the element's own declaration for prefix.
func (p *elemPlan) declURI(prefix string) string {
	for _, d := range p.ownDecls {
		if d.prefix == prefix {
			return d.uri
		}
	}
	return ""
}

// ownDeclDefault reports whether the element declares a default namespace.
func (p *elemPlan) ownDeclDefault() bool {
	for _, d := range p.ownDecls {
		if d.prefix == "" {
			return true
		}
	}
	return false
}

// analysisResult is the output of the subtree analysis pass.
type analysisResult struct {
	plans map[*Element]*elemPlan
	binds []bindInfo // distinct bindings, ordered by declaration depth
	// defaultNeeds maps a subtree element to the inherited default URI it
	// must establish (including the empty URI, i.e. xmlns="").
	defaultNeeds map[*Element]string
}

// transplant carries the working state of a single transplant operation.
type transplant struct {
	root         *Element
	taken        map[string]bool // prefix names unavailable to fresh minting
	counter      int
	plans        map[*Element]*elemPlan
	binds        []bindInfo
	defaultNeeds map[*Element]string
	assign       map[bindKey]string // stable source-binding to target-prefix map
}

// ancestorScopes returns the namespace scopes declared by ancestors of e,
// ordered from the document root toward e's parent.
func ancestorScopes(e *Element) []nsScope {
	var chain []*Element
	for p := e.parent; p != nil; p = p.parent {
		chain = append(chain, p)
	}
	scopes := make([]nsScope, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		if f := elementFrame(chain[i]); f != nil {
			scopes = append(scopes, nsScope{frame: f, owner: chain[i]})
		}
	}
	return scopes
}

// ancestorFrames returns the namespace frames declared by ancestors of e,
// ordered from the document root toward e's parent.
func ancestorFrames(e *Element) []nsFrame {
	scopes := ancestorScopes(e)
	frames := make([]nsFrame, len(scopes))
	for i, s := range scopes {
		frames[i] = s.frame
	}
	return frames
}

// elementFrame returns the namespace declarations made directly on e. It
// returns nil if the element declares no namespaces. Repeated declarations
// of the same prefix follow XML last-wins semantics.
func elementFrame(e *Element) nsFrame {
	var frame nsFrame
	for _, a := range e.Attr {
		switch {
		case a.Space == "xmlns":
			if a.Key == "xml" || a.Key == "xmlns" {
				continue
			}
			if frame == nil {
				frame = nsFrame{}
			}
			frame[a.Key] = a.Value
		case a.Space == "" && a.Key == "xmlns":
			if frame == nil {
				frame = nsFrame{}
			}
			frame[""] = a.Value
		}
	}
	return frame
}

// isXMLNSAttr reports whether the attribute is a namespace declaration.
func isXMLNSAttr(a Attr) bool {
	return a.Space == "xmlns" || (a.Space == "" && a.Key == "xmlns")
}

// resolvePrefix resolves a prefix using the scope stack (ancestor frames
// followed by the current element's frame).
func resolvePrefix(scopes []nsScope, prefix string) (nsRef, error) {
	if prefix == "xml" {
		return nsRef{prefix: "xml", uri: xmlNamespaceURI, builtin: true}, nil
	}
	for i := len(scopes) - 1; i >= 0; i-- {
		if uri, ok := scopes[i].frame[prefix]; ok {
			return nsRef{prefix: prefix, uri: uri, owner: scopes[i].owner}, nil
		}
	}
	return nsRef{}, fmt.Errorf("%w: %q", ErrUndefinedPrefix, prefix)
}

// resolveTag resolves the element tag's prefix. An empty prefix resolves to
// the default namespace in effect (which may be the empty URI).
func resolveTag(scopes []nsScope, space string) (nsRef, error) {
	if space == "" {
		uri := ""
		var owner *Element
		for i := len(scopes) - 1; i >= 0; i-- {
			if u, ok := scopes[i].frame[""]; ok {
				uri, owner = u, scopes[i].owner
				break
			}
		}
		return nsRef{prefix: "", uri: uri, owner: owner}, nil
	}
	return resolvePrefix(scopes, space)
}

// qnameTokenRE locates the whitespace-delimited tokens inside a QName-list
// value, so separators can be preserved unchanged.
var qnameTokenRE = regexp.MustCompile(`\S+`)

var ncnameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\-]*$`)

// analyzeSubtree walks the source subtree and records every namespace
// binding its names depend on.
func analyzeSubtree(root *Element, base []nsScope, qna *QNameAttrs) (*analysisResult, error) {
	res := &analysisResult{
		plans:        make(map[*Element]*elemPlan),
		defaultNeeds: make(map[*Element]string),
	}
	used := make(map[bindKey]string)

	var walk func(e *Element, scopes []nsScope) error
	walk = func(e *Element, scopes []nsScope) error {
		plan := &elemPlan{
			ownUsed:  make(map[string]bool),
			attrRefs: make(map[int]nsRef),
		}

		frame := nsFrame{}
		seenDecl := map[string]bool{}
		for i := range e.Attr {
			a := &e.Attr[i]
			if !isXMLNSAttr(*a) {
				continue
			}
			prefix := ""
			if a.Space == "xmlns" {
				prefix = a.Key
				if prefix == "xml" || prefix == "xmlns" {
					continue
				}
				if a.Value == "" {
					return fmt.Errorf("%w: %q", ErrUndefinedPrefix, prefix)
				}
			}
			if !seenDecl[prefix] {
				seenDecl[prefix] = true
				plan.ownDecls = append(plan.ownDecls, nsDecl{prefix: prefix, uri: a.Value})
			}
			frame[prefix] = a.Value
		}

		childScopes := append(scopes, nsScope{frame: frame, owner: e})

		tagRef, err := resolveTag(childScopes, e.Space)
		if err != nil {
			return fmt.Errorf("element %q: %w", e.FullTag(), err)
		}
		plan.tagRef = tagRef

		for i := range e.Attr {
			a := &e.Attr[i]
			if isXMLNSAttr(*a) || a.Space == "" {
				continue
			}
			ref, err := resolvePrefix(childScopes, a.Space)
			if err != nil {
				return fmt.Errorf("attribute %q: %w", a.FullKey(), err)
			}
			plan.attrRefs[i] = ref
		}

		if qna != nil {
			for i := range e.Attr {
				a := &e.Attr[i]
				if isXMLNSAttr(*a) {
					continue
				}
				attrURI := ""
				if a.Space != "" {
					ref, ok := plan.attrRefs[i]
					if !ok {
						continue
					}
					attrURI = ref.uri
				}
				if !qna.attrs[qnameAttrKey{attrURI, a.Key}] {
					continue
				}
				qr := qnameRef{attrIndex: i}
				for _, m := range qnameTokenRE.FindAllStringIndex(a.Value, -1) {
					token := a.Value[m[0]:m[1]]
					prefix, local := "", token
					if colon := strings.IndexByte(token, ':'); colon >= 0 {
						if strings.IndexByte(token[colon+1:], ':') >= 0 {
							return fmt.Errorf("etree: invalid QName %q in attribute %q", token, a.FullKey())
						}
						prefix, local = token[:colon], token[colon+1:]
					}
					if !ncnameRE.MatchString(prefix) || !ncnameRE.MatchString(local) {
						return fmt.Errorf("etree: invalid QName %q in attribute %q", token, a.FullKey())
					}
					part := qnamePart{prefix: prefix, text: token}
					if prefix != "" {
						ref, err := resolvePrefix(childScopes, prefix)
						if err != nil {
							return fmt.Errorf("qname attribute %q: %w", a.FullKey(), err)
						}
						part.ref = ref
					}
					qr.parts = append(qr.parts, part)
				}
				plan.qnameAttrs = append(plan.qnameAttrs, qr)
			}
		}

		res.plans[e] = plan

		for _, t := range e.Child {
			if child, ok := t.(*Element); ok {
				if err := walk(child, childScopes); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := walk(root, base); err != nil {
		return nil, err
	}

	// Collect the distinct bindings referenced by the subtree.
	normalize := func(ref nsRef) nsRef {
		if ref.builtin {
			return ref
		}
		owner := ref.owner
		if owner != nil {
			if _, ok := res.plans[owner]; !ok {
				owner = nil
			}
		}
		return nsRef{prefix: ref.prefix, uri: ref.uri, owner: owner, builtin: ref.builtin}
	}
	mark := func(ref nsRef, referrer *Element) nsRef {
		ref = normalize(ref)
		owner := ref.owner
		if owner == nil && ref.prefix == "" {
			// Inherited default bindings must be established at the
			// referring element because unprefixed names cannot be
			// rewritten.
			res.defaultNeeds[referrer] = ref.uri
			return ref
		}
		key := bindKey{owner: owner, prefix: ref.prefix}
		if _, ok := used[key]; !ok {
			used[key] = ref.uri
		}
		if owner != nil {
			res.plans[owner].ownUsed[ref.prefix] = true
		}
		return ref
	}

	for e, plan := range res.plans {
		plan.tagRef = mark(plan.tagRef, e)
		for i, ref := range plan.attrRefs {
			plan.attrRefs[i] = mark(ref, e)
		}
		for _, qr := range plan.qnameAttrs {
			for j, part := range qr.parts {
				if part.prefix != "" {
					qr.parts[j].ref = mark(part.ref, e)
				}
			}
		}
	}

	keys := make([]bindKey, 0, len(used))
	for k := range used {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if (keys[i].owner == nil) != (keys[j].owner == nil) {
			return keys[i].owner == nil
		}
		di, dj := elementDepth(keys[i].owner, root), elementDepth(keys[j].owner, root)
		if di != dj {
			return di < dj
		}
		return keys[i].prefix < keys[j].prefix
	})
	for _, k := range keys {
		res.binds = append(res.binds, bindInfo{key: k, uri: used[k]})
	}
	return res, nil
}

// elementDepth reports the depth of e relative to root (root is depth 0,
// nil is depth -1).
func elementDepth(e, root *Element) int {
	if e == nil {
		return -1
	}
	d := 0
	for p := e; p != nil && p != root; p = p.parent {
		d++
	}
	return d
}

// emit constructs the namespace-adjusted copy of element e. dstFrames are
// the namespace frames visible at the insertion point.
func (tx *transplant) emit(e *Element, dstFrames []nsFrame) (*Element, error) {
	plan := tx.plans[e]

	// Effective bindings visible here: target ancestor frames merged with
	// frames introduced by emitted elements.
	visible := effectiveFrame(dstFrames)

	var declared []Attr

	// First, introduce prefixed bindings declared at this source element.
	ownKeys := make([]string, 0, len(plan.ownDecls))
	for _, d := range plan.ownDecls {
		if d.prefix != "" && plan.ownUsed[d.prefix] {
			ownKeys = append(ownKeys, d.prefix)
		}
	}
	sort.Strings(ownKeys)
	for _, p := range ownKeys {
		uri := plan.declURI(p)
		tp, reused := tx.assignPrefix(bindKey{owner: e, prefix: p}, p, uri, visible)
		if !reused {
			declared = append(declared, makeXMLNSAttr(tp, uri))
			visible[tp] = uri
		}
	}

	// Introduce this element's own default namespace if the element's
	// unprefixed tag (or an unprefixed name in its subtree) uses it.
	ownDefault := plan.ownDeclDefault()
	ownDefaultUsed := plan.ownUsed[""]
	if ownDefaultUsed && ownDefault {
		uri := plan.declURI("")
		if cur, ok := visible[""]; !ok || cur != uri {
			declared = append(declared, makeXMLNSAttr("", uri))
			visible[""] = uri
		}
	}

	// Then introduce inherited prefixed bindings, hoisted onto the root.
	if e == tx.root {
		var hoistPrefixes []string
		hoistURI := map[string]string{}
		for _, b := range tx.binds {
			if b.key.owner != nil {
				continue
			}
			if b.key.prefix == "" {
				continue
			}
			hoistPrefixes = append(hoistPrefixes, b.key.prefix)
			hoistURI[b.key.prefix] = b.uri
		}
		sort.Strings(hoistPrefixes)
		for _, p := range hoistPrefixes {
			uri := hoistURI[p]
			tp, reused := tx.assignPrefix(bindKey{owner: nil, prefix: p}, p, uri, visible)
			if !reused {
				declared = append(declared, makeXMLNSAttr(tp, uri))
				visible[tp] = uri
			}
		}
	}

	// Finally, establish the inherited default namespace at any element
	// whose unprefixed names depended on it.
	defaultURI, hasDefault := tx.defaultNeeds[e]
	if hasDefault {
		if cur, ok := visible[""]; !ok && defaultURI != "" {
			declared = append(declared, makeXMLNSAttr("", defaultURI))
			visible[""] = defaultURI
		} else if ok && cur != defaultURI {
			declared = append(declared, makeXMLNSAttr("", defaultURI))
			visible[""] = defaultURI
		}
	}

	resolvePrefix := func(ref nsRef) (string, error) {
		if ref.builtin {
			return ref.prefix, nil
		}
		if ref.prefix == "" {
			return "", nil
		}
		if tp, ok := tx.assign[bindKey{owner: ref.owner, prefix: ref.prefix}]; ok {
			return tp, nil
		}
		return "", fmt.Errorf("%w: %q", ErrUndefinedPrefix, ref.prefix)
	}

	tagSpace, err := resolvePrefix(plan.tagRef)
	if err != nil {
		return nil, err
	}
	ne := newElement(tagSpace, e.Tag, nil)

	for i := range declared {
		a := declared[i]
		a.element = ne
		ne.Attr = append(ne.Attr, a)
	}

	qnameSet := make(map[int]qnameRef, len(plan.qnameAttrs))
	for _, qr := range plan.qnameAttrs {
		qnameSet[qr.attrIndex] = qr
	}

	for i := range e.Attr {
		a := &e.Attr[i]
		if isXMLNSAttr(*a) {
			continue
		}
		copied := Attr{Key: a.Key, Value: a.Value}
		if a.Space != "" {
			copied.Space, err = resolvePrefix(plan.attrRefs[i])
			if err != nil {
				return nil, err
			}
		}
		if qr, ok := qnameSet[i]; ok {
			copied.Value = tx.rewriteQNameValue(a.Value, qr, resolvePrefix)
		}
		copied.element = ne
		ne.Attr = append(ne.Attr, copied)
	}

	var childFrames = dstFrames
	if f := frameOfDecls(declared); f != nil {
		childFrames = appendFrames(dstFrames, []nsFrame{f})
	}

	for _, t := range e.Child {
		if child, ok := t.(*Element); ok {
			ce, err := tx.emit(child, childFrames)
			if err != nil {
				return nil, err
			}
			ne.addChild(ce)
		} else {
			ne.addChild(t.dup(ne))
		}
	}

	return ne, nil
}

// assignPrefix selects and records the target prefix for a source binding,
// reusing an effective prefix with the same URI where possible. It reports
// whether the binding is already in effect (so no declaration is needed).
func (tx *transplant) assignPrefix(key bindKey, srcPrefix, uri string, visible nsFrame) (tp string, reused bool) {
	if tp, ok := tx.assign[key]; ok {
		return tp, visible[tp] == uri
	}
	tp, reused = pickPrefixForURI(visible, uri, srcPrefix, tx.taken)
	tx.assign[key] = tp
	tx.taken[tp] = true
	return tp, reused
}

// pickPrefixForURI chooses a target prefix: an existing effective prefix
// mapped to uri if one exists, otherwise the source prefix if free, and a
// freshly minted prefix as a last resort.
func pickPrefixForURI(visible nsFrame, uri, srcPrefix string, taken map[string]bool) (string, bool) {
	var candidates []string
	for p, u := range visible {
		if p != "" && u == uri {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) > 0 {
		sort.Strings(candidates)
		return candidates[0], true
	}
	if srcPrefix != "" && srcPrefix != "xml" && srcPrefix != "xmlns" {
		if u, bound := visible[srcPrefix]; !bound || u == uri {
			return srcPrefix, false
		}
	}
	for n := 1; ; n++ {
		p := fmt.Sprintf("ns%d", n)
		if !taken[p] {
			taken[p] = true
			return p, false
		}
	}
}

// rewriteQNameValue rewrites the prefixed tokens of a registered QName-list
// value while preserving its original whitespace separators.
func (tx *transplant) rewriteQNameValue(value string, qr qnameRef, resolve func(nsRef) (string, error)) string {
	idx := 0
	return qnameTokenRE.ReplaceAllStringFunc(value, func(token string) string {
		if idx >= len(qr.parts) {
			return token
		}
		part := qr.parts[idx]
		idx++
		if part.prefix == "" {
			return part.text
		}
		local := part.text[strings.IndexByte(part.text, ':')+1:]
		tp, err := resolve(part.ref)
		if err != nil {
			return part.text
		}
		return tp + ":" + local
	})
}

// effectiveFrame collapses a frame stack into the bindings in effect, with
// nearer frames overriding outer ones.
func effectiveFrame(frames []nsFrame) nsFrame {
	effective := nsFrame{}
	for _, f := range frames {
		for p, u := range f {
			effective[p] = u
		}
	}
	return effective
}

// frameOfDecls converts emitted declaration attributes back into a frame.
func frameOfDecls(decls []Attr) nsFrame {
	var f nsFrame
	for _, a := range decls {
		if a.Space == "xmlns" {
			if f == nil {
				f = nsFrame{}
			}
			f[a.Key] = a.Value
		} else if a.Key == "xmlns" {
			if f == nil {
				f = nsFrame{}
			}
			f[""] = a.Value
		}
	}
	return f
}

func appendFrames(base, extra []nsFrame) []nsFrame {
	if len(extra) == 0 {
		return base
	}
	out := make([]nsFrame, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}

// makeXMLNSAttr builds a namespace declaration attribute.
func makeXMLNSAttr(prefix, uri string) Attr {
	if prefix == "" {
		return Attr{Key: "xmlns", Value: uri}
	}
	return Attr{Space: "xmlns", Key: prefix, Value: uri}
}
