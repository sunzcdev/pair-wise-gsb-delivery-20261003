package batch

import (
	"fmt"

	"github.com/expr-lang/expr/ast"
)

// nonscalarBuiltins are the collection/predicate builtins which operate on
// arrays and are not covered by the scalar batch evaluator.
var nonscalarBuiltins = map[string]bool{
	"all":           true,
	"none":          true,
	"any":           true,
	"one":           true,
	"filter":        true,
	"map":           true,
	"count":         true,
	"sum":           true,
	"find":          true,
	"findIndex":     true,
	"findLast":      true,
	"findLastIndex": true,
	"groupBy":       true,
	"sortBy":        true,
	"reduce":        true,
}

// unsupported returns a non empty reason if the tree contains a node which
// the columnar evaluator cannot handle.
func unsupported(root ast.Node) string {
	s := &supportScanner{}
	ast.Walk(&root, s)
	return s.reason
}

type supportScanner struct {
	reason string
}

func (s *supportScanner) Visit(node *ast.Node) {
	if s.reason != "" || node == nil {
		return
	}
	switch n := (*node).(type) {
	case *ast.PredicateNode, *ast.PointerNode, *ast.SliceNode:
		s.reason = fmt.Sprintf("unsupported node %T", *node)
	case *ast.BuiltinNode:
		if nonscalarBuiltins[n.Name] {
			s.reason = fmt.Sprintf("unsupported builtin %q", n.Name)
		}
	}
}
