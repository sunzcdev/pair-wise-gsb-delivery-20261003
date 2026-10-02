package cspbuilder

import (
	"strings"
	"testing"
)

func TestPolicyBuilder_Build(t *testing.T) {
	tests := []struct {
		name    string
		builder *PolicyBuilder
		want    string
		wantErr bool
	}{
		{
			name:    "empty builder",
			builder: &PolicyBuilder{},
			want:    "",
		},
		{
			name:    "single directive",
			builder: (&PolicyBuilder{}).Add(DefaultSrc, "'self'"),
			want:    "default-src 'self'",
		},
		{
			name: "stable output order",
			builder: (&PolicyBuilder{}).
				Add(StyleSrc, "'self'").
				Add(DefaultSrc, "'none'").
				Add(ScriptSrc, "'self'", "example.com").
				Add(ImgSrc, "*"),
			want: "default-src 'none'; img-src *; script-src 'self' example.com; style-src 'self'",
		},
		{
			name:    "directive without values",
			builder: (&PolicyBuilder{}).Add(UpgradeInsecureRequests),
			want:    "upgrade-insecure-requests",
		},
		{
			name:    "duplicate source expressions are removed",
			builder: (&PolicyBuilder{}).Add(ScriptSrc, "'self'", "example.com", "'self'", "example.com", "example.com"),
			want:    "script-src 'self' example.com",
		},
		{
			name:    "duplicate none collapses to single value",
			builder: (&PolicyBuilder{}).Add(DefaultSrc, "'none'", "'none'"),
			want:    "default-src 'none'",
		},
		{
			name:    "nonce slot",
			builder: (&PolicyBuilder{}).Add(ScriptSrc, "'self'", Nonce).Add(StyleSrc, Nonce),
			want:    "script-src 'self' $NONCE; style-src $NONCE",
		},
		{
			name:    "duplicate directive",
			builder: (&PolicyBuilder{}).Add(DefaultSrc, "'self'").Add(DefaultSrc, "example.com"),
			wantErr: true,
		},
		{
			name:    "none conflicts with other values",
			builder: (&PolicyBuilder{}).Add(DefaultSrc, "'none'", "'self'"),
			wantErr: true,
		},
		{
			name:    "none conflicts with preceding values",
			builder: (&PolicyBuilder{}).Add(DefaultSrc, "'self'", "'none'"),
			wantErr: true,
		},
		{
			name:    "none conflicts with nonce",
			builder: (&PolicyBuilder{}).Add(ScriptSrc, Nonce, "'none'"),
			wantErr: true,
		},
		{
			name:    "empty directive name",
			builder: (&PolicyBuilder{}).Add("", "'self'"),
			wantErr: true,
		},
		{
			name:    "directive name with semicolon",
			builder: (&PolicyBuilder{}).Add("default-src; script-src", "'self'"),
			wantErr: true,
		},
		{
			name:    "directive name with whitespace",
			builder: (&PolicyBuilder{}).Add("default src", "'self'"),
			wantErr: true,
		},
		{
			name:    "empty source expression",
			builder: (&PolicyBuilder{}).Add(DefaultSrc, "'self'", ""),
			wantErr: true,
		},
		{
			name:    "source expression with semicolon",
			builder: (&PolicyBuilder{}).Add(DefaultSrc, "'self'; upgrade-insecure-requests"),
			wantErr: true,
		},
		{
			name:    "source expression with comma",
			builder: (&PolicyBuilder{}).Add(DefaultSrc, "example.com,evil.com"),
			wantErr: true,
		},
		{
			name:    "source expression with whitespace",
			builder: (&PolicyBuilder{}).Add(DefaultSrc, "'self' 'unsafe-inline'"),
			wantErr: true,
		},
		{
			name:    "source expression with control character",
			builder: (&PolicyBuilder{}).Add(DefaultSrc, "example.com\n"),
			wantErr: true,
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

func TestPolicyBuilder_Build_Deterministic(t *testing.T) {
	builder := (&PolicyBuilder{}).
		Add(StyleSrc, "'self'").
		Add(DefaultSrc, "'self'").
		Add(ScriptSrc, Nonce).
		Add(ImgSrc, "data:")

	first, err := builder.Build()
	if err != nil {
		t.Fatalf("PolicyBuilder.Build() unexpected error: %v", err)
	}

	for i := 0; i < 10; i++ {
		got, err := builder.Build()
		if err != nil {
			t.Fatalf("PolicyBuilder.Build() unexpected error: %v", err)
		}

		if got != first {
			t.Errorf("PolicyBuilder.Build() = %q, want stable output %q", got, first)
		}
	}
}

func TestPolicyBuilder_MustBuild(t *testing.T) {
	t.Run("valid policy", func(t *testing.T) {
		got := (&PolicyBuilder{}).Add(DefaultSrc, "'self'").MustBuild()
		if got != "default-src 'self'" {
			t.Errorf("PolicyBuilder.MustBuild() = %q, want %q", got, "default-src 'self'")
		}
	})

	t.Run("invalid policy panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("PolicyBuilder.MustBuild() did not panic on invalid policy")
			}
		}()

		(&PolicyBuilder{}).Add(DefaultSrc, "'none'", "'self'").MustBuild()
	})
}

func TestPolicyBuilder_NonceOnlyInDeclaredSlots(t *testing.T) {
	policy, err := (&PolicyBuilder{}).
		Add(DefaultSrc, "'self'").
		Add(ScriptSrc, "'self'", Nonce).
		Add(StyleSrc, "'self'").
		Build()
	if err != nil {
		t.Fatalf("PolicyBuilder.Build() unexpected error: %v", err)
	}

	if strings.Count(policy, Nonce) != 1 {
		t.Errorf("expected exactly one nonce slot, got policy %q", policy)
	}

	if !strings.Contains(policy, "script-src 'self' "+Nonce) {
		t.Errorf("expected nonce slot in script-src, got policy %q", policy)
	}

	if strings.Contains(policy, "style-src 'self' "+Nonce) || strings.Contains(policy, "default-src 'self' "+Nonce) {
		t.Errorf("nonce slot leaked into a directive that did not declare it: %q", policy)
	}
}
