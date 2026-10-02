package nosurf

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
)

// MatchExemptRule evaluates exemption matchers only. It does not generate a
// token or invoke the wrapped success or failure handler.
func (h *CSRFHandler) MatchExemptRule(r *http.Request) ExemptionDecision {
	h.exemptRulesLock.RLock()
	rules := make([]ExemptRule, len(h.exemptRules))
	copy(rules, h.exemptRules)
	h.exemptRulesLock.RUnlock()

	for _, rule := range rules {
		if ruleMatches(rule, r) {
			return ExemptionDecision{Matched: true, RuleID: rule.ID}
		}
	}

	return ExemptionDecision{}
}

// IsExempt reports whether the request matches any exemption rule.
func (h *CSRFHandler) IsExempt(r *http.Request) bool {
	return h.MatchExemptRule(r).Matched
}

// AddExemptRule registers an exemption with a stable ID. Lower rule priorities
// are evaluated first; registration order is used as a stable tie-breaker.
// It returns an error when the ID is duplicated or the rule is invalid.
func (h *CSRFHandler) AddExemptRule(rule ExemptRule) error {
	rule, err := validateExemptRule(rule)
	if err != nil {
		return err
	}

	h.exemptRulesLock.Lock()
	defer h.exemptRulesLock.Unlock()

	for _, existing := range h.exemptRules {
		if existing.ID == rule.ID {
			return ErrDuplicateExemptRuleID
		}
	}

	h.exemptRules = append(h.exemptRules, rule)
	h.sortExemptRules()

	return nil
}

func (h *CSRFHandler) sortExemptRules() {
	sort.SliceStable(h.exemptRules, func(i, j int) bool {
		return h.exemptRules[i].Priority < h.exemptRules[j].Priority
	})
}

func (h *CSRFHandler) addLegacyExemptRule(kind string, rule ExemptRule) {
	h.exemptRulesLock.Lock()
	defer h.exemptRulesLock.Unlock()

	h.exemptRuleID++
	rule.ID = fmt.Sprintf("legacy:%s:%d", kind, h.exemptRuleID)
	rule.Priority = defaultRulePriority(rule)
	h.exemptRules = append(h.exemptRules, rule)
	h.sortExemptRules()
}

// ExemptPath exempts an exact path from CSRF checks.
func (h *CSRFHandler) ExemptPath(path string) {
	h.addLegacyExemptRule("path", ExemptRule{Path: path})
}

// ExemptPaths is a variadic version of ExemptPath.
func (h *CSRFHandler) ExemptPaths(paths ...string) {
	for _, path := range paths {
		h.ExemptPath(path)
	}
}

// ExemptGlob exempts URLs matching the specified glob pattern from CSRF checks.
func (h *CSRFHandler) ExemptGlob(pattern string) {
	h.addLegacyExemptRule("glob", ExemptRule{Glob: pattern})
}

// ExemptGlobs is a variadic version of ExemptGlob.
func (h *CSRFHandler) ExemptGlobs(patterns ...string) {
	for _, pattern := range patterns {
		h.ExemptGlob(pattern)
	}
}

// ExemptRegexp accepts a regular expression string or a compiled *regexp.Regexp
// and exempts matching URLs. It panics for an invalid type or pattern.
func (h *CSRFHandler) ExemptRegexp(re interface{}) {
	var compiled *regexp.Regexp

	switch re := re.(type) {
	case string:
		compiled = regexp.MustCompile(re)
	case *regexp.Regexp:
		compiled = re
	default:
		panic(fmt.Errorf("%T is not a valid type for ExemptRegexp()", re))
	}

	h.addLegacyExemptRule("regexp", ExemptRule{Regexp: compiled})
}

// ExemptRegexps is a variadic version of ExemptRegexp.
func (h *CSRFHandler) ExemptRegexps(res ...interface{}) {
	for _, re := range res {
		h.ExemptRegexp(re)
	}
}

// ExemptFunc exempts requests for which the supplied function returns true.
func (h *CSRFHandler) ExemptFunc(fn func(r *http.Request) bool) {
	h.exemptRulesLock.Lock()
	defer h.exemptRulesLock.Unlock()

	for i := range h.exemptRules {
		if h.exemptRules[i].ID == "legacy:func" {
			h.exemptRules[i].Func = fn
			return
		}
	}

	rule := ExemptRule{ID: "legacy:func", Func: fn}
	rule.Priority = defaultRulePriority(rule)
	h.exemptRules = append(h.exemptRules, rule)
	h.sortExemptRules()
}
