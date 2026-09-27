package checker

import (
	"fmt"
	"strings"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/conf"
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/parser"
)

// ValidatePure enforces pure rule mode on a checked tree. It is called during
// the type-checking phase (see Checker.check) and must be called again after
// the optimization phase, as the optimizer rewrites the tree. Only functions
// explicitly declared by the host (conf.Config.PureFuncs) and builtins known
// to be pure may be called; method calls, indirect calls through variables or
// computed values, and builtins that read external state are rejected. The
// returned error reports the call chain that broke the constraint.
func ValidatePure(tree *parser.Tree, config *conf.Config) error {
	if config == nil || !config.Pure {
		return nil
	}
	p := &pureValidator{
		config: config,
		lets:   make(map[string]bool),
	}
	p.walk(tree.Node, nil)
	if p.err != nil {
		return p.err.Bind(tree.Source)
	}
	return nil
}

type pureValidator struct {
	config *conf.Config
	lets   map[string]bool
	err    *file.Error
}

func (p *pureValidator) fail(node ast.Node, chain []string, format string, args ...any) {
	if p.err != nil {
		return // keep the first error
	}
	msg := fmt.Sprintf(format, args...)
	if len(chain) > 0 {
		msg += fmt.Sprintf(" (call chain: %s)", strings.Join(chain, " -> "))
	}
	p.err = &file.Error{
		Location: node.Location(),
		Message:  msg,
	}
}

// checkCall validates a call and returns the call chain including the call
// itself, so nested calls inside its arguments report the full chain.
func (p *pureValidator) checkCall(node ast.Node, name string, chain []string) []string {
	chain = append(chain, name+"(...)")
	if fn, ok := p.config.Builtins[name]; ok {
		if fn.Impure {
			p.fail(node, chain, "pure mode: builtin %q reads external state and is not allowed", name)
		}
		return chain
	}
	if p.config.PureFuncs[name] {
		return chain
	}
	if p.lets[name] {
		p.fail(node, chain, "pure mode: indirect call of variable %q is not allowed", name)
	} else {
		p.fail(node, chain, "pure mode: call to undeclared function %q is not allowed", name)
	}
	return chain
}

func (p *pureValidator) walk(node ast.Node, chain []string) {
	if p.err != nil || node == nil {
		return
	}
	switch n := node.(type) {
	case *ast.CallNode:
		switch callee := n.Callee.(type) {
		case *ast.IdentifierNode:
			chain = p.checkCall(n, callee.Value, chain)
		case *ast.MemberNode:
			name := "method"
			if prop, ok := callee.Property.(*ast.StringNode); ok {
				name = prop.Value
			}
			chain = append(chain, name+"(...)")
			p.fail(n, chain, "pure mode: method call %q is not allowed", name)
			return
		default:
			p.fail(n, append(chain, "(...)"), "pure mode: indirect call of a computed function value is not allowed")
			return
		}
		p.walk(n.Callee, chain)
		for _, arg := range n.Arguments {
			p.walk(arg, chain)
		}

	case *ast.BuiltinNode:
		chain = p.checkCall(n, n.Name, chain)
		for _, arg := range n.Arguments {
			p.walk(arg, chain)
		}

	case *ast.VariableDeclaratorNode:
		p.walk(n.Value, chain)
		shadowed := p.lets[n.Name]
		p.lets[n.Name] = true
		p.walk(n.Expr, chain)
		p.lets[n.Name] = shadowed

	case *ast.PredicateNode:
		p.walk(n.Node, chain)
	case *ast.UnaryNode:
		p.walk(n.Node, chain)
	case *ast.BinaryNode:
		p.walk(n.Left, chain)
		p.walk(n.Right, chain)
	case *ast.ChainNode:
		p.walk(n.Node, chain)
	case *ast.MemberNode:
		p.walk(n.Node, chain)
		p.walk(n.Property, chain)
	case *ast.SliceNode:
		p.walk(n.Node, chain)
		p.walk(n.From, chain)
		p.walk(n.To, chain)
	case *ast.ConditionalNode:
		p.walk(n.Cond, chain)
		p.walk(n.Exp1, chain)
		p.walk(n.Exp2, chain)
	case *ast.SequenceNode:
		for _, item := range n.Nodes {
			p.walk(item, chain)
		}
	case *ast.ArrayNode:
		for _, item := range n.Nodes {
			p.walk(item, chain)
		}
	case *ast.MapNode:
		for _, pair := range n.Pairs {
			p.walk(pair, chain)
		}
	case *ast.PairNode:
		p.walk(n.Key, chain)
		p.walk(n.Value, chain)
	}
}
