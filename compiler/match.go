package compiler

import (
	"reflect"

	"github.com/expr-lang/expr/ast"
	. "github.com/expr-lang/expr/vm"
	"github.com/expr-lang/expr/vm/runtime"
)

func (c *compiler) MatchNode(node *ast.MatchNode) {
	// One variable slot per distinct binding name. Slots are shared across
	// arms, but scopes ensure a name resolves only in the arm which bound it.
	slots := map[string]int{}
	for _, arm := range node.Cases {
		collectPatternBindings(arm.Pattern, slots, c)
	}

	c.compile(node.Expr)
	c.derefInNeeded(node.Expr)

	var matchEnds []int

	for _, arm := range node.Cases {
		failures := c.compilePattern(arm.Pattern)

		// Store bindings before evaluating the guard: guards may use them.
		c.storeBindings(arm.Pattern, slots)
		bindingNames := c.pushBindingScopes(arm.Pattern, slots)

		if arm.Guard != nil {
			c.compile(arm.Guard)
			c.derefInNeeded(arm.Guard)
			guardFalse := c.emit(OpJumpIfFalse, placeholder)
			c.emit(OpPop) // discard "true" on the guard-success path
			guardOk := c.emit(OpJump, placeholder)
			c.patchJump(guardFalse)
			c.emit(OpPop) // discard "false" on the guard-failure path
			guardFail := c.emit(OpJump, placeholder)
			c.patchJump(guardOk)
			failures = append(failures, guardFail)
		}

		// Selected: the scrutinee is on top of the stack and is no longer
		// needed; discard it so only the body result remains.
		c.emit(OpPop)

		// Evaluate the body with arm bindings in scope.
		c.compile(arm.Body)
		c.popBindingScopes(bindingNames)

		end := c.emit(OpJump, placeholder)
		matchEnds = append(matchEnds, end)

		// Arm failure target: the stack holds only the scrutinee value.
		for _, ph := range failures {
			c.patchJump(ph)
		}
	}

	// No arm matched (the scrutinee is still on the stack).
	c.emit(OpPop)
	c.emit(OpFailMatch, c.addConstant("no match arm matched"))

	for _, ph := range matchEnds {
		c.patchJump(ph)
	}
}

// compilePattern emits a test for the value on top of the stack. On the fall
// through path the value remains on the stack and the pattern matched. Each
// returned placeholder is a jump to be patched to the failure continuation;
// at that target the stack holds only the same value.
func (c *compiler) compilePattern(p ast.PatternNode) []int {
	switch p := p.(type) {
	case *ast.WildcardPatternNode, *ast.BindingPatternNode:
		return nil

	case *ast.NilPatternNode:
		notNil := c.emit(OpJumpIfNotNil, placeholder)
		return []int{notNil}

	case *ast.LiteralPatternNode:
		c.emit(OpDup) // keep the input on the stack for both paths
		c.compile(p.Value)
		c.emit(OpEqual)
		equalResult := c.emit(OpJumpIfFalse, placeholder)
		// Success: equal left true on top; pop it to leave the value.
		c.emit(OpPop) // discard "true"
		success := c.emit(OpJump, placeholder)
		// Failure: OpEqual already consumed the input and literal; pop the
		// "false" so the duplicated input remains on the stack.
		c.patchJump(equalResult)
		c.emit(OpPop) // discard "false"
		bail := c.emit(OpJump, placeholder)
		c.patchJump(success)
		return []int{bail}

	case *ast.VariantPatternNode:
		payload := p.Type()
		info := &runtime.VariantInfo{
			Type:    payload,
			PtrType: reflect.PtrTo(payload),
			Name:    p.Name,
		}
		c.emit(OpDup)
		c.emit(OpVariant, c.addConstant(info))
		typeMismatchCond := c.emit(OpJumpIfFalse, placeholder)
		c.emit(OpPop) // discard "true"

		var failures []int

		// Destructure fields. Each child test runs on a fresh duplicate so the
		// parent value survives for later fields.
		for _, field := range p.Fields {
			c.emit(OpDup)
			c.emit(OpFetchField, c.addConstant(&runtime.Field{
				Index: field.Index,
				Path:  []string{field.Name},
			}))
			childFailures := c.compilePattern(field.Pattern)

			// Success: child value was left on top; discard it.
			fieldDone := c.emit(OpJump, placeholder)

			// Failure: child failures land with the field value on top.
			for _, ph := range childFailures {
				c.patchJump(ph)
			}
			c.emit(OpPop) // discard field value, parent value is below
			fieldFail := c.emit(OpJump, placeholder)
			failures = append(failures, fieldFail)

			c.patchJump(fieldDone)
			c.emit(OpPop) // discard field value on the success path
		}

		success := c.emit(OpJump, placeholder)
		c.patchJump(typeMismatchCond)
		c.emit(OpPop) // discard "false" on the failure path
		typeBail := c.emit(OpJump, placeholder)
		c.patchJump(success)
		return append(failures, typeBail)
	}

	return nil
}

// storeBindings emits stores for identifier patterns. Nested fields are read
// from duplicated parent values so the matched value is not consumed.
func (c *compiler) storeBindings(p ast.PatternNode, slots map[string]int) {
	switch p := p.(type) {
	case *ast.WildcardPatternNode, *ast.NilPatternNode, *ast.LiteralPatternNode:
		// nothing to bind
	case *ast.BindingPatternNode:
		c.emit(OpDup)
		c.emit(OpStore, slots[p.Name])
	case *ast.VariantPatternNode:
		for _, field := range p.Fields {
			if !hasBinding(field.Pattern) {
				continue
			}
			c.emit(OpDup)
			c.emit(OpFetchField, c.addConstant(&runtime.Field{
				Index: field.Index,
				Path:  []string{field.Name},
			}))
			c.storeBindings(field.Pattern, slots)
			c.emit(OpPop) // discard field value
		}
	}
}

func (c *compiler) pushBindingScopes(p ast.PatternNode, slots map[string]int) []string {
	var names []string
	walkBindingNames(p, func(name string) {
		c.beginScope(name, slots[name])
		names = append(names, name)
	})
	return names
}

func (c *compiler) popBindingScopes(names []string) {
	for range names {
		c.endScope()
	}
}

func collectPatternBindings(p ast.PatternNode, slots map[string]int, c *compiler) {
	walkBindingNames(p, func(name string) {
		if _, ok := slots[name]; !ok {
			slots[name] = c.addVariable(name)
		}
	})
}

func hasBinding(p ast.PatternNode) bool {
	found := false
	walkBindingNames(p, func(string) { found = true })
	return found
}

func walkBindingNames(p ast.PatternNode, fn func(string)) {
	switch p := p.(type) {
	case *ast.BindingPatternNode:
		fn(p.Name)
	case *ast.VariantPatternNode:
		for _, field := range p.Fields {
			walkBindingNames(field.Pattern, fn)
		}
	}
}
