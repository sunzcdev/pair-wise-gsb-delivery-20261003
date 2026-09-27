package checker

import (
	"fmt"
	"strings"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/conf"
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/parser"
)

// CheckPurity verifies that a typed expression tree only calls functions and
// builtins explicitly declared as pure by the host (see expr.PureRule). It is
// a no-op unless pure rule mode is enabled.
//
// The check runs on the typed AST after all patchers were applied, so calls
// introduced by patchers (for example, operator overloading) are verified as
// well. It is meant to run twice: after type checking and again after the
// optimization passes, so calls introduced or exposed by optimizer rewrites
// cannot bypass the restriction.
//
// The host declaration is the trust boundary: CheckPurity does not attempt to
// prove that a declared Go function is free of side effects.
func CheckPurity(tree *parser.Tree, config *conf.Config) error {
	if config == nil || !config.PureMode {
		return nil
	}
	p := &purityChecker{config: config}
	p.walk(tree.Node)
	if p.err != nil {
		return p.err.Bind(tree.Source)
	}
	return nil
}

type purityChecker struct {
	config *conf.Config
	chain  []string
	err    *file.Error
}

func (p *purityChecker) walk(node ast.Node) {
	if p.err != nil || node == nil {
		return
	}
	switch n := node.(type) {
	case *ast.CallNode:
		p.callNode(n)
	case *ast.BuiltinNode:
		p.builtinNode(n)
	case *ast.UnaryNode:
		p.walk(n.Node)
	case *ast.BinaryNode:
		p.walk(n.Left)
		p.walk(n.Right)
	case *ast.ChainNode:
		p.walk(n.Node)
	case *ast.MemberNode:
		p.walk(n.Node)
		p.walk(n.Property)
	case *ast.SliceNode:
		p.walk(n.Node)
		if n.From != nil {
			p.walk(n.From)
		}
		if n.To != nil {
			p.walk(n.To)
		}
	case *ast.PredicateNode:
		p.walk(n.Node)
	case *ast.VariableDeclaratorNode:
		p.walk(n.Value)
		p.walk(n.Expr)
	case *ast.SequenceNode:
		for i := range n.Nodes {
			p.walk(n.Nodes[i])
		}
	case *ast.ConditionalNode:
		p.walk(n.Cond)
		p.walk(n.Exp1)
		p.walk(n.Exp2)
	case *ast.ArrayNode:
		for i := range n.Nodes {
			p.walk(n.Nodes[i])
		}
	case *ast.MapNode:
		for i := range n.Pairs {
			p.walk(n.Pairs[i])
		}
	case *ast.PairNode:
		p.walk(n.Key)
		p.walk(n.Value)
	}
}

func (p *purityChecker) callNode(node *ast.CallNode) {
	switch callee := node.Callee.(type) {
	case *ast.IdentifierNode:
		name := callee.Value
		if p.config.PureFunctions[name] {
			p.enter(name, node.Arguments)
			return
		}
		if p.isKnownFunction(name) {
			p.fail(node, name, fmt.Sprintf("call to function %q is not allowed: not declared as pure by the host", name))
		} else {
			p.fail(node, name, fmt.Sprintf("indirect call through %q is not allowed in pure rule mode", name))
		}
	case *ast.MemberNode:
		// The object expression may itself contain calls.
		p.walk(callee.Node)
		if property, ok := callee.Property.(*ast.StringNode); ok {
			p.fail(node, property.Value, fmt.Sprintf("method call %q is not allowed in pure rule mode", property.Value))
		} else {
			p.fail(node, "call", "indirect call through a computed callee is not allowed in pure rule mode")
		}
	default:
		p.walk(node.Callee)
		p.fail(node, "call", "indirect call through a computed callee is not allowed in pure rule mode")
	}
}

func (p *purityChecker) builtinNode(node *ast.BuiltinNode) {
	if !p.config.PureFunctions[node.Name] {
		p.fail(node, node.Name, fmt.Sprintf("call to builtin %q is not allowed: not declared as pure by the host", node.Name))
		return
	}
	p.enter(node.Name, node.Arguments)
}

// enter descends into the arguments of an allowed call, extending the call
// chain so a nested violation can report the full path that led to it.
func (p *purityChecker) enter(name string, args []ast.Node) {
	p.chain = append(p.chain, name)
	for _, arg := range args {
		p.walk(arg)
	}
	p.chain = p.chain[:len(p.chain)-1]
}

func (p *purityChecker) fail(node ast.Node, name, message string) {
	if p.err != nil {
		return
	}
	chain := append(append(make([]string, 0, len(p.chain)+1), p.chain...), name)
	p.err = &file.Error{
		Location: node.Location(),
		Message:  fmt.Sprintf("pure rule violation: %s (call chain: %s)", message, strings.Join(chain, " -> ")),
	}
}

// isKnownFunction reports whether the name refers to a function the host
// registered (custom function, builtin, or env method), as opposed to a
// variable that merely holds a function value.
func (p *purityChecker) isKnownFunction(name string) bool {
	if _, ok := p.config.Functions[name]; ok {
		return true
	}
	if _, ok := p.config.Builtins[name]; ok {
		return true
	}
	if nt, ok := p.config.Env.Get(&p.config.NtCache, name); ok && nt.IsFunc() {
		return true
	}
	return false
}
