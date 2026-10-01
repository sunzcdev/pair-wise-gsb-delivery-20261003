// Package variants allows a host program to register closed variant sets
// (sum types) used by match expressions.
//
// A variant set is described by an interface type and a fixed list of
// concrete payload types. Every payload type carries a name which is used in
// patterns, for example:
//
//	type Event interface{ event() }
//
//	type Created struct{ ID string }
//	type Paid struct{ Amount int }
//	type Closed struct{}
//
//	registry := variants.MustRegistry(
//	    variants.New((*Event)(nil)).
//	        Case("Created", Created{}).
//	        Case("Paid", Paid{}).
//	        Case("Closed", Closed{}),
//	)
//
// Values of the variants may be passed to an expression either by value or
// by pointer.
package variants

import (
	"fmt"
	"reflect"
)

// Variant describes a single member of a closed variant set.
type Variant struct {
	// Name is the name used in match patterns.
	Name string

	// Type is the concrete payload type of the variant. It is always a
	// non-pointer type; pointer values are accepted at runtime as well.
	Type reflect.Type
}

// Set is a closed set of variants belonging to a single interface type.
type Set struct {
	iface    reflect.Type
	variants []*Variant
	byName   map[string]*Variant
	byType   map[reflect.Type]*Variant
}

// Interface returns the interface type the set is registered for.
func (s *Set) Interface() reflect.Type { return s.iface }

// Variants returns all registered variants in registration order.
func (s *Set) Variants() []*Variant { return s.variants }

// ByName returns a variant by its pattern name.
func (s *Set) ByName(name string) (*Variant, bool) {
	v, ok := s.byName[name]
	return v, ok
}

// ByType returns a variant by a concrete runtime type. Both the value and
// the pointer payload types are recognized.
func (s *Set) ByType(t reflect.Type) (*Variant, bool) {
	v, ok := s.byType[t]
	return v, ok
}

// Names returns the names of all variants in registration order.
func (s *Set) Names() []string {
	names := make([]string, len(s.variants))
	for i, v := range s.variants {
		names[i] = v.Name
	}
	return names
}

// Builder builds a single variant [Set].
type Builder struct {
	iface reflect.Type
	cases []*Variant
	seen  map[string]struct{}
	types map[reflect.Type]struct{}
}

// New starts a declaration of a variant set for the interface type carried
// by the given example. The argument is typically a nil pointer to the
// interface, like (*Event)(nil).
func New(ifaceExample any) *Builder {
	t := reflect.TypeOf(ifaceExample)
	if t == nil {
		return &Builder{seen: map[string]struct{}{}, types: map[reflect.Type]struct{}{}}
	}
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return &Builder{
		iface: t,
		seen:  map[string]struct{}{},
		types: map[reflect.Type]struct{}{},
	}
}

// Case registers a variant under the given name. The example determines the
// concrete payload type; its value is not retained.
func (b *Builder) Case(name string, example any) *Builder {
	b.cases = append(b.cases, &Variant{Name: name, Type: payloadType(example)})
	return b
}

func payloadType(example any) reflect.Type {
	t := reflect.TypeOf(example)
	if t == nil {
		return nil
	}
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// Build validates the declaration and returns the closed variant set.
func (b *Builder) Build() (*Set, error) {
	if b.iface == nil || b.iface.Kind() != reflect.Interface {
		return nil, fmt.Errorf("variants: variant set must be declared for an interface type")
	}
	if len(b.cases) == 0 {
		return nil, fmt.Errorf("variants: variant set for %v must declare at least one variant", b.iface)
	}

	set := &Set{
		iface:  b.iface,
		byName: map[string]*Variant{},
		byType: map[reflect.Type]*Variant{},
	}

	for _, v := range b.cases {
		if v.Name == "" {
			return nil, fmt.Errorf("variants: variant name cannot be empty (in set for %v)", b.iface)
		}
		if v.Type == nil {
			return nil, fmt.Errorf("variants: variant %q must have a concrete payload type (use a typed nil example)", v.Name)
		}
		if _, ok := set.byName[v.Name]; ok {
			return nil, fmt.Errorf("variants: duplicate variant name %q in set for %v", v.Name, b.iface)
		}
		if _, ok := set.byType[v.Type]; ok {
			return nil, fmt.Errorf("variants: duplicate payload type %v in set for %v", v.Type, b.iface)
		}
		if !v.Type.Implements(set.iface) && !reflect.PtrTo(v.Type).Implements(set.iface) {
			return nil, fmt.Errorf("variants: payload type %v does not implement %v", v.Type, set.iface)
		}
		set.variants = append(set.variants, v)
		set.byName[v.Name] = v
		set.byType[v.Type] = v
		set.byType[reflect.PtrTo(v.Type)] = v
	}

	return set, nil
}

// MustBuild is like [Builder.Build] but panics on error.
func (b *Builder) MustBuild() *Set {
	s, err := b.Build()
	if err != nil {
		panic(err)
	}
	return s
}

// Registry holds all closed variant sets known to a compiler.
type Registry struct {
	sets map[reflect.Type]*Set
}

// NewRegistry creates an empty registry and adds the given sets.
func NewRegistry(sets ...*Set) *Registry {
	r := &Registry{sets: map[reflect.Type]*Set{}}
	for _, s := range sets {
		if err := r.Add(s); err != nil {
			panic(err)
		}
	}
	return r
}

// Add registers a set. Cases are merged with any previously registered set
// for the same interface type.
func (r *Registry) Add(s *Set) error {
	if s == nil {
		return fmt.Errorf("variants: cannot add a nil variant set")
	}
	if existing, ok := r.sets[s.iface]; ok {
		return mergeSets(existing, s)
	}
	r.sets[s.iface] = cloneSet(s)
	return nil
}

// SetFor returns the closed set declared for the given interface type.
func (r *Registry) SetFor(t reflect.Type) (*Set, bool) {
	if r == nil || t == nil {
		return nil, false
	}
	s, ok := r.sets[t]
	return s, ok
}

func cloneSet(s *Set) *Set {
	cp := &Set{
		iface:    s.iface,
		variants: make([]*Variant, len(s.variants)),
		byName:   map[string]*Variant{},
		byType:   map[reflect.Type]*Variant{},
	}
	copy(cp.variants, s.variants)
	for k, v := range s.byName {
		cp.byName[k] = v
	}
	for k, v := range s.byType {
		cp.byType[k] = v
	}
	return cp
}

func mergeSets(existing, extra *Set) error {
	for _, v := range extra.variants {
		if _, ok := existing.byName[v.Name]; ok {
			return fmt.Errorf("variants: duplicate variant name %q in set for %v", v.Name, existing.iface)
		}
		if other, ok := existing.byType[v.Type]; ok {
			return fmt.Errorf("variants: payload type %v is already registered as %q in set for %v", v.Type, other.Name, existing.iface)
		}
		existing.variants = append(existing.variants, v)
		existing.byName[v.Name] = v
		existing.byType[v.Type] = v
		existing.byType[reflect.PtrTo(v.Type)] = v
	}
	return nil
}

// MustRegistry builds a registry from the given set builders, panicking on
// declaration errors.
func MustRegistry(builders ...*Builder) *Registry {
	r := NewRegistry()
	for _, b := range builders {
		r.Add(b.MustBuild())
	}
	return r
}
