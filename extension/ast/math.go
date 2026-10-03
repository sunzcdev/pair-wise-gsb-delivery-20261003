package ast

import (
	gast "github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/text"
)

// An InlineMath struct represents an inline math expression delimited by
// single dollar signs ($...$).
//
// Value holds the raw content between the delimiters, without any
// decoding or TeX processing. The value may span multiple source lines.
type InlineMath struct {
	gast.BaseInline

	// Value holds the raw content of this math expression.
	Value text.MultiLineValue
}

// Dump implements Node.Dump.
func (n *InlineMath) Dump(_ []byte) *gast.NodeDump {
	return gast.NewNodeDump(n, map[string]any{
		"Value": n.Value,
	})
}

// KindInlineMath is a NodeKind of the InlineMath node.
var KindInlineMath = gast.NewNodeKind("InlineMath")

// Kind implements Node.Kind.
func (n *InlineMath) Kind() gast.NodeKind {
	return KindInlineMath
}

// NewInlineMath returns a new InlineMath node with the given value.
func NewInlineMath(value text.MultiLineValue) *InlineMath {
	n := &InlineMath{Value: value}
	n.Init(n)
	return n
}

// A BlockMath struct represents a display math block delimited by lines of
// double dollar signs ($$).
//
// Value holds the raw body lines exactly as they appeared in the source,
// without indentation stripping, decoding, or TeX processing.
type BlockMath struct {
	gast.BaseBlock

	// Value holds the raw content of this math block.
	Value text.Lines
}

// Dump implements Node.Dump.
func (n *BlockMath) Dump(_ []byte) *gast.NodeDump {
	return gast.NewNodeDump(n, map[string]any{
		"Value": n.Value,
	})
}

// KindBlockMath is a NodeKind of the BlockMath node.
var KindBlockMath = gast.NewNodeKind("BlockMath")

// Kind implements Node.Kind.
func (n *BlockMath) Kind() gast.NodeKind {
	return KindBlockMath
}

// NewBlockMath returns a new BlockMath node with the given value.
func NewBlockMath(value text.Lines) *BlockMath {
	n := &BlockMath{Value: value}
	n.Init(n)
	return n
}
