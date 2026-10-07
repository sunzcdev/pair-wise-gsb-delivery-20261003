package participle

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/alecthomas/participle/v2/lexer"
)

// ChangeKind categorises a single structural difference found inside a
// production by CompareGrammars.
type ChangeKind int

const (
	// TokenChange is a change to a terminal: a literal string, a named lexer
	// token reference, or a literal's token-type constraint.
	TokenChange ChangeKind = iota + 1
	// QuantifierChange is a change to a cardinality modifier such as ?, *, +
	// or the non-empty ! marker.
	QuantifierChange
	// SequenceChange is a change to the ordered composition of a production:
	// an element added, removed, replaced, reordered, or a change of node
	// shape (wrapping, negation, lookahead, opaque node identity).
	SequenceChange
	// OrderedBranchChange is a reordering of the alternatives of an ordered
	// disjunction. PEG alternatives are ordered, so swapping branches is a
	// semantic change even when the set of branches is identical.
	OrderedBranchChange
)

func (c ChangeKind) String() string {
	switch c {
	case TokenChange:
		return "token"
	case QuantifierChange:
		return "quantifier"
	case SequenceChange:
		return "sequence"
	case OrderedBranchChange:
		return "ordered branch"
	default:
		return "unknown"
	}
}

// ProductionChange describes how a single production differs between two
// grammars.
type ProductionChange struct {
	// ID is the stable identifier of the production: the fully qualified Go
	// type name.
	ID string
	// Kinds lists the categories of modification, sorted and de-duplicated.
	// Every entry in GrammarReport.Modified carries one or more kinds.
	Kinds []ChangeKind
	// Details is a sorted list of human-readable descriptions of each
	// individual modification.
	Details []string
}

// GrammarReport is the structural comparison of two grammars.
type GrammarReport struct {
	Added    []string
	Removed  []string
	Modified []ProductionChange
}

// Changed reports whether the two grammars have any structural difference.
func (r *GrammarReport) Changed() bool {
	return len(r.Added) > 0 || len(r.Removed) > 0 || len(r.Modified) > 0
}

// String renders a stable, human-readable report. Productions are always
// reported in sorted identifier order so output is deterministic.
func (r *GrammarReport) String() string {
	var b strings.Builder
	if !r.Changed() {
		return "no grammar changes\n"
	}
	if len(r.Added) > 0 {
		b.WriteString("Added productions:\n")
		for _, id := range r.Added {
			fmt.Fprintf(&b, "  + %s\n", id)
		}
	}
	if len(r.Removed) > 0 {
		b.WriteString("Removed productions:\n")
		for _, id := range r.Removed {
			fmt.Fprintf(&b, "  - %s\n", id)
		}
	}
	if len(r.Modified) > 0 {
		b.WriteString("Modified productions:\n")
		for _, m := range r.Modified {
			kinds := make([]string, len(m.Kinds))
			for i, k := range m.Kinds {
				kinds[i] = k.String()
			}
			fmt.Fprintf(&b, "  ~ %s [%s]\n", m.ID, strings.Join(kinds, ", "))
			for _, detail := range m.Details {
				fmt.Fprintf(&b, "      %s\n", detail)
			}
		}
	}
	return b.String()
}

// Grammar is the type-erased view of a parser used by CompareGrammars. It is
// satisfied by *Parser[G] for any grammar type, allowing two different
// versions of a grammar (typically distinct, evolving type definitions) to be
// compared.
type Grammar interface {
	Lexer() lexer.Definition
	grammarNodes() map[reflect.Type]node
	fmt.Stringer
}

func (p *Parser[G]) grammarNodes() map[reflect.Type]node { return p.typeNodes }

// CompareGrammars structurally compares the productions of two grammars. The
// two parsers commonly represent an old and a new version of the same
// evolving grammar, and therefore need not share an identical root type.
//
// Productions are identified by their declared Go type name and lexer tokens
// by their declared symbol name. The recursive grammar graph is only compared
// one level deep: references to other productions and to opaque custom
// parsers are compared by their declared identity rather than being
// recursively inlined, so cyclic grammars are handled without unbounded
// expansion.
//
// Cosmetic differences (struct-tag whitespace, or the parentheses and spacing
// used purely to render the grammar) are ignored, but reordering the
// alternatives of an ordered disjunction is reported.
func CompareGrammars(old, neo Grammar) (*GrammarReport, error) {
	if old == nil || neo == nil {
		return nil, fmt.Errorf("CompareGrammars: both parsers must be non-nil")
	}
	oldModel := buildGrammarModel(old.Lexer(), old.grammarNodes())
	neoModel := buildGrammarModel(neo.Lexer(), neo.grammarNodes())

	report := &GrammarReport{}
	ids := make([]string, 0, len(oldModel)+len(neoModel))
	seen := make(map[string]bool, len(oldModel)+len(neoModel))
	for id := range oldModel {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for id := range neoModel {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	for _, id := range ids {
		op, oldOK := oldModel[id]
		np, newOK := neoModel[id]
		switch {
		case oldOK && !newOK:
			report.Removed = append(report.Removed, id)
		case !oldOK && newOK:
			report.Added = append(report.Added, id)
		default:
			if change := diffProduction(id, op, np); change != nil {
				report.Modified = append(report.Modified, *change)
			}
		}
	}
	sort.Strings(report.Added)
	sort.Strings(report.Removed)
	return report, nil
}

// productionModel is the normalised, identity-based view of a single
// production used for comparison.
type productionModel struct {
	id   string
	kind productionKind
	expr *gExpr
}

type productionKind int

const (
	kindStruct productionKind = iota + 1
	kindUnion
	kindOpaque
)

func (k productionKind) String() string {
	switch k {
	case kindStruct:
		return "struct"
	case kindUnion:
		return "union"
	case kindOpaque:
		return "custom"
	default:
		return "unknown"
	}
}

// gExpr is the normalised expression tree. Each variant occupies the fields
// relevant to it; the pointers of variants not in use are nil, which makes
// structural comparison direct.
type gExpr struct {
	kind exprKind

	literal *gLiteral
	token   *gToken
	ref     *gRef

	quant *gQuant
	neg   *gNeg
	look  *gLook

	children []*gExpr
}

type exprKind int

const (
	exprLiteral exprKind = iota + 1
	exprToken
	exprRef
	exprQuant
	exprNeg
	exprLook
	exprSeq
	exprAlt
)

// gLiteral is a terminal literal "..." optionally constrained to a token type.
type gLiteral struct {
	value     string
	tokenType string // empty when unconstrained
}

// gToken is a named lexer token reference <ident>.
type gToken struct{ name string }

// gRef is a reference to another production or to an opaque custom parser. It
// is compared by declared identity only and never recursively expanded.
type gRef struct {
	id   string
	kind productionKind
}

type gQuant struct {
	mode groupMatchMode
	expr *gExpr
}

type gNeg struct{ expr *gExpr }

type gLook struct {
	negative bool
	expr     *gExpr
}

// grammarModel is keyed by the stable production identifier.
type grammarModel map[string]*productionModel

func buildGrammarModel(def lexer.Definition, typeNodes map[reflect.Type]node) grammarModel {
	symbols := lexer.SymbolsByRune(def)
	model := grammarModel{}
	for typ, n := range typeNodes {
		// A type may appear both by value and by pointer (slice-of-pointer
		// fields register both T and *T); they are the same production.
		if typ.Kind() == reflect.Pointer {
			continue
		}
		id := typeID(typ)
		switch n := n.(type) {
		case *strct:
			model[id] = &productionModel{id: id, kind: kindStruct, expr: normalize(n.expr, symbols)}
		case *union:
			model[id] = &productionModel{id: id, kind: kindUnion, expr: normalize(&n.disjunction, symbols)}
		case *custom:
			model[id] = &productionModel{id: id, kind: kindOpaque, expr: opaqueExpr(id)}
		default:
			panic(fmt.Sprintf("unsupported top-level grammar node %T", n))
		}
	}
	return model
}

// opaqueExpr is the one-level body assigned to a custom production: a
// self-reference that records only its declared identity.
func opaqueExpr(id string) *gExpr {
	return &gExpr{kind: exprRef, ref: &gRef{id: id, kind: kindOpaque}}
}

// typeID is the stable production identifier. Named types use their declared
// name (reflect.Type.String() embeds a function suffix for locally declared
// types, which would make equivalent grammars built in different call sites
// look different). Anonymous types carry no declared name, so their field
// layout string (with volatile struct tags stripped) is used instead. Within
// a single parser named productions are unique (duplicate definitions are
// rejected at build).
func typeID(t reflect.Type) string {
	if name := t.Name(); name != "" {
		return name
	}
	return anonymousID(t.String())
}

// reflect.Type.String() renders anonymous struct tags as quoted strings.
var structTagRE = regexp.MustCompile(`\s+"(?:[^"\\]|\\.)*"`)
var whitespaceRE = regexp.MustCompile(`\s+`)

// anonymousID renders an anonymous type's identity without struct tags, so
// cosmetic tag formatting (extra whitespace within the grammar) never affects
// the identifier.
func anonymousID(s string) string {
	cleaned := structTagRE.ReplaceAllString(s, "")
	cleaned = whitespaceRE.ReplaceAllString(cleaned, " ")
	return strings.TrimSpace(cleaned)
}

// literalTokenType resolves a literal's optional token-type constraint. An
// unconstrained literal has t == lexer.EOF and maps to the empty string.
func literalTokenType(t lexer.TokenType, symbols map[lexer.TokenType]string) string {
	if t == lexer.EOF {
		return ""
	}
	return symbols[t]
}

func shortName(id string) string {
	if strings.ContainsAny(id, " {}") {
		return id
	}
	if i := strings.LastIndex(id, "."); i >= 0 {
		return id[i+1:]
	}
	return id
}

func normalize(n node, symbols map[lexer.TokenType]string) *gExpr {
	// capture and plain (groupMatchOnce) groups carry no grammar of their own.
	for {
		switch x := n.(type) {
		case *capture:
			n = x.node
		case *group:
			if x.mode == groupMatchOnce {
				n = x.expr
			} else {
				return &gExpr{
					kind:  exprQuant,
					quant: &gQuant{mode: x.mode, expr: normalize(x.expr, symbols)},
				}
			}
		default:
			return normalizeExpr(n, symbols)
		}
	}
}

func normalizeExpr(n node, symbols map[lexer.TokenType]string) *gExpr {
	switch n := n.(type) {
	case *capture:
		return normalize(n.node, symbols)
	case *group:
		if n.mode == groupMatchOnce {
			return normalize(n.expr, symbols)
		}
		return &gExpr{
			kind:  exprQuant,
			quant: &gQuant{mode: n.mode, expr: normalize(n.expr, symbols)},
		}
	case *literal:
		return &gExpr{kind: exprLiteral, literal: &gLiteral{value: n.s, tokenType: literalTokenType(n.t, symbols)}}
	case *reference:
		return &gExpr{kind: exprToken, token: &gToken{name: n.identifier}}
	case *negation:
		return &gExpr{kind: exprNeg, neg: &gNeg{expr: normalize(n.node, symbols)}}
	case *lookaheadGroup:
		return &gExpr{kind: exprLook, look: &gLook{negative: n.negative, expr: normalize(n.expr, symbols)}}
	case *disjunction:
		children := make([]*gExpr, 0, len(n.nodes))
		for _, child := range n.nodes {
			children = appendAlt(children, normalize(child, symbols))
		}
		return altOrSingle(children)
	case *sequence:
		children := []*gExpr{}
		for cur := n; cur != nil; cur = cur.next {
			children = appendSeq(children, normalize(cur.node, symbols))
		}
		return seqOrSingle(children)
	case *strct:
		return productionRef(typeID(n.typ), kindStruct)
	case *union:
		return productionRef(typeID(n.typ), kindUnion)
	case *custom:
		return productionRef(typeID(n.typ), kindOpaque)
	case *parseable:
		return productionRef(typeID(n.t), kindOpaque)
	default:
		panic(fmt.Sprintf("unsupported grammar node %T", n))
	}
}

func productionRef(id string, kind productionKind) *gExpr {
	return &gExpr{kind: exprRef, ref: &gRef{id: id, kind: kind}}
}

// appendAlt flattens nested, semantically-neutral disjunctions.
func appendAlt(out []*gExpr, e *gExpr) []*gExpr {
	if e.kind == exprAlt {
		return append(out, e.children...)
	}
	return append(out, e)
}

// appendSeq flattens nested, semantically-neutral sequences.
func appendSeq(out []*gExpr, e *gExpr) []*gExpr {
	if e.kind == exprSeq {
		return append(out, e.children...)
	}
	return append(out, e)
}

func altOrSingle(children []*gExpr) *gExpr {
	if len(children) == 1 {
		return children[0]
	}
	return &gExpr{kind: exprAlt, children: children}
}

func seqOrSingle(children []*gExpr) *gExpr {
	if len(children) == 1 {
		return children[0]
	}
	return &gExpr{kind: exprSeq, children: children}
}
