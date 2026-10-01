package conf

import (
	"fmt"
	"reflect"
	"strings"
)

// Variant describes a single variant of a closed variant set registered
// for match expressions. Name is the name used in match patterns and Type
// is the concrete Go type of the variant.
type Variant struct {
	Name string
	Type reflect.Type
}

// VariantSet is a closed set of variants registered by the host for a base
// type (usually an interface type). The checker uses the set to verify
// exhaustiveness of match expressions and to resolve variant patterns.
type VariantSet struct {
	Type     reflect.Type // Base type the variants belong to.
	Variants []Variant    // Variants in registration order.
	byName   map[string]int
}

// AddVariant adds a variant to the set. It panics on invalid names and
// duplicate names or types.
func (s *VariantSet) AddVariant(v Variant) {
	if v.Name == "" {
		panic("expr: variant name must not be empty")
	}
	if v.Type == nil {
		panic(fmt.Sprintf("expr: variant %s must have a non-nil type", v.Name))
	}
	if s.byName == nil {
		s.byName = make(map[string]int)
	}
	if _, ok := s.byName[v.Name]; ok {
		panic(fmt.Sprintf("expr: duplicate variant name %s", v.Name))
	}
	for _, existing := range s.Variants {
		if existing.Type == v.Type {
			panic(fmt.Sprintf("expr: duplicate variant type %v", v.Type))
		}
	}
	s.byName[v.Name] = len(s.Variants)
	s.Variants = append(s.Variants, v)
}

// Variant returns the variant with the given name.
func (s *VariantSet) Variant(name string) (Variant, bool) {
	if i, ok := s.byName[name]; ok {
		return s.Variants[i], true
	}
	return Variant{}, false
}

// Names returns the names of all variants in registration order.
func (s *VariantSet) Names() []string {
	names := make([]string, len(s.Variants))
	for i, v := range s.Variants {
		names[i] = v.Name
	}
	return names
}

// String returns a human-readable list of variant names.
func (s *VariantSet) String() string {
	return strings.Join(s.Names(), ", ")
}
