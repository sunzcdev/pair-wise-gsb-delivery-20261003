package nosurf

import (
	"errors"
	"fmt"
	"net/http"
	pathModule "path"
	"regexp"
	"sort"
)

// DecisionStage identifies the stage of the CSRF check
// that produced the final conclusion for a request.
type DecisionStage string

const (
	// The request was allowed because it uses a safe HTTP method
	// (GET, HEAD, OPTIONS or TRACE).
	StageSafeMethod DecisionStage = "safe_method"
	// The request was allowed because it matched an exemption rule.
	StageExemptRule DecisionStage = "exempt_rule"
	// The request was rejected by the origin check
	// (Sec-Fetch-Site, Origin and Referer headers).
	StageOrigin DecisionStage = "origin"
	// The request was allowed or rejected by the token check.
	StageToken DecisionStage = "token"
)

// Decision describes how CSRFHandler concluded a request:
// why it was allowed, or at which stage it was rejected.
//
// A Decision only carries rule identifiers and the conclusion.
// It never contains cookie or token values.
type Decision struct {
	// Allowed reports whether the request passed the CSRF check.
	Allowed bool
	// Stage is the check stage that produced the conclusion.
	Stage DecisionStage
	// RuleID is the ID of the exemption rule that matched the request.
	// It is only set when Stage is StageExemptRule.
	RuleID string
	// Reason is the failure reason. It is only set when Allowed is false.
	Reason error
}

// RuleKind identifies the matching strategy of an ExemptRule.
type RuleKind int

const (
	// RuleExactPath matches Request.URL.Path against an exact path.
	RuleExactPath RuleKind = iota
	// RuleGlob matches Request.URL.Path against a glob
	// (as used by path.Match()).
	RuleGlob
	// RuleRegexp matches Request.URL.Path against a regular expression.
	RuleRegexp
	// RuleFunc matches the request with a custom function.
	RuleFunc
)

// Default priorities used by the legacy Exempt* registration methods.
// They preserve the historical evaluation order:
// the callback first, then exact paths, globs and finally regexps.
// A lower priority value is evaluated earlier.
const (
	PriorityFunc   = 10
	PriorityPath   = 20
	PriorityGlob   = 30
	PriorityRegexp = 40
)

// Errors returned by AddExemptRule.
var (
	ErrRuleEmptyID     = errors.New("exempt rule must have a non-empty ID")
	ErrRuleDuplicateID = errors.New("an exempt rule with the same ID is already registered")
	ErrRuleInvalidKind = errors.New("exempt rule has an invalid kind")
	ErrRuleNoMatcher   = errors.New("exempt rule is missing its matcher")
	ErrRuleBadGlob     = errors.New("exempt rule has an invalid glob pattern")
	ErrRuleBadRegexp   = errors.New("exempt rule has an invalid regular expression")
)

// ExemptRule is a named exemption from CSRF checks.
// Rules carry a stable ID and an explicit priority,
// so the outcome of matching is reproducible
// even when different kinds of rules are mixed:
// matching rules are considered in order of ascending Priority,
// with ties broken by registration order,
// and the first matching rule wins.
type ExemptRule struct {
	// ID is the stable identifier of the rule.
	// It is reported in Decision.RuleID when the rule matches.
	ID string
	// Kind selects the matching strategy and
	// which of the matcher fields below is used.
	Kind RuleKind
	// Priority orders the rule against other rules.
	// Lower values are evaluated earlier.
	Priority int

	// Path is the exact path to match. Used with RuleExactPath.
	Path string
	// Glob is the glob pattern to match. Used with RuleGlob.
	Glob string
	// Regexp is a regular expression string or a *regexp.Regexp.
	// Used with RuleRegexp.
	Regexp interface{}
	// Func is a custom matcher. Used with RuleFunc.
	Func func(r *http.Request) bool
}

// exemptRule is the compiled, registered form of ExemptRule.
type exemptRule struct {
	id       string
	kind     RuleKind
	priority int
	seq      int

	path string
	glob string
	re   *regexp.Regexp
	fn   func(r *http.Request) bool
}

func (rule *exemptRule) matches(r *http.Request) bool {
	switch rule.kind {
	case RuleExactPath:
		return r.URL.Path == rule.path
	case RuleGlob:
		matched, err := pathModule.Match(rule.glob, r.URL.Path)
		return matched && err == nil
	case RuleRegexp:
		return rule.re.MatchString(r.URL.Path)
	case RuleFunc:
		return rule.fn(r)
	}
	return false
}

// compileRule validates an ExemptRule spec
// and returns its compiled, internal form.
func compileRule(spec ExemptRule) (*exemptRule, error) {
	if spec.ID == "" {
		return nil, ErrRuleEmptyID
	}

	rule := &exemptRule{
		id:       spec.ID,
		kind:     spec.Kind,
		priority: spec.Priority,
	}

	switch spec.Kind {
	case RuleExactPath:
		if spec.Path == "" {
			return nil, fmt.Errorf("%w: empty path for rule %q", ErrRuleNoMatcher, spec.ID)
		}
		rule.path = spec.Path
	case RuleGlob:
		if spec.Glob == "" {
			return nil, fmt.Errorf("%w: empty glob for rule %q", ErrRuleNoMatcher, spec.ID)
		}
		if _, err := pathModule.Match(spec.Glob, ""); err != nil {
			return nil, fmt.Errorf("%w: %q", ErrRuleBadGlob, spec.Glob)
		}
		rule.glob = spec.Glob
	case RuleRegexp:
		switch re := spec.Regexp.(type) {
		case string:
			compiled, err := regexp.Compile(re)
			if err != nil {
				return nil, fmt.Errorf("%w: %q", ErrRuleBadRegexp, re)
			}
			rule.re = compiled
		case *regexp.Regexp:
			if re == nil {
				return nil, fmt.Errorf("%w: nil *regexp.Regexp for rule %q", ErrRuleNoMatcher, spec.ID)
			}
			rule.re = re
		default:
			return nil, fmt.Errorf("%w: %T is not a valid regexp type", ErrRuleBadRegexp, spec.Regexp)
		}
	case RuleFunc:
		if spec.Func == nil {
			return nil, fmt.Errorf("%w: nil func for rule %q", ErrRuleNoMatcher, spec.ID)
		}
		rule.fn = spec.Func
	default:
		return nil, fmt.Errorf("%w: %d", ErrRuleInvalidKind, spec.Kind)
	}

	return rule, nil
}

// AddExemptRule registers an exemption rule with a stable ID
// and an explicit priority.
//
// It returns an error if the rule is invalid
// (empty ID, unknown kind, missing or malformed matcher)
// or if a rule with the same ID is already registered.
func (h *CSRFHandler) AddExemptRule(spec ExemptRule) error {
	rule, err := compileRule(spec)
	if err != nil {
		return err
	}

	return h.addRule(rule)
}

// addRule registers a compiled rule, keeping h.rules
// sorted by ascending priority with registration order
// as the tie-breaker, so that matching is reproducible.
func (h *CSRFHandler) addRule(rule *exemptRule) error {
	if h.ruleIDs == nil {
		h.ruleIDs = make(map[string]struct{})
	}
	if _, ok := h.ruleIDs[rule.id]; ok {
		return fmt.Errorf("%w: %q", ErrRuleDuplicateID, rule.id)
	}

	h.ruleIDs[rule.id] = struct{}{}
	rule.seq = h.ruleSeq
	h.ruleSeq++

	h.rules = append(h.rules, rule)
	sort.SliceStable(h.rules, func(i, j int) bool {
		return h.rules[i].priority < h.rules[j].priority
	})

	return nil
}

// removeRule unregisters the rule with the given ID, if any.
func (h *CSRFHandler) removeRule(id string) {
	if _, ok := h.ruleIDs[id]; !ok {
		return
	}
	delete(h.ruleIDs, id)

	for i, rule := range h.rules {
		if rule.id == id {
			h.rules = append(h.rules[:i], h.rules[i+1:]...)
			break
		}
	}
}

// matchExempt returns the ID of the first matching exemption rule.
// Rules are considered in order of ascending priority,
// with ties broken by registration order.
func (h *CSRFHandler) matchExempt(r *http.Request) (string, bool) {
	for _, rule := range h.rules {
		if rule.matches(r) {
			return rule.id, true
		}
	}
	return "", false
}

// Check evaluates the CSRF decision for the given request
// without generating tokens, setting cookies
// or calling the wrapped handlers.
// It mirrors the verdict ServeHTTP would reach for the same request.
func (h *CSRFHandler) Check(r *http.Request) Decision {
	if sContains(safeMethods, r.Method) {
		return Decision{Allowed: true, Stage: StageSafeMethod}
	}

	if id, ok := h.matchExempt(r); ok {
		return Decision{Allowed: true, Stage: StageExemptRule, RuleID: id}
	}

	if err := h.ensureSameOrigin(r); err != nil {
		return Decision{Stage: StageOrigin, Reason: err}
	}

	var realToken []byte
	if cookie, err := r.Cookie(h.getCookieName()); err == nil {
		realToken = b64decode(cookie.Value)
	}

	if !verifyToken(realToken, extractToken(r)) {
		return Decision{Stage: StageToken, Reason: ErrBadToken}
	}

	return Decision{Allowed: true, Stage: StageToken}
}
