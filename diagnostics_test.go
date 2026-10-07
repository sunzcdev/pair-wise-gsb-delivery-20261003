package semver

import (
	"errors"
	"strings"
	"testing"
)

type wantDiag struct {
	kind    ConstraintErrorKind
	branch  int
	snippet string // the exact text the diagnostic span must select
}

func TestNewConstraintWithDiagnosticsErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []wantDiag
	}{
		{"empty string", "", []wantDiag{
			{ConstraintErrEmptyBranch, 0, ""},
		}},
		{"whitespace only", " ", []wantDiag{
			{ConstraintErrEmptyBranch, 0, " "},
		}},
		{"trailing empty branch", "1.2.3 || ", []wantDiag{
			{ConstraintErrEmptyBranch, 1, " "},
		}},
		{"trailing empty branch no space", "1.2.3 ||", []wantDiag{
			{ConstraintErrEmptyBranch, 1, ""},
		}},
		{"leading empty branch", "|| 1.2.3", []wantDiag{
			{ConstraintErrEmptyBranch, 0, ""},
		}},
		{"middle empty branch", "1.2.3 |||| 2.0.0", []wantDiag{
			{ConstraintErrEmptyBranch, 1, ""},
		}},
		{"garbage", "foo", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, "foo"},
		}},
		{"garbage between constraints", "1.2.3 foo 4.5.6", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, "foo"},
		}},
		{"double comma", "1.2.3,,4.5.6", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, ",,"},
		}},
		{"missing separator", "1.2.3>=4.0.0", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, ""},
		}},
		{"operator before hyphen range", ">= 1.2.3 - 4.5.6", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, ">="},
		}},
		{"dangling hyphen", "1.2.3 - ", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, "-"},
		}},
		{"leading hyphen", "- 4.5.6", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, "-"},
		}},
		{"extra version segment", "1.2.3.4.5", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, "."},
		}},
		{"out of range number", "99999999999999999999.0.0", []wantDiag{
			{ConstraintErrInvalidVersion, 0, "99999999999999999999.0.0"},
		}},
		{"out of range range endpoint", "99999999999999999999 - 4.5.6", []wantDiag{
			{ConstraintErrInvalidVersion, 0, "99999999999999999999"},
		}},
		{"nbsp separator", "1.2.3 2.0.0", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, " "},
		}},
		{"ideographic space separator", "1.2.3　2.0.0", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, "　"},
		}},
		{"nbsp around or", "1.2.3 || 2.0.0", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, " "},
			{ConstraintErrInvalidSyntax, 1, " "},
		}},
		{"multiple problems one branch", ">= 1.2.3 foo 99999999999999999999", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, "foo"},
			{ConstraintErrInvalidVersion, 0, "99999999999999999999"},
		}},
		{"problems in multiple branches", "foo || 1.2.3 || bar", []wantDiag{
			{ConstraintErrInvalidSyntax, 0, "foo"},
			{ConstraintErrInvalidSyntax, 2, "bar"},
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cons, diags := NewConstraintWithDiagnostics(tc.input)
			if cons != nil {
				t.Errorf("expected nil constraints for %q", tc.input)
			}
			if !diags.HasErrors() {
				t.Fatalf("expected diagnostics for %q", tc.input)
			}
			if len(diags.Diagnostics) != len(tc.want) {
				t.Fatalf("expected %d diagnostics for %q, got %d: %v",
					len(tc.want), tc.input, len(diags.Diagnostics), diags.Diagnostics)
			}
			for i, want := range tc.want {
				d := diags.Diagnostics[i]
				if d.Kind != want.kind {
					t.Errorf("diagnostic %d: expected kind %v, got %v", i, want.kind, d.Kind)
				}
				if d.Branch != want.branch {
					t.Errorf("diagnostic %d: expected branch %d, got %d", i, want.branch, d.Branch)
				}
				if d.Start < 0 || d.End > len(tc.input) || d.Start > d.End {
					t.Errorf("diagnostic %d: span %d-%d out of bounds for %q", i, d.Start, d.End, tc.input)
					continue
				}
				// The span must select exactly the problematic text so an
				// editor can highlight only that part.
				if got := tc.input[d.Start:d.End]; got != want.snippet {
					t.Errorf("diagnostic %d: span selects %q, expected %q", i, got, want.snippet)
				}
			}
		})
	}
}

func TestNewConstraintWithDiagnosticsSpans(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  [][]string // expected text selected by each span, per branch
	}{
		{
			"single comparison",
			"1.2.3",
			[][]string{{"1.2.3"}},
		},
		{
			"and group with comma",
			">= 1.2.3, < 2.0.0",
			[][]string{{">= 1.2.3", "< 2.0.0"}},
		},
		{
			"or branches",
			">= 1.2.3, < 2.0.0 || ^3.1.0",
			[][]string{{">= 1.2.3", "< 2.0.0"}, {"^3.1.0"}},
		},
		{
			"hyphen range",
			"1.2.3 - 4.5.6",
			[][]string{{"1.2.3 - 4.5.6", "1.2.3 - 4.5.6"}},
		},
		{
			"hyphen range among comparisons",
			"!= 1.0.0 1.2.3 - 4.5.6 || 6.0.0 - 7.0.0",
			[][]string{{"!= 1.0.0", "1.2.3 - 4.5.6", "1.2.3 - 4.5.6"}, {"6.0.0 - 7.0.0", "6.0.0 - 7.0.0"}},
		},
		{
			"whitespace variations",
			"\t>= 1.2.3\r\n< 2.0.0 ",
			[][]string{{">= 1.2.3", "< 2.0.0"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cons, diags := NewConstraintWithDiagnostics(tc.input)
			if cons == nil {
				t.Fatalf("expected constraints for %q, got diagnostics %v", tc.input, diags.Diagnostics)
			}
			if diags.HasErrors() {
				t.Fatalf("expected no diagnostics for %q, got %v", tc.input, diags.Diagnostics)
			}
			if len(diags.Spans) != len(tc.want) {
				t.Fatalf("expected %d branches for %q, got %d", len(tc.want), tc.input, len(diags.Spans))
			}
			for b, wantSpans := range tc.want {
				if len(diags.Spans[b]) != len(wantSpans) {
					t.Fatalf("branch %d: expected %d spans, got %d", b, len(wantSpans), len(diags.Spans[b]))
				}
				for i, want := range wantSpans {
					span := diags.Spans[b][i]
					if span.Branch != b || span.Index != i {
						t.Errorf("span %d/%d: expected branch/index %d/%d, got %d/%d",
							b, i, b, i, span.Branch, span.Index)
					}
					if got := tc.input[span.Start:span.End]; got != want {
						t.Errorf("span %d/%d: selects %q, expected %q", b, i, got, want)
					}
				}
			}
		})
	}
}

func TestNewConstraintWithDiagnosticsCompatible(t *testing.T) {
	valid := []string{
		"*",
		"1.2.3",
		"v1.2.3",
		"= 1.2.3",
		"!= 1.2.3",
		"> 1.2.3",
		"< 1.2.3",
		">= 1.2.3",
		"<= 1.2.3",
		"=> 1.2.3",
		"=< 1.2.3",
		"~ 1.2.3",
		"~> 1.2.3",
		"^ 1.2.3",
		"1.x",
		"1.2.x",
		">= 1.2.x",
		"1.2.3-alpha.1+build.5",
		"> 1.2.3, < 2.0.0",
		"> 1.2.3 < 2.0.0",
		"1.2.3,4.5.6",
		"1.2.3 , 4.5.6",
		"1.2.3 - 4.5.6",
		"1.2 - 1.4.5",
		">= 1.0.0 < 2.0.0 || >= 3.0.0",
		"1.2.3 - 4.5.6 || 6.0.0 - 7.0.0",
		"\t~2.0.0\r\n",
		"1.2.3, < 2.0.0",
		"!= 4.1.0",
	}

	versions := []string{
		"0.5.0",
		"1.0.0",
		"1.2.3",
		"1.5.0",
		"2.0.0",
		"4.5.6",
		"4.5.7",
		"6.5.0",
		"1.2.3-alpha",
	}

	for _, input := range valid {
		ref, err := NewConstraint(input)
		if err != nil {
			t.Fatalf("NewConstraint(%q) failed: %v", input, err)
		}
		cons, diags := NewConstraintWithDiagnostics(input)
		if diags.HasErrors() {
			t.Errorf("NewConstraintWithDiagnostics(%q) reported diagnostics: %v", input, diags.Diagnostics)
			continue
		}
		if cons.String() != ref.String() {
			t.Errorf("String() mismatch for %q: %q != %q", input, cons.String(), ref.String())
		}
		for _, vs := range versions {
			v, err := NewVersion(vs)
			if err != nil {
				t.Fatalf("NewVersion(%q) failed: %v", vs, err)
			}
			if cons.Check(v) != ref.Check(v) {
				t.Errorf("Check(%q) mismatch for constraint %q", vs, input)
			}
		}
	}

	invalid := []string{
		"",
		" ",
		"foo",
		"1.2.3 || ",
		"|| 1.2.3",
		"1.2.3 |||| 2.0.0",
		"1.2.3,,4.5.6",
		"1.2.3 foo 4.5.6",
		">= 1.2.3 - 4.5.6",
		"99999999999999999999.0.0",
		"1.2.3 2.0.0",
		"1.2.3.4.5",
		"1.2.3 - ",
		"- 4.5.6",
		"1.2.3>=4.0.0",
		"1.2.3 || 2.0.0 foo 3.0.0",
	}

	for _, input := range invalid {
		if _, err := NewConstraint(input); err == nil {
			t.Errorf("NewConstraint(%q) unexpectedly succeeded", input)
		}
		cons, diags := NewConstraintWithDiagnostics(input)
		if !diags.HasErrors() {
			t.Errorf("NewConstraintWithDiagnostics(%q) reported no diagnostics", input)
		}
		if cons != nil {
			t.Errorf("NewConstraintWithDiagnostics(%q) returned non-nil constraints on error", input)
		}
	}
}

func TestNewConstraintWithDiagnosticsLimits(t *testing.T) {
	long := strings.Repeat("1", MaxConstraintLen+1)
	cons, diags := NewConstraintWithDiagnostics(long)
	if cons != nil {
		t.Error("expected nil constraints for too long input")
	}
	if len(diags.Diagnostics) != 1 || diags.Diagnostics[0].Kind != ConstraintErrTooLong {
		t.Fatalf("expected a single too long diagnostic, got %v", diags.Diagnostics)
	}
	d := diags.Diagnostics[0]
	if d.Branch != -1 || d.Start != 0 || d.End != len(long) {
		t.Errorf("unexpected too long diagnostic position: %+v", d)
	}
	if !errors.Is(d, ErrConstraintTooLong) {
		t.Errorf("expected diagnostic to match ErrConstraintTooLong")
	}

	parts := make([]string, MaxConstraintGroups+1)
	for i := range parts {
		parts[i] = "1.2.3"
	}
	many := strings.Join(parts, " || ")
	cons, diags = NewConstraintWithDiagnostics(many)
	if cons != nil {
		t.Error("expected nil constraints for too many groups")
	}
	if len(diags.Diagnostics) != 1 || diags.Diagnostics[0].Kind != ConstraintErrTooManyGroups {
		t.Fatalf("expected a single too many groups diagnostic, got %v", diags.Diagnostics)
	}
	if !errors.Is(diags.Diagnostics[0], ErrTooManyConstraintGroups) {
		t.Errorf("expected diagnostic to match ErrTooManyConstraintGroups")
	}
}

func TestConstraintDiagnosticErrorString(t *testing.T) {
	d := ConstraintDiagnostic{
		Kind:    ConstraintErrInvalidSyntax,
		Branch:  1,
		Start:   5,
		End:     8,
		Message: "unexpected text: \"foo\"",
	}
	if s := d.Error(); !strings.Contains(s, "foo") || !strings.Contains(s, "5-8") {
		t.Errorf("unexpected error string: %q", s)
	}
	if ConstraintErrInvalidVersion.String() != "invalid version" {
		t.Errorf("unexpected kind string: %q", ConstraintErrInvalidVersion.String())
	}
}
