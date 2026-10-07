package participle

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// GrammarChangeKind classifies a single structural difference between two grammars.
type GrammarChangeKind int

const (
	// AddedProduction names a production that exists only in the new grammar.
	AddedProduction GrammarChangeKind = iota + 1
	// RemovedProduction names a production that exists only in the old grammar.
	RemovedProduction
	// TokenChanged reports that a terminal token (a named reference or a literal) changed.
	TokenChanged
	// QuantifierChanged reports that a ?, *, + or ! quantifier was added, removed or altered.
	QuantifierChanged
	// SequenceChanged reports that the ordered terms of a sequence changed.
	SequenceChanged
	// OrderedBranchChanged reports that alternatives were added, removed or reordered.
	OrderedBranchChanged
)

// String returns a short, human readable name for the change kind.
func (k GrammarChangeKind) String() string {
	switch k {
	case AddedProduction:
		return "added production"
	case RemovedProduction:
		return "removed production"
	case TokenChanged:
		return "token"
	case QuantifierChanged:
		return "quantifier"
	case SequenceChanged:
		return "sequence"
	case OrderedBranchChanged:
		return "ordered branch"
	default:
		return "unknown"
	}
}

// GrammarChange is a single structural difference found while comparing two parsers.
//
// Production is the stable identity of the affected production (its Go type package
// path and name). Kind distinguishes production-level and body-level changes, while Path,
// From and To describe where in the production the change occurred.
type GrammarChange struct {
	Production string
	Kind       GrammarChangeKind
	Path       string
	From       string
	To         string
}

// String renders the change as a single readable line.
func (c GrammarChange) String() string {
	switch c.Kind {
	case AddedProduction:
		return fmt.Sprintf("added production %s", c.Production)
	case RemovedProduction:
		return fmt.Sprintf("removed production %s", c.Production)
	}
	detail := fmt.Sprintf("%s changed from %s to %s", c.Kind, c.From, c.To)
	if c.Path != "" {
		detail = fmt.Sprintf("%s: %s changed from %s to %s", c.Path, c.Kind, c.From, c.To)
	}
	return fmt.Sprintf("production %s: %s", c.Production, detail)
}

// GrammarDiffReport is the result of comparing two grammars.
type GrammarDiffReport struct {
	Added    []GrammarChange
	Removed  []GrammarChange
	Modified []GrammarChange
}

// Changed reports whether the two grammars are structurally different.
func (r *GrammarDiffReport) Changed() bool {
	return len(r.Added) != 0 || len(r.Removed) != 0 || len(r.Modified) != 0
}

// String renders the report as a stable, human readable block.
//
// Productions and changes are listed in a deterministic order so the output is safe to
// commit or diff between runs. Whitespace used only for presentation is not significant.
func (r *GrammarDiffReport) String() string {
	var b strings.Builder
	writeSection := func(heading string, changes []GrammarChange) {
		if len(changes) == 0 {
			return
		}
		if b.Len() != 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s:\n", heading)
		for _, c := range changes {
			fmt.Fprintf(&b, "  %s\n", c.String())
		}
	}
	writeSection("Added productions", r.Added)
	writeSection("Removed productions", r.Removed)
	writeSection("Modified productions", r.Modified)
	if b.Len() == 0 {
		return "no grammar changes"
	}
	return strings.TrimRight(b.String(), "\n")
}

// CompareGrammar compares the grammar built into oldParser with the one built into
// newParser and returns a structural change report.
//
// Productions are matched by the stable identity of their Go type (package path plus
// type name), not by pointer identity or by formatting their EBNF. Recursive references
// are treated as opaque edges: only the referenced production and its token references are
// compared, never expanded recursively. Custom parsers (ParseTypeWith and Parseable types)
// are likewise compared by their declared type identity alone.
//
// Whitespace and other presentation-only differences in struct tags are ignored, but the
// order of alternatives in an ordered branch is significant and reported when changed.
func CompareGrammar[A, B any](oldParser *Parser[A], newParser *Parser[B]) (*GrammarDiffReport, error) {
	if oldParser == nil || newParser == nil {
		return nil, fmt.Errorf("CompareGrammar: both parsers must be non-nil")
	}
	oldProds, err := extractProductions(oldParser.typeNodes)
	if err != nil {
		return nil, err
	}
	newProds, err := extractProductions(newParser.typeNodes)
	if err != nil {
		return nil, err
	}
	return buildDiffReport(oldProds, newProds), nil
}

// grammarProductions exposes the canonical production set of a parser. It lets grammars
// built from function-local root types be compared without referencing those local types
// in a function signature.
func (p *Parser[G]) grammarProductions() (map[string]*diffProduction, error) {
	return extractProductions(p.typeNodes)
}

// buildDiffReport matches productions by stable identity and diffs the bodies of those
// present in both grammars.
func buildDiffReport(oldProds, newProds map[string]*diffProduction) *GrammarDiffReport {
	oldByID := map[string]*diffProduction{}
	newByID := map[string]*diffProduction{}
	for _, prod := range oldProds {
		oldByID[prod.id] = prod
	}
	for _, prod := range newProds {
		newByID[prod.id] = prod
	}

	report := &GrammarDiffReport{}
	matched := make(map[string]struct{})

	for id := range oldByID {
		if _, ok := newByID[id]; !ok {
			report.Removed = append(report.Removed, GrammarChange{
				Production: id,
				Kind:       RemovedProduction,
			})
		} else {
			matched[id] = struct{}{}
		}
	}
	for id := range newByID {
		if _, ok := oldByID[id]; !ok {
			report.Added = append(report.Added, GrammarChange{
				Production: id,
				Kind:       AddedProduction,
			})
		}
	}

	for id := range matched {
		changes := diffNodes(oldByID[id].body, newByID[id].body)
		for i := range changes {
			changes[i].Production = id
			report.Modified = append(report.Modified, changes[i])
		}
	}

	sortChanges(report.Added)
	sortChanges(report.Removed)
	sortChanges(report.Modified)
	return report
}

func sortChanges(changes []GrammarChange) {
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].Production != changes[j].Production {
			return changes[i].Production < changes[j].Production
		}
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		if changes[i].Kind != changes[j].Kind {
			return changes[i].Kind < changes[j].Kind
		}
		if changes[i].From != changes[j].From {
			return changes[i].From < changes[j].From
		}
		return changes[i].To < changes[j].To
	})
}

// diffProduction is one top-level production keyed by a stable identity.
type diffProduction struct {
	id   string
	name string
	kind dKind
	body dNode
}

// dKind discriminates the production kinds that carry a declared identity.
type dKind int

const (
	dStruct dKind = iota
	dUnion
	dOpaque // custom parse function or external Parseable type
)

// dNode is a canonicalised, presentation-independent view of an expression node.
//
// It is derived purely from grammar structure (node kind, token identity, quantifier and
// child order), never from its rendered EBNF text.
type dNode struct {
	kind dNodeKind
	// tok carries the stable terminal or production identity.
	tok string
	// lit marks tok as a string literal rather than a named token reference.
	lit bool
	// mode carries the quantifier for dGroup.
	mode groupMatchMode
	// negative carries the polarity for dLookahead.
	negative bool
	children []dNode
}

type dNodeKind int

const (
	dRef        dNodeKind = iota // named lexer token reference
	dLiteral                     // matched string literal
	dProd                        // reference to another production, identified by tok
	dOpaqueNode                  // custom parser / Parseable, identified by tok
	dGroup                       // quantified expression
	dNegation
	dLookahead
	dCapture // transparent marker, carries its single child
	dSeq     // ordered sequence of terms
	dAlt     // ordered set of alternatives
)

// extractProductions builds one diffProduction per top-level grammar node. Nodes that only
// appear inline (external Parseable types) are registered as opaque productions as they
// are encountered, so their declared identity is compared rather than ignored.
// extractProductions builds one diffProduction per top-level grammar node. Nodes that only
// appear inline (external Parseable types, or productions reached through a recursive
// field) are registered as they are encountered, so their declared identity is compared
// rather than ignored.
func extractProductions(typeNodes map[reflect.Type]node) (map[string]*diffProduction, error) {
	prods := map[string]*diffProduction{}
	inProgress := map[string]bool{}

	// reserve records a production identity before its body is normalised. Combined with
	// inProgress this makes recursive references resolve to an opaque edge instead of
	// re-entering and expanding the body forever.
	reserve := func(t reflect.Type, kind dKind) *diffProduction {
		id := productionID(t)
		if p, ok := prods[id]; ok {
			return p
		}
		p := &diffProduction{id: id, name: productionName(t), kind: kind}
		prods[id] = p
		return p
	}

	types := make([]reflect.Type, 0, len(typeNodes))
	for t := range typeNodes {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return productionID(types[i]) < productionID(types[j]) })

	for _, t := range types {
		switch n := typeNodes[t].(type) {
		case *strct:
			p := reserve(t, dStruct)
			if !isPlaceholder(p) {
				continue
			}
			inProgress[p.id] = true
			body, err := normalizeNode(n.expr, reserve, inProgress)
			delete(inProgress, p.id)
			if err != nil {
				return nil, err
			}
			p.body = body
		case *union:
			p := reserve(t, dUnion)
			if !isPlaceholder(p) {
				continue
			}
			inProgress[p.id] = true
			children := make([]dNode, 0, len(n.disjunction.nodes))
			for _, member := range n.disjunction.nodes {
				child, err := normalizeNode(member, reserve, inProgress)
				if err != nil {
					return nil, err
				}
				children = append(children, child)
			}
			delete(inProgress, p.id)
			p.body = dNode{kind: dAlt, children: children}
		case *custom:
			p := reserve(t, dOpaque)
			p.body = dNode{kind: dOpaqueNode, tok: p.id}
		default:
			return nil, fmt.Errorf("unsupported production node %T for %s", typeNodes[t], productionID(t))
		}
	}
	return prods, nil
}

func normalizeNode(
	n node,
	reserve func(reflect.Type, dKind) *diffProduction,
	inProgress map[string]bool,
) (dNode, error) {
	switch n := n.(type) {
	case *strct:
		p := reserve(n.typ, dStruct)
		// A re-entrant (in-progress) or already-built production is an opaque edge.
		if isPlaceholder(p) && !inProgress[p.id] {
			inProgress[p.id] = true
			body, err := normalizeNode(n.expr, reserve, inProgress)
			delete(inProgress, p.id)
			if err != nil {
				return dNode{}, err
			}
			p.body = body
		}
		return dNode{kind: dProd, tok: p.id}, nil

	case *union:
		p := reserve(n.typ, dUnion)
		if isPlaceholder(p) && !inProgress[p.id] {
			inProgress[p.id] = true
			children := make([]dNode, 0, len(n.disjunction.nodes))
			for _, member := range n.disjunction.nodes {
				child, err := normalizeNode(member, reserve, inProgress)
				if err != nil {
					return dNode{}, err
				}
				children = append(children, child)
			}
			delete(inProgress, p.id)
			p.body = dNode{kind: dAlt, children: children}
		}
		return dNode{kind: dProd, tok: p.id}, nil

	case *custom:
		p := reserve(n.typ, dOpaque)
		p.body = dNode{kind: dOpaqueNode, tok: p.id}
		return dNode{kind: dOpaqueNode, tok: p.id}, nil

	case *parseable:
		p := reserve(n.t, dOpaque)
		p.body = dNode{kind: dOpaqueNode, tok: p.id}
		return dNode{kind: dOpaqueNode, tok: p.id}, nil

	case *reference:
		return dNode{kind: dRef, tok: n.identifier}, nil

	case *literal:
		return dNode{kind: dLiteral, tok: n.s, lit: true}, nil

	case *negation:
		child, err := normalizeNode(n.node, reserve, inProgress)
		if err != nil {
			return dNode{}, err
		}
		return dNode{kind: dNegation, children: []dNode{child}}, nil

	case *lookaheadGroup:
		child, err := normalizeNode(n.expr, reserve, inProgress)
		if err != nil {
			return dNode{}, err
		}
		return dNode{kind: dLookahead, negative: n.negative, children: []dNode{child}}, nil

	case *capture:
		child, err := normalizeNode(n.node, reserve, inProgress)
		if err != nil {
			return dNode{}, err
		}
		return dNode{kind: dCapture, children: []dNode{child}}, nil

	case *group:
		child, err := normalizeNode(n.expr, reserve, inProgress)
		if err != nil {
			return dNode{}, err
		}
		// A bare parenthesised group without a quantifier is presentation only.
		if n.mode == groupMatchOnce {
			return child, nil
		}
		return dNode{kind: dGroup, mode: n.mode, children: []dNode{child}}, nil

	case *disjunction:
		children := make([]dNode, 0, len(n.nodes))
		for _, branch := range n.nodes {
			child, err := normalizeNode(branch, reserve, inProgress)
			if err != nil {
				return dNode{}, err
			}
			// Flatten non-quantified nested alternatives while preserving order.
			if child.kind == dAlt {
				children = append(children, child.children...)
			} else {
				children = append(children, child)
			}
		}
		if len(children) == 1 {
			return children[0], nil
		}
		return dNode{kind: dAlt, children: children}, nil

	case *sequence:
		terms := []dNode{}
		for cur := n; cur != nil; cur = cur.next {
			child, err := normalizeNode(cur.node, reserve, inProgress)
			if err != nil {
				return dNode{}, err
			}
			// Flatten non-quantified nested sequences while preserving order.
			if child.kind == dSeq {
				terms = append(terms, child.children...)
			} else {
				terms = append(terms, child)
			}
		}
		if len(terms) == 1 {
			return terms[0], nil
		}
		return dNode{kind: dSeq, children: terms}, nil

	default:
		return dNode{}, fmt.Errorf("unsupported grammar node %T", n)
	}
}
func isPlaceholder(p *diffProduction) bool {
	return p.body.kind == 0 && len(p.body.children) == 0 && p.body.tok == ""
}

// productionID is the stable identity of a production: package path plus declared name.
// It is independent of reflect.Type pointer identity, so the same named type compares
// equal even across separately constructed grammars.
func productionID(t reflect.Type) string {
	// The root production may be keyed by its pointer type; normalise so it matches the
	// struct's own entry in typeNodes.
	t = indirectType(t)
	if pkg := t.PkgPath(); pkg != "" {
		return pkg + "." + t.Name()
	}
	return t.String()
}

func productionName(t reflect.Type) string {
	t = indirectType(t)
	if name := t.Name(); name != "" {
		return name
	}
	return t.String()
}
