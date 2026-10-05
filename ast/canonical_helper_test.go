package ast_test

import (
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/text"
)

var customCanonicalKind = ast.NewNodeKind("CustomCanonical")

// customCanonicalNode is an extension-style node used to verify that nodes
// without a built-in representation require an explicit adapter.
type customCanonicalNode struct {
	ast.BaseBlock
	marker string
	count  int
}

func newCustomCanonicalNode(marker string, count int) *customCanonicalNode {
	n := &customCanonicalNode{marker: marker, count: count}
	n.Init(n)
	return n
}

func (n *customCanonicalNode) Kind() ast.NodeKind { return customCanonicalKind }
func (n *customCanonicalNode) Dump(_ []byte) *ast.NodeDump {
	return ast.NewNodeDump(n, map[string]any{"Marker": n.marker, "Count": n.count})
}

var (
	_ ast.Node      = (*customCanonicalNode)(nil)
	_ ast.BlockNode = (*customCanonicalNode)(nil)
)

func mustOwnedValue(s string) text.MultiLineValue {
	return text.NewMultiLineValueFromString(s, text.IdentityDecoder)
}
