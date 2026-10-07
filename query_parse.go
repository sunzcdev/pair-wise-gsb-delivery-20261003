package msgpack

import (
	"fmt"
	"strconv"
	"strings"
)

func parseQuery(query string) (queryNode, error) {
	if query == "" {
		return &leafNode{}, nil
	}
	segs, err := splitQueryString(query, '.')
	if err != nil {
		return nil, err
	}
	return buildQueryChain(query, segs)
}

// splitQueryString splits s on sep characters that are not nested inside
// brackets, braces, or quotes.
func splitQueryString(s string, sep byte) ([]string, error) {
	var parts []string
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("msgpack: query %q: unbalanced %q", s, string(c))
			}
		default:
			if c == sep && depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 || quote != 0 {
		return nil, fmt.Errorf("msgpack: query %q: unbalanced brackets", s)
	}
	return append(parts, s[start:]), nil
}

func buildQueryChain(query string, segs []string) (queryNode, error) {
	var nodes []queryNode
	for i, seg := range segs {
		if seg == "" {
			return nil, fmt.Errorf("msgpack: query %q: empty segment", query)
		}
		if seg[0] == '{' {
			if i != len(segs)-1 {
				return nil, fmt.Errorf("msgpack: query %q: %q must be the last segment", query, seg)
			}
			node, err := parseMultiSegment(query, seg)
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, node)
			continue
		}
		segNodes, err := parseQuerySegment(query, seg)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, segNodes...)
	}

	// Fold a slice or an index right after a filter into the filter, so that
	// [?status==failed][:10] limits the number of matching elements.
	folded := nodes[:0]
	for _, node := range nodes {
		if f, ok := lastFilterNode(folded); ok {
			switch node := node.(type) {
			case *sliceNode:
				f.lo, f.hi = node.lo, node.hi
				f.seg += node.seg
				continue
			case *indexNode:
				f.lo, f.hi = node.idx, node.idx+1
				f.seg += node.seg
				continue
			}
		}
		folded = append(folded, node)
	}

	for i := 0; i < len(folded)-1; i++ {
		folded[i].(chainNode).setNext(folded[i+1])
	}
	if last, ok := folded[len(folded)-1].(chainNode); ok {
		last.setNext(&leafNode{})
	}

	for _, node := range folded {
		if f, ok := node.(*filterNode); ok {
			switch f.next.(type) {
			case *keyNode, *multiNode, *leafNode:
			default:
				return nil, fmt.Errorf(
					"msgpack: query %q: filter %q must be followed by a map key selection",
					query, f.seg)
			}
		}
	}
	return folded[0], nil
}

func lastFilterNode(nodes []queryNode) (*filterNode, bool) {
	if len(nodes) == 0 {
		return nil, false
	}
	f, ok := nodes[len(nodes)-1].(*filterNode)
	return f, ok
}

// parseQuerySegment parses a single path segment such as
// "records[?status==failed][:10]" into a chain of nodes.
func parseQuerySegment(query, seg string) ([]queryNode, error) {
	var nodes []queryNode

	name := seg
	rest := ""
	if i := strings.IndexByte(seg, '['); i >= 0 {
		name = seg[:i]
		rest = seg[i:]
	}
	if strings.ContainsAny(name, "{}") {
		return nil, fmt.Errorf("msgpack: query %q: invalid segment %q", query, seg)
	}

	switch {
	case name == "":
	case name == "*":
		nodes = append(nodes, &sliceNode{lo: 0, hi: -1, seg: "[*]"})
	case isQueryInt(name):
		idx, _ := strconv.Atoi(name)
		nodes = append(nodes, newIndexNode(idx))
	default:
		nodes = append(nodes, &keyNode{key: name})
	}

	for rest != "" {
		end, err := queryBracketEnd(rest)
		if err != nil {
			return nil, fmt.Errorf("msgpack: query %q: %s", query, err)
		}
		node, err := parseQueryBracket(query, rest[1:end])
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
		rest = rest[end+1:]
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("msgpack: query %q: empty segment", query)
	}
	return nodes, nil
}

// queryBracketEnd returns the index of the closing bracket matching the
// opening bracket at s[0].
func queryBracketEnd(s string) (int, error) {
	var quote byte
	for i := 1; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case ']':
			return i, nil
		}
	}
	return 0, fmt.Errorf("unbalanced brackets in %q", s)
}

func parseQueryBracket(query, content string) (queryNode, error) {
	if content == "" {
		return nil, fmt.Errorf("msgpack: query %q: empty []", query)
	}

	if content[0] == '?' {
		pred, err := parsePredicate(query, content[1:])
		if err != nil {
			return nil, err
		}
		return &filterNode{
			pred: pred,
			hi:   -1,
			seg:  "[" + content + "]",
		}, nil
	}

	if strings.Contains(content, ":") {
		parts := strings.SplitN(content, ":", 2)
		lo, hi := 0, -1
		if parts[0] != "" {
			n, err := strconv.Atoi(parts[0])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("msgpack: query %q: invalid slice bound %q", query, parts[0])
			}
			lo = n
		}
		if parts[1] != "" {
			n, err := strconv.Atoi(parts[1])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("msgpack: query %q: invalid slice bound %q", query, parts[1])
			}
			hi = n
		}
		return &sliceNode{
			lo:  lo,
			hi:  hi,
			seg: "[" + content + "]",
		}, nil
	}

	idx, err := strconv.Atoi(content)
	if err != nil || idx < 0 {
		return nil, fmt.Errorf("msgpack: query %q: invalid array index %q", query, content)
	}
	return newIndexNode(idx), nil
}

func parsePredicate(query, s string) (predicate, error) {
	var pred predicate

	eq := strings.Index(s, "==")
	ne := strings.Index(s, "!=")
	switch {
	case eq >= 0 && (ne < 0 || eq < ne):
		pred.field = s[:eq]
		pred.value = parsePredicateValue(s[eq+2:])
	case ne >= 0 && (eq < 0 || ne < eq):
		pred.field = s[:ne]
		pred.ne = true
		pred.value = parsePredicateValue(s[ne+2:])
	default:
		return pred, fmt.Errorf("msgpack: query %q: invalid predicate %q", query, s)
	}

	if pred.field == "" {
		return pred, fmt.Errorf("msgpack: query %q: predicate %q: empty field", query, s)
	}
	return pred, nil
}

func parsePredicateValue(s string) interface{} {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "nil", "null":
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// parseMultiSegment parses a terminal "{key1,key2.nested}" segment into
// a multiNode that extracts all listed paths in a single pass over the map.
func parseMultiSegment(query, seg string) (queryNode, error) {
	if !strings.HasSuffix(seg, "}") {
		return nil, fmt.Errorf("msgpack: query %q: unbalanced %q", query, seg)
	}
	parts, err := splitQueryString(seg[1:len(seg)-1], ',')
	if err != nil {
		return nil, err
	}

	multi := &multiNode{
		branches: make(map[string]queryNode, len(parts)),
		seg:      seg,
	}
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("msgpack: query %q: empty branch in %q", query, seg)
		}
		subsegs, err := splitQueryString(part, '.')
		if err != nil {
			return nil, err
		}
		chain, err := buildQueryChain(query, subsegs)
		if err != nil {
			return nil, err
		}
		key, ok := chain.(*keyNode)
		if !ok {
			return nil, fmt.Errorf(
				"msgpack: query %q: multi-select branch %q must start with a map key",
				query, part)
		}
		if _, dup := multi.branches[key.key]; dup {
			return nil, fmt.Errorf("msgpack: query %q: duplicate key %q in %q", query, key.key, seg)
		}
		multi.branches[key.key] = key.next
	}
	return multi, nil
}

func isQueryInt(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}
