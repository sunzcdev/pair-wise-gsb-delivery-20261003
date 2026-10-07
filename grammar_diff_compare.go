package participle

import (
	"fmt"
	"strings"
)

// diffNodes compares two canonical nodes and returns the structural changes found within
// them. It never expands production references (dProd) or opaque nodes, so recursive
// grammars are handled as finite node-and-reference graphs.
func diffNodes(old, new dNode) []GrammarChange {
	changes := []GrammarChange{}
	diffInto(&changes, nil, old, new)
	return changes
}

func diffInto(out *[]GrammarChange, path []string, old, new dNode) {
	if nodeSignature(old) == nodeSignature(new) {
		return
	}

	if old.kind == new.kind {
		switch old.kind {
		case dRef, dLiteral, dProd, dOpaqueNode:
			emit(out, path, terminalChangeKind(old.kind), renderNode(old), renderNode(new))
			return
		case dGroup:
			if old.mode != new.mode {
				emit(out, path, QuantifierChanged, quantifierName(old.mode), quantifierName(new.mode))
			}
			diffInto(out, path, old.children[0], new.children[0])
			return
		case dNegation:
			diffInto(out, path, old.children[0], new.children[0])
			return
		case dLookahead:
			if old.negative != new.negative {
				emit(out, path, OrderedBranchChanged, lookaheadName(old.negative), lookaheadName(new.negative))
			}
			diffInto(out, path, old.children[0], new.children[0])
			return
		case dCapture:
			diffInto(out, path, old.children[0], new.children[0])
			return
		case dSeq:
			diffContainer(out, path, "term", SequenceChanged, old.children, new.children)
			return
		case dAlt:
			diffContainer(out, path, "branch", OrderedBranchChanged, old.children, new.children)
			return
		}
	}

	kind := mismatchKind(old, new)
	emit(out, path, kind, renderNode(old), renderNode(new))
}

// diffContainer compares the ordered children of a sequence or ordered branch.
//
// An exact permutation of the same children (same length, same multiset) is reported as a
// pure reordering, which is significant for ordered branches. Otherwise children are
// compared by position and any surplus children are reported as added or removed, so that
// genuine token edits inside retained elements are still surfaced rather than hidden.
func diffContainer(
	out *[]GrammarChange,
	path []string,
	element string,
	kind GrammarChangeKind,
	oldChildren, newChildren []dNode,
) {
	if len(oldChildren) == len(newChildren) {
		if isPermutation(oldChildren, newChildren) {
			emit(out, path, kind,
				joinElements(element, oldChildren), joinElements(element, newChildren))
			return
		}
		for i := range oldChildren {
			diffInto(out, childPath(path, element, i), oldChildren[i], newChildren[i])
		}
		return
	}

	oldCount, newCount := signatureMultiset(oldChildren), signatureMultiset(newChildren)
	for sig, n := range oldCount {
		if m := newCount[sig]; n > m {
			emit(out, path, kind,
				element+" "+renderNode(mustNode(oldChildren, newChildren, sig)),
				"(removed)")
		}
	}
	for sig, n := range newCount {
		if m := oldCount[sig]; n > m {
			emit(out, path, kind,
				"(added)",
				element+" "+renderNode(mustNode(newChildren, oldChildren, sig)))
		}
	}
}

func mustNode(primary, _ []dNode, sig string) dNode {
	for _, n := range primary {
		if nodeSignature(n) == sig {
			return n
		}
	}
	return dNode{}
}

func terminalChangeKind(k dNodeKind) GrammarChangeKind {
	switch k {
	case dRef, dLiteral:
		return TokenChanged
	default:
		return TokenChanged
	}
}

// mismatchKind classifies a change between nodes of different kinds. The presence of a
// quantifier wins, then an ordered branch, then a sequence; anything else is a token
// change (including swapping one terminal for another, or replacing an opaque parser).
func mismatchKind(old, new dNode) GrammarChangeKind {
	if old.kind == dGroup || new.kind == dGroup {
		return QuantifierChanged
	}
	if old.kind == dAlt || new.kind == dAlt {
		return OrderedBranchChanged
	}
	if old.kind == dSeq || new.kind == dSeq {
		return SequenceChanged
	}
	return TokenChanged
}

func emit(out *[]GrammarChange, path []string, kind GrammarChangeKind, from, to string) {
	*out = append(*out, GrammarChange{
		Path: strings.Join(path, " > "),
		Kind: kind,
		From: from,
		To:   to,
	})
}

func childPath(path []string, element string, index int) []string {
	p := append([]string{}, path...)
	return append(p, fmt.Sprintf("%s %d", element, index+1))
}

func isPermutation(a, b []dNode) bool {
	if len(a) != len(b) {
		return false
	}
	ca, cb := signatureMultiset(a), signatureMultiset(b)
	if len(ca) != len(cb) {
		return false
	}
	for sig, n := range ca {
		if cb[sig] != n {
			return false
		}
	}
	return true
}

func signatureMultiset(nodes []dNode) map[string]int {
	counts := map[string]int{}
	for _, n := range nodes {
		counts[nodeSignature(n)]++
	}
	return counts
}

func joinElements(element string, nodes []dNode) string {
	parts := make([]string, len(nodes))
	for i, n := range nodes {
		parts[i] = fmt.Sprintf("%s %d: %s", element, i+1, renderNode(n))
	}
	return strings.Join(parts, "; ")
}

func quantifierName(mode groupMatchMode) string {
	switch mode {
	case groupMatchOnce:
		return "none"
	case groupMatchZeroOrOne:
		return "?"
	case groupMatchZeroOrMore:
		return "*"
	case groupMatchOneOrMore:
		return "+"
	case groupMatchNonEmpty:
		return "!"
	default:
		return mode.String()
	}
}

func lookaheadName(negative bool) string {
	if negative {
		return "(?! ...)"
	}
	return "(?= ...)"
}

// nodeSignature is a canonical, presentation-independent signature used for stable
// matching of children. It is structural, not a comparison of whole EBNF lines.
func nodeSignature(n dNode) string {
	var b strings.Builder
	writeSignature(&b, n)
	return b.String()
}

func writeSignature(b *strings.Builder, n dNode) {
	switch n.kind {
	case dRef:
		fmt.Fprintf(b, "ref:%s", n.tok)
	case dLiteral:
		fmt.Fprintf(b, "lit:%q", n.tok)
	case dProd:
		fmt.Fprintf(b, "prod:%s", n.tok)
	case dOpaqueNode:
		fmt.Fprintf(b, "opaque:%s", n.tok)
	case dGroup:
		fmt.Fprintf(b, "group:%s[", quantifierName(n.mode))
		writeSignature(b, n.children[0])
		b.WriteByte(']')
	case dNegation:
		b.WriteString("not:[")
		writeSignature(b, n.children[0])
		b.WriteByte(']')
	case dLookahead:
		if n.negative {
			b.WriteString("laneg:[")
		} else {
			b.WriteString("lapos:[")
		}
		writeSignature(b, n.children[0])
		b.WriteByte(']')
	case dCapture:
		b.WriteString("cap:[")
		writeSignature(b, n.children[0])
		b.WriteByte(']')
	case dSeq:
		b.WriteString("seq:[")
		for i, child := range n.children {
			if i > 0 {
				b.WriteByte(',')
			}
			writeSignature(b, child)
		}
		b.WriteByte(']')
	case dAlt:
		b.WriteString("alt:[")
		for i, child := range n.children {
			if i > 0 {
				b.WriteByte(',')
			}
			writeSignature(b, child)
		}
		b.WriteByte(']')
	}
}

// renderNode renders a canonical node for human readable reports.
func renderNode(n dNode) string {
	switch n.kind {
	case dRef:
		// Keep the token's declared spelling; case is part of the token identity.
		return "<" + n.tok + ">"
	case dLiteral:
		return fmt.Sprintf("%q", n.tok)
	case dProd, dOpaqueNode:
		return shortName(n.tok)
	case dCapture:
		return renderNode(n.children[0])
	case dNegation:
		inner := renderNode(n.children[0])
		if compound(n.children[0]) {
			return "~(" + inner + ")"
		}
		return "~" + inner
	case dLookahead:
		if n.negative {
			return "(?! " + renderNode(n.children[0]) + ")"
		}
		return "(?= " + renderNode(n.children[0]) + ")"
	case dGroup:
		return renderGrouped(n.children[0]) + quantifierNameOrEmpty(n.mode)
	case dSeq:
		parts := make([]string, len(n.children))
		for i, child := range n.children {
			parts[i] = renderGrouped(child)
		}
		return strings.Join(parts, " ")
	case dAlt:
		parts := make([]string, len(n.children))
		for i, child := range n.children {
			parts[i] = renderNode(child)
		}
		return strings.Join(parts, " | ")
	default:
		return ""
	}
}

// shortName reduces a stable package-qualified identity to its declared name for display.
func shortName(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.Index(id, "."); i >= 0 {
		id = id[i+1:]
	}
	return id
}

func renderGrouped(n dNode) string {
	if n.kind == dSeq || n.kind == dAlt {
		return "(" + renderNode(n) + ")"
	}
	return renderNode(n)
}

func quantifierNameOrEmpty(mode groupMatchMode) string {
	switch mode {
	case groupMatchZeroOrOne:
		return "?"
	case groupMatchZeroOrMore:
		return "*"
	case groupMatchOneOrMore:
		return "+"
	case groupMatchNonEmpty:
		return "!"
	default:
		return ""
	}
}

func compound(n dNode) bool {
	switch n.kind {
	case dSeq, dAlt, dGroup:
		return true
	default:
		return false
	}
}
