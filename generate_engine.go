package participle

import (
	"fmt"
	"math/rand/v2"
	"reflect"
)

type generator struct {
	opts     generateOptions
	rng      *rand.Rand
	costs    map[reflect.Type]int
	adapters map[reflect.Type]Adapter
	steps    int
	terms    []GeneratedToken
	path     []Choice
	// backtracks bounds local recovery when an initially feasible alternative turns
	// out not to fit once an earlier sibling consumed terminals.
	backtracks int
}

const maxBacktracks = 128

func newGenerator(opts generateOptions, seed uint64, costs map[reflect.Type]int) *generator {
	return &generator{
		opts:     opts,
		rng:      rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15)),
		costs:    costs,
		adapters: opts.adapters,
		steps:    opts.maxSteps,
	}
}

func (g *generator) productionCost(n node) int {
	if t := productionType(n); t != nil {
		return g.costs[t]
	}
	return g.nodeCost(n)
}

// nodeCost is the minimum terminal cost of a node under the fixed-point costs.
func (g *generator) nodeCost(n node) int {
	return staticCost(n, g.costs, g.adapters, genInfinity)
}

// genNode expands n while never exceeding budget, returning the number of
// terminals emitted. production labels recorded choices with the current enclosing
// non-terminal.
func (g *generator) genNode(n node, production string, budget int) (emitted int, err error) {
	switch n := n.(type) {
	case *literal:
		if n.s == "" {
			return 0, nil
		}
		if budget < 1 {
			return 0, g.noBudget(production, n)
		}
		g.terms = append(g.terms, GeneratedToken{Literal: n.s, TokenType: n.t})
		return 1, nil

	case *reference:
		if budget < 1 {
			return 0, g.noBudget(production, n)
		}
		g.terms = append(g.terms, GeneratedToken{Name: n.identifier, TokenType: n.typ})
		return 1, nil

	case *capture:
		return g.genNode(n.node, production, budget)

	case *sequence:
		remaining := budget
		for s := n; s != nil; s = s.next {
			got, err := g.genNode(s.node, production, remaining)
			if err != nil {
				return emitted, err
			}
			emitted += got
			remaining -= got
		}
		return emitted, nil

	case *disjunction:
		return g.genDisjunction(n, production, budget)

	case *group:
		return g.genGroup(n, production, budget)

	case *strct:
		return g.expand(production, budget, n.typ, n.expr)

	case *union:
		return g.expand(production, budget, n.typ, &n.disjunction)

	case *parseable:
		return g.genAdapter(n.t, production, budget)

	case *custom:
		return g.genAdapter(n.typ, production, budget)

	case *negation, *lookaheadGroup:
		return 0, fmt.Errorf("%w: %s cannot be synthesised", ErrUnsupportedGeneration, n)

	default:
		return 0, fmt.Errorf("%w: unknown grammar node %T", ErrUnsupportedGeneration, n)
	}
}

func (g *generator) expand(parentProduction string, budget int, typ reflect.Type, expr node) (int, error) {
	name := typ.Name()
	if g.costs[typ] > budget {
		return 0, g.noBudget(parentProduction, labelNode("production "+name))
	}
	if g.steps <= 0 {
		return 0, fmt.Errorf("%w: production %s cannot be expanded further; the structural step limit of %d was "+
			"reached (raise GenerateMaxSteps or lower maxTokens)", ErrNoDerivation, name, g.opts.maxSteps)
	}
	g.steps--
	return g.genNode(expr, name, budget)
}

func (g *generator) genDisjunction(d *disjunction, production string, budget int) (int, error) {
	feasible := make([]int, 0, len(d.nodes))
	for i, alt := range d.nodes {
		if g.nodeCost(alt) <= budget {
			feasible = append(feasible, i)
		}
	}
	if len(feasible) == 0 {
		return 0, g.noBudget(production, d)
	}
	// Randomly order the feasible alternatives, then recover locally if a pick only
	// fails once earlier siblings in the enclosing sequence have spent the budget.
	order := g.shuffled(len(feasible))
	var lastErr error
	for attempt, chosenPos := range order {
		chosen := feasible[chosenPos]
		savedTerms, savedPath, savedSteps := len(g.terms), len(g.path), g.steps
		g.path = append(g.path, Choice{
			Production: production,
			Kind:       ChoiceAlternative,
			Chosen:     chosen,
			Options:    len(feasible),
		})
		got, err := g.genNode(d.nodes[chosen], production, budget)
		if err == nil {
			return got, nil
		}
		lastErr = err
		g.terms = g.terms[:savedTerms]
		g.path = g.path[:savedPath]
		g.steps = savedSteps
		if attempt == 0 {
			g.backtracks++
			if g.backtracks > maxBacktracks {
				break
			}
		}
	}
	return 0, lastErr
}

// shuffled returns indices [0,n) in random order using Fisher-Yates.
func (g *generator) shuffled(n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	for i := n - 1; i > 0; i-- {
		j := g.intN(i + 1)
		order[i], order[j] = order[j], order[i]
	}
	return order
}

func (g *generator) genGroup(grp *group, production string, budget int) (int, error) {
	switch grp.mode {
	case groupMatchOnce, groupMatchNonEmpty:
		return g.genNode(grp.expr, production, budget)

	case groupMatchZeroOrOne:
		cost := g.nodeCost(grp.expr)
		if cost <= budget && g.steps > 0 && g.bool() {
			g.steps--
			return g.genNode(grp.expr, production, budget)
		}
		return 0, nil

	case groupMatchZeroOrMore:
		return g.genRepetition(grp, production, budget, false)

	case groupMatchOneOrMore:
		return g.genRepetition(grp, production, budget, true)

	default:
		return 0, fmt.Errorf("%w: unknown group mode %s", ErrUnsupportedGeneration, grp.mode)
	}
}

// genRepetition emits iterations one decision at a time. Output length is bounded by
// budget alone: an iteration that emits one or more terminals can only repeat while
// budget remains. The structural step limit exists solely to guarantee termination
// for iterations whose body emits zero terminals (a nullable loop).
func (g *generator) genRepetition(grp *group, production string, budget int, required bool) (int, error) {
	bodyCost := g.nodeCost(grp.expr)
	remaining := budget
	iterations := 0
	guardStop := false

	for {
		if required && iterations == 0 {
			if bodyCost > remaining {
				return 0, g.noBudget(production, grp)
			}
		} else {
			if bodyCost > remaining {
				break
			}
			if g.steps <= 0 {
				guardStop = true
				break
			}
			if !g.bool() {
				break
			}
		}
		g.steps--
		got, err := g.genNode(grp.expr, production, remaining)
		if err != nil {
			if required && iterations == 0 {
				return 0, err
			}
			break
		}
		iterations++
		remaining -= got
		required = false
	}

	g.path = append(g.path, Choice{
		Production: production,
		Kind:       ChoiceRepetition,
		Chosen:     iterations,
		Options:    boolToInt(guardStop),
	})
	return budget - remaining, nil
}

func (g *generator) genAdapter(typ reflect.Type, production string, budget int) (int, error) {
	adapter := g.adapters[typ]
	tokens, err := adapter.Generate(g.rng)
	if err != nil {
		return 0, fmt.Errorf("adapter for %s failed: %w", typ, err)
	}
	if len(tokens) < adapter.MinTokens {
		return 0, fmt.Errorf("adapter for %s promised at least %d token(s) but produced %d",
			typ, adapter.MinTokens, len(tokens))
	}
	if len(tokens) > budget {
		return 0, g.noBudget(production, labelNode("production "+typ.Name()))
	}
	name := typ.Name()
	for i := range tokens {
		tokens[i].From = name
	}
	g.terms = append(g.terms, tokens...)
	return len(tokens), nil
}

// labelNode renders a free-form production label in budget errors.
type labelNode string

func (l labelNode) String() string   { return string(l) }
func (l labelNode) GoString() string { return string(l) }

func (g *generator) noBudget(production string, n fmt.Stringer) error {
	at := production
	if at == "" {
		at = "root"
	}
	return fmt.Errorf("%w: cannot place %s in production %s within the remaining budget",
		ErrNoDerivation, n, at)
}

func (g *generator) intN(n int) int {
	if n <= 1 {
		return 0
	}
	return g.rng.IntN(n)
}

func (g *generator) bool() bool {
	return g.rng.Uint64()&1 == 0
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// visitGeneration walks the grammar node graph for analysis. Unlike [visit] it is
// cycle-safe: non-terminal productions are only descended into once per type, so
// recursive grammars terminate.
func visitGeneration(root node, fn func(node)) {
	seen := map[reflect.Type]bool{}
	var walk func(node)
	walk = func(n node) {
		if n == nil {
			return
		}
		fn(n)
		switch n := n.(type) {
		case *disjunction:
			for _, child := range n.nodes {
				walk(child)
			}
		case *strct:
			if seen[n.typ] {
				return
			}
			seen[n.typ] = true
			walk(n.expr)
		case *union:
			if seen[n.typ] {
				return
			}
			seen[n.typ] = true
			for _, child := range n.disjunction.nodes {
				walk(child)
			}
		case *sequence:
			for s := n; s != nil; s = s.next {
				walk(s.node)
			}
		case *capture:
			walk(n.node)
		case *negation:
			walk(n.node)
		case *group:
			walk(n.expr)
		case *lookaheadGroup:
			walk(n.expr)
		case *reference, *literal, *parseable, *custom:
			// Leaves (and opaque productions have no inspectable children).
		default:
			panic(fmt.Sprintf("unknown grammar node %T", n))
		}
	}
	walk(root)
}
