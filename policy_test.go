package semver

import (
	"strings"
	"sync"
	"testing"
)

// publishPolicy mirrors the publish rule "allow 2.x, retract two ranges,
// ban one build number".
func publishPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := CompilePolicy(PolicySpec{
		Allow: []string{"2.x"},
		Deny:  []string{"2.1.x", "2.3.0 - 2.3.5"},
		Ban:   []string{"2.5.0+exp.sha.5114f85"},
	})
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
	return p
}

func TestPolicyDecisions(t *testing.T) {
	p := publishPolicy(t)

	tests := []struct {
		version string
		allowed bool
		rule    RuleKind
		index   int
	}{
		{"2.0.0", true, RuleAllow, 0},
		{"2.2.0", true, RuleAllow, 0},
		{"2.1.0", false, RuleDeny, 0},
		{"2.1.9", false, RuleDeny, 0},
		{"2.3.0", false, RuleDeny, 1},
		{"2.3.5", false, RuleDeny, 1},
		{"2.3.6", true, RuleAllow, 0},
		{"2.5.0+exp.sha.5114f85", false, RuleBan, 0},
		// Same precedence, different build number: not banned.
		{"2.5.0+exp.sha.aabbccd", true, RuleAllow, 0},
		{"2.5.0", true, RuleAllow, 0},
		// Not allowed and not excluded: still rejected.
		{"1.9.9", false, RuleNone, -1},
		{"3.0.0", false, RuleNone, -1},
		// Prerelease boundaries: 2.x does not cover prereleases by default.
		{"2.0.0-rc.1", false, RuleNone, -1},
		{"2.2.0-rc.1", false, RuleNone, -1},
	}

	for _, tc := range tests {
		d, err := p.CheckString(tc.version)
		if err != nil {
			t.Fatalf("CheckString(%q) error: %v", tc.version, err)
		}
		if d.Allowed != tc.allowed || d.Rule != tc.rule || d.Index != tc.index {
			t.Errorf("CheckString(%q) = %+v, want allowed=%v rule=%v index=%d",
				tc.version, d, tc.allowed, tc.rule, tc.index)
		}
	}
}

func TestPolicyDenyBeatsAllow(t *testing.T) {
	p, err := CompilePolicy(PolicySpec{
		Allow: []string{"2.1.x"},
		Deny:  []string{"2.1.3"},
	})
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}

	d, err := p.CheckString("2.1.3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Allowed || d.Rule != RuleDeny || d.Index != 0 {
		t.Errorf("expected exclusion to win over allow, got %+v (%s)", d, d)
	}
}

func TestPolicyPrereleaseBoundaries(t *testing.T) {
	p, err := CompilePolicy(PolicySpec{
		Allow: []string{"2.x"},
		Deny:  []string{">= 2.3.0-0 < 2.3.6"},
	})
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}

	// The deny range carries a prerelease, so prereleases inside the
	// range are excluded even though 2.x alone would ignore them.
	d, err := p.CheckString("2.3.0-rc.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Allowed || d.Rule != RuleDeny || d.Index != 0 {
		t.Errorf("expected 2.3.0-rc.1 denied by exclude rule 0, got %+v (%s)", d, d)
	}

	// Outside the deny range the prerelease is not excluded, but the
	// allow range still ignores prereleases. Note 2.3.6-rc.1 is inside
	// the deny range because it precedes 2.3.6.
	d, err = p.CheckString("2.4.0-rc.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Allowed || d.Rule != RuleNone {
		t.Errorf("expected 2.4.0-rc.1 rejected with no matching rule, got %+v (%s)", d, d)
	}

	// With IncludePrerelease the allow range considers prereleases.
	p, err = CompilePolicy(PolicySpec{
		Allow:             []string{">= 2.0.0"},
		IncludePrerelease: true,
	})
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
	d, err = p.CheckString("2.1.0-rc.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !d.Allowed || d.Rule != RuleAllow || d.Index != 0 {
		t.Errorf("expected 2.1.0-rc.1 allowed by allow rule 0, got %+v (%s)", d, d)
	}
}

func TestPolicyBanDistinguishesBuildMetadata(t *testing.T) {
	p, err := CompilePolicy(PolicySpec{
		Allow: []string{"= 1.2.3"},
		Ban:   []string{"1.2.3+100", "1.2.3+200"},
	})
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}

	// 1.2.3+100 and 1.2.3+200 have equal precedence but only those exact
	// builds are banned.
	for _, tc := range []struct {
		version string
		allowed bool
		index   int
	}{
		{"1.2.3+100", false, 0},
		{"1.2.3+200", false, 1},
		{"1.2.3+300", true, 0},
		{"1.2.3", true, 0},
	} {
		d, err := p.CheckString(tc.version)
		if err != nil {
			t.Fatalf("CheckString(%q) error: %v", tc.version, err)
		}
		if d.Allowed != tc.allowed {
			t.Errorf("CheckString(%q) allowed = %v, want %v (%s)", tc.version, d.Allowed, tc.allowed, d)
		}
		if !tc.allowed && (d.Rule != RuleBan || d.Index != tc.index) {
			t.Errorf("CheckString(%q) = %+v, want ban rule %d", tc.version, d, tc.index)
		}
	}
}

func TestCompilePolicyReportsAllProblems(t *testing.T) {
	_, err := CompilePolicy(PolicySpec{
		Allow: []string{"2.x", "not a constraint"},
		Deny:  []string{"???", "1.x"},
		Ban:   []string{"1.2.3+bad_meta!", "also not a version"},
	})
	if err == nil {
		t.Fatal("expected compile error")
	}
	msg := err.Error()
	for _, want := range []string{"allow rule 1", "deny rule 0", "ban rule 0", "ban rule 1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "allow rule 0") || strings.Contains(msg, "deny rule 1") {
		t.Errorf("error %q mentions valid rules", msg)
	}
}

func TestPolicyConcurrentCheckIndependentOfGlobals(t *testing.T) {
	p := publishPolicy(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	done := make(chan struct{})

	// Flip the package-level switches while queries run; decisions must
	// not change.
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				CoerceNewVersion = !CoerceNewVersion
				DetailedNewVersionErrors = !DetailedNewVersionErrors
			}
		}
	}()

	want := map[string]bool{
		"2.0.0":                 true,
		"2.1.0":                 false,
		"2.5.0+exp.sha.5114f85": false,
		"2.5.0+exp.sha.aabbccd": true,
		"1.9.9":                 false,
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				for s, allowed := range want {
					d, err := p.CheckString(s)
					if err != nil {
						t.Errorf("CheckString(%q) error: %v", s, err)
						return
					}
					if d.Allowed != allowed {
						t.Errorf("CheckString(%q) allowed = %v, want %v", s, d.Allowed, allowed)
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(stop)
	<-done
}
