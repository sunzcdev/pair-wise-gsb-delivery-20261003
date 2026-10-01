package checker

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/checker/nature"
	"github.com/expr-lang/expr/variants"
)

func (v *Checker) matchNode(node *ast.MatchNode) nature.Nature {
	if len(node.Cases) == 0 {
		return v.error(node, "match expression must have at least one arm")
	}

	scrutinee := v.visit(node.Expr)
	scrutinee = scrutinee.Deref(&v.config.NtCache)

	set := v.variantSet(&scrutinee)

	var remaining map[string]bool
	if set != nil {
		remaining = make(map[string]bool, len(set.Variants()))
		for _, variant := range set.Variants() {
			remaining[variant.Name] = true
		}
	}

	bodyNatures := make([]nature.Nature, 0, len(node.Cases))

	// First pass: rewrite unit variants and fully type check every arm. Type
	// errors (unknown variants/fields, bad guards) are reported before
	// exhaustiveness diagnostics.
	for _, arm := range node.Cases {
		bindings := map[string]bool{}
		bindingNatures := map[string]nature.Nature{}

		v.rewriteUnitVariant(&arm.Pattern, &scrutinee, set)
		v.checkPattern(arm.Pattern, &scrutinee, set, bindings, bindingNatures)

		names := make([]string, 0, len(bindings))
		for name := range bindings {
			names = append(names, name)
			v.varScopes = append(v.varScopes, varScope{name: name, nature: bindingNatures[name]})
		}

		if arm.Guard != nil {
			guardNature := v.visit(arm.Guard)
			guardNature = guardNature.Deref(&v.config.NtCache)
			if !guardNature.IsBool() && !guardNature.IsUnknown(&v.config.NtCache) {
				v.error(arm.Guard, "match arm guard must be boolean (got %v)", guardNature.String())
			}
		}

		bodyNatures = append(bodyNatures, v.visit(arm.Body))

		v.varScopes = v.varScopes[:len(v.varScopes)-len(names)]
	}

	// Second pass: exhaustiveness and reachability. A guarded arm never
	// covers a variant unconditionally.
	if set != nil {
		nilCovered := false
		for _, arm := range node.Cases {
			if arm.Guard != nil {
				continue
			}
			covered, coversNil := patternCoverage(arm.Pattern, set, &scrutinee, v)

			if coversNil && nilCovered {
				v.error(arm, "unreachable match arm")
			}

			switch {
			case covered == nil:
				nilCovered = true
				if len(remaining) == 0 {
					v.error(arm, "unreachable match arm")
				}
				remaining = map[string]bool{}

			case len(covered) > 0:
				unreachable := true
				for name := range covered {
					if remaining[name] {
						unreachable = false
						delete(remaining, name)
					}
				}
				if coversNil {
					nilCovered = true
				}
				if unreachable && !coversNil {
					v.error(arm, "unreachable match arm")
				}

			default:
				if !coversNil {
					v.error(arm, "unreachable match arm")
				}
				nilCovered = true
			}
		}
	}

	if set != nil && len(remaining) > 0 {
		missing := make([]string, 0, len(remaining))
		for name := range remaining {
			missing = append(missing, name)
		}
		sort.Slice(missing, func(i, j int) bool {
			return variantOrder(set, missing[i]) < variantOrder(set, missing[j])
		})
		return v.error(node, "match is not exhaustive: missing variant(s) %v", quoteList(missing))
	}

	return unifyBodies(bodyNatures, &v.config.NtCache)
}

func variantOrder(set *variants.Set, name string) int {
	for i, variant := range set.Variants() {
		if variant.Name == name {
			return i
		}
	}
	return len(set.Variants())
}

func quoteList(items []string) []string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = fmt.Sprintf("%q", item)
	}
	return quoted
}

// variantSet returns the closed variant set registered for the scrutinee
// type. It returns nil for unregistered types; such scrutinees accept only
// wildcard/binding/nil/literal patterns.
func (v *Checker) variantSet(scrutinee *nature.Nature) *variants.Set {
	if v.config.Variants == nil || scrutinee.Type == nil {
		return nil
	}
	if scrutinee.Kind != reflect.Interface {
		return nil
	}
	set, ok := v.config.Variants.SetFor(scrutinee.Type)
	if !ok {
		return nil
	}
	return set
}

// rewriteUnitVariant replaces a bare identifier naming a unit variant with a
// field-less variant pattern. Variant names take precedence over bindings.
// The source location is preserved.
func (v *Checker) rewriteUnitVariant(p *ast.PatternNode, expected *nature.Nature, set *variants.Set) {
	if set == nil || expected.Kind != reflect.Interface {
		return
	}
	switch pattern := (*p).(type) {
	case *ast.BindingPatternNode:
		variant, ok := set.ByName(pattern.Name)
		if !ok {
			return
		}
		rewritten := &ast.VariantPatternNode{Name: variant.Name}
		rewritten.SetLocation(pattern.Location())
		rewritten.SetType(variant.Type)
		*p = rewritten

	case *ast.VariantPatternNode:
		variant, ok := set.ByName(pattern.Name)
		if !ok {
			return
		}
		payload := v.config.NtCache.FromType(variant.Type)
		for _, field := range pattern.Fields {
			if fieldNature, found := payload.FieldByName(&v.config.NtCache, field.Name); found {
				childSet := v.variantSet(&fieldNature)
				v.rewriteUnitVariant(&field.Pattern, &fieldNature, childSet)
			}
		}
	}
}

// checkPattern type checks a pattern against an expected nature, records the
// names bound by it and infers their natures.
func (v *Checker) checkPattern(
	p ast.PatternNode,
	expected *nature.Nature,
	set *variants.Set,
	bindings map[string]bool,
	bindingNatures map[string]nature.Nature,
) nature.Nature {
	switch p := p.(type) {
	case *ast.WildcardPatternNode:
		return *expected

	case *ast.BindingPatternNode:
		if bindings[p.Name] {
			v.error(p, "variable %q is bound more than once in the same pattern", p.Name)
		}
		bindings[p.Name] = true
		bindingNatures[p.Name] = *expected
		return *expected

	case *ast.NilPatternNode:
		if !nillable(*expected) && !expected.IsUnknown(&v.config.NtCache) {
			v.error(p, "nil pattern cannot match value of type %v", expected.String())
		}
		return v.config.NtCache.NatureOf(nil)

	case *ast.LiteralPatternNode:
		lit := v.visit(p.Value)
		if set != nil && expected.Kind == reflect.Interface {
			v.error(p, "literal pattern cannot match closed variant set; name a variant instead")
		}
		if !lit.IsUnknown(&v.config.NtCache) && !expected.IsUnknown(&v.config.NtCache) {
			if !lit.AssignableTo(*expected) && !expected.AssignableTo(lit) {
				v.error(p, "pattern of type %v cannot match value of type %v", lit.String(), expected.String())
			}
		}
		return lit

	case *ast.VariantPatternNode:
		variant, resolved := v.resolveVariant(p, expected, set)
		if !resolved {
			return nature.Nature{}
		}
		p.SetType(variant.Type)

		payload := v.config.NtCache.FromType(variant.Type)
		seenFields := map[string]bool{}
		for _, field := range p.Fields {
			if seenFields[field.Name] {
				v.error(field, "field %q is listed more than once in pattern %q", field.Name, variant.Name)
			}
			seenFields[field.Name] = true

			fieldNature, ok := payload.FieldByName(&v.config.NtCache, field.Name)
			if !ok {
				v.error(field, "variant %q has no field %q", variant.Name, field.Name)
				continue
			}
			if index, ok := payload.FieldIndex(&v.config.NtCache, field.Name); ok {
				field.Index = index
			}
			fieldSet := v.variantSet(&fieldNature)
			v.checkPattern(field.Pattern, &fieldNature, fieldSet, bindings, bindingNatures)
		}
		return payload
	}

	return nature.Nature{}
}

func (v *Checker) resolveVariant(p *ast.VariantPatternNode, expected *nature.Nature, set *variants.Set) (*variants.Variant, bool) {
	if set == nil || expected.Kind != reflect.Interface {
		v.error(p, "variant pattern %q cannot match value of type %v", p.Name, expected.String())
		return nil, false
	}
	variant, ok := set.ByName(p.Name)
	if !ok {
		v.error(p, "unknown variant %q for type %v", p.Name, expected.String())
		return nil, false
	}
	return variant, true
}

func nillable(nt nature.Nature) bool {
	switch nt.Kind {
	case reflect.Interface, reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return true
	}
	return false
}

// patternCoverage returns the set of variant names unconditionally covered by
// a pattern together with whether the pattern covers the nil value. A nil set
// means the pattern is irrefutable (matches every variant).
func patternCoverage(p ast.PatternNode, set *variants.Set, scrutinee *nature.Nature, v *Checker) (map[string]bool, bool) {
	switch p := p.(type) {
	case *ast.WildcardPatternNode:
		return nil, true
	case *ast.BindingPatternNode:
		if variant := unitVariant(set, scrutinee, p.Name); variant != nil {
			return map[string]bool{variant.Name: true}, false
		}
		return nil, true
	case *ast.NilPatternNode:
		return map[string]bool{}, true
	case *ast.LiteralPatternNode:
		return map[string]bool{}, false
	case *ast.VariantPatternNode:
		variant, ok := set.ByName(p.Name)
		if !ok {
			return map[string]bool{}, false
		}
		payload := v.config.NtCache.FromType(variant.Type)
		for _, field := range p.Fields {
			fieldNature, ok := payload.FieldByName(&v.config.NtCache, field.Name)
			if !ok {
				continue
			}
			fieldSet := v.variantSet(&fieldNature)
			if !irrefutablePattern(field.Pattern, fieldSet, &fieldNature, v) {
				return map[string]bool{}, false
			}
		}
		return map[string]bool{variant.Name: true}, false
	}
	return map[string]bool{}, false
}

func irrefutablePattern(p ast.PatternNode, set *variants.Set, nt *nature.Nature, v *Checker) bool {
	switch p := p.(type) {
	case *ast.WildcardPatternNode:
		return true
	case *ast.BindingPatternNode:
		return unitVariant(set, nt, p.Name) == nil
	case *ast.VariantPatternNode:
		if set == nil {
			return false
		}
		variant, ok := set.ByName(p.Name)
		if !ok {
			return false
		}
		payload := v.config.NtCache.FromType(variant.Type)
		for _, field := range p.Fields {
			fieldNature, ok := payload.FieldByName(&v.config.NtCache, field.Name)
			if !ok {
				return false
			}
			childSet := v.variantSet(&fieldNature)
			if !irrefutablePattern(field.Pattern, childSet, &fieldNature, v) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func unitVariant(set *variants.Set, nt *nature.Nature, name string) *variants.Variant {
	if set == nil || nt == nil || nt.Kind != reflect.Interface {
		return nil
	}
	variant, ok := set.ByName(name)
	if !ok {
		return nil
	}
	return variant
}

func unifyBodies(bodies []nature.Nature, cache *nature.Cache) nature.Nature {
	if len(bodies) == 0 {
		return nature.Nature{}
	}
	result := bodies[0]
	for i := 1; i < len(bodies); i++ {
		t2 := bodies[i]
		switch {
		case result.IsUnknown(cache) || t2.IsUnknown(cache):
			result = nature.Nature{}
		case result.Nil && !t2.Nil:
			result = t2
		case !result.Nil && t2.Nil:
			// keep result
		case result.Nil && t2.Nil:
			result = cache.NatureOf(nil)
		case sameType(result, t2):
			// keep result
		case t2.AssignableTo(result) || result.AssignableTo(t2):
			// Mixed concrete types: treat as unknown at runtime.
			result = nature.Nature{}
		default:
			result = nature.Nature{}
		}
	}
	return result
}

func sameType(a, b nature.Nature) bool {
	return a.Type != nil && b.Type != nil && a.Type == b.Type
}
