package ast

import (
	"unicode/utf8"

	gast "github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

var _ gast.InlineNode = (*InlineMath)(nil)

// InlineMath represents inline math surrounded by single dollar signs.
type InlineMath struct {
	gast.BaseInline

	// Value holds the formula without its surrounding dollar signs.
	Value text.MultiLineValue
}

// Dump implements gast.Node.Dump.
func (n *InlineMath) Dump(_ []byte) *gast.NodeDump {
	return gast.NewNodeDump(n, map[string]any{
		"Value": n.Value,
	})
}

// FirstRune implements gast.InlineNode.FirstRune.
func (n *InlineMath) FirstRune(source []byte) (rune, bool) {
	value := n.Value.Bytes(source)
	if len(value) == 0 {
		return 0, false
	}
	return util.ToRune(value, 0), true
}

// LastRune implements gast.InlineNode.LastRune.
func (n *InlineMath) LastRune(source []byte) (rune, bool) {
	value := n.Value.Bytes(source)
	if len(value) == 0 {
		return 0, false
	}
	i := len(value) - 1
	for i > 0 && !utf8.RuneStart(value[i]) {
		i--
	}
	return util.ToRune(value, i), true
}

// KindInlineMath is a NodeKind of the InlineMath node.
var KindInlineMath = gast.NewNodeKind("InlineMath")

// Kind implements gast.Node.Kind.
func (n *InlineMath) Kind() gast.NodeKind {
	return KindInlineMath
}

// NewInlineMath returns a new InlineMath node.
func NewInlineMath(value text.MultiLineValue) *InlineMath {
	n := &InlineMath{Value: value}
	n.Init(n)
	return n
}

var _ gast.BlockNode = (*MathBlock)(nil)

// MathBlock represents display math surrounded by dollar-fence lines.
type MathBlock struct {
	gast.BaseBlock

	// Value holds the formula without its dollar-fence lines.
	Value text.Lines
}

// Dump implements gast.Node.Dump.
func (n *MathBlock) Dump(_ []byte) *gast.NodeDump {
	return gast.NewNodeDump(n, map[string]any{
		"Value": n.Value,
	})
}

// KindMathBlock is a NodeKind of the MathBlock node.
var KindMathBlock = gast.NewNodeKind("MathBlock")

// Kind implements gast.Node.Kind.
func (n *MathBlock) Kind() gast.NodeKind {
	return KindMathBlock
}

// NewMathBlock returns a new MathBlock node.
func NewMathBlock(value text.Lines) *MathBlock {
	n := &MathBlock{Value: value}
	n.Init(n)
	return n
}
