package federation

import (
	"encoding/json"
	"testing"
)

func TestParsePolicyRejectsUnsupportedOperators(t *testing.T) {
	// Operators outside the declared support set must be rejected, not
	// ignored. Draft operators such as less_than/greater_than are not in
	// OpenID Federation 1.0's final operator set.
	for _, raw := range []string{
		`{"http_timeout":{"less_than":1024}}`,
		`{"http_timeout":{"greater_than":10}}`,
		`{"scopes_supported":{"unknown_op":["openid"]}}`,
	} {
		if _, err := parsePolicy([]byte(raw)); err == nil {
			t.Errorf("parsePolicy(%s) = nil error, want rejection", raw)
		}
	}
}

func TestParsePolicyRejectsMultipleOperatorsPerStatement(t *testing.T) {
	if _, err := parsePolicy([]byte(`{"scopes_supported":{"value":["openid"],"add":["email"]}}`)); err == nil {
		t.Fatal("expected rejection of two operators on one parameter in one statement")
	}
}

func TestParsePolicyRejectsWrongTypes(t *testing.T) {
	for _, raw := range []string{
		`{"scopes_supported":{"add":"openid"}}`,
		`{"scopes_supported":{"subset_of":"openid"}}`,
		`{"scopes_supported":{"one_of":[]}}`,
		`{"scopes_supported":{"merge":[]}}`,
	} {
		if _, err := parsePolicy([]byte(raw)); err == nil {
			t.Errorf("parsePolicy(%s) = nil error, want type rejection", raw)
		}
	}
}

func TestComposeAndApply(t *testing.T) {
	tests := []struct {
		name      string
		steps     []PolicyContribution
		leaf      string
		want      string
		wantError bool
	}{
		{
			name: "anchor subset then subordinate add, within bounds",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"scopes_supported":{"subset_of":["openid","profile","email"]}}`)},
				{Issuer: "https://b", Policy: mustPolicy(t, `{"scopes_supported":{"add":["profile","email"]}}`)},
			},
			leaf: `{"scopes_supported":["openid"]}`,
			want: `{"scopes_supported":["openid","profile","email"]}`,
		},
		{
			name: "add beyond upstream subset is an error",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"scopes_supported":{"subset_of":["openid"]}}`)},
				{Issuer: "https://b", Policy: mustPolicy(t, `{"scopes_supported":{"add":["email"]}}`)},
			},
			leaf:      `{"scopes_supported":["openid"]}`,
			wantError: true,
		},
		{
			name: "downstream value wins but must satisfy earlier subset_of",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"m":{"subset_of":["x","y"]}}`)},
				{Issuer: "https://b", Policy: mustPolicy(t, `{"m":{"value":"y"}}`)},
			},
			leaf: `{"m":"x"}`,
			want: `{"m":"y"}`,
		},
		{
			name: "downstream value violating subset_of is an error",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"m":{"subset_of":["x","y"]}}`)},
				{Issuer: "https://b", Policy: mustPolicy(t, `{"m":{"value":"z"}}`)},
			},
			leaf:      `{"m":"x"}`,
			wantError: true,
		},
		{
			name: "equal defaults compose",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"contacts":{"default":["a@example"]}}`)},
				{Issuer: "https://b", Policy: mustPolicy(t, `{"contacts":{"default":["a@example"]}}`)},
			},
			leaf: `{}`,
			want: `{"contacts":["a@example"]}`,
		},
		{
			name: "value overrides superior default",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"m":{"default":"d"}}`)},
				{Issuer: "https://b", Policy: mustPolicy(t, `{"m":{"value":"v"}}`)},
			},
			leaf: `{}`,
			want: `{"m":"v"}`,
		},
		{
			name: "superset_of meets subset_of becomes one_of and is applied",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"m":{"subset_of":["x","y","z"]}}`)},
				{Issuer: "https://b", Policy: mustPolicy(t, `{"m":{"superset_of":["y"]}}`)},
			},
			leaf:      `{"m":"y"}`,
			want:      `{"m":"y"}`,
			wantError: false,
		},
		{
			name: "one_of narrows and accepts leaf boolean",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"flag":{"one_of":[true]}}`)},
			},
			leaf: `{"flag":true}`,
			want: `{"flag":true}`,
		},
		{
			name: "one_of rejects disallowed leaf value",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"flag":{"one_of":[true]}}`)},
			},
			leaf:      `{"flag":false}`,
			wantError: true,
		},
		{
			name: "merge combines objects",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"cap":{"merge":{"a":true}}}`)},
				{Issuer: "https://b", Policy: mustPolicy(t, `{"cap":{"merge":{"b":false}}}`)},
			},
			leaf: `{"cap":{"leaf":1}}`,
			want: `{"cap":{"a":true,"b":false,"leaf":1}}`,
		},
		{
			name: "leaf list violates subset_of",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"m":{"subset_of":["x"]}}`)},
			},
			leaf:      `{"m":["x","y"]}`,
			wantError: true,
		},
		{
			name: "default missing parameter is introduced",
			steps: []PolicyContribution{
				{Issuer: "https://a", Policy: mustPolicy(t, `{"m":{"default":"d"}}`)},
			},
			leaf: `{}`,
			want: `{"m":"d"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			composed, origins, err := composePoliciesWithOrigins(tc.steps)
			if err != nil {
				if tc.wantError {
					return
				}
				t.Fatalf("compose: %v", err)
			}
			got, _, applyErr := applyPolicy("https://leaf", []byte(tc.leaf), composed, origins)
			if tc.wantError {
				if applyErr == nil {
					t.Fatalf("expected error, got metadata %s", got)
				}
				return
			}
			if applyErr != nil {
				t.Fatalf("apply: %v", applyErr)
			}
			if !jsonEqual(got, []byte(tc.want)) {
				t.Fatalf("metadata = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestApplyPolicyOrigins(t *testing.T) {
	steps := []PolicyContribution{
		{Issuer: "https://a", Policy: mustPolicy(t, `{"m":{"value":"v"}}`)},
	}
	composed, origins, err := composePoliciesWithOrigins(steps)
	if err != nil {
		t.Fatal(err)
	}
	_, gotOrigins, err := applyPolicy("https://leaf", []byte(`{"untouched":1}`), composed, origins)
	if err != nil {
		t.Fatal(err)
	}
	byParam := map[string]ClaimOrigin{}
	for _, o := range gotOrigins {
		byParam[o.Parameter] = o
	}
	if byParam["m"].Source != "https://a" || byParam["m"].Operator != PolicyValue {
		t.Fatalf("origin for m = %+v", byParam["m"])
	}
	if byParam["untouched"].Source != "https://leaf" || byParam["untouched"].Operator != "" {
		t.Fatalf("origin for untouched = %+v", byParam["untouched"])
	}
}

func mustPolicy(t *testing.T, raw string) Policy {
	t.Helper()
	var inner map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &inner); err != nil {
		t.Fatalf("decode policy %q: %v", raw, err)
	}
	policy := Policy{}
	for param, operators := range inner {
		policy[param] = Operators(operators)
	}
	return policy
}
