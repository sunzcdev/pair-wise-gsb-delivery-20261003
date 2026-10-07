package participle

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"

	"github.com/alecthomas/participle/v2/lexer"
)

// ErrNoDerivation is returned by [Parser.Generate] when the grammar cannot produce
// any sentence that fits the requested token budget, even when all recursion and
// repetitions have been collapsed to their minimum.
var ErrNoDerivation = fmt.Errorf("no derivation within token budget")

// ErrUnsupportedGeneration is returned when a grammar construct is intrinsically
// unable to be synthesised without semantic knowledge: token negation, lookahead
// assertions, or an opaque production without an explicit adapter.
var ErrUnsupportedGeneration = fmt.Errorf("grammar construct is not supported by the generator")

// GeneratedToken is a single terminal produced during generation.
//
// Exactly one of Literal or Name is non-empty. Literal is the verbatim source text
// of a string literal from the grammar; Name is the symbolic name of a named lexer
// token (eg. "Ident" or "Number"), whose concrete text is produced by the caller's
// [TokenRenderer].
type GeneratedToken struct {
	// Literal is the exact text of a quoted grammar literal, or empty.
	Literal string
	// Name is the symbolic name of a lexer token reference, or empty.
	Name string
	// TokenType is the lexer TokenType associated with this terminal.
	TokenType lexer.TokenType
	// From is the type name of the custom adapter that emitted this token, if any.
	From string
}

// Adapter generates terminals for an opaque production (one implementing Parseable,
// or registered via ParseTypeWith) whose shape the grammar cannot analyse.
//
// MinTokens is the minimum number of terminals returned by Generate and must be a
// tight, non-negative lower bound; it is used for the minimum-cost analysis. The
// generated terminals may be named tokens or concrete literals and are validated by
// re-parsing just like every other terminal. Generate receives a seeded rand source
// so that adapters participate in deterministic regeneration.
type Adapter struct {
	MinTokens int
	Generate  func(r *rand.Rand) ([]GeneratedToken, error)
}

// Choice records a single nondeterministic decision made while generating.
//
// Production names the enclosing non-terminal (a grammar struct or interface
// production), or "" for the implicit root expression. For a disjunction, Chosen is
// the index of the selected alternative and Options is the number of alternatives
// that fit the remaining budget. For a repetition, Chosen is the number of
// iterations emitted and Options is 1 when the loop was forced to stop by the
// structural depth guard, 0 otherwise.
type Choice struct {
	Production string
	Kind       ChoiceKind
	Chosen     int
	Options    int
}

// ChoiceKind identifies what a [Choice] record describes.
type ChoiceKind int

const (
	// ChoiceAlternative records the selection of one alternative of a disjunction.
	ChoiceAlternative ChoiceKind = iota
	// ChoiceRepetition records the iteration count chosen for a "*" group.
	ChoiceRepetition
)

func (c ChoiceKind) String() string {
	switch c {
	case ChoiceAlternative:
		return "alternative"
	case ChoiceRepetition:
		return "repetition"
	default:
		return "unknown"
	}
}

// Generated is the result of a successful [Parser.Generate].
type Generated struct {
	// Text is the generated, re-parsed input, with terminals joined by the
	// configured separator.
	Text string
	// Tokens is the sequence of terminals the derivation produced.
	Tokens []GeneratedToken
	// Path records every nondeterministic choice made while generating. Replaying
	// the same seed (and the same adapters) reproduces the same Path and Text.
	Path []Choice
}

// TokenRenderer maps a named lexer token (eg. "Ident", "Number") to concrete source
// text. The grammar guarantees that only tokens which fit the budget are requested,
// but it is the renderer's responsibility to return text that actually lexes as the
// named token; re-parsing validates this.
type TokenRenderer func(tokenName string, r *rand.Rand) (string, error)

type generateOptions struct {
	adapters map[reflect.Type]Adapter
	join     string
	maxSteps int
}

// GenerateOption configures [Parser.Generate].
type GenerateOption func(*generateOptions) error

// GenerationAdapter registers an [Adapter] for the opaque production P.
//
// P is normally a type implementing Parseable or an interface registered with
// ParseTypeWith. Without an explicit adapter the generator refuses to invent any
// tokens for such a production and returns an error naming the type.
func GenerationAdapter[P any](adapter Adapter) GenerateOption {
	target := reflect.TypeFor[P]()
	if target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	return func(o *generateOptions) error {
		if adapter.Generate == nil {
			return fmt.Errorf("GenerationAdapter for %s: Generate function must not be nil", target)
		}
		if adapter.MinTokens < 0 {
			return fmt.Errorf("GenerationAdapter for %s: MinTokens must be non-negative", target)
		}
		if _, exists := o.adapters[target]; exists {
			return fmt.Errorf("duplicate GenerationAdapter for %s", target)
		}
		o.adapters[target] = adapter
		return nil
	}
}

// GenerateJoin sets the separator used to concatenate generated terminals into
// [Generated.Text]. It defaults to a single space. An empty separator is allowed
// when the caller's [TokenRenderer] already provides all necessary spacing.
func GenerateJoin(separator string) GenerateOption {
	return func(o *generateOptions) error {
		o.join = separator
		return nil
	}
}

// GenerateMaxSteps bounds the structural expansion of a derivation: the total
// number of non-terminal expansions and repetition iterations. It exists purely to
// guarantee termination in the presence of nullable ("*") repetitions and
// zero-minimum-cost recursion; it never bounds output length, which is controlled
// exclusively by maxTokens. It defaults to 64.
func GenerateMaxSteps(steps int) GenerateOption {
	return func(o *generateOptions) error {
		if steps < 1 {
			return fmt.Errorf("GenerateMaxSteps must be at least 1, got %d", steps)
		}
		o.maxSteps = steps
		return nil
	}
}

// Generate produces a small valid input for the grammar by grammar-driven random
// generation.
//
// Each production's minimum terminal cost is computed first; branches whose minimum
// cost exceeds the remaining budget are pruned, then a feasible branch is picked
// using the given seed. maxTokens is a hard upper bound on the number of emitted
// terminals and is never exceeded. The same seed (and adapters) always yields the
// same [Generated.Text] and [Generated.Path].
//
// render supplies concrete text for named lexer tokens. When no derivation fits the
// budget, an error wrapping [ErrNoDerivation] is returned. The resulting text is
// always re-parsed by the owning parser to prove it is a legal input.
func (p *Parser[G]) Generate(seed uint64, maxTokens int, render TokenRenderer, options ...GenerateOption) (*Generated, error) {
	if maxTokens < 0 {
		return nil, fmt.Errorf("maxTokens must be non-negative, got %d", maxTokens)
	}
	if render == nil {
		return nil, fmt.Errorf("a TokenRenderer is required")
	}
	opts := generateOptions{
		adapters: map[reflect.Type]Adapter{},
		join:     " ",
		maxSteps: 64,
	}
	for _, option := range options {
		if err := option(&opts); err != nil {
			return nil, err
		}
	}

	rootNode := p.typeNodes[p.rootType]
	analysis, err := analyseGeneration(rootNode, opts.adapters)
	if err != nil {
		return nil, err
	}

	g := newGenerator(opts, seed, analysis.costs)
	if minCost := g.productionCost(rootNode); minCost > maxTokens {
		return nil, fmt.Errorf("%w: production %s needs at least %d token(s) but the budget is %d",
			ErrNoDerivation, productionName(rootNode), minCost, maxTokens)
	}
	emitted, err := g.genNode(rootNode, "", maxTokens)
	if err != nil {
		return nil, err
	}

	parts := make([]string, emitted)
	terms := g.terms[:emitted]
	for i, term := range terms {
		if term.Literal != "" {
			parts[i] = term.Literal
			continue
		}
		text, err := render(term.Name, g.rng)
		if err != nil {
			return nil, fmt.Errorf("rendering token %q: %w", term.Name, err)
		}
		parts[i] = text
	}
	text := strings.Join(parts, opts.join)
	if _, err := p.ParseString("", text); err != nil {
		return nil, fmt.Errorf("generated input %q was rejected by the parser: %w", text, err)
	}

	return &Generated{Text: text, Tokens: append([]GeneratedToken(nil), terms...), Path: g.path}, nil
}

// productionType returns the non-terminal type a node stands for, or nil.
func productionType(n node) reflect.Type {
	switch n := n.(type) {
	case *strct:
		return n.typ
	case *union:
		return n.typ
	case *custom:
		return n.typ
	case *parseable:
		return n.t
	default:
		return nil
	}
}

func productionName(n node) string {
	if t := productionType(n); t != nil {
		return t.Name()
	}
	return "root"
}

type generationAnalysis struct {
	// costs maps every non-terminal type to its minimum terminal cost (least fixed
	// point). Recursive references resolve through the same map.
	costs map[reflect.Type]int
}

func analyseGeneration(root node, adapters map[reflect.Type]Adapter) (*generationAnalysis, error) {
	opaque := map[reflect.Type]bool{}
	var unsupported string
	visitGeneration(root, func(n node) {
		switch n := n.(type) {
		case *parseable:
			if _, ok := adapters[n.t]; !ok {
				opaque[n.t] = true
			}
		case *custom:
			if _, ok := adapters[n.typ]; !ok {
				opaque[n.typ] = true
			}
		case *negation:
			if unsupported == "" {
				unsupported = "token negation (~ / !)"
			}
		case *lookaheadGroup:
			if unsupported == "" {
				if n.negative {
					unsupported = "negative lookahead assertion (?!...)"
				} else {
					unsupported = "positive lookahead assertion (?=...)"
				}
			}
		}
	})
	if len(opaque) > 0 {
		names := make([]string, 0, len(opaque))
		for t := range opaque {
			names = append(names, t.String())
		}
		return nil, fmt.Errorf("%w: no GenerationAdapter registered for opaque production(s) %s; "+
			"the generator cannot fabricate matches for custom Parseable types",
			ErrUnsupportedGeneration, strings.Join(names, ", "))
	}
	if unsupported != "" {
		return nil, fmt.Errorf("%w: %s cannot be synthesised; remove it from the generated fragment "+
			"or isolate it behind an opaque production with a GenerationAdapter", ErrUnsupportedGeneration, unsupported)
	}

	// Collect every non-terminal reachable from the root.
	nonTerms := map[reflect.Type]node{}
	visitGeneration(root, func(n node) {
		if t := productionType(n); t != nil {
			nonTerms[t] = n
		}
	})

	const inf = genInfinity
	costs := make(map[reflect.Type]int, len(nonTerms))
	for t := range nonTerms {
		costs[t] = inf
	}
	// Least fixed point: repeatedly relax every production until costs stabilise.
	// Each strict relaxation decreases a cost, so this terminates after finitely
	// many rounds regardless of recursion.
	for {
		changed := false
		for t, n := range nonTerms {
			var c int
			switch n := n.(type) {
			case *strct:
				c = staticCost(n.expr, costs, adapters, inf)
			case *union:
				c = staticCost(&n.disjunction, costs, adapters, inf)
			case *parseable:
				c = adapters[n.t].MinTokens
			case *custom:
				c = adapters[n.typ].MinTokens
			default:
				c = inf
			}
			if c < costs[t] {
				costs[t] = c
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return &generationAnalysis{costs: costs}, nil
}

const genInfinity = int(^uint(0) >> 1)

// staticCost computes the minimum cost of a node given current per-type costs.
// Types absent from (or infinite in) costs are treated as infinite, which is what
// makes recursive references infinite in the first fixed-point rounds.
func staticCost(n node, costs map[reflect.Type]int, adapters map[reflect.Type]Adapter, inf int) int {
	add := func(a, b int) int {
		if a >= inf || b >= inf || a > inf-b {
			return inf
		}
		return a + b
	}
	minCost := func(a, b int) int {
		if a < b {
			return a
		}
		return b
	}
	switch n := n.(type) {
	case *literal:
		if n.s == "" {
			return 0
		}
		return 1
	case *reference:
		return 1
	case *capture:
		return staticCost(n.node, costs, adapters, inf)
	case *sequence:
		total := 0
		for s := n; s != nil; s = s.next {
			total = add(total, staticCost(s.node, costs, adapters, inf))
		}
		return total
	case *disjunction:
		best := inf
		for _, alt := range n.nodes {
			best = minCost(best, staticCost(alt, costs, adapters, inf))
		}
		return best
	case *union, *strct:
		return costs[productionType(n)]
	case *parseable:
		return adapters[n.t].MinTokens
	case *custom:
		return adapters[n.typ].MinTokens
	case *group:
		body := staticCost(n.expr, costs, adapters, inf)
		switch n.mode {
		case groupMatchZeroOrOne, groupMatchZeroOrMore:
			return 0
		case groupMatchOneOrMore, groupMatchNonEmpty:
			return body
		default:
			return body
		}
	case *negation, *lookaheadGroup:
		return inf
	default:
		return inf
	}
}
