package participle

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/alecthomas/participle/v2/lexer"
)

// DiagnosticKind classifies an ordered-branch finding reported by a build-time
// grammar analysis.
type DiagnosticKind int

const (
	// DiagnosticShadow means the earlier branch definitely accepts every token
	// sequence that the later branch can start with, so the later branch is
	// unreachable regardless of the configured lookahead.
	DiagnosticShadow DiagnosticKind = iota
	// DiagnosticPossibleConflict means the branches can start with the same
	// token. This is not necessarily an error: if the earlier branch fails after
	// consuming that token, ordered choice falls back to the later branch. It
	// only becomes an error when disambiguation needs more than the configured
	// lookahead.
	DiagnosticPossibleConflict
)

// String returns a stable, human readable name for the DiagnosticKind.
func (k DiagnosticKind) String() string {
	switch k {
	case DiagnosticShadow:
		return "shadow"
	case DiagnosticPossibleConflict:
		return "possible-conflict"
	default:
		return "unknown"
	}
}

// FirstToken describes a single token that a production can start with.
type FirstToken struct {
	// Name is the token as it appears in grammar error messages, such as
	// `"ab"` for a literal or `<ident>` for a named token.
	Name string
	// Literal is true for an exact literal match and false for a named token
	// reference or a wildcard.
	Literal bool
	// Value holds the literal text when Literal is true.
	Value string
	// Wildcard matches any token. It is used for negations such as `!"a"`,
	// whose first set cannot be enumerated.
	Wildcard bool
}

// ProductionAnalysis is the build-time analysis result for a single named
// production (a struct, a union, a Parseable or a ParseTypeWith production).
type ProductionAnalysis struct {
	// Name is the production name, using the same capitalisation as the EBNF
	// output.
	Name string
	// Kind is the kind of production: "struct", "union", "parseable" or
	// "custom".
	Kind string
	// Nullable reports whether the production can match the empty token
	// sequence. NullableKnown is false when the answer could not be determined
	// because the production delegates to custom code.
	Nullable      bool
	NullableKnown bool
	// First lists the tokens the production can start with. FirstKnown is
	// false when one or more elements of the production could not be analysed.
	First      []FirstToken
	FirstKnown bool
	// Paths lists the EBNF production paths at which the production is reached
	// from the root. The root itself has a single empty path.
	Paths []string
}

// BranchDiagnostic describes a relationship between two ordered alternatives
// of a single disjunction.
type BranchDiagnostic struct {
	// Kind classifies the finding as a definite shadow or a possible conflict.
	Kind DiagnosticKind
	// Production is the named production containing the disjunction.
	Production string
	// Path locates the disjunction inside the production using field names and
	// EBNF fragments, such as `Tail: "a" "b" | "a" "c"`.
	Path string
	// Earlier is the 1-based index of the branch that is tried first.
	Earlier int
	// Later is the 1-based index of the branch that may be masked.
	Later int
	// Overlapping lists the first tokens both branches can start with. It is
	// empty when the finding is caused solely by a nullable earlier branch.
	Overlapping []FirstToken
	// Unknown is true when one or both branches contain a Parseable or
	// ParseTypeWith production, so the overlap may be incomplete.
	Unknown bool
	// Message is a human readable explanation.
	Message string
}

// AnalysisReport is the build-time analysis of a complete grammar.
//
// The report is informational only: it never changes how the parser selects
// branches.
type AnalysisReport struct {
	// Productions lists the analysis of every production reachable from the
	// root, sorted by name.
	Productions []ProductionAnalysis
	// Diagnostics lists ordered-branch findings, sorted by production, path
	// and branch indices.
	Diagnostics []BranchDiagnostic
}

// Analysis computes a build-time report of nullable productions, first token
// sets and ordered-branch findings.
//
// Custom Parseable implementations and productions registered with
// ParseTypeWith are opaque: their nullability and first sets are reported as
// unknown. Recursive grammars are resolved by iterating the nullability and
// first set equations to a fixed point.
//
// The report does not alter parsing decisions. In particular, overlapping
// first sets are reported as possible conflicts, not definite errors, because
// an earlier branch that fails after consuming a shared prefix still falls
// back to a later branch.
func (p *Parser[G]) Analysis() *AnalysisReport {
	return analyzeGrammar(p.typeNodes, p.rootType)
}

// tri is a three-valued boolean used for nullability.
type tri int

const (
	triUnknown tri = iota
	triFalse
	triTrue
)

func triOr(a, b tri) tri {
	if a == triTrue || b == triTrue {
		return triTrue
	}
	if a == triUnknown || b == triUnknown {
		return triUnknown
	}
	return triFalse
}

func triAnd(a, b tri) tri {
	if a == triFalse || b == triFalse {
		return triFalse
	}
	if a == triUnknown || b == triUnknown {
		return triUnknown
	}
	return triTrue
}

// atomKey identifies a concrete terminal token. A wildcard token matches
// anything.
type atomKey struct {
	literal  bool
	value    string
	typ      lexer.TokenType
	wildcard bool
}

type firstAtom struct {
	key  atomKey
	name string
}

type grammarAnalyzer struct {
	nodes    []node
	nullable map[node]tri
	first    map[node][]firstAtom
	known    map[node]bool
}

func analyzeGrammar(typeNodes map[reflect.Type]node, rootType reflect.Type) *AnalysisReport {
	root := typeNodes[rootType]
	seen := map[node]bool{}
	var nodes []node
	stack := []node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == nil || seen[n] {
			continue
		}
		seen[n] = true
		nodes = append(nodes, n)
		stack = append(stack, childrenOf(n)...)
	}

	a := &grammarAnalyzer{
		nodes:    nodes,
		nullable: map[node]tri{},
		first:    map[node][]firstAtom{},
		known:    map[node]bool{},
	}
	a.solve()
	a.solveKnownness()

	report := &AnalysisReport{}
	report.Productions = a.collectProductions(typeNodes, root)
	report.Diagnostics = a.collectDiagnostics(root)
	return report
}

// solve iterates the nullability and first-set equations to a fixed point.
// Values start empty, which models the base case for recursive productions.
func (a *grammarAnalyzer) solve() {
	for changed := true; changed; {
		changed = false
		for _, n := range a.nodes {
			nullable, first, _ := a.evaluate(n)
			if a.nullable[n] != nullable {
				a.nullable[n] = nullable
				changed = true
			}
			if !sameAtoms(a.first[n], first) {
				a.first[n] = first
				changed = true
			}
		}
	}
}

// solveKnownness marks every node that can be analysed without crossing a
// Parseable or ParseTypeWith production. It is monotone: every node starts
// known and only becomes unknown when it reaches an opaque node.
func (a *grammarAnalyzer) solveKnownness() {
	for _, n := range a.nodes {
		a.known[n] = true
	}
	for changed := true; changed; {
		changed = false
		for _, n := range a.nodes {
			if !a.known[n] {
				continue
			}
			if _, opaque := n.(*custom); opaque {
				a.known[n] = false
				changed = true
				continue
			}
			if _, opaque := n.(*parseable); opaque {
				a.known[n] = false
				changed = true
				continue
			}
			for _, child := range childrenOf(n) {
				if !a.known[child] {
					a.known[n] = false
					changed = true
					break
				}
			}
		}
	}
}

func sameAtoms(a, b []firstAtom) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].key != b[i].key || a[i].name != b[i].name {
			return false
		}
	}
	return true
}

// evaluate computes one iteration of the equations for a single node, reading
// the current fixed-point approximation of every referenced node.
func (a *grammarAnalyzer) evaluate(n node) (tri, []firstAtom, bool) {
	switch n := n.(type) {
	case *strct:
		return a.nullable[n.expr], a.first[n.expr], a.known[n.expr]

	case *union:
		nullable := triFalse
		known := true
		var first []firstAtom
		for _, member := range n.disjunction.nodes {
			nullable = triOr(nullable, a.nullable[member])
			known = known && a.known[member]
			first = appendAtoms(first, a.first[member]...)
		}
		return nullable, first, known

	case *custom, *parseable:
		return triUnknown, nil, false

	case *disjunction:
		nullable := triFalse
		known := true
		var first []firstAtom
		for _, branch := range n.nodes {
			nullable = triOr(nullable, a.nullable[branch])
			known = known && a.known[branch]
			first = appendAtoms(first, a.first[branch]...)
		}
		return nullable, first, known

	case *sequence:
		nullable := triTrue
		known := true
		var first []firstAtom
		for cur := n; cur != nil; cur = cur.next {
			child := cur.node
			known = known && a.known[child]
			first = appendAtoms(first, a.first[child]...)
			nullable = triAnd(nullable, a.nullable[child])
			// Tokens after a non-nullable element can never be first tokens.
			if a.nullable[child] != triTrue {
				break
			}
		}
		return nullable, first, known

	case *capture:
		return a.nullable[n.node], a.first[n.node], a.known[n.node]

	case *group:
		switch n.mode {
		case groupMatchZeroOrOne, groupMatchZeroOrMore:
			return triTrue, a.first[n.expr], a.known[n.expr]
		default: // groupMatchOnce, groupMatchOneOrMore, groupMatchNonEmpty
			return a.nullable[n.expr], a.first[n.expr], a.known[n.expr]
		}

	case *lookaheadGroup:
		// Assertions consume no input, so they are nullable and contribute no
		// first tokens.
		return triTrue, nil, true

	case *literal:
		return triFalse, []firstAtom{{key: a.literalKey(n), name: formatLiteral(n.s)}}, true

	case *reference:
		return triFalse, []firstAtom{{
			key:  atomKey{literal: false, typ: n.typ},
			name: "<" + strings.ToLower(n.identifier) + ">",
		}}, true

	case *negation:
		// A negation succeeds on anything except its operand, so the exact
		// first set cannot be enumerated.
		return triFalse, []firstAtom{{
			key:  atomKey{wildcard: true},
			name: "~" + ebnf(n.node),
		}}, true

	default:
		return triUnknown, nil, false
	}
}

func (a *grammarAnalyzer) literalKey(n *literal) atomKey {
	return atomKey{literal: true, value: n.s, typ: n.t}
}

func appendAtoms(existing []firstAtom, atoms ...firstAtom) []firstAtom {
	for _, atom := range atoms {
		duplicate := false
		for _, seen := range existing {
			if seen.key == atom.key {
				duplicate = true
				break
			}
		}
		if !duplicate {
			existing = append(existing, atom)
		}
	}
	return existing
}

func formatLiteral(s string) string {
	return fmt.Sprintf("%q", s)
}

// atomOverlaps reports whether two terminal descriptions can match the same
// token.
func atomOverlaps(x, y atomKey) bool {
	switch {
	case x.wildcard || y.wildcard:
		return true
	case x.literal && y.literal:
		if x.value != y.value {
			return false
		}
		return x.typ == lexer.EOF || y.typ == lexer.EOF || x.typ == y.typ
	case !x.literal && !y.literal:
		return x.typ == y.typ
	default:
		// A literal always belongs to some token type. Without proving that
		// relationship we only claim an overlap for an unconstrained literal,
		// keeping definite shadows free of false positives.
		literal, reference := x, y
		if !x.literal {
			literal, reference = y, x
		}
		return literal.typ == lexer.EOF || literal.typ == reference.typ
	}
}
