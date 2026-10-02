package nosurf

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func mustAddExemptRule(t *testing.T, h *CSRFHandler, rule ExemptRule) {
	t.Helper()
	if err := h.AddExemptRule(rule); err != nil {
		t.Fatalf("AddExemptRule(%q) returned error: %v", rule.ID, err)
	}
}

func TestAddExemptRuleRejectsDuplicateID(t *testing.T) {
	h := New(nil)
	rule := ExemptRule{ID: "same-id", Path: "/one"}

	if err := h.AddExemptRule(rule); err != nil {
		t.Fatalf("first AddExemptRule returned error: %v", err)
	}

	rule.Path = "/two"
	if err := h.AddExemptRule(rule); err != ErrDuplicateExemptRuleID {
		t.Fatalf("duplicate rule returned %v, want %v", err, ErrDuplicateExemptRuleID)
	}
}

func TestAddExemptRuleRejectsInvalidRules(t *testing.T) {
	testCases := []struct {
		name string
		rule ExemptRule
	}{
		{name: "missing id", rule: ExemptRule{Path: "/"}},
		{name: "no matcher", rule: ExemptRule{ID: "no-matcher"}},
		{name: "legacy id prefix", rule: ExemptRule{ID: "legacy:reserved", Path: "/"}},
		{name: "invalid glob", rule: ExemptRule{ID: "bad-glob", Glob: "["}},
		{name: "invalid regexp pattern", rule: ExemptRule{ID: "bad-regexp", Pattern: "("}},
		{name: "glob and pattern", rule: ExemptRule{ID: "two-matchers", Glob: "/*", Pattern: ".*"}},
		{name: "compiled regexp and pattern", rule: ExemptRule{
			ID:      "regexp-and-pattern",
			Regexp:  regexp.MustCompile(".*"),
			Pattern: ".*",
		}},
		{name: "glob and callback", rule: ExemptRule{
			ID:   "glob-and-callback",
			Glob: "/*",
			Func: func(*http.Request) bool { return true },
		}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := New(nil)
			if err := h.AddExemptRule(tc.rule); err != ErrInvalidExemptRule {
				t.Fatalf("AddExemptRule returned %v, want %v", err, ErrInvalidExemptRule)
			}
		})
	}
}

func TestMatchExemptRuleUsesPriority(t *testing.T) {
	h := New(nil)
	path := "/same"
	callbackCalled := false

	rules := []ExemptRule{
		{
			ID:       "callback",
			Priority: 40,
			Func: func(*http.Request) bool {
				callbackCalled = true
				return true
			},
		},
		{ID: "exact", Priority: 30, Path: path},
		{ID: "glob", Priority: 20, Glob: "/*"},
		{ID: "regexp", Priority: 10, Regexp: regexp.MustCompile("/.*")},
	}
	for _, rule := range rules {
		mustAddExemptRule(t, h, rule)
	}

	req := httptest.NewRequest(http.MethodPost, path, nil)
	got := h.MatchExemptRule(req)

	if !got.Matched || got.RuleID != "regexp" {
		t.Fatalf("MatchExemptRule returned %+v, want matched rule %q", got, "regexp")
	}
	if callbackCalled {
		t.Fatal("lower-priority callback was evaluated after a rule matched")
	}
}

func TestMatchExemptRuleDefaultPriority(t *testing.T) {
	h := New(nil)
	path := "/same"

	mustAddExemptRule(t, h, ExemptRule{ID: "regexp", Regexp: regexp.MustCompile("/.*")})
	mustAddExemptRule(t, h, ExemptRule{ID: "glob", Glob: "/*"})
	mustAddExemptRule(t, h, ExemptRule{ID: "exact", Path: path})
	mustAddExemptRule(t, h, ExemptRule{
		ID:   "callback",
		Func: func(*http.Request) bool { return true },
	})

	req := httptest.NewRequest(http.MethodPost, path, nil)
	got := h.MatchExemptRule(req)

	if !got.Matched || got.RuleID != "callback" {
		t.Fatalf("MatchExemptRule returned %+v, want matched rule %q", got, "callback")
	}
}

func TestMatchExemptRuleUsesRegistrationOrderForEqualPriority(t *testing.T) {
	h := New(nil)
	mustAddExemptRule(t, h, ExemptRule{
		ID:       "first",
		Priority: 100,
		Glob:     "/*",
	})
	mustAddExemptRule(t, h, ExemptRule{
		ID:       "second",
		Priority: 100,
		Regexp:   regexp.MustCompile("/.*"),
	})

	got := h.MatchExemptRule(httptest.NewRequest(http.MethodPost, "/same", nil))

	if !got.Matched || got.RuleID != "first" {
		t.Fatalf("MatchExemptRule returned %+v, want %q", got, "first")
	}
}

func TestCheckRequestRecordsDecision(t *testing.T) {
	realToken := generateToken()

	testCases := []struct {
		name      string
		method    string
		path      string
		rule      *ExemptRule
		headers   map[string]string
		cookie    bool
		sentToken bool
		allowed   bool
		stage     CheckStage
		ruleID    string
	}{
		{
			name:    "safe method",
			method:  http.MethodGet,
			path:    "/",
			allowed: true,
			stage:   StageSafeMethod,
			ruleID:  SafeMethodRuleID,
		},
		{
			name:    "exemption",
			method:  http.MethodPost,
			path:    "/exempt",
			rule:    &ExemptRule{ID: "explicit-exempt", Path: "/exempt"},
			allowed: true,
			stage:   StageExemption,
			ruleID:  "explicit-exempt",
		},
		{
			name:    "origin failure",
			method:  http.MethodPost,
			path:    "/",
			allowed: false,
			stage:   StageOrigin,
		},
		{
			name:    "token failure",
			method:  http.MethodPost,
			path:    "/",
			headers: map[string]string{"Origin": "https://example.com"},
			allowed: false,
			stage:   StageToken,
		},
		{
			name:      "token success",
			method:    http.MethodPost,
			path:      "/",
			headers:   map[string]string{"Origin": "https://example.com"},
			cookie:    true,
			sentToken: true,
			allowed:   true,
			stage:     StageToken,
			ruleID:    TokenRuleID,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := New(nil)
			h.SetIsTLSFunc(func(*http.Request) bool { return true })
			if tc.rule != nil {
				mustAddExemptRule(t, h, *tc.rule)
			}

			req := httptest.NewRequest(tc.method, "https://example.com"+tc.path, nil)
			req.Host = "example.com"
			for key, value := range tc.headers {
				req.Header.Set(key, value)
			}
			if tc.cookie {
				req.AddCookie(&http.Cookie{Name: CookieName, Value: b64encode(realToken)})
			}
			if tc.sentToken {
				req.Header.Set(HeaderName, b64encode(maskToken(realToken)))
			}

			got := h.CheckRequest(req)

			if got.Allowed != tc.allowed {
				t.Fatalf("Allowed = %v, want %v", got.Allowed, tc.allowed)
			}
			if got.Stage != tc.stage {
				t.Fatalf("Stage = %q, want %q", got.Stage, tc.stage)
			}
			if got.RuleID != tc.ruleID {
				t.Fatalf("RuleID = %q, want %q", got.RuleID, tc.ruleID)
			}
		})
	}
}

func TestDecisionForRecordsServeHTTPDecision(t *testing.T) {
	var got Decision
	h := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = DecisionFor(r)
		w.WriteHeader(http.StatusAccepted)
	}))
	mustAddExemptRule(t, h, ExemptRule{ID: "recorded-exempt", Path: "/exempt"})

	req := httptest.NewRequest(http.MethodPost, "https://example.com/exempt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if !got.Allowed || got.Stage != StageExemption || got.RuleID != "recorded-exempt" {
		t.Fatalf("recorded decision = %+v", got)
	}
}

func TestCheckRequestDoesNotCallDownstream(t *testing.T) {
	called := 0
	h := New(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called++
	}))

	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	got := h.CheckRequest(req)

	if !got.Allowed {
		t.Fatal("safe request was not allowed")
	}
	if called != 0 {
		t.Fatalf("downstream handler was called %d times", called)
	}
}
