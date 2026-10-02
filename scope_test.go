package gjson

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

const scopeTestJSON = `{"name":"Tom Anderson","tags":["a","b"],"friends":["ALICE","BOB"]}`

func lowerModifier(json, arg string) string {
	return `"` + strings.ToLower(Parse(json).String()) + `"`
}

func upperModifier(json, arg string) string {
	return `"` + strings.ToUpper(Parse(json).String()) + `"`
}

// Two scopes register the same modifier name with different meanings while
// queries are interleaved with registration, replacement, and deletion.
func TestScopeIsolationInterleaved(t *testing.T) {
	a := NewScope(true)
	b := NewScope(true)

	a.AddModifier("normalize", lowerModifier)
	if got := a.Get(scopeTestJSON, "name.@normalize").String(); got != "tom anderson" {
		t.Fatalf("scope A: expected %q, got %q", "tom anderson", got)
	}

	// Registering a different meaning on B must not affect A.
	b.AddModifier("normalize", upperModifier)
	if got := b.Get(scopeTestJSON, "name.@normalize").String(); got != "TOM ANDERSON" {
		t.Fatalf("scope B: expected %q, got %q", "TOM ANDERSON", got)
	}
	if got := a.Get(scopeTestJSON, "name.@normalize").String(); got != "tom anderson" {
		t.Fatalf("scope A after B registered: expected %q, got %q", "tom anderson", got)
	}

	// Replacing B's meaning must not affect A.
	b.AddModifier("normalize", func(json, arg string) string { return `"replaced"` })
	if got := b.Get(scopeTestJSON, "name.@normalize").String(); got != "replaced" {
		t.Fatalf("scope B after replace: expected %q, got %q", "replaced", got)
	}
	if got := a.Get(scopeTestJSON, "name.@normalize").String(); got != "tom anderson" {
		t.Fatalf("scope A after B replaced: expected %q, got %q", "tom anderson", got)
	}

	// Deleting from B must not affect A.
	b.DeleteModifier("normalize")
	if b.ModifierExists("normalize") {
		t.Fatal("scope B: @normalize should be deleted")
	}
	if b.Get(scopeTestJSON, "name.@normalize").Exists() {
		t.Fatal("scope B: @normalize should not resolve after delete")
	}
	if got := a.Get(scopeTestJSON, "name.@normalize").String(); got != "tom anderson" {
		t.Fatalf("scope A after B deleted: expected %q, got %q", "tom anderson", got)
	}

	// Neither scope leaked into the package-level registry.
	if ModifierExists("normalize", nil) {
		t.Fatal("package registry: @normalize should not exist")
	}
	if Get(scopeTestJSON, "name.@normalize").Exists() {
		t.Fatal("package-level Get: @normalize should not resolve")
	}
}

// A scope can opt into the built-in modifier set or start empty.
func TestScopeInheritBuiltins(t *testing.T) {
	with := NewScope(true)
	if !with.ModifierExists("reverse") || !with.ModifierExists("this") {
		t.Fatal("NewScope(true) should inherit built-in modifiers")
	}
	if got := with.Get(scopeTestJSON, "friends.@reverse.0").String(); got != "BOB" {
		t.Fatalf("inherited @reverse: expected %q, got %q", "BOB", got)
	}

	without := NewScope(false)
	if without.ModifierExists("reverse") {
		t.Fatal("NewScope(false) should not inherit built-in modifiers")
	}
	if without.Get(scopeTestJSON, "friends.@reverse.0").Exists() {
		t.Fatal("NewScope(false): @reverse should not resolve")
	}

	// Replacing an inherited modifier only affects that scope.
	with.AddModifier("reverse", func(json, arg string) string { return `"scoped"` })
	if got := with.Get(scopeTestJSON, "friends.@reverse").String(); got != "scoped" {
		t.Fatalf("overridden @reverse: expected %q, got %q", "scoped", got)
	}
	if got := Get(scopeTestJSON, "friends.@reverse.0").String(); got != "BOB" {
		t.Fatalf("package-level @reverse: expected %q, got %q", "BOB", got)
	}
}

// A query pins a snapshot of the registry when it starts; registering a
// modifier while a query is running must not affect that query.
func TestScopeQueryPinsRegistrySnapshot(t *testing.T) {
	s := NewScope(false)
	s.AddModifier("install", func(json, arg string) string {
		// Registered mid-query, while the outer query is in flight.
		s.AddModifier("late", func(json, arg string) string { return `"added"` })
		return json
	})
	if res := s.Get(`{"a":1}`, "@install.@late"); res.Exists() {
		t.Fatalf("in-flight query must not see @late, got %q", res.Raw)
	}
	if got := s.Get(`{"a":1}`, "@late").String(); got != "added" {
		t.Fatalf("subsequent query should see @late, got %q", got)
	}

	// Deleting mid-query must not affect the in-flight query either.
	s.AddModifier("remove", func(json, arg string) string {
		s.DeleteModifier("late")
		return json
	})
	if got := s.Get(`{"a":1}`, "@remove.@late").String(); got != "added" {
		t.Fatalf("in-flight query should still see @late, got %q", got)
	}
	if s.Get(`{"a":1}`, "@late").Exists() {
		t.Fatal("subsequent query should not see @late after delete")
	}
}

// A modifier that runs a nested query re-enters through the scope so the
// nested query resolves modifiers against the same registry.
func TestScopeNestedQueryFromModifier(t *testing.T) {
	s := NewScope(false)
	s.AddModifier("normalize", lowerModifier)
	s.AddModifier("firstNormalized", func(json, arg string) string {
		res := s.Get(json, "friends.0.@normalize")
		if !res.Exists() {
			return `null`
		}
		return res.Raw
	})
	if got := s.Get(scopeTestJSON, "@firstNormalized").String(); got != "alice" {
		t.Fatalf("nested scoped query: expected %q, got %q", "alice", got)
	}
	// The same nested path cannot resolve outside the scope.
	if Get(scopeTestJSON, "friends.0.@normalize").Exists() {
		t.Fatal("package-level Get should not resolve @normalize")
	}
}

// The package-level API keeps its existing behavior and does not leak into
// scopes.
func TestScopePackageAPIUnchanged(t *testing.T) {
	AddModifier("pkgscopedtest", upperModifier)
	if !ModifierExists("pkgscopedtest", nil) {
		t.Fatal("package-level ModifierExists should find pkgscopedtest")
	}
	if got := Get(scopeTestJSON, "name.@pkgscopedtest").String(); got != "TOM ANDERSON" {
		t.Fatalf("package-level Get: expected %q, got %q", "TOM ANDERSON", got)
	}
	// Scopes only inherit the built-in set, not package-level additions.
	if NewScope(true).ModifierExists("pkgscopedtest") {
		t.Fatal("NewScope(true) should not inherit package-level additions")
	}
	if NewScope(false).Get(scopeTestJSON, "name.@pkgscopedtest").Exists() {
		t.Fatal("scope should not resolve package-level modifiers")
	}
}

// Registration, deletion, and queries can run concurrently; each query sees
// a consistent snapshot.
func TestScopeConcurrentRegistration(t *testing.T) {
	s := NewScope(true)
	s.AddModifier("flip", func(json, arg string) string { return `"a"` })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("mod%d", i)
			for j := 0; j < 200; j++ {
				s.AddModifier(name, lowerModifier)
				s.Get(scopeTestJSON, "name."+"@"+name)
				s.ModifierExists(name)
				s.DeleteModifier(name)
			}
		}(i)
	}
	// Concurrently replace @flip; every query must observe one consistent
	// meaning per query, never a torn registry.
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		flip := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			flip = !flip
			if flip {
				s.AddModifier("flip", func(json, arg string) string { return `"a"` })
			} else {
				s.AddModifier("flip", func(json, arg string) string { return `"b"` })
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		got := s.Get(`{"x":1}`, "@flip").String()
		if got != "a" && got != "b" {
			close(stop)
			t.Fatalf("torn registry view: got %q", got)
		}
	}
	close(stop)
	wg.Wait()
}

func TestScopeGetBytes(t *testing.T) {
	s := NewScope(false)
	s.AddModifier("normalize", lowerModifier)
	if got := s.GetBytes([]byte(scopeTestJSON), "name.@normalize").String(); got != "tom anderson" {
		t.Fatalf("GetBytes: expected %q, got %q", "tom anderson", got)
	}
}
