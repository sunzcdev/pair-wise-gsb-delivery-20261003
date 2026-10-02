package nosurf

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestAddExemptRuleDuplicateID(t *testing.T) {
	hand := New(nil)
	rule := ExemptRule{ID: "home", Kind: RuleExactPath, Path: "/home"}

	if err := hand.AddExemptRule(rule); err != nil {
		t.Fatalf("The first registration failed: %v", err)
	}

	err := hand.AddExemptRule(rule)
	if !errors.Is(err, ErrRuleDuplicateID) {
		t.Errorf("Expected ErrRuleDuplicateID, got %v", err)
	}

	// A duplicate ID must not be accepted even for a different kind.
	err = hand.AddExemptRule(ExemptRule{ID: "home", Kind: RuleGlob, Glob: "/ho*"})
	if !errors.Is(err, ErrRuleDuplicateID) {
		t.Errorf("Expected ErrRuleDuplicateID, got %v", err)
	}
}

func TestAddExemptRuleInvalid(t *testing.T) {
	testCases := []struct {
		name string
		rule ExemptRule
		err  error
	}{
		{"empty ID", ExemptRule{Kind: RuleExactPath, Path: "/x"}, ErrRuleEmptyID},
		{"invalid kind", ExemptRule{ID: "k", Kind: RuleKind(99)}, ErrRuleInvalidKind},
		{"missing path", ExemptRule{ID: "p", Kind: RuleExactPath}, ErrRuleNoMatcher},
		{"missing glob", ExemptRule{ID: "g", Kind: RuleGlob}, ErrRuleNoMatcher},
		{"bad glob", ExemptRule{ID: "g2", Kind: RuleGlob, Glob: "[unclosed"}, ErrRuleBadGlob},
		{"missing regexp", ExemptRule{ID: "r", Kind: RuleRegexp}, ErrRuleBadRegexp},
		{"bad regexp type", ExemptRule{ID: "r2", Kind: RuleRegexp, Regexp: 123}, ErrRuleBadRegexp},
		{"bad regexp pattern", ExemptRule{ID: "r3", Kind: RuleRegexp, Regexp: "a(b"}, ErrRuleBadRegexp},
		{"nil regexp", ExemptRule{ID: "r4", Kind: RuleRegexp, Regexp: (*regexp.Regexp)(nil)}, ErrRuleNoMatcher},
		{"missing func", ExemptRule{ID: "f", Kind: RuleFunc}, ErrRuleNoMatcher},
	}

	for _, tc := range testCases {
		hand := New(nil)
		err := hand.AddExemptRule(tc.rule)
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: expected %v, got %v", tc.name, tc.err, err)
		}

		// A rejected rule must not take effect.
		req, _ := http.NewRequest("POST", "http://dummy.us/x", nil)
		if hand.IsExempt(req) {
			t.Errorf("%s: an invalid rule should not be registered", tc.name)
		}
	}
}

// The same path matches rules of every kind.
// The selected rule must be reproducible:
// the lowest priority wins, ties break by registration order.
func TestExemptRuleMixedKindsSelection(t *testing.T) {
	const path = "/multi"

	allKinds := func(hand *CSRFHandler, priority int) {
		hand.AddExemptRule(ExemptRule{ID: "exact", Kind: RuleExactPath, Priority: priority, Path: path})
		hand.AddExemptRule(ExemptRule{ID: "glob", Kind: RuleGlob, Priority: priority, Glob: "/mul*"})
		hand.AddExemptRule(ExemptRule{ID: "regexp", Kind: RuleRegexp, Priority: priority, Regexp: "^/multi$"})
		hand.AddExemptRule(ExemptRule{ID: "func", Kind: RuleFunc, Priority: priority,
			Func: func(r *http.Request) bool { return r.URL.Path == path }})
	}

	// Case 1: distinct priorities. The lowest priority value wins,
	// regardless of registration order.
	hand := New(nil)
	allKinds(hand, 50)
	if err := hand.AddExemptRule(ExemptRule{
		ID: "first", Kind: RuleRegexp, Priority: 5, Regexp: "^/multi$",
	}); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("POST", "http://dummy.us"+path, nil)
	if decision := hand.Check(req); decision.RuleID != "first" {
		t.Errorf("Expected rule %q to win, got %q", "first", decision.RuleID)
	}

	// Case 2: equal priorities. Registration order breaks the tie.
	hand = New(nil)
	allKinds(hand, 10)
	if decision := hand.Check(req); decision.RuleID != "exact" {
		t.Errorf("Expected the first registered rule %q to win, got %q", "exact", decision.RuleID)
	}

	// The verdict must be stable across repeated evaluations.
	for i := 0; i < 3; i++ {
		if decision := hand.Check(req); decision.RuleID != "exact" {
			t.Fatalf("The selection is not reproducible: got %q on iteration %d", decision.RuleID, i)
		}
	}
}

// The legacy Exempt* methods keep their historical evaluation order:
// the callback first, then exact paths, globs and regexps.
func TestExemptRuleLegacyDefaultPriorities(t *testing.T) {
	hand := New(nil)
	hand.ExemptPath("/legacy")
	hand.ExemptFunc(func(r *http.Request) bool { return r.URL.Path == "/legacy" })

	req, _ := http.NewRequest("POST", "http://dummy.us/legacy", nil)
	decision := hand.Check(req)
	if !decision.Allowed || decision.Stage != StageExemptRule {
		t.Fatalf("Expected an exempting decision, got %+v", decision)
	}
	if decision.RuleID != "func" {
		t.Errorf("Expected the legacy func rule to win, got %q", decision.RuleID)
	}
}

// Serves a request through the handler
// and returns the decision recorded in the success handler.
func successDecision(hand *CSRFHandler, req *http.Request) (Decision, bool) {
	var decision Decision
	var ok bool
	hand.successHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decision, ok = GetDecision(r)
	})
	hand.ServeHTTP(httptest.NewRecorder(), req)
	return decision, ok
}

// Serves a request through the handler
// and returns the decision recorded in the failure handler.
func failureDecision(hand *CSRFHandler, req *http.Request) (Decision, bool) {
	var decision Decision
	var ok bool
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decision, ok = GetDecision(r)
	}))
	hand.ServeHTTP(httptest.NewRecorder(), req)
	return decision, ok
}

func TestDecisionRecordedSafeMethod(t *testing.T) {
	hand := New(nil)
	req, _ := http.NewRequest("GET", "http://dummy.us/", nil)

	decision, ok := successDecision(hand, req)
	if !ok {
		t.Fatal("No decision was recorded")
	}
	if !decision.Allowed || decision.Stage != StageSafeMethod || decision.RuleID != "" {
		t.Errorf("Unexpected decision: %+v", decision)
	}
}

func TestDecisionRecordedExemptRule(t *testing.T) {
	hand := New(nil)
	hand.ExemptPath("/faq")

	req, _ := http.NewRequest("POST", "http://dummy.us/faq", nil)
	decision, ok := successDecision(hand, req)
	if !ok {
		t.Fatal("No decision was recorded")
	}
	if !decision.Allowed || decision.Stage != StageExemptRule || decision.RuleID != "path:/faq" {
		t.Errorf("Unexpected decision: %+v", decision)
	}
}

func TestDecisionRecordedTokenPass(t *testing.T) {
	hand := New(nil)
	req := tokenizedRequest(t)

	decision, ok := successDecision(hand, req)
	if !ok {
		t.Fatal("No decision was recorded")
	}
	if !decision.Allowed || decision.Stage != StageToken {
		t.Errorf("Unexpected decision: %+v", decision)
	}
}

func TestDecisionRecordedOriginFailure(t *testing.T) {
	hand := New(nil)
	req, _ := http.NewRequest("POST", "http://dummy.us/", nil)

	decision, ok := failureDecision(hand, req)
	if !ok {
		t.Fatal("No decision was recorded")
	}
	if decision.Allowed || decision.Stage != StageOrigin || decision.Reason != ErrNoReferer {
		t.Errorf("Unexpected decision: %+v", decision)
	}
}

func TestDecisionRecordedTokenFailure(t *testing.T) {
	hand := New(nil)
	req, _ := http.NewRequest("POST", "http://dummy.us/", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	decision, ok := failureDecision(hand, req)
	if !ok {
		t.Fatal("No decision was recorded")
	}
	if decision.Allowed || decision.Stage != StageToken || decision.Reason != ErrBadToken {
		t.Errorf("Unexpected decision: %+v", decision)
	}
}

// Builds a POST request that passes both the origin and the token check.
func tokenizedRequest(t *testing.T) *http.Request {
	t.Helper()

	realToken := generateToken()
	sentToken := b64encode(maskToken(realToken))

	req, err := http.NewRequest("POST", "http://dummy.us/",
		formBodyR([][]string{{FormFieldName, sentToken}}))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(&http.Cookie{Name: CookieName, Value: b64encode(realToken)})

	return req
}

// Check only evaluates the matching conditions:
// it must not generate tokens, set cookies or call the wrapped handlers.
func TestCheckOnlyEvaluates(t *testing.T) {
	called := false
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	hand.ExemptPath("/faq")

	testCases := []struct {
		name    string
		req     *http.Request
		allowed bool
		stage   DecisionStage
		ruleID  string
		reason  error
	}{
		{
			name:    "safe method",
			req:     dummyGet(),
			allowed: true,
			stage:   StageSafeMethod,
		},
		{
			name:    "exempt rule",
			req:     mustRequest(t, "POST", "http://dummy.us/faq"),
			allowed: true,
			stage:   StageExemptRule,
			ruleID:  "path:/faq",
		},
		{
			name:   "origin failure",
			req:    mustRequest(t, "POST", "http://dummy.us/"),
			stage:  StageOrigin,
			reason: ErrNoReferer,
		},
		{
			name: "token failure",
			req: func() *http.Request {
				req := mustRequest(t, "POST", "http://dummy.us/")
				req.Header.Set("Sec-Fetch-Site", "same-origin")
				return req
			}(),
			stage:  StageToken,
			reason: ErrBadToken,
		},
		{
			name:    "token pass",
			req:     tokenizedRequest(t),
			allowed: true,
			stage:   StageToken,
		},
	}

	for _, tc := range testCases {
		decision := hand.Check(tc.req)
		if decision.Allowed != tc.allowed || decision.Stage != tc.stage ||
			decision.RuleID != tc.ruleID || decision.Reason != tc.reason {
			t.Errorf("%s: unexpected decision: %+v", tc.name, decision)
		}

		// Check must not record anything on the request.
		if _, ok := GetDecision(tc.req); ok {
			t.Errorf("%s: Check recorded a decision on the request", tc.name)
		}
	}

	if called {
		t.Error("Check called a wrapped handler")
	}
}

func mustRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
