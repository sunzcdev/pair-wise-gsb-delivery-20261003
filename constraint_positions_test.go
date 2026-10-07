package semver

import (
	"errors"
	"strings"
	"testing"
)

func parseDiags(t *testing.T, c string) []ConstraintDiagnostic {
	t.Helper()
	_, _, err := NewConstraintWithPositions(c)
	if err == nil {
		t.Fatalf("expected error for %q", c)
	}
	var perr *ConstraintParseError
	if !errors.As(err, &perr) {
		t.Fatalf("expected *ConstraintParseError for %q, got %T", c, err)
	}
	return perr.Diagnostics
}

func diagKinds(diags []ConstraintDiagnostic) []ConstraintDiagnosticKind {
	out := make([]ConstraintDiagnosticKind, len(diags))
	for i, d := range diags {
		out[i] = d.Kind
	}
	return out
}

func hasKind(kind ConstraintDiagnosticKind, diags []ConstraintDiagnostic) bool {
	for _, d := range diags {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

func TestPositionedSuccessComparisons(t *testing.T) {
	c := ">= 1.2.3 < 2.0 || 1.0.0 - 2.0.0"
	cs, pos, err := NewConstraintWithPositions(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The returned Constraints behave exactly like NewConstraint's result.
	legacy, err := NewConstraint(c)
	if err != nil {
		t.Fatalf("legacy parse error: %v", err)
	}
	if cs.String() != legacy.String() {
		t.Fatalf("String mismatch: %q vs %q", cs.String(), legacy.String())
	}

	if len(pos.Branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(pos.Branches))
	}

	first := pos.Branches[0].Comparisons
	if len(first) != 2 {
		t.Fatalf("expected 2 comparisons in branch 0, got %d", len(first))
	}
	if got := first[0].Operator.Slice(c); got != ">=" {
		t.Fatalf("first operator = %q, want >=", got)
	}
	if got := first[0].Version.Slice(c); got != "1.2.3" {
		t.Fatalf("first version = %q, want 1.2.3", got)
	}
	if first[0].HyphenRange {
		t.Fatalf("first comparison should not be a hyphen range")
	}
	if got := first[1].Item.Slice(c); got != "< 2.0" {
		t.Fatalf("second item = %q, want < 2.0", got)
	}

	ranges := pos.Branches[1].Comparisons
	if len(ranges) != 2 {
		t.Fatalf("expected 2 range bounds, got %d", len(ranges))
	}
	for _, cmp := range ranges {
		if !cmp.HyphenRange {
			t.Fatalf("expected hyphen range bound, got %+v", cmp)
		}
		if cmp.Operator != nil {
			t.Fatalf("hyphen range must not expose a synthesized operator")
		}
		if got := cmp.Item.Slice(c); got != "1.0.0 - 2.0.0" {
			t.Fatalf("range item = %q, want the whole hyphen range", got)
		}
	}
	if got := ranges[0].Version.Slice(c); got != "1.0.0" {
		t.Fatalf("low bound = %q, want 1.0.0", got)
	}
	if got := ranges[1].Version.Slice(c); got != "2.0.0" {
		t.Fatalf("high bound = %q, want 2.0.0", got)
	}
}

func TestPositionedCheckParity(t *testing.T) {
	c := ">= 1.0.0 < 2.0.0 || 3.0.0 - 4.0.0"
	got, _, err := NewConstraintWithPositions(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	legacy, _ := NewConstraint(c)

	for _, v := range []string{"0.9.0", "1.5.0", "2.0.0", "3.5.0", "4.0.1"} {
		ver, _ := NewVersion(v)
		if got.Check(ver) != legacy.Check(ver) {
			t.Fatalf("Check mismatch for %s: new=%v old=%v", v, got.Check(ver), legacy.Check(ver))
		}
	}
}

func TestPositionedEmptyBranch(t *testing.T) {
	c := "1.0.0 ||"
	diags := parseDiags(t, c)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(diags))
	}
	d := diags[0]
	if d.Kind != DiagEmptyBranch || d.Branch != 1 || d.Span.Start != 8 || d.Span.End != 8 {
		t.Fatalf("unexpected diagnostic: %+v", d)
	}

	for _, c := range []string{"", "   ", "|| 1.0.0", "1.0.0 || || 2.0.0"} {
		diags := parseDiags(t, c)
		if !hasKind(DiagEmptyBranch, diags) {
			t.Fatalf("expected empty branch diagnostic for %q, got %+v", c, diags)
		}
	}
}

func TestPositionedOutOfRangeNumber(t *testing.T) {
	c := ">= 99999999999999999999999999.0.0"
	diags := parseDiags(t, c)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(diags))
	}
	d := diags[0]
	if d.Kind != DiagNumberOutOfRange || d.Branch != 0 {
		t.Fatalf("unexpected diagnostic: %+v", d)
	}
	if got := d.Span.Slice(c); got != "99999999999999999999999999" {
		t.Fatalf("span = %q, want the overflowing segment only", got)
	}
}

func TestPositionedHyphenRangeBound(t *testing.T) {
	// Only the invalid upper bound is marked; the lower bound and hyphen stay
	// unmarked even though the internal rewrite produces ">= ... <=".
	c := "1.0.0 - 99999999999999999999999999.0.0"
	diags := parseDiags(t, c)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %+v", len(diags), diags)
	}
	d := diags[0]
	if d.Kind != DiagNumberOutOfRange {
		t.Fatalf("kind = %v, want number_out_of_range", d.Kind)
	}
	if got := d.Span.Slice(c); got != "99999999999999999999999999" {
		t.Fatalf("span = %q, want the upper bound digits only", got)
	}
}

func TestPositionedMultiByteWhitespace(t *testing.T) {
	// An ideographic space (3 bytes) must be marked precisely rather than
	// highlighting the whole row.
	c := ">=1.1\u3000<2"
	diags := parseDiags(t, c)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %+v", len(diags), diags)
	}
	d := diags[0]
	if d.Kind != DiagInvalidSeparator {
		t.Fatalf("kind = %v, want invalid_separator", d.Kind)
	}
	if got := d.Span.Slice(c); got != "\u3000" {
		t.Fatalf("span = %q, want the U+3000 bytes only", got)
	}
	if d.Span.End-d.Span.Start != 3 {
		t.Fatalf("span width = %d, want 3 bytes", d.Span.End-d.Span.Start)
	}
}

func TestPositionedMultipleProblemsInOneBranch(t *testing.T) {
	// Two independent problems in one branch are reported together.
	c := ">= 99999999999999999999999999.0.0 < 18446744073709551616.0.0"
	diags := parseDiags(t, c)
	if len(diags) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d: %+v", len(diags), diags)
	}
	for _, d := range diags {
		if d.Kind != DiagNumberOutOfRange || d.Branch != 0 {
			t.Fatalf("unexpected diagnostic: %+v", d)
		}
	}
	first, second := diags[0].Span.Slice(c), diags[1].Span.Slice(c)
	if first == second || !strings.HasPrefix(first, "9999") || second != "18446744073709551616" {
		t.Fatalf("expected distinct overflow spans, got %q and %q", first, second)
	}
}

func TestPositionedBranchIndexing(t *testing.T) {
	c := "1.0.0 || >= 99999999999999999999999999.0.0 || 2.0.0"
	diags := parseDiags(t, c)
	if len(diags) != 1 || diags[0].Branch != 1 {
		t.Fatalf("expected a single branch-1 diagnostic, got %+v", diags)
	}
}

func TestPositionedInvalidPrerelease(t *testing.T) {
	c := "1.2.3-beta.01"
	diags := parseDiags(t, c)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(diags))
	}
	d := diags[0]
	if d.Kind != DiagInvalidVersion {
		t.Fatalf("kind = %v, want invalid_version", d.Kind)
	}
	if got := d.Span.Slice(c); got != "beta.01" {
		t.Fatalf("span = %q, want the prerelease identifier", got)
	}
}

func TestPositionedSentinelErrors(t *testing.T) {
	long := strings.Repeat(">= 1.0.0 || ", 100)
	_, _, err := NewConstraintWithPositions(long)
	if !errors.Is(err, ErrConstraintTooLong) {
		t.Fatalf("expected ErrConstraintTooLong, got %v", err)
	}
	var perr *ConstraintParseError
	errors.As(err, &perr)
	if !perr.Diagnostics[0].Fatal || perr.Diagnostics[0].Kind != DiagTooLong {
		t.Fatalf("expected fatal too_long diagnostic, got %+v", perr.Diagnostics[0])
	}

	parts := make([]string, MaxConstraintGroups+1)
	for i := range parts {
		parts[i] = ">= 1.0.0"
	}
	_, _, err = NewConstraintWithPositions(strings.Join(parts, " || "))
	if !errors.Is(err, ErrTooManyConstraintGroups) {
		t.Fatalf("expected ErrTooManyConstraintGroups, got %v", err)
	}
}

func TestPositionedSyntaxJunk(t *testing.T) {
	c := "1.0.0 - foo"
	diags := parseDiags(t, c)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic (the bad upper bound), got %d: %+v", len(diags), diags)
	}
	if diags[0].Kind != DiagSyntax || diags[0].Span.Slice(c) != "foo" {
		t.Fatalf("unexpected diagnostic: %+v", diags[0])
	}
}

func TestPositionedParityTable(t *testing.T) {
	tests := []struct {
		in  string
		err bool
	}{
		{">= 1.2.3 < 2.0", false},
		{"1.0.0 - 2.0.0, <=2.0.0", false},
		{"3 - 4 || => 3.0, < 4", false},
		{"2 - 3 4.0.0 - 5.1", false},
		{"1.0.0,2.0.0", false},
		{">= bar", true},
		{"12.3.4.1234", true},
		{"12.3.34~ 1.2.3", true},
		{"v1.2.3-beta.01", true},
		{"1.0.0 ||", true},
		{"", true},
		{"1|2", true},
	}
	for _, tc := range tests {
		_, legacyErr := NewConstraint(tc.in)
		cs, _, err := NewConstraintWithPositions(tc.in)
		if (legacyErr != nil) != (err != nil) || (legacyErr != nil) != tc.err {
			t.Fatalf("parity mismatch for %q: legacy=%v new=%v wantErr=%v", tc.in, legacyErr, err, tc.err)
		}
		if legacyErr == nil {
			legacy, _ := NewConstraint(tc.in)
			if cs.String() != legacy.String() {
				t.Fatalf("String mismatch for %q: %q vs %q", tc.in, cs.String(), legacy.String())
			}
		}
	}
}
