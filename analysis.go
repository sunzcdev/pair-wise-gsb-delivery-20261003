package participle

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/alecthomas/participle/v2/lexer"
)

// AnalysisSeverity classifies how certain a reported branch interaction is.
type AnalysisSeverity int

const (
	// SeverityUnknown marks an interaction that cannot be decided statically.
	SeverityUnknown AnalysisSeverity = iota
	// SeverityPossible marks an interaction that may cause a conflict but is not necessarily wrong.
	SeverityPossible
	// SeverityDefinite marks an interaction that deterministically hides a later ordered branch.
	SeverityDefinite
)

func (s AnalysisSeverity) String() string {
	switch s {
	case SeverityDefinite:
		return "definite"
	case SeverityPossible:
		return "possible"
	default:
		return "unknown"
	}
}

// AnalysisKind categorises a single branch finding.
type AnalysisKind int

const (
	// KindNullableBranch means an earlier ordered branch can match the empty input, so it is
	// always taken and every later branch is unreachable.
	KindNullableBranch AnalysisKind = iota
	// KindShadowed means an earlier ordered branch structurally accepts every token sequence a
	// later branch can begin with, so the later branch is never reached.
	KindShadowed
	// KindFirstConflict means the FIRST sets of two branches overlap but static analysis cannot
	// prove that one dominates the other. Ordered-choice parsing may still resolve this correctly.
	KindFirstConflict
	// KindUnknownConflict means a branch involves an opaque production (a custom Parseable or a
	// ParseTypeWith function) whose language is not available for analysis.
	KindUnknownConflict
)

func (k AnalysisKind) String() string {
	switch k {
	case KindNullableBranch:
		return "nullable branch"
	case KindShadowed:
		return "shadowed branch"
	case KindFirstConflict:
		return "FIRST overlap"
	default:
		return "unknown interaction"
	}
}

// Nullability describes whether a production can derive the empty string.
type Nullability int

const (
	// NullableUnknown means nullability cannot be determined because an opaque production is involved.
	NullableUnknown Nullability = iota
	// NullableFalse means the production always consumes at least one token.
	NullableFalse
	// NullableTrue means the production can match the empty string.
	NullableTrue
)

func (n Nullability) String() string {
	switch n {
	case NullableTrue:
		return "nullable"
	case NullableFalse:
		return "non-nullable"
	default:
		return "unknown"
	}
}

// FirstToken is a single terminal that can appear at the start of a production.
//
// A literal without a token-type constraint matches any token carrying the literal value.
type FirstToken struct {
	// Value is the literal value or, for a bare token reference, the token text.
	Value string
	// Symbol is the lexer symbol name for a named token reference, or "" for a literal.
	Symbol string
	// TokenType is the lexer token type, or lexer.EOF when unconstrained by a type.
	TokenType lexer.TokenType
	// Literal is true when the token originates from a quoted literal.
	Literal bool
}

func (f FirstToken) String() string {
	if f.Literal {
		return fmt.Sprintf("%q", f.Value)
	}
	return "<" + strings.ToLower(f.Symbol) + ">"
}

// BranchFinding describes an interaction between two ordered alternatives of one disjunction.
type BranchFinding struct {
	Production string
	// Path records the production chain that leads from the grammar root to this disjunction.
	Path     []string
	Severity AnalysisSeverity
	Kind     AnalysisKind
	// Earlier is the index of the winning (earlier) alternative, zero based.
	Earlier int
	// Later is the index of the hidden (later) alternative, zero based.
	Later int
	// EarlierEBNF and LaterEBNF are single-branch EBNF snippets for display.
	EarlierEBNF string
	LaterEBNF   string
	// Overlap lists the FIRST tokens common to both branches.
	Overlap []FirstToken
	Message string
}

// ProductionAnalysis summarises the fixed-point results for one named production.
type ProductionAnalysis struct {
	Name     string
	Nullable Nullability
	First    []FirstToken
	// Opaque is true for custom Parseable or ParseTypeWith productions that cannot be inspected.
	Opaque bool
}

// AnalysisReport is a purely informational build-time view of the grammar. It never changes
// parser behaviour: FIRST overlap in particular is only a hint, not a proof of an error.
type AnalysisReport struct {
	Productions []ProductionAnalysis
	Findings    []BranchFinding
}

func (r *AnalysisReport) String() string {
	var b strings.Builder
	b.WriteString("Grammar analysis report\n")
	b.WriteString("Productions:\n")
	for _, prod := range r.Productions {
		first := make([]string, len(prod.First))
		for i, t := range prod.First {
			first[i] = t.String()
		}
		if len(first) == 0 {
			if prod.Opaque {
				first = []string{"<unknown>"}
			} else {
				first = []string{"<none>"}
			}
		}
		fmt.Fprintf(&b, "  %s: %s, FIRST {%s}\n", prod.Name, prod.Nullable, strings.Join(first, ", "))
	}
	if len(r.Findings) == 0 {
		b.WriteString("No ordered-branch findings.\n")
		return b.String()
	}
	b.WriteString("Ordered-branch findings:\n")
	for _, f := range r.Findings {
		path := strings.Join(f.Path, " -> ")
		fmt.Fprintf(&b, "  [%s] %s: %s\n", f.Severity, f.Kind, f.Message)
		fmt.Fprintf(&b, "    at %s, branch %d (%s) vs branch %d (%s)\n",
			path, f.Earlier+1, f.EarlierEBNF, f.Later+1, f.LaterEBNF)
		if len(f.Overlap) > 0 {
			overlap := make([]string, len(f.Overlap))
			for i, t := range f.Overlap {
				overlap[i] = t.String()
			}
			fmt.Fprintf(&b, "    overlapping FIRST: %s\n", strings.Join(overlap, ", "))
		}
	}
	return b.String()
}

// Analysis computes the build-time report of nullable productions, FIRST token sets and
// ordered-branch interactions. It performs no parsing and never alters parser choices.
//
// Productions implemented with a custom Parseable or registered through ParseTypeWith are
// reported as opaque ("unknown" nullability, no derivable FIRST set).
func (p *Parser[G]) Analysis() *AnalysisReport {
	return analyze(p.typeNodes, p.rootType)
}

// grammarAnalyzer computes nullability and FIRST sets by least-fixed-point iteration so that
// recursive grammars converge instead of looping forever.
type grammarAnalyzer struct {
	nullable map[node]Nullability
	first    map[node][]FirstToken
}

func analyze(typeNodes map[reflect.Type]node, rootType reflect.Type) *AnalysisReport {
	a := &grammarAnalyzer{
		nullable: map[node]Nullability{},
		first:    map[node][]FirstToken{},
	}

	// Seed the fixed point: every inspectable node starts as non-nullable with an empty FIRST.
	nodes := collectAnalyzableNodes(typeNodes, rootType)
	for _, n := range nodes {
		a.nullable[n] = NullableFalse
		a.first[n] = nil
	}

	// Iterate until both maps stabilise. Values only move toward "nullable"/unknown and FIRST
	// sets only grow, so this is guaranteed to converge.
	const maxRounds = 10000
	for range maxRounds {
		nextNullable := make(map[node]Nullability, len(nodes))
		nextFirst := make(map[node][]FirstToken, len(nodes))
		for _, n := range nodes {
			nextNullable[n] = a.computeNullable(n)
			nextFirst[n] = a.computeFirst(n)
		}
		if mapsEqualNullable(a.nullable, nextNullable) && mapsEqualFirst(a.first, nextFirst) {
			break
		}
		a.nullable = nextNullable
		a.first = nextFirst
	}

	report := &AnalysisReport{}
	report.Productions = a.collectProductions(typeNodes, rootType)
	report.Findings = a.collectFindings(typeNodes, rootType)
	return report
}

// isOpaque reports whether a named production hides its structure from analysis.
func isOpaque(n node) bool {
	switch n.(type) {
	case *custom, *parseable:
		return true
	}
	return false
}

// isNilNode reports whether n is an untyped nil or a nil typed pointer. The latter arises when a
// terminal sequence element passes its nil *sequence "next" link into a visitor.
func isNilNode(n node) bool {
	if n == nil {
		return true
	}
	switch n := n.(type) {
	case *sequence:
		return n == nil
	}
	return false
}

// collectAnalyzableNodes gathers every node reachable from the root or from a named production
// that the root references. Nodes belonging to opaque productions (*custom/*parseable) are
// excluded; opaque nodes encountered inside a tree are handled as unknown leaves.
func collectAnalyzableNodes(typeNodes map[reflect.Type]node, rootType reflect.Type) []node {
	roots := map[node]bool{}
	roots[typeNodes[rootType]] = true
	for _, n := range typeNodes {
		if s, ok := n.(*strct); ok {
			roots[s] = true
		} else if u, ok := n.(*union); ok {
			roots[u] = true
		}
	}
	out := []node{}
	seen := map[node]bool{}
	var walk func(n node)
	walk = func(n node) {
		if isNilNode(n) || seen[n] || isOpaque(n) {
			return
		}
		seen[n] = true
		out = append(out, n)
		switch n := n.(type) {
		case *disjunction:
			for _, c := range n.nodes {
				walk(c)
			}
		case *union:
			for _, c := range n.disjunction.nodes {
				walk(c)
			}
		case *strct:
			walk(n.expr)
		case *sequence:
			walk(n.node)
			walk(n.next)
		case *capture:
			walk(n.node)
		case *negation:
			walk(n.node)
		case *group:
			walk(n.expr)
		case *lookaheadGroup:
			walk(n.expr)
		}
	}
	for r := range roots {
		walk(r)
	}
	return out
}

func mapsEqualNullable(a, b map[node]Nullability) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func mapsEqualFirst(a, b map[node][]FirstToken) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !firstTokensEqual(v, b[k]) {
			return false
		}
	}
	return true
}

func firstTokensEqual(a, b []FirstToken) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func unionNullability(values []Nullability) Nullability {
	result := NullableFalse
	for _, v := range values {
		switch v {
		case NullableTrue:
			return NullableTrue
		case NullableUnknown:
			result = NullableUnknown
		}
	}
	return result
}

func intersectNullability(values []Nullability) Nullability {
	result := NullableTrue
	for _, v := range values {
		if v == NullableFalse {
			return NullableFalse
		}
		if v == NullableUnknown {
			result = NullableUnknown
		}
	}
	return result
}

// nullOf returns the current approximation for a child node, treating unanalysed nodes
// (opaque productions) as unknown.
func (a *grammarAnalyzer) nullOf(n node) Nullability {
	if isOpaque(n) {
		return NullableUnknown
	}
	if v, ok := a.nullable[n]; ok {
		return v
	}
	return NullableUnknown
}

func (a *grammarAnalyzer) computeNullable(n node) Nullability {
	switch n := n.(type) {
	case *literal, *reference:
		return NullableFalse

	case *capture:
		return a.nullOf(n.node)

	case *negation, *lookaheadGroup:
		// Negation and lookahead never consume through the analysed expression, but we cannot
		// make sound claims about an opaque child.
		return NullableFalse

	case *disjunction:
		values := make([]Nullability, len(n.nodes))
		for i, c := range n.nodes {
			values[i] = a.nullOf(c)
		}
		return unionNullability(values)

	case *union:
		values := make([]Nullability, len(n.disjunction.nodes))
		for i, c := range n.disjunction.nodes {
			values[i] = a.nullOf(c)
		}
		return unionNullability(values)

	case *sequence:
		var values []Nullability
		for cur := n; cur != nil; cur = cur.next {
			values = append(values, a.nullOf(cur.node))
		}
		return intersectNullability(values)

	case *strct:
		return a.nullOf(n.expr)

	case *group:
		switch n.mode {
		case groupMatchZeroOrOne, groupMatchZeroOrMore:
			return NullableTrue
		case groupMatchOneOrMore:
			return a.nullOf(n.expr)
		case groupMatchNonEmpty:
			return NullableFalse
		default: // groupMatchOnce
			return a.nullOf(n.expr)
		}

	default:
		return NullableUnknown
	}
}

func sortFirstTokens(tokens []FirstToken) []FirstToken {
	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].Value != tokens[j].Value {
			return tokens[i].Value < tokens[j].Value
		}
		if tokens[i].Symbol != tokens[j].Symbol {
			return tokens[i].Symbol < tokens[j].Symbol
		}
		if tokens[i].Literal != tokens[j].Literal {
			return !tokens[i].Literal
		}
		return tokens[i].TokenType < tokens[j].TokenType
	})
	return tokens
}

func dedupeFirstTokens(tokens []FirstToken) []FirstToken {
	out := tokens[:0]
	var last FirstToken
	for i, t := range tokens {
		if i > 0 && t == last {
			continue
		}
		out = append(out, t)
		last = t
	}
	return out
}

func mergeFirst(groups ...[]FirstToken) []FirstToken {
	var out []FirstToken
	for _, g := range groups {
		out = append(out, g...)
	}
	sortFirstTokens(out)
	return dedupeFirstTokens(out)
}

// firstOf returns the current FIRST approximation for a child, or nil for an opaque child
// (opaque children contribute no known tokens).
func (a *grammarAnalyzer) firstOf(n node) []FirstToken {
	if isOpaque(n) {
		return nil
	}
	if v, ok := a.first[n]; ok {
		return v
	}
	return nil
}

// computeFirst implements FIRST over the current nullability approximation. A sequence
// contributes the FIRST of each leading nullable element plus the FIRST of the first element
// known to require a token.
func (a *grammarAnalyzer) computeFirst(n node) []FirstToken {
	switch n := n.(type) {
	case *literal:
		return []FirstToken{{Value: n.s, TokenType: n.t, Literal: true}}

	case *reference:
		return []FirstToken{{Value: n.identifier, Symbol: n.identifier, TokenType: n.typ}}

	case *capture:
		return a.firstOf(n.node)

	case *negation, *lookaheadGroup:
		// "anything but this" and syntactic assertions have no static first token.
		return nil

	case *disjunction:
		groups := make([][]FirstToken, 0, len(n.nodes))
		for _, c := range n.nodes {
			groups = append(groups, a.firstOf(c))
		}
		return mergeFirst(groups...)

	case *union:
		groups := make([][]FirstToken, 0, len(n.disjunction.nodes))
		for _, c := range n.disjunction.nodes {
			groups = append(groups, a.firstOf(c))
		}
		return mergeFirst(groups...)

	case *strct:
		return a.firstOf(n.expr)

	case *sequence:
		var out []FirstToken
		for cur := n; cur != nil; cur = cur.next {
			out = mergeFirst(out, a.firstOf(cur.node))
			if a.nullOf(cur.node) != NullableTrue {
				break
			}
		}
		return out

	case *group:
		// FIRST(G?) == FIRST(G)? == FIRST(G)+ == FIRST(G!) == FIRST(G): every group that can
		// match at all begins with the first token of its content.
		return a.firstOf(n.expr)

	default:
		return nil
	}
}

func productionName(n node) string {
	switch n := n.(type) {
	case *strct:
		return strings.ToUpper(n.typ.Name()[:1]) + n.typ.Name()[1:]
	case *union:
		return strings.ToUpper(n.typ.Name()[:1]) + n.typ.Name()[1:]
	case *custom:
		return n.typ.Name()
	case *parseable:
		return n.t.Name()
	default:
		return n.String()
	}
}

// collectProductions reports one summary per named production, including opaque productions
// discovered while walking the grammar (custom Parseable types are not cached in typeNodes).
func (a *grammarAnalyzer) collectProductions(typeNodes map[reflect.Type]node, rootType reflect.Type) []ProductionAnalysis {
	named := map[string]ProductionAnalysis{}
	add := func(n node) {
		name := productionName(n)
		prod := ProductionAnalysis{Name: name}
		if isOpaque(n) {
			prod.Nullable = NullableUnknown
			prod.Opaque = true
		} else {
			prod.Nullable = a.nullOf(n)
			prod.First = append([]FirstToken(nil), a.firstOf(n)...)
		}
		named[name] = prod
	}

	roots := []node{typeNodes[rootType]}
	for _, n := range typeNodes {
		if n != typeNodes[rootType] {
			roots = append(roots, n)
		}
	}
	seen := map[node]bool{}
	for _, r := range roots {
		var walk func(n node)
		walk = func(n node) {
			if isNilNode(n) || seen[n] {
				return
			}
			switch n.(type) {
			case *strct, *union, *custom, *parseable:
				if seen[n] {
					return
				}
				seen[n] = true
				add(n)
			}
			switch n := n.(type) {
			case *disjunction:
				for _, c := range n.nodes {
					walk(c)
				}
			case *union:
				for _, c := range n.disjunction.nodes {
					walk(c)
				}
			case *strct:
				walk(n.expr)
			case *sequence:
				walk(n.node)
				walk(n.next)
			case *capture:
				walk(n.node)
			case *negation:
				walk(n.node)
			case *group:
				walk(n.expr)
			case *lookaheadGroup:
				walk(n.expr)
			}
		}
		walk(r)
	}

	out := make([]ProductionAnalysis, 0, len(named))
	for _, p := range named {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// branchEBNF renders a single ordered alternative as an inline EBNF snippet.
func branchEBNF(n node) string {
	p := &ebnfp{}
	outp := []*ebnfp{}
	buildEBNF(false, n, map[node]bool{}, p, &outp)
	return p.out
}

// collectFindings walks the grammar once from the root, threading the production path so every
// disjunction is reported with the chain that reaches it. Each disjunction node is examined
// once even if a production is referenced from several places.
func (a *grammarAnalyzer) collectFindings(typeNodes map[reflect.Type]node, rootType reflect.Type) []BranchFinding {
	var findings []BranchFinding
	checked := map[node]bool{}
	root := typeNodes[rootType]
	rootPath := []string{productionName(root)}
	checked[root] = true
	switch root := root.(type) {
	case *strct:
		a.walkForFindings(root.expr, rootPath, checked, &findings)
	case *union:
		if !checked[&root.disjunction] {
			checked[&root.disjunction] = true
			findings = append(findings, a.disjunctionFindings(&root.disjunction, rootPath)...)
		}
		for _, member := range root.disjunction.nodes {
			a.walkForFindings(member, rootPath, checked, &findings)
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Production != findings[j].Production {
			return findings[i].Production < findings[j].Production
		}
		if findings[i].Earlier != findings[j].Earlier {
			return findings[i].Earlier < findings[j].Earlier
		}
		return findings[i].Later < findings[j].Later
	})
	return findings
}

func (a *grammarAnalyzer) walkForFindings(n node, path []string, seen map[node]bool, findings *[]BranchFinding) {
	if isNilNode(n) || seen[n] {
		return
	}
	switch n := n.(type) {
	case *strct:
		seen[n] = true
		childPath := append(append([]string{}, path...), productionName(n))
		a.walkForFindings(n.expr, childPath, seen, findings)

	case *union:
		seen[n] = true
		if !seen[&n.disjunction] {
			seen[&n.disjunction] = true
			*findings = append(*findings, a.disjunctionFindings(&n.disjunction, path)...)
		}
		for _, c := range n.disjunction.nodes {
			a.walkForFindings(c, path, seen, findings)
		}

	case *disjunction:
		if seen[n] {
			return
		}
		seen[n] = true
		*findings = append(*findings, a.disjunctionFindings(n, path)...)
		for _, c := range n.nodes {
			a.walkForFindings(c, path, seen, findings)
		}

	case *sequence:
		a.walkForFindings(n.node, path, seen, findings)
		a.walkForFindings(n.next, path, seen, findings)

	case *capture:
		a.walkForFindings(n.node, path, seen, findings)

	case *negation:
		a.walkForFindings(n.node, path, seen, findings)

	case *group:
		a.walkForFindings(n.expr, path, seen, findings)

	case *lookaheadGroup:
		a.walkForFindings(n.expr, path, seen, findings)
	}
}

// disjunctionFindings compares every earlier branch against every later branch in source order.
func (a *grammarAnalyzer) disjunctionFindings(d *disjunction, path []string) []BranchFinding {
	var findings []BranchFinding
	for i := 0; i < len(d.nodes); i++ {
		for j := i + 1; j < len(d.nodes); j++ {
			if f := a.compareBranches(d, path, i, j); f != nil {
				findings = append(findings, *f)
			}
		}
	}
	return findings
}

func (a *grammarAnalyzer) compareBranches(d *disjunction, path []string, i, j int) *BranchFinding {
	earlier, later := d.nodes[i], d.nodes[j]
	finding := &BranchFinding{
		Production:  path[0],
		Path:        append([]string{}, path...),
		Earlier:     i,
		Later:       j,
		EarlierEBNF: branchEBNF(earlier),
		LaterEBNF:   branchEBNF(later),
	}

	earlierNull := a.nullOf(earlier)
	laterNull := a.nullOf(later)
	opaque := containsOpaque(earlier) || containsOpaque(later)

	// An earlier branch that can match empty input always wins, so every later branch is hidden.
	if earlierNull == NullableTrue {
		finding.Severity = SeverityDefinite
		finding.Kind = KindNullableBranch
		finding.Message = fmt.Sprintf(
			"branch %d can match the empty string, so it is always taken and branch %d is unreachable",
			i+1, j+1)
		return finding
	}

	// A pure structural prefix ("a" | "a" "b") is a definite shadow independent of FIRST overlap.
	if laterNull == NullableFalse && !containsOpaque(earlier) && !containsOpaque(later) {
		shortAtoms, shortOK := flattenBranch(earlier)
		longAtoms, longOK := flattenBranch(later)
		if prefixRelation(shortAtoms, longAtoms, shortOK && longOK) == relationPrefix {
			finding.Severity = SeverityDefinite
			finding.Kind = KindShadowed
			finding.Message = fmt.Sprintf(
				"branch %d matches every token sequence branch %d can begin with, so branch %d is unreachable",
				i+1, j+1, j+1)
			finding.Overlap = a.firstOf(later)
			return finding
		}
	}

	overlap, certain := intersectFirst(a.firstOf(earlier), a.firstOf(later))
	if opaque {
		finding.Severity = SeverityUnknown
		finding.Kind = KindUnknownConflict
		finding.Message = fmt.Sprintf(
			"branch %d or %d involves an opaque custom production, so their first-token interaction cannot be checked at build time",
			i+1, j+1)
		finding.Overlap = overlap
		return finding
	}

	if len(overlap) > 0 {
		severity := SeverityPossible
		if !certain {
			severity = SeverityUnknown
		}
		finding.Severity = severity
		finding.Kind = KindFirstConflict
		finding.Message = fmt.Sprintf(
			"branches %d and %d share a first token; ordered choice may hide branch %d, but shared prefixes are often resolved later",
			i+1, j+1, j+1)
		finding.Overlap = overlap
		return finding
	}

	return nil
}

// containsOpaque reports whether a branch tree references an uninspectable production.
func containsOpaque(n node) bool {
	found := false
	_ = visit(n, func(n node, next func() error) error {
		if isOpaque(n) {
			found = true
			return nil
		}
		return next()
	})
	return found
}

// intersectFirst returns the terminals common to two FIRST sets. The boolean is false when an
// opaque element made the comparison non-derivable. For statically known terminals equality of
// value is decisive, including untyped literals (the lexer must emit that literal value).
func intersectFirst(a, b []FirstToken) ([]FirstToken, bool) {
	certain := true
	var out []FirstToken
	for _, x := range a {
		for _, y := range b {
			if !tokensCanOverlap(x, y) {
				continue
			}
			merged := x
			if x.Value == "" {
				merged = y
			}
			out = append(out, merged)
		}
	}
	sortFirstTokens(out)
	return dedupeFirstTokens(out), certain
}

// tokensCanOverlap reports whether two static first tokens could match the same input token.
func tokensCanOverlap(a, b FirstToken) bool {
	if a.Literal && b.Literal {
		// Values are unquoted literal strings; EOF token type means "any token with this value".
		if a.Value != b.Value {
			return false
		}
		if a.TokenType == lexer.EOF || b.TokenType == lexer.EOF {
			return true
		}
		return a.TokenType == b.TokenType
	}
	lit, ref := a, b
	if !lit.Literal {
		lit, ref = b, a
	}
	// Literal vs named token reference: overlap by value, restricted by the literal's type
	// constraint if it carries one.
	if lit.TokenType != lexer.EOF && lit.TokenType != ref.TokenType {
		return false
	}
	return lit.Value == ref.Value
}

type prefixKind int

const (
	relationDistinct prefixKind = iota
	relationPrefix
)

// branchAtom is one flattened terminal-or-production position of an ordered branch.
type branchAtom struct {
	n        node
	optional bool
}

// flattenBranch reduces a branch to an ordered atom list. Branches containing alternatives,
// negations or lookahead are too rich for the structural prefix rule and yield ok=false.
func flattenBranch(n node) (atoms []branchAtom, ok bool) {
	switch n := n.(type) {
	case *literal, *reference, *strct, *union:
		return []branchAtom{{n: n}}, true

	case *capture:
		return flattenBranch(n.node)

	case *sequence:
		var out []branchAtom
		for cur := n; cur != nil; cur = cur.next {
			part, partOK := flattenBranch(cur.node)
			if !partOK {
				return nil, false
			}
			out = append(out, part...)
		}
		return out, true

	case *group:
		part, ok := flattenBranch(n.expr)
		if !ok {
			return nil, false
		}
		switch n.mode {
		case groupMatchZeroOrOne, groupMatchZeroOrMore:
			for i := range part {
				part[i].optional = true
			}
		case groupMatchOneOrMore, groupMatchOnce, groupMatchNonEmpty:
		}
		return part, true

	default:
		// *disjunction, *negation, *lookaheadGroup, *custom, *parseable cannot be compared by
		// the conservative structural rule.
		return nil, false
	}
}

// prefixRelation reports whether short is a definite prefix shadow of long.
//
// The rule is intentionally conservative: the required (mandatory) atoms of short must form an
// unbroken run from the start of both branches, and every corresponding atom in long must be
// required and identical. Optional atoms trailing short (eg. an optional qualifier) are ignored.
func prefixRelation(shortAtoms, longAtoms []branchAtom, ok bool) prefixKind {
	if !ok {
		return relationDistinct
	}
	lastRequired := -1
	for i, a := range shortAtoms {
		if !a.optional {
			lastRequired = i
		}
	}
	if lastRequired < 0 {
		return relationDistinct
	}
	// Optional atoms interspersed in short's mandatory prefix break the claim.
	for i := 0; i <= lastRequired; i++ {
		if shortAtoms[i].optional {
			return relationDistinct
		}
	}
	li := 0
	for si := 0; si <= lastRequired; si++ {
		if li >= len(longAtoms) {
			return relationDistinct
		}
		// An optional atom in long before the aligned required atom means short does not cover
		// every way long can begin; do not claim a definite shadow.
		if longAtoms[li].optional || !atomsEqual(shortAtoms[si].n, longAtoms[li].n) {
			return relationDistinct
		}
		li++
	}
	return relationPrefix
}

// atomsEqual reports whether two flattened atoms match the same terminal or production.
func atomsEqual(a, b node) bool {
	switch a := a.(type) {
	case *literal:
		b, ok := b.(*literal)
		if !ok || a.s != b.s {
			return false
		}
		if a.t == lexer.EOF || b.t == lexer.EOF {
			return true
		}
		return a.t == b.t

	case *reference:
		b, ok := b.(*reference)
		return ok && a.typ == b.typ

	case *strct:
		b, ok := b.(*strct)
		return ok && a.typ == b.typ

	case *union:
		b, ok := b.(*union)
		return ok && a.typ == b.typ

	default:
		return false
	}
}
