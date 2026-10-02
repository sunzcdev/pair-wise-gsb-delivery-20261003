package cspbuilder

import (
	"strings"
	"testing"
)

func TestPolicyBuilderBuild(t *testing.T) {
	tests := []struct {
		name    string
		builder *PolicyBuilder
		want    string
		wantErr bool
	}{
		{
			name:    "empty",
			builder: NewPolicyBuilder(),
			want:    "",
		},
		{
			name:    "single directive",
			builder: NewPolicyBuilder().Add(ScriptSrc, "'self'"),
			want:    "script-src 'self'",
		},
		{
			name: "directives are emitted in stable sorted order",
			builder: NewPolicyBuilder().
				Add(StyleSrc, "'self'").
				Add(ScriptSrc, "'self'").
				Add(DefaultSrc, "'none'"),
			want: "default-src 'none'; script-src 'self'; style-src 'self'",
		},
		{
			name: "duplicate source expressions are removed",
			builder: NewPolicyBuilder().
				Add(ScriptSrc, "'self'", "example.com", "'self'", "example.com", "'self'"),
			want: "script-src 'self' example.com",
		},
		{
			name: "nonce placeholder in declared slots only",
			builder: NewPolicyBuilder().
				Add(DefaultSrc, "'self'").
				Add(ScriptSrc, "'strict-dynamic'", Nonce).
				Add(StyleSrc, Nonce),
			want: "default-src 'self'; script-src 'strict-dynamic' $NONCE; style-src $NONCE",
		},
		{
			name: "duplicate directive",
			builder: NewPolicyBuilder().
				Add(ScriptSrc, "'self'").
				Add(ScriptSrc, "example.com"),
			wantErr: true,
		},
		{
			name: "none combined with other source expressions",
			builder: NewPolicyBuilder().
				Add(ScriptSrc, "'none'", "'self'"),
			wantErr: true,
		},
		{
			name: "none combined with nonce",
			builder: NewPolicyBuilder().
				Add(ScriptSrc, Nonce, "'none'"),
			wantErr: true,
		},
		{
			name: "repeated none is deduplicated",
			builder: NewPolicyBuilder().
				Add(ScriptSrc, "'none'", "'none'"),
			want: "script-src 'none'",
		},
		{
			name: "semicolon in source expression",
			builder: NewPolicyBuilder().
				Add(ScriptSrc, "'self'; img-src *"),
			wantErr: true,
		},
		{
			name: "comma in source expression",
			builder: NewPolicyBuilder().
				Add(ScriptSrc, "example.com,evil.example"),
			wantErr: true,
		},
		{
			name: "whitespace in source expression",
			builder: NewPolicyBuilder().
				Add(ScriptSrc, "example.com evil.example"),
			wantErr: true,
		},
		{
			name: "empty source expression",
			builder: NewPolicyBuilder().
				Add(ScriptSrc, ""),
			wantErr: true,
		},
		{
			name: "empty directive name",
			builder: NewPolicyBuilder().
				Add("", "'self'"),
			wantErr: true,
		},
		{
			name: "directive name with separator",
			builder: NewPolicyBuilder().
				Add("script-src; img-src", "*"),
			wantErr: true,
		},
		{
			name: "directive without values",
			builder: NewPolicyBuilder().
				Add(ScriptSrc),
			wantErr: true,
		},
		{
			name: "valueless directive",
			builder: NewPolicyBuilder().
				Add(DefaultSrc, "'self'").
				Add(UpgradeInsecureRequests),
			want: "default-src 'self'; upgrade-insecure-requests",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.builder.Build()
			if (err != nil) != tt.wantErr {
				t.Errorf("PolicyBuilder.Build() error = %v, wantErr %v", err, tt.wantErr)

				return
			}

			if got != tt.want {
				t.Errorf("PolicyBuilder.Build() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPolicyBuilderStableOutput(t *testing.T) {
	builder := NewPolicyBuilder().
		Add(ImgSrc, "*").
		Add(ScriptSrc, Nonce).
		Add(DefaultSrc, "'self'").
		Add(StyleSrc, Nonce, "'unsafe-inline'")

	first := builder.MustBuild()

	for i := 0; i < 10; i++ {
		if got := builder.MustBuild(); got != first {
			t.Fatalf("PolicyBuilder.Build() is not stable: got %q, want %q", got, first)
		}
	}
}

func TestPolicyBuilderMustBuildPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("PolicyBuilder.MustBuild() did not panic on invalid policy")
		}
	}()

	NewPolicyBuilder().Add(ScriptSrc, "'none'", "'self'").MustBuild()
}

func TestPolicyBuilderNonceExpansionSlots(t *testing.T) {
	policy := NewPolicyBuilder().
		Add(DefaultSrc, "'self'").
		Add(ScriptSrc, Nonce).
		Add(StyleSrc, Nonce).
		MustBuild()

	// Emulate the middleware's per-request nonce substitution.
	expanded := strings.ReplaceAll(policy, Nonce, "'nonce-dGVzdA=='")

	want := "default-src 'self'; script-src 'nonce-dGVzdA=='; style-src 'nonce-dGVzdA=='"
	if expanded != want {
		t.Errorf("expanded policy = %q, want %q", expanded, want)
	}

	if strings.Contains(strings.Split(expanded, ";")[0], "nonce-") {
		t.Errorf("nonce expanded in default-src, which did not declare it: %q", expanded)
	}
}
