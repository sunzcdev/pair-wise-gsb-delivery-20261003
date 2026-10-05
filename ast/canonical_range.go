package ast

import "github.com/yuin/goldmark/v2/text"

// canonicalRange derives a best-effort source byte range for a node.
// The returned booleans report whether start and stop are meaningful.
//
// Ranges are only used for debugging (see [WithCanonicalSource]); nodes are
// not required to provide an exact span.
func canonicalRange(node Node, source []byte) (start, stop int, hasStart, hasStop bool) {
	if r, ok := node.(CanonicalSourceRanger); ok {
		s, e := r.CanonicalSourceRange()
		return s, e, true, true
	}

	switch n := node.(type) {
	case *Document:
		return 0, len(source), true, true
	case *CodeBlock:
		if s, e, ok := linesRange(n.Value); ok {
			return s, e, true, true
		}
	case *HTMLBlock:
		if s, e, ok := linesRange(n.Value); ok {
			return s, e, true, true
		}
	case *LinkReferenceDefinition:
		return valuesRange(
			n.Label,
			n.Destination,
			n.Title,
		)
	case *Text:
		return valueRange(n.Value)
	case *CodeSpan:
		return valueRange(n.Value)
	case *Link:
		start, stop, hasStart, hasStop = valuesRange(n.Destination, n.Title)
		if n.Reference != nil && !n.Reference.Value.IsOwned() {
			rs, re, rok := firstLastIndices(n.Reference.Value.Indices())
			if rok {
				start, stop, hasStart, hasStop = unionRange(start, stop, hasStart, hasStop, rs, re, true, true)
			}
		}
		return
	case *Image:
		start, stop, hasStart, hasStop = valuesRange(n.Destination, n.Title)
		if n.Reference != nil && !n.Reference.Value.IsOwned() {
			rs, re, rok := firstLastIndices(n.Reference.Value.Indices())
			if rok {
				start, stop, hasStart, hasStop = unionRange(start, stop, hasStart, hasStop, rs, re, true, true)
			}
		}
		return
	case *AutoLink:
		return valuesRange(n.Destination, n.Label, n.Text)
	case *RawHTML:
		return valueRange(n.Value)
	}

	// Blocks that carry source segments (Paragraph, Heading, Blockquote,
	// List, ListItem, extension blocks such as tables).
	if block, ok := node.(BlockNode); ok {
		segs := block.Source()
		if len(segs) > 0 {
			return segs[0].Start, segs[len(segs)-1].Stop, true, true
		}
	}

	// Container nodes: span from the first to the last descendant range.
	if node.HasChildren() {
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			cs, ce, cHasStart, cHasStop := canonicalRange(c, source)
			start, stop, hasStart, hasStop = unionRange(start, stop, hasStart, hasStop, cs, ce, cHasStart, cHasStop)
		}
		if hasStart {
			return
		}
	}

	if pos := node.Pos(); pos > -1 {
		return pos, 0, true, false
	}
	return 0, 0, false, false
}

type sourceValue interface {
	IsOwned() bool
	Indices() []text.Index
}

func valueRange(v sourceValue) (start, stop int, hasStart, hasStop bool) {
	if v == nil || v.IsOwned() {
		return 0, 0, false, false
	}
	start, stop, ok := firstLastIndices(v.Indices())
	return start, stop, ok, ok
}

func valuesRange(values ...sourceValue) (start, stop int, hasStart, hasStop bool) {
	for _, v := range values {
		vs, ve, vStart, vStop := valueRange(v)
		start, stop, hasStart, hasStop = unionRange(start, stop, hasStart, hasStop, vs, ve, vStart, vStop)
	}
	return
}

func firstLastIndices(indices []text.Index) (start, stop int, ok bool) {
	for _, idx := range indices {
		if idx.Start >= idx.Stop {
			continue
		}
		if !ok {
			start, stop, ok = idx.Start, idx.Stop, true
			continue
		}
		if idx.Start < start {
			start = idx.Start
		}
		if idx.Stop > stop {
			stop = idx.Stop
		}
	}
	return
}

func unionRange(
	start, stop int, hasStart, hasStop bool,
	otherStart, otherStop int, otherHasStart, otherHasStop bool,
) (int, int, bool, bool) {
	if otherHasStart {
		if !hasStart || otherStart < start {
			start = otherStart
		}
		hasStart = true
	}
	if otherHasStop {
		if !hasStop || otherStop > stop {
			stop = otherStop
		}
		hasStop = true
	}
	return start, stop, hasStart, hasStop
}

func linesRange(lines text.Lines) (start, stop int, ok bool) {
	segs := lines.Segments()
	if len(segs) == 0 {
		return 0, 0, false
	}
	return segs[0].Start, segs[len(segs)-1].Stop, true
}
