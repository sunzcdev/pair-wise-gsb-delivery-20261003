// Copyright 2024 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
//
// https://codeberg.org/tidwall/gjson

package gjson

import "sync"

// Scope is an isolated query environment with its own modifier registry.
//
// Multiple services can each own a Scope and register the same modifier name
// with different meanings without affecting each other or the package-level
// registry used by Get. Every query executed through a Scope pins a snapshot
// of the scope's registry for the duration of that query, so registering,
// replacing, or deleting a modifier never affects queries that have already
// started.
//
// A Scope is safe for concurrent use, including registering modifiers while
// queries are running. Scope queries are not affected by the package-level
// DisableModifiers flag.
//
// Custom modifiers that need to run nested queries should re-enter through
// the scope so the nested query resolves modifiers against the same registry:
//
//	scope := gjson.NewScope(true)
//	scope.AddModifier("normalize", func(json, arg string) string {
//		res := scope.Get(json, "name.@this")
//		return `"` + strings.ToLower(res.String()) + `"`
//	})
type Scope struct {
	mu  sync.RWMutex
	fns map[string]func(json, arg string) string
}

// NewScope returns a new Scope with an empty modifier registry.
// When inheritBuiltins is true the registry starts with a copy of the
// built-in modifier set (pretty, ugly, reverse, this, flatten, join, valid,
// keys, values, tostr, fromstr, group, dig). Modifiers added later through
// the package-level AddModifier function are not inherited.
func NewScope(inheritBuiltins bool) *Scope {
	s := &Scope{fns: make(map[string]func(json, arg string) string)}
	if inheritBuiltins {
		for name, fn := range builtinModifiers {
			s.fns[name] = fn
		}
	}
	return s
}

// AddModifier binds a custom modifier command to the scope, replacing any
// existing modifier with the same name. It does not affect other scopes,
// the package-level registry, or queries that have already started.
func (s *Scope) AddModifier(name string, fn func(json, arg string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fns[name] = fn
}

// DeleteModifier removes a modifier from the scope. It does not affect
// other scopes, the package-level registry, or queries that have already
// started.
func (s *Scope) DeleteModifier(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.fns, name)
}

// ModifierExists returns true when the specified modifier exists in the
// scope's registry.
func (s *Scope) ModifierExists(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.fns[name]
	return ok
}

// view pins a snapshot of the scope's modifier registry for a single query.
func (s *Scope) view() *modifierView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fns := make(map[string]func(json, arg string) string, len(s.fns))
	for name, fn := range s.fns {
		fns[name] = fn
	}
	return &modifierView{fns: fns}
}

// Get searches json for the specified path, resolving modifiers against the
// scope's registry instead of the package-level registry. It otherwise
// behaves exactly like the package-level Get function.
func (s *Scope) Get(json, path string) Result {
	return getWithMods(json, path, s.view())
}

// GetBytes searches json for the specified path, resolving modifiers against
// the scope's registry instead of the package-level registry. It otherwise
// behaves exactly like the package-level GetBytes function.
func (s *Scope) GetBytes(json []byte, path string) Result {
	return getBytes(json, path, s.view())
}
