package nosurf

import (
	"fmt"
	"net/http"
	"regexp"
)

// Checks if the given request is exempt from CSRF checks.
// It considers the registered exemption rules
// in order of ascending priority,
// with ties broken by registration order.
// Rules registered via the legacy Exempt* methods
// use the default priorities of their kind:
// the callback first, then exact paths, globs and finally regexps.
func (h *CSRFHandler) IsExempt(r *http.Request) bool {
	_, exempt := h.matchExempt(r)
	return exempt
}

// Exempts an exact path from CSRF checks
// With this (and other Exempt* methods)
// you should take note that Go's paths
// include a leading slash.
//
// The rule is registered under the ID "path:<path>"
// with the default priority PriorityPath.
func (h *CSRFHandler) ExemptPath(path string) {
	h.exemptPaths = append(h.exemptPaths, path)
	// Duplicate registrations are idempotent for the legacy API.
	_ = h.addRule(&exemptRule{
		id:       "path:" + path,
		kind:     RuleExactPath,
		priority: PriorityPath,
		path:     path,
	})
}

// A variadic argument version of ExemptPath()
func (h *CSRFHandler) ExemptPaths(paths ...string) {
	for _, path := range paths {
		h.ExemptPath(path)
	}
}

// Exempts URLs that match the specified glob pattern
// (as used by filepath.Match()) from CSRF checks
//
// Note that ExemptGlob() is unable to detect syntax errors,
// because it doesn't have a path to check it against
// and filepath.Match() doesn't report an error
// if the path is empty.
// If we find a way to check the syntax, ExemptGlob
// MIGHT PANIC on a syntax error in the future.
// ALWAYS check your globs for syntax errors.
//
// The rule is registered under the ID "glob:<pattern>"
// with the default priority PriorityGlob.
func (h *CSRFHandler) ExemptGlob(pattern string) {
	h.exemptGlobs = append(h.exemptGlobs, pattern)
	_ = h.addRule(&exemptRule{
		id:       "glob:" + pattern,
		kind:     RuleGlob,
		priority: PriorityGlob,
		glob:     pattern,
	})
}

// A variadic argument version of ExemptGlob()
func (h *CSRFHandler) ExemptGlobs(patterns ...string) {
	for _, pattern := range patterns {
		h.ExemptGlob(pattern)
	}
}

// Accepts a regular expression string or a compiled *regexp.Regexp
// and exempts URLs that match it from CSRF checks.
//
// If the given argument is neither of the accepted values,
// or the given string fails to compile, ExemptRegexp() panics.
//
// The rule is registered under the ID "regexp:<pattern>"
// with the default priority PriorityRegexp.
func (h *CSRFHandler) ExemptRegexp(re interface{}) {
	var compiled *regexp.Regexp

	switch re := re.(type) {
	case string:
		compiled = regexp.MustCompile(re)
	case *regexp.Regexp:
		compiled = re
	default:
		err := fmt.Errorf("%T is not a valid type for ExemptRegexp()", re)
		panic(err)
	}

	h.exemptRegexps = append(h.exemptRegexps, compiled)
	_ = h.addRule(&exemptRule{
		id:       "regexp:" + compiled.String(),
		kind:     RuleRegexp,
		priority: PriorityRegexp,
		re:       compiled,
	})
}

// A variadic argument version of ExemptRegexp()
func (h *CSRFHandler) ExemptRegexps(res ...interface{}) {
	for _, v := range res {
		h.ExemptRegexp(v)
	}
}

// ExemptFunc exempts requests for which fn returns true.
// Registering a new func replaces the previously registered one.
//
// The rule is registered under the ID "func"
// with the default priority PriorityFunc.
func (h *CSRFHandler) ExemptFunc(fn func(r *http.Request) bool) {
	h.exemptFunc = fn
	h.removeRule("func")
	if fn == nil {
		return
	}
	_ = h.addRule(&exemptRule{
		id:       "func",
		kind:     RuleFunc,
		priority: PriorityFunc,
		fn:       fn,
	})
}
