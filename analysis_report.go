package participle

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// collectProductions builds one analysis entry per named production reachable
// from the root, together with the production paths that reach it.
func (a *grammarAnalyzer) collectProductions(typeNodes map[reflect.Type]node, root node) []ProductionAnalysis {
	type prodInfo struct {
		prod node
		kind string
	}
	var infos []prodInfo
	reachableTypes := map[reflect.Type]bool{}
	for _, prod := range a.nodes {
		switch prod := prod.(type) {
		case *strct:
			reachableTypes[prod.typ] = true
		case *union:
			reachableTypes[prod.typ] = true
		case *custom:
			reachableTypes[prod.typ] = true
		}
	}
	for typ, prod := range typeNodes {
		if !reachableTypes[typ] {
			continue
		}
		kind := productionKind(prod)
		if kind == "" {
			continue
		}
		infos = append(infos, prodInfo{prod: prod, kind: kind})
	}
	if root, ok := root.(*strct); ok && !reachableTypes[root.typ] {
		// The root is always reachable even if its node was constructed after
		// the node graph was collected.
		infos = append(infos, prodInfo{prod: root, kind: productionKind(root)})
	}
	sort.Slice(infos, func(i, j int) bool {
		return productionName(infos[i].prod) < productionName(infos[j].prod)
	})

	paths := a.collectPaths(root)
	out := make([]ProductionAnalysis, 0, len(infos))
	for _, info := range infos {
		entry := ProductionAnalysis{
			Name:          productionName(info.prod),
			Kind:          info.kind,
			Nullable:      a.nullable[info.prod] == triTrue,
			NullableKnown: a.nullable[info.prod] != triUnknown && a.known[info.prod],
			First:         toFirstTokens(a.first[info.prod]),
			FirstKnown:    a.known[info.prod],
			Paths:         paths[info.prod],
		}
		sortAtoms(entry.First)
		out = append(out, entry)
	}
	return out
}

func productionKind(prod node) string {
	switch prod.(type) {
	case *strct:
		return "struct"
	case *union:
		return "union"
	case *custom:
		return "custom"
	default:
		return ""
	}
}

func sortAtoms(tokens []FirstToken) {
	sort.Slice(tokens, func(i, j int) bool {
		return tokens[i].Name < tokens[j].Name
	})
}

func toFirstTokens(atoms []firstAtom) []FirstToken {
	out := make([]FirstToken, 0, len(atoms))
	for _, atom := range atoms {
		out = append(out, FirstToken{
			Name:     atom.name,
			Literal:  atom.key.literal,
			Value:    atom.key.value,
			Wildcard: atom.key.wildcard,
		})
	}
	return out
}

func productionName(n node) string {
	switch n := n.(type) {
	case *strct:
		return exportName(n.typ.Name())
	case *union:
		return exportName(n.typ.Name())
	case *custom:
		return exportName(n.typ.Name())
	case *parseable:
		return exportName(n.t.Name())
	default:
		return ""
	}
}

func exportName(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// collectPaths returns, for each production, the sequence of production names
// traversed from the root to reach it.
func (a *grammarAnalyzer) collectPaths(root node) map[node][]string {
	paths := map[node]map[string]bool{}
	visitedProductions := map[node]bool{}
	visitedInline := map[node]bool{}

	var walk func(n node, prefix []string)
	walk = func(n node, prefix []string) {
		if n == nil {
			return
		}
		add := func(path string) {
			if paths[n] == nil {
				paths[n] = map[string]bool{}
			}
			paths[n][path] = true
		}
		switch n := n.(type) {
		case *strct, *union, *custom, *parseable:
			add(strings.Join(prefix, " > "))
			if visitedProductions[n] {
				return
			}
			visitedProductions[n] = true
			prefix = append(prefix, productionName(n))
			switch n := n.(type) {
			case *strct:
				walk(n.expr, prefix)
			case *union:
				for _, member := range n.disjunction.nodes {
					walk(member, prefix)
				}
			}
			return
		}

		// The same non-production node can be reached through multiple
		// productions, but within one traversal it has a single structure, so
		// a per-walk visited set is enough to break cycles.
		if visitedInline[n] {
			return
		}
		visitedInline[n] = true
		for _, child := range childrenOf(n) {
			walk(child, prefix)
		}
	}

	walk(root, nil)

	out := make(map[node][]string, len(paths))
	for prod, set := range paths {
		list := make([]string, 0, len(set))
		for path := range set {
			list = append(list, path)
		}
		sort.Strings(list)
		out[prod] = list
	}
	return out
}

// childrenOf enumerates the grammar children of nodes that do not introduce a
// new named production.
func childrenOf(n node) []node {
	switch n := n.(type) {
	case *strct:
		return []node{n.expr}
	case *union:
		return n.disjunction.nodes
	case *disjunction:
		return n.nodes
	case *sequence:
		var out []node
		for cur := n; cur != nil; cur = cur.next {
			out = append(out, cur.node)
		}
		return out
	case *capture:
		return []node{n.node}
	case *group:
		return []node{n.expr}
	case *lookaheadGroup:
		return []node{n.expr}
	case *negation:
		return []node{n.node}
	default:
		return nil
	}
}

// collectDiagnostics walks every disjunction in the grammar and records the
// ordered-branch findings between each earlier/later branch pair.
func (a *grammarAnalyzer) collectDiagnostics(root node) []BranchDiagnostic {
	var diagnostics []BranchDiagnostic
	visited := map[node]bool{}

	var walk func(n node, production string, path string)
	walk = func(n node, production string, path string) {
		if n == nil {
			return
		}
		switch n := n.(type) {
		case *strct, *union, *custom, *parseable:
			if visited[n] {
				return
			}
			visited[n] = true
			production = productionName(n)
			path = ""
			switch n := n.(type) {
			case *strct:
				walk(n.expr, production, path)
			case *union:
				for _, member := range n.disjunction.nodes {
					walk(member, production, path)
				}
			}
			return

		case *disjunction:
			if visited[n] {
				return
			}
			visited[n] = true
			disjPath := path
			if disjPath != "" {
				disjPath += ": "
			}
			disjPath += ebnf(n)
			for i := 0; i < len(n.nodes); i++ {
				for j := i + 1; j < len(n.nodes); j++ {
					if d := a.analyzePair(production, disjPath, n.nodes[i], n.nodes[j], i, j); d != nil {
						diagnostics = append(diagnostics, *d)
					}
				}
			}
			for _, branch := range n.nodes {
				walk(branch, production, disjPath)
			}
			return

		case *sequence:
			if visited[n] {
				return
			}
			visited[n] = true
			for cur := n; cur != nil; cur = cur.next {
				walk(cur.node, production, path)
			}
			return

		case *capture:
			label := fieldLabel(n)
			childPath := path
			if label != "" {
				if childPath != "" {
					childPath += " > "
				}
				childPath += label
			}
			walk(n.node, production, childPath)
			return

		case *group:
			walk(n.expr, production, path)
			return

		case *lookaheadGroup, *negation:
			if inner, ok := childExpr(n); ok {
				walk(inner, production, path)
			}
			return
		}

		// Literals and references are leaves.
	}

	walk(root, "", "")

	sort.Slice(diagnostics, func(i, j int) bool {
		x, y := diagnostics[i], diagnostics[j]
		if x.Production != y.Production {
			return x.Production < y.Production
		}
		if x.Path != y.Path {
			return x.Path < y.Path
		}
		if x.Earlier != y.Earlier {
			return x.Earlier < y.Earlier
		}
		return x.Later < y.Later
	})
	return diagnostics
}

func childExpr(n node) (node, bool) {
	switch n := n.(type) {
	case *lookaheadGroup:
		return n.expr, true
	case *negation:
		return n.node, true
	default:
		return nil, false
	}
}

func fieldLabel(c *capture) string {
	name := c.field.Name
	if name == "" {
		return ""
	}
	return name
}

// analyzePair classifies the relationship between an earlier and a later
// ordered branch.
func (a *grammarAnalyzer) analyzePair(production, path string, earlier, later node, earlierIndex, laterIndex int) *BranchDiagnostic {
	overlap, unknown := a.overlap(earlier, later)

	// A definitely nullable earlier branch matches the empty prefix of every
	// later branch and is always picked.
	if a.nullable[earlier] == triTrue {
		return &BranchDiagnostic{
			Kind:       DiagnosticShadow,
			Production: production,
			Path:       path,
			Earlier:    earlierIndex + 1,
			Later:      laterIndex + 1,
			Unknown:    a.nullable[earlier] == triUnknown || a.nullable[later] == triUnknown,
			Message: fmt.Sprintf(
				"branch %d can match the empty input and is ordered before branch %d, so branch %d is never tried",
				earlierIndex+1, laterIndex+1, laterIndex+1),
		}
	}

	earlierSkeleton := a.skeleton(earlier)
	laterSkeleton := a.skeleton(later)
	if !unknown && earlierSkeleton.complete && len(earlierSkeleton.atoms) > 0 &&
		skeletonPrefix(earlierSkeleton.atoms, laterSkeleton.atoms) {
		return &BranchDiagnostic{
			Kind:        DiagnosticShadow,
			Production:  production,
			Path:        path,
			Earlier:     earlierIndex + 1,
			Later:       laterIndex + 1,
			Overlapping: toFirstTokens(overlap),
			Message: fmt.Sprintf(
				"every token sequence matched by branch %d is a prefix of branch %d, which is ordered after it, so branch %d can never be selected",
				earlierIndex+1, laterIndex+1, laterIndex+1),
		}
	}

	if len(overlap) == 0 && !unknown {
		return nil
	}

	return &BranchDiagnostic{
		Kind:        DiagnosticPossibleConflict,
		Production:  production,
		Path:        path,
		Earlier:     earlierIndex + 1,
		Later:       laterIndex + 1,
		Overlapping: toFirstTokens(overlap),
		Unknown:     unknown,
		Message: fmt.Sprintf(
			"branches %d and %d can start with the same token; this is only an error when branch %d fails after consuming more than the configured lookahead",
			earlierIndex+1, laterIndex+1, earlierIndex+1),
	}
}

// overlap returns the terminal tokens that both branches can start with and
// whether either branch is opaque.
func (a *grammarAnalyzer) overlap(earlier, later node) ([]firstAtom, bool) {
	unknown := !a.known[earlier] || !a.known[later]
	var out []firstAtom
	for _, x := range a.first[earlier] {
		for _, y := range a.first[later] {
			if atomOverlaps(x.key, y.key) {
				out = appendAtoms(out, x)
				break
			}
		}
	}
	return out, unknown
}

type branchSkeleton struct {
	atoms    []firstAtom
	complete bool
}

// skeleton extracts the leading run of fixed terminal tokens that a branch
// always consumes. complete is true only when the entire branch is made up of
// fixed terminals, which is required to prove a definite shadow.
func (a *grammarAnalyzer) skeleton(n node) branchSkeleton {
	s := branchSkeleton{complete: true}
	a.appendSkeleton(n, &s)
	return s
}

func (a *grammarAnalyzer) appendSkeleton(n node, s *branchSkeleton) {
	if !s.complete {
		return
	}
	switch n := n.(type) {
	case *sequence:
		for cur := n; cur != nil; cur = cur.next {
			a.appendSkeleton(cur.node, s)
			if !s.complete {
				return
			}
		}

	case *capture:
		a.appendSkeleton(n.node, s)

	case *group:
		switch n.mode {
		case groupMatchOnce, groupMatchNonEmpty, groupMatchOneOrMore:
			a.appendSkeleton(n.expr, s)
		default:
			s.complete = false
		}

	case *literal, *reference:
		for _, atom := range a.first[n] {
			s.atoms = append(s.atoms, atom)
		}

	default:
		// Optional groups, repetitions, disjunctions, sub-productions and
		// opaque parsing cannot be proven to consume a fixed prefix.
		s.complete = false
	}
}

func skeletonPrefix(earlier, later []firstAtom) bool {
	if len(earlier) > len(later) {
		return false
	}
	for i := range earlier {
		if !skeletonTokenEqual(earlier[i].key, later[i].key) {
			return false
		}
	}
	return true
}

func skeletonTokenEqual(x, y atomKey) bool {
	if x.literal != y.literal {
		return false
	}
	if x.literal {
		return x.value == y.value
	}
	return x.typ == y.typ
}
