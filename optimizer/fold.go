package optimizer

import (
	"math"

	. "github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/decimal"
	"github.com/expr-lang/expr/file"
)

type fold struct {
	applied  bool
	err      *file.Error
	settings decimal.Settings
}

func (fold *fold) Visit(node *Node) {
	patch := func(newNode Node) {
		fold.applied = true
		patchWithType(node, newNode)
	}
	patchCopy := func(newNode Node) {
		fold.applied = true
		patchCopyType(node, newNode)
	}

	switch n := (*node).(type) {
	case *UnaryNode:
		switch n.Operator {
		case "-":
			if i, ok := n.Node.(*IntegerNode); ok {
				patch(&IntegerNode{Value: -i.Value})
			}
			if i, ok := n.Node.(*FloatNode); ok {
				patch(&FloatNode{Value: -i.Value})
			}
			if i, ok := n.Node.(*DecimalNode); ok {
				patch(&DecimalNode{Value: i.Value.Neg()})
			}
		case "+":
			if i, ok := n.Node.(*IntegerNode); ok {
				patch(&IntegerNode{Value: i.Value})
			}
			if i, ok := n.Node.(*FloatNode); ok {
				patch(&FloatNode{Value: i.Value})
			}
			if i, ok := n.Node.(*DecimalNode); ok {
				patch(&DecimalNode{Value: i.Value.Clone()})
			}
		case "!", "not":
			if a := toBool(n.Node); a != nil {
				patch(&BoolNode{Value: !a.Value})
			}
		}

	case *BinaryNode:
		switch n.Operator {
		case "<", ">", "<=", ">=":
			if a, b, ok := foldDecimalOperands(n.Left, n.Right); ok {
				cmp, err := decimal.CompareWithSettings(a, b, fold.settings)
				if err != nil {
					fold.err = &file.Error{Location: (*node).Location(), Message: err.Error()}
					return
				}
				value := false
				switch n.Operator {
				case "<":
					value = cmp < 0
				case ">":
					value = cmp > 0
				case "<=":
					value = cmp <= 0
				case ">=":
					value = cmp >= 0
				case "==":
					value = cmp == 0
				case "!=":
					value = cmp != 0
				}
				patch(&BoolNode{Value: value})
			}
		case "+":
			if a, b, ok := foldDecimalOperands(n.Left, n.Right); ok {
				value, err := decimal.Add(a, b, fold.settings)
				if foldDecimalError(node, fold, err) {
					return
				}
				patch(&DecimalNode{Value: value})
			}
			{
				a := toInteger(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&IntegerNode{Value: a.Value + b.Value})
				}
			}
			{
				a := toInteger(n.Left)
				b := toFloat(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: float64(a.Value) + b.Value})
				}
			}
			{
				a := toFloat(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: a.Value + float64(b.Value)})
				}
			}
			{
				a := toFloat(n.Left)
				b := toFloat(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: a.Value + b.Value})
				}
			}
			{
				a := toString(n.Left)
				b := toString(n.Right)
				if a != nil && b != nil {
					patch(&StringNode{Value: a.Value + b.Value})
				}
			}
		case "-":
			if a, b, ok := foldDecimalOperands(n.Left, n.Right); ok {
				value, err := decimal.Subtract(a, b, fold.settings)
				if foldDecimalError(node, fold, err) {
					return
				}
				patch(&DecimalNode{Value: value})
			}
			{
				a := toInteger(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&IntegerNode{Value: a.Value - b.Value})
				}
			}
			{
				a := toInteger(n.Left)
				b := toFloat(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: float64(a.Value) - b.Value})
				}
			}
			{
				a := toFloat(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: a.Value - float64(b.Value)})
				}
			}
			{
				a := toFloat(n.Left)
				b := toFloat(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: a.Value - b.Value})
				}
			}
		case "*":
			if a, b, ok := foldDecimalOperands(n.Left, n.Right); ok {
				value, err := decimal.Multiply(a, b, fold.settings)
				if foldDecimalError(node, fold, err) {
					return
				}
				patch(&DecimalNode{Value: value})
			}
			{
				a := toInteger(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&IntegerNode{Value: a.Value * b.Value})
				}
			}
			{
				a := toInteger(n.Left)
				b := toFloat(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: float64(a.Value) * b.Value})
				}
			}
			{
				a := toFloat(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: a.Value * float64(b.Value)})
				}
			}
			{
				a := toFloat(n.Left)
				b := toFloat(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: a.Value * b.Value})
				}
			}
		case "/":
			if a, b, ok := foldDecimalOperands(n.Left, n.Right); ok {
				value, err := decimal.Divide(a, b, fold.settings)
				if foldDecimalError(node, fold, err) {
					return
				}
				patch(&DecimalNode{Value: value})
			}
			{
				a := toInteger(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: float64(a.Value) / float64(b.Value)})
				}
			}
			{
				a := toInteger(n.Left)
				b := toFloat(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: float64(a.Value) / b.Value})
				}
			}
			{
				a := toFloat(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: a.Value / float64(b.Value)})
				}
			}
			{
				a := toFloat(n.Left)
				b := toFloat(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: a.Value / b.Value})
				}
			}
		case "%":
			if a, ok := n.Left.(*IntegerNode); ok {
				if b, ok := n.Right.(*IntegerNode); ok {
					if b.Value == 0 {
						fold.err = &file.Error{
							Location: (*node).Location(),
							Message:  "integer divide by zero",
						}
						return
					}
					patch(&IntegerNode{Value: a.Value % b.Value})
				}
			}
		case "**", "^":
			{
				a := toInteger(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: math.Pow(float64(a.Value), float64(b.Value))})
				}
			}
			{
				a := toInteger(n.Left)
				b := toFloat(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: math.Pow(float64(a.Value), b.Value)})
				}
			}
			{
				a := toFloat(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: math.Pow(a.Value, float64(b.Value))})
				}
			}
			{
				a := toFloat(n.Left)
				b := toFloat(n.Right)
				if a != nil && b != nil {
					patch(&FloatNode{Value: math.Pow(a.Value, b.Value)})
				}
			}
		case "and", "&&":
			a := toBool(n.Left)
			b := toBool(n.Right)

			if a != nil && a.Value { // true and x
				patchCopy(n.Right)
			} else if b != nil && b.Value { // x and true
				patchCopy(n.Left)
			} else if (a != nil && !a.Value) || (b != nil && !b.Value) { // "x and false" or "false and x"
				patch(&BoolNode{Value: false})
			}
		case "or", "||":
			a := toBool(n.Left)
			b := toBool(n.Right)

			if a != nil && !a.Value { // false or x
				patchCopy(n.Right)
			} else if b != nil && !b.Value { // x or false
				patchCopy(n.Left)
			} else if (a != nil && a.Value) || (b != nil && b.Value) { // "x or true" or "true or x"
				patch(&BoolNode{Value: true})
			}
		case "==":
			if a, b, ok := foldDecimalOperands(n.Left, n.Right); ok {
				equal, err := decimal.EqualWithSettings(a, b, fold.settings)
				if foldDecimalError(node, fold, err) {
					return
				}
				patch(&BoolNode{Value: equal})
			}
			{
				a := toInteger(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&BoolNode{Value: a.Value == b.Value})
				}
			}
			{
				a := toString(n.Left)
				b := toString(n.Right)
				if a != nil && b != nil {
					patch(&BoolNode{Value: a.Value == b.Value})
				}
			}
			{
				a := toBool(n.Left)
				b := toBool(n.Right)
				if a != nil && b != nil {
					patch(&BoolNode{Value: a.Value == b.Value})
				}
			}
		case "!=":
			if a, b, ok := foldDecimalOperands(n.Left, n.Right); ok {
				equal, err := decimal.EqualWithSettings(a, b, fold.settings)
				if foldDecimalError(node, fold, err) {
					return
				}
				patch(&BoolNode{Value: !equal})
			}
			{
				a := toInteger(n.Left)
				b := toInteger(n.Right)
				if a != nil && b != nil {
					patch(&BoolNode{Value: a.Value != b.Value})
				}
			}
			{
				a := toString(n.Left)
				b := toString(n.Right)
				if a != nil && b != nil {
					patch(&BoolNode{Value: a.Value != b.Value})
				}
			}
			{
				a := toBool(n.Left)
				b := toBool(n.Right)
				if a != nil && b != nil {
					patch(&BoolNode{Value: a.Value != b.Value})
				}
			}
		}

	case *ArrayNode:
		if len(n.Nodes) > 0 {
			for _, a := range n.Nodes {
				switch a.(type) {
				case *IntegerNode, *FloatNode, *DecimalNode, *StringNode, *BoolNode:
					continue
				default:
					return
				}
			}
			value := make([]any, len(n.Nodes))
			for i, a := range n.Nodes {
				switch b := a.(type) {
				case *IntegerNode:
					value[i] = b.Value
				case *FloatNode:
					value[i] = b.Value
				case *DecimalNode:
					value[i] = b.Value.Clone()
				case *StringNode:
					value[i] = b.Value
				case *BoolNode:
					value[i] = b.Value
				}
			}
			patch(&ConstantNode{Value: value})
		}

	case *BuiltinNode:
		// TODO: Move this to a separate visitor filter_filter.go
		switch n.Name {
		case "filter":
			if len(n.Arguments) != 2 {
				return
			}
			if base, ok := n.Arguments[0].(*BuiltinNode); ok && base.Name == "filter" {
				patchCopy(&BuiltinNode{
					Name: "filter",
					Arguments: []Node{
						base.Arguments[0],
						&PredicateNode{
							Node: &BinaryNode{
								Operator: "&&",
								Left:     base.Arguments[1].(*PredicateNode).Node,
								Right:    n.Arguments[1].(*PredicateNode).Node,
							},
						},
					},
				})
			}
		}
	}
}

func toString(n Node) *StringNode {
	switch a := n.(type) {
	case *StringNode:
		return a
	}
	return nil
}

func toInteger(n Node) *IntegerNode {
	switch a := n.(type) {
	case *IntegerNode:
		return a
	}
	return nil
}

func toFloat(n Node) *FloatNode {
	switch a := n.(type) {
	case *FloatNode:
		return a
	}
	return nil
}

func foldDecimalOperands(left, right Node) (any, any, bool) {
	if _, ok := left.(*DecimalNode); !ok {
		if _, ok := right.(*DecimalNode); !ok {
			return nil, nil, false
		}
	}
	a, ok := foldDecimalValue(left)
	if !ok {
		return nil, nil, false
	}
	b, ok := foldDecimalValue(right)
	if !ok {
		return nil, nil, false
	}
	return a, b, true
}

func foldDecimalValue(node Node) (any, bool) {
	switch n := node.(type) {
	case *DecimalNode:
		return n.Value.Clone(), true
	case *IntegerNode:
		return n.Value, true
	default:
		return nil, false
	}
}

func foldDecimalError(node *Node, fold *fold, err error) bool {
	if err == nil {
		return false
	}
	fold.err = &file.Error{Location: (*node).Location(), Message: err.Error()}
	return true
}

func toBool(n Node) *BoolNode {
	switch a := n.(type) {
	case *BoolNode:
		return a
	}
	return nil
}
