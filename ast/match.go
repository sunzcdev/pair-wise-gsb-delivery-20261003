package ast

import (
	"fmt"
	"strings"
)

// PatternNode is a Node which can appear only as a pattern of a match arm.
type PatternNode interface {
	Node
	patternNode()
}

// MatchNode represents a match expression:
//
//	match expr {
//	    Variant{field: x} -> x;
//	    _                 -> 0;
//	}
type MatchNode struct {
	base
	Expr  Node // Scrutinee expression.
	Cases []*MatchCase
}

// MatchCase is a single arm of a match expression.
type MatchCase struct {
	base
	Pattern PatternNode // Pattern matched against the scrutinee.
	Guard   Node        // Optional boolean expression.
	Body    Node        // Expression evaluated when the arm is selected.
}

// WildcardPatternNode represents the "_" pattern.
type WildcardPatternNode struct {
	base
}

// BindingPatternNode represents an identifier pattern ("x") which binds the
// matched value to the given variable inside the guard and the body.
type BindingPatternNode struct {
	base
	Name string
}

// LiteralPatternNode represents a constant pattern (number, string, bool).
type LiteralPatternNode struct {
	base
	Value Node // IntegerNode, FloatNode, StringNode or BoolNode.
}

// NilPatternNode represents the nil pattern.
type NilPatternNode struct {
	base
}

// VariantPatternNode matches a closed-set variant and optionally destructures
// its fields. Field patterns can be nested.
type VariantPatternNode struct {
	base
	Name   string
	Fields []*FieldPattern
}

// FieldPattern is a single destructured field of a [VariantPatternNode].
type FieldPattern struct {
	base
	Name    string
	Pattern PatternNode

	// Index is populated by the checker and used by the compiler to read the
	// field value directly from the concrete payload.
	Index []int
}

func (f *FieldPattern) String() string {
	return fmt.Sprintf("%s: %s", f.Name, f.Pattern.String())
}

func (*WildcardPatternNode) patternNode() {}
func (*BindingPatternNode) patternNode()  {}
func (*LiteralPatternNode) patternNode()  {}
func (*NilPatternNode) patternNode()      {}
func (*VariantPatternNode) patternNode()  {}

func (n *MatchNode) String() string {
	var b strings.Builder
	b.WriteString("match ")
	b.WriteString(n.Expr.String())
	b.WriteString(" { ")
	for i, c := range n.Cases {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.String())
	}
	b.WriteString(" }")
	return b.String()
}

func (c *MatchCase) String() string {
	var b strings.Builder
	b.WriteString(c.Pattern.String())
	if c.Guard != nil {
		b.WriteString(" if ")
		b.WriteString(c.Guard.String())
	}
	b.WriteString(" -> { ")
	b.WriteString(c.Body.String())
	b.WriteString(" }")
	return b.String()
}

func (*WildcardPatternNode) String() string { return "_" }

func (n *BindingPatternNode) String() string { return n.Name }

func (n *LiteralPatternNode) String() string { return n.Value.String() }

func (*NilPatternNode) String() string { return "nil" }

func (n *VariantPatternNode) String() string {
	if len(n.Fields) == 0 {
		return n.Name
	}
	fields := make([]string, len(n.Fields))
	for i, f := range n.Fields {
		fields[i] = fmt.Sprintf("%s: %s", f.Name, f.Pattern.String())
	}
	return fmt.Sprintf("%s{%s}", n.Name, strings.Join(fields, ", "))
}
