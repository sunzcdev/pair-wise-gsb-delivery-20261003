package chi

import (
	"fmt"
	"strings"
)

// Outcome values used by RouteStep.Outcome.
const (
	// OutcomeMatch marks a step where the node matched and consumed input.
	OutcomeMatch = "match"
	// OutcomeMiss marks a step where the branch was rejected, e.g. a static
	// prefix mismatch or a regexp that did not match its candidate value.
	OutcomeMiss = "miss"
	// OutcomeBacktrack marks a branch that matched locally but dead-ended
	// deeper in the tree, so the search fell back to sibling branches.
	OutcomeBacktrack = "backtrack"
	// OutcomeMethodNotAllowed marks a leaf that has endpoints registered,
	// but not for the requested HTTP method.
	OutcomeMethodNotAllowed = "method-not-allowed"
	// OutcomeMount marks crossing a Mount() boundary into a subrouter.
	OutcomeMount = "mount"
)

// RouteStep describes a single decision made while walking the routing tree
// during a call to Mux.Explain.
type RouteStep struct {
	// Depth is the traversal depth in the routing tree, including descents
	// into mounted subrouters.
	Depth int
	// Node is the kind of tree node evaluated: "static", "param", "regexp",
	// "catchall", or "mount" for a Mount() boundary.
	Node string
	// Prefix is the node prefix: literal text for static nodes, the regexp
	// source for regexp nodes, or the mount pattern at a mount boundary.
	Prefix string
	// Path is the remaining path when the node was evaluated.
	Path string
	// Value is the value consumed by the node when it matched: the static
	// prefix, a param value, or the remainder swallowed by a wildcard.
	Value string
	// Outcome is one of the Outcome* constants.
	Outcome string
	// Note is a human-readable detail, e.g. why a branch was rejected.
	Note string
}

// RouteExplanation describes how the router resolved, or failed to resolve,
// a method and path. It is produced by Mux.Explain and is a plain snapshot:
// it shares no state with the router or any pooled routing Context, so it
// stays valid after request contexts are reset and reused.
type RouteExplanation struct {
	// Method and Path are the inputs that were explained.
	Method string
	Path   string

	// Matched reports whether a handler pattern matched, mirroring Mux.Find.
	Matched bool
	// Pattern is the full matched routing pattern, or "" when unmatched.
	Pattern string
	// Params are the final URL params captured by the match.
	Params RouteParams

	// MethodNotAllowed reports that the path matched a route, just not for
	// the requested HTTP method (a real request would answer 405).
	MethodNotAllowed bool
	// AllowedMethods lists the methods the matched route does support.
	AllowedMethods []string

	// Steps is the ordered trace of every branch the search attempted.
	Steps []RouteStep
}

// URLParam returns the value of a single param from the explanation.
func (e *RouteExplanation) URLParam(key string) string {
	for i, k := range e.Params.Keys {
		if k == key {
			return e.Params.Values[i]
		}
	}
	return ""
}

// String renders a human-readable trace of the routing decision, useful for
// debugging unexpected 404s.
func (e *RouteExplanation) String() string {
	sb := &strings.Builder{}
	fmt.Fprintf(sb, "%s %s\n", e.Method, e.Path)
	for _, s := range e.Steps {
		fmt.Fprintf(sb, "  %s%-8s %-18s", strings.Repeat("  ", s.Depth), s.Node, s.Outcome)
		if s.Prefix != "" {
			fmt.Fprintf(sb, " prefix=%q", s.Prefix)
		}
		fmt.Fprintf(sb, " path=%q", s.Path)
		if s.Value != "" {
			fmt.Fprintf(sb, " value=%q", s.Value)
		}
		if s.Note != "" {
			fmt.Fprintf(sb, " (%s)", s.Note)
		}
		sb.WriteByte('\n')
	}
	switch {
	case e.Matched:
		fmt.Fprintf(sb, "=> matched %q\n", e.Pattern)
		for i, k := range e.Params.Keys {
			fmt.Fprintf(sb, "   param %s=%q\n", k, e.Params.Values[i])
		}
	case e.MethodNotAllowed:
		fmt.Fprintf(sb, "=> path matched, but method %s is not allowed (allowed: %s)\n",
			e.Method, strings.Join(e.AllowedMethods, ", "))
	default:
		sb.WriteString("=> no matching route\n")
	}
	return sb.String()
}

// routeTracer accumulates RouteSteps during a diagnostic tree traversal.
type routeTracer struct {
	steps []RouteStep
	depth int
}

func (t *routeTracer) add(step RouteStep) {
	step.Depth = t.depth
	t.steps = append(t.steps, step)
}

func nodeTypName(t nodeTyp) string {
	switch t {
	case ntStatic:
		return "static"
	case ntRegexp:
		return "regexp"
	case ntParam:
		return "param"
	case ntCatchAll:
		return "catchall"
	}
	return "unknown"
}

// Explain traces how the router would resolve the given method and path and
// reports why: every branch attempted, where regexp params rejected their
// candidate, what a wildcard consumed, where a Mount() boundary was crossed,
// the final pattern and params, and whether the path merely doesn't support
// the HTTP method.
//
// Explain is side-effect free: it never executes handlers or middleware and
// never modifies the routing tree. It also does not touch the mux's pooled
// routing contexts, and everything it returns is a private copy, so the
// RouteExplanation remains stable after request contexts are recycled.
func (mx *Mux) Explain(method, path string) *RouteExplanation {
	exp := &RouteExplanation{Method: method, Path: path}
	m, ok := methodMap[method]
	if !ok {
		return exp
	}

	// Use a dedicated routing context rather than a pooled one, so nothing
	// in the result aliases state that a later request could reset.
	rctx := NewRouteContext()
	tr := &routeTracer{}

	exp.Pattern = mx.explain(rctx, m, method, path, tr)
	exp.Steps = tr.steps
	exp.Matched = exp.Pattern != ""
	exp.MethodNotAllowed = !exp.Matched && rctx.methodNotAllowed
	for _, mt := range rctx.methodsAllowed {
		exp.AllowedMethods = append(exp.AllowedMethods, reverseMethodMap[mt])
	}
	if exp.Matched {
		exp.Params.Keys = append(exp.Params.Keys, rctx.URLParams.Keys...)
		exp.Params.Values = append(exp.Params.Values, rctx.URLParams.Values...)
	}
	return exp
}

// explain mirrors Mux.Find, recording the traversal into the tracer and
// descending into mounted chi subrouters.
func (mx *Mux) explain(rctx *Context, m methodTyp, method, path string, tr *routeTracer) string {
	node, _, _ := mx.tree.findRoute(rctx, m, path, tr)
	pattern := rctx.routePattern

	if node == nil {
		return ""
	}
	if node.subroutes == nil {
		return node.endpoints[m].pattern
	}

	rctx.RoutePath = mx.nextRoutePath(rctx)

	// Reset the wildcard param connecting the subrouter, exactly as the
	// real mount handler does, so the reported params match what an
	// endpoint handler would observe.
	n := len(rctx.URLParams.Keys) - 1
	if n >= 0 && rctx.URLParams.Keys[n] == "*" && len(rctx.URLParams.Values) > n {
		rctx.URLParams.Values[n] = ""
	}

	subMux, ok := node.subroutes.(*Mux)
	if !ok {
		tr.add(RouteStep{
			Node:    "mount",
			Prefix:  pattern,
			Path:    rctx.RoutePath,
			Outcome: OutcomeMount,
			Note:    "mounted handler is not a chi router; cannot inspect further",
		})
		return pattern
	}

	tr.add(RouteStep{
		Node:    "mount",
		Prefix:  pattern,
		Path:    rctx.RoutePath,
		Outcome: OutcomeMount,
		Note:    "crossing mount boundary into subrouter",
	})
	tr.depth++
	subPattern := subMux.explain(rctx, m, method, rctx.RoutePath, tr)
	tr.depth--
	if subPattern == "" {
		return ""
	}

	pattern = strings.TrimSuffix(pattern, "/*")
	pattern += subPattern
	return pattern
}
