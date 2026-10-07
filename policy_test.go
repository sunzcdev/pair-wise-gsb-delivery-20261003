package semver

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCompositePolicyDecide(t *testing.T) {
	policy, err := CompileCompositePolicy([]PolicyRule{
		{ID: 10, Action: PolicyRuleAllow, Expression: "2.x"},
		{ID: 20, Action: PolicyRuleExclude, Expression: "2.0.0 - 2.0.5"},
		{ID: 21, Action: PolicyRuleExclude, Expression: "2.1.0-alpha.1"},
		{ID: 30, Action: PolicyRuleBlock, Expression: "2.1.0+build.42"},
	})
	if err != nil {
		t.Fatalf("CompileCompositePolicy returned error: %s", err)
	}

	tests := []struct {
		name       string
		version    string
		allowed    bool
		ruleID     int
		action     PolicyRuleAction
		expression string
	}{
		{name: "allowed", version: "2.1.0", allowed: true, ruleID: 10, action: PolicyRuleAllow, expression: "2.x"},
		{name: "excluded stable range", version: "2.0.4", allowed: false, ruleID: 20, action: PolicyRuleExclude, expression: "2.0.0 - 2.0.5"},
		{name: "excluded prerelease", version: "2.1.0-alpha.1", allowed: false, ruleID: 21, action: PolicyRuleExclude, expression: "2.1.0-alpha.1"},
		{name: "blocked exact build", version: "2.1.0+build.42", allowed: false, ruleID: 30, action: PolicyRuleBlock, expression: "2.1.0+build.42"},
		{name: "different build remains allowed", version: "2.1.0+build.43", allowed: true, ruleID: 10, action: PolicyRuleAllow, expression: "2.x"},
		{name: "prerelease below stable boundary", version: "2.1.0-rc.1", allowed: false, ruleID: 0},
		{name: "outside allow range", version: "3.0.0", allowed: false, ruleID: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := NewVersion(tc.version)
			if err != nil {
				t.Fatalf("NewVersion(%q) returned error: %s", tc.version, err)
			}

			decision := policy.Decide(v)
			if decision.Allowed != tc.allowed {
				t.Fatalf("Allowed = %v, want %v", decision.Allowed, tc.allowed)
			}
			if decision.RuleID != tc.ruleID {
				t.Fatalf("RuleID = %d, want %d", decision.RuleID, tc.ruleID)
			}
			if decision.Action != tc.action {
				t.Fatalf("Action = %q, want %q", decision.Action, tc.action)
			}
			if decision.Expression != tc.expression {
				t.Fatalf("Expression = %q, want %q", decision.Expression, tc.expression)
			}
			if tc.ruleID != 0 && !strings.Contains(decision.Reason, fmt.Sprintf("%d", tc.ruleID)) {
				t.Fatalf("Reason %q does not identify rule %d", decision.Reason, tc.ruleID)
			}
			if decision.Reason == "" {
				t.Fatal("Reason is empty")
			}
		})
	}
}

func TestCompositePolicyExcludeTakesPrecedenceOverAllow(t *testing.T) {
	policy, err := CompileCompositePolicy([]PolicyRule{
		{ID: 1, Action: PolicyRuleAllow, Expression: ">=1.0.0"},
		{ID: 2, Action: PolicyRuleAllow, Expression: "2.x"},
		{ID: 3, Action: PolicyRuleExclude, Expression: "2.1.x"},
	})
	if err != nil {
		t.Fatalf("CompileCompositePolicy returned error: %s", err)
	}

	v := MustParse("2.1.5")
	decision := policy.Decide(v)
	if decision.Allowed {
		t.Fatal("version was allowed despite an exclude rule")
	}
	if decision.RuleID != 3 {
		t.Fatalf("RuleID = %d, want 3", decision.RuleID)
	}
}

func TestCompositePolicyExactBlockDistinguishesBuildMetadata(t *testing.T) {
	policy, err := CompileCompositePolicy([]PolicyRule{
		{ID: 1, Action: PolicyRuleAllow, Expression: ">=1.0.0-0"},
		{ID: 2, Action: PolicyRuleBlock, Expression: "1.2.3+bad"},
	})
	if err != nil {
		t.Fatalf("CompileCompositePolicy returned error: %s", err)
	}

	blocked := MustParse("1.2.3+bad")
	if decision := policy.Decide(blocked); decision.Allowed || decision.RuleID != 2 {
		t.Fatalf("blocked decision = %+v, want denial from rule 2", decision)
	}

	allowed := MustParse("1.2.3+good")
	if decision := policy.Decide(allowed); !decision.Allowed || decision.RuleID != 1 {
		t.Fatalf("allowed decision = %+v, want allow from rule 1", decision)
	}

	withoutMetadata := MustParse("1.2.3")
	if decision := policy.Decide(withoutMetadata); !decision.Allowed || decision.RuleID != 1 {
		t.Fatalf("decision without metadata = %+v, want allow from rule 1", decision)
	}
}

func TestCompositePolicyBlockTakesPrecedenceOverExclude(t *testing.T) {
	policy, err := CompileCompositePolicy([]PolicyRule{
		{ID: 1, Action: PolicyRuleAllow, Expression: ">=1.0.0-0"},
		{ID: 2, Action: PolicyRuleExclude, Expression: "1.2.3"},
		{ID: 3, Action: PolicyRuleBlock, Expression: "1.2.3+blocked"},
	})
	if err != nil {
		t.Fatalf("CompileCompositePolicy returned error: %s", err)
	}

	decision := policy.Decide(MustParse("1.2.3+blocked"))
	if decision.Allowed || decision.RuleID != 3 || decision.Action != PolicyRuleBlock {
		t.Fatalf("decision = %+v, want block from rule 3", decision)
	}
}

func TestCompositePolicyBlockUsesOriginalIdentifier(t *testing.T) {
	policy, err := CompileCompositePolicy([]PolicyRule{
		{ID: 1, Action: PolicyRuleAllow, Expression: ">=1.0.0-0"},
		{ID: 2, Action: PolicyRuleBlock, Expression: "v1.2.3+bad"},
	})
	if err != nil {
		t.Fatalf("CompileCompositePolicy returned error: %s", err)
	}

	prefixed := MustParse("v1.2.3+bad")
	if decision := policy.Decide(prefixed); decision.Allowed || decision.RuleID != 2 {
		t.Fatalf("prefixed decision = %+v, want block from rule 2", decision)
	}

	withoutPrefix := MustParse("1.2.3+bad")
	if decision := policy.Decide(withoutPrefix); !decision.Allowed || decision.RuleID != 1 {
		t.Fatalf("unprefixed decision = %+v, want allow from rule 1", decision)
	}
}

func TestCompileCompositePolicyReportsAllConfigurationErrors(t *testing.T) {
	_, err := CompileCompositePolicy([]PolicyRule{
		{ID: 0, Action: PolicyRuleAllow, Expression: "2.x"},
		{ID: 1, Action: PolicyRuleAllow, Expression: "not-a-range"},
		{ID: 1, Action: PolicyRuleExclude, Expression: "2.0.x"},
		{ID: 2, Action: PolicyRuleBlock, Expression: "02.0.0+build"},
		{ID: 3, Action: PolicyRuleBlock, Expression: "1.0.0+build"},
		{ID: 4, Action: PolicyRuleBlock, Expression: "1.0.0+build"},
		{ID: 5, Action: "withdraw", Expression: "1.0.0"},
		{ID: 6, Action: PolicyRuleExclude, Expression: "   "},
	})
	if err == nil {
		t.Fatal("CompileCompositePolicy did not return an error")
	}

	wantMessages := []string{
		"ID must be positive",
		`invalid constraint "not-a-range"`,
		"rule ID 1 is used more than once",
		`invalid block version "02.0.0+build"`,
		`block version "1.0.0+build" is used more than once`,
		`unknown action "withdraw"`,
		"expression is empty",
	}
	for _, want := range wantMessages {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}

	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		t.Fatalf("error %T does not join multiple errors", err)
	}
	if len(joined.Unwrap()) < len(wantMessages) {
		t.Fatalf("joined %d errors, want at least %d", len(joined.Unwrap()), len(wantMessages))
	}
}

func TestCompositePolicyConcurrentDecisionsDoNotDependOnPackageSwitch(t *testing.T) {
	originalCoercion := CoerceNewVersion
	t.Cleanup(func() { CoerceNewVersion = originalCoercion })

	CoerceNewVersion = true
	policy, err := CompileCompositePolicy([]PolicyRule{
		{ID: 1, Action: PolicyRuleAllow, Expression: "2.x"},
		{ID: 2, Action: PolicyRuleExclude, Expression: "2.0.x"},
		{ID: 3, Action: PolicyRuleBlock, Expression: "2.1.0+blocked"},
	})
	if err != nil {
		t.Fatalf("CompileCompositePolicy returned error: %s", err)
	}
	CoerceNewVersion = false

	versions := []*Version{
		MustParse("2.1.0+ok"),
		MustParse("2.0.1"),
		MustParse("2.1.0+blocked"),
		MustParse("3.0.0"),
	}

	const goroutines = 32
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for j, v := range versions {
				policy.Decide(v)
				policy.Evaluate(v)
				_ = (offset + j) % len(versions)
			}
		}(i)
	}
	wg.Wait()
}
