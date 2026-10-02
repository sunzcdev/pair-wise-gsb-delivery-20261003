package nosurf

import (
	"errors"
	"net/http"
	pathModule "path"
	"regexp"
	"strings"
)

const (
	// SafeMethodRuleID is recorded when a safe HTTP method bypasses CSRF checks.
	SafeMethodRuleID = "safe-method"
	// TokenRuleID is recorded when CSRF token verification succeeds.
	TokenRuleID = "csrf-token"

	// Default exemption priorities are ordered callback, exact path, glob and
	// regexp for compatibility with the historical exemption order.
	ExemptCallbackPriority = 100
	ExemptPathPriority     = 200
	ExemptGlobPriority     = 300
	ExemptRegexpPriority   = 400
)

const legacyExemptRuleIDPrefix = "legacy:"

var (
	// ErrDuplicateExemptRuleID is returned when an exemption rule ID is already registered.
	ErrDuplicateExemptRuleID = errors.New("nosurf: duplicate exempt rule ID")
	// ErrInvalidExemptRule is returned when an exemption rule is missing an ID or matcher.
	ErrInvalidExemptRule = errors.New("nosurf: invalid exempt rule")
)

// CheckStage identifies the CSRF check stage that made the final decision.
type CheckStage string

const (
	StageSafeMethod CheckStage = "safe-method"
	StageExemption  CheckStage = "exemption"
	StageOrigin     CheckStage = "origin"
	StageToken      CheckStage = "token"
)

// ExemptRule describes one stable exemption rule. Exactly one matcher must be
// set. An empty Regexp, Pattern, Glob and Func makes Path the matcher.
// Positive lower Priority values are evaluated first.
type ExemptRule struct {
	ID       string
	Priority int

	Glob    string
	Regexp  *regexp.Regexp
	Pattern string
	Func    func(*http.Request) bool
	Path    string
}

// Decision contains the final decision for one request. It deliberately excludes
// cookie and token values.
type Decision struct {
	Allowed bool
	Stage   CheckStage
	RuleID  string
}

// ExemptionDecision describes the result of evaluating exemption matchers only.
type ExemptionDecision struct {
	Matched bool
	RuleID  string
}

func defaultRulePriority(rule ExemptRule) int {
	switch {
	case rule.Func != nil:
		return ExemptCallbackPriority
	case rule.Glob != "":
		return ExemptGlobPriority
	case rule.Regexp != nil || rule.Pattern != "":
		return ExemptRegexpPriority
	default:
		return ExemptPathPriority
	}
}

func validateExemptRule(rule ExemptRule) (ExemptRule, error) {
	if rule.ID == "" {
		return rule, ErrInvalidExemptRule
	}
	if strings.HasPrefix(rule.ID, legacyExemptRuleIDPrefix) {
		return rule, ErrInvalidExemptRule
	}
	matchers := 0
	if rule.Path != "" {
		matchers++
	}
	if rule.Glob != "" {
		matchers++
	}
	if rule.Regexp != nil {
		matchers++
	}
	if rule.Pattern != "" {
		matchers++
	}
	if rule.Func != nil {
		matchers++
	}
	if matchers != 1 {
		return rule, ErrInvalidExemptRule
	}

	if rule.Glob != "" {
		if _, err := pathModule.Match(rule.Glob, "/"); err != nil {
			return rule, ErrInvalidExemptRule
		}
	}
	if rule.Pattern != "" {
		compiled, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return rule, ErrInvalidExemptRule
		}
		rule.Regexp = compiled
		rule.Pattern = ""
	}

	if rule.Priority == 0 {
		rule.Priority = defaultRulePriority(rule)
	}
	return rule, nil
}

func ruleMatches(rule ExemptRule, r *http.Request) bool {
	switch {
	case rule.Func != nil:
		return rule.Func(r)
	case rule.Glob != "":
		matched, err := pathModule.Match(rule.Glob, r.URL.Path)
		return err == nil && matched
	case rule.Regexp != nil:
		return rule.Regexp.MatchString(r.URL.Path)
	default:
		return rule.Path == r.URL.Path
	}
}
