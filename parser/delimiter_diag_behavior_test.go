package parser

import (
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/util"
)

// diagnosticsOf builds the given parser, forces its one-time initialization,
// and returns the emitted build-time diagnostics.
func diagnosticsOf(t *testing.T, options ...Option) []DelimiterDiagnostic {
	t.Helper()
	var diagnostics []DelimiterDiagnostic
	options = append(options, WithDelimiterDiagnostics(func(d []DelimiterDiagnostic) {
		diagnostics = d
	}))
	p := New(options...)
	p.Parse(util.StringToReadOnlyBytes(""))
	return diagnostics
}

func traceOf(options ...Option) (*[]DelimiterTraceEvent, Parser) {
	events := &[]DelimiterTraceEvent{}
	options = append(options, WithDelimiterTrace(func(e DelimiterTraceEvent) {
		*events = append(*events, e)
	}))
	return events, New(options...)
}

func containsKind(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// Two extensions contend for '^'. The handler with the smaller priority value
// wins and the other is reported as fully shadowed.
func TestDelimiterProcessorPriorityWins(t *testing.T) {
	diagnostics := diagnosticsOf(t,
		WithInlineParsers(
			caret("alpha", 400, 1, -1),
			caret("beta", 500, 1, -1),
		),
	)
	if len(diagnostics) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %+v", len(diagnostics), diagnostics)
	}
	d := diagnostics[0]
	if d.Handler.Name != "beta" || d.ShadowedBy.Name != "alpha" {
		t.Fatalf("unexpected shadow: %s", d.Error())
	}
	if d.Handler.Priority != 500 || d.ShadowedBy.Priority != 400 {
		t.Fatalf("unexpected priorities: %s", d.Error())
	}
	if !strings.Contains(d.Error(), `trigger "^"`) {
		t.Fatalf("diagnostic is not locatable by trigger: %s", d.Error())
	}

	events, p := traceOf(
		WithInlineParsers(
			caret("alpha", 400, 1, -1),
			caret("beta", 500, 1, -1),
		),
	)
	doc := p.Parse(util.StringToReadOnlyBytes("^a^"))
	kinds := nodeKindNames(doc)
	if !containsKind(kinds, "alpha") || containsKind(kinds, "beta") {
		t.Fatalf("expected alpha to win, got kinds %v", kinds)
	}

	var tried []string
	for _, e := range *events {
		if e.Stage == DelimiterStageProcessor && e.Result == DelimiterTried {
			tried = append(tried, e.Handler)
		}
	}
	if len(tried) != 1 || tried[0] != "alpha" {
		t.Fatalf("expected only alpha to be tried, got %v (higher-priority handler must not run)", tried)
	}
}

// Equal priorities are resolved by registration order: the first registered
// handler wins and the tie-break is explained in the diagnostic.
func TestDelimiterProcessorTieRegistrationOrder(t *testing.T) {
	diagnostics := diagnosticsOf(t,
		WithInlineParsers(
			caret("first", 500, 1, -1),
			caret("second", 500, 1, -1),
		),
	)
	if len(diagnostics) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %+v", len(diagnostics), diagnostics)
	}
	d := diagnostics[0]
	if d.Handler.Name != "second" || d.ShadowedBy.Name != "first" {
		t.Fatalf("unexpected shadow: %s", d.Error())
	}
	if !strings.Contains(d.Error(), "tie-break") {
		t.Fatalf("diagnostic must explain the equal-priority tie-break: %s", d.Error())
	}

	p := New(WithInlineParsers(
		caret("first", 500, 1, -1),
		caret("second", 500, 1, -1),
	))
	kinds := nodeKindNames(p.Parse(util.StringToReadOnlyBytes("^a^")))
	if !containsKind(kinds, "first") || containsKind(kinds, "second") {
		t.Fatalf("expected first-registered handler to win tie, got %v", kinds)
	}
}

// Handlers sharing a trigger character but covering different run lengths
// coexist: single carets go to one handler, double carets to the other, and no
// shadow diagnostic is produced.
func TestDelimiterProcessorCoexistByLength(t *testing.T) {
	diagnostics := diagnosticsOf(t,
		WithInlineParsers(
			caret("single", 500, 1, 1),
			caret("double", 600, 2, -1),
		),
	)
	if len(diagnostics) != 0 {
		t.Fatalf("expected no diagnostics for length-split handlers, got %+v", diagnostics)
	}

	events, p := traceOf(
		WithInlineParsers(
			caret("single", 500, 1, 1),
			caret("double", 600, 2, -1),
		),
	)

	single := nodeKindNames(p.Parse(util.StringToReadOnlyBytes("^a^")))
	if !containsKind(single, "single") || containsKind(single, "double") {
		t.Fatalf("single caret run should select single, got %v", single)
	}
	double := nodeKindNames(p.Parse(util.StringToReadOnlyBytes("^^a^^")))
	if !containsKind(double, "double") || containsKind(double, "single") {
		t.Fatalf("double caret run should select double, got %v", double)
	}

	// The double run must first decline "single" (length out of range) before
	// "double" consumes it.
	var sequence []string
	for _, e := range *events {
		if e.Stage != DelimiterStageProcessor || e.RunLength != 2 {
			continue
		}
		outcome := e.Handler + ":"
		switch e.Result {
		case DelimiterTried:
			outcome += "tried"
		case DelimiterRejected:
			outcome += "rejected"
		case DelimiterConsumed:
			outcome += "consumed"
		}
		sequence = append(sequence, outcome)
	}
	want := []string{"single:tried", "single:rejected", "double:tried", "double:consumed"}
	if strings.Join(sequence, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected handler sequence for ^^ run: %v want %v", sequence, want)
	}
}

// Handlers without an explicit name receive a compatibility default name
// derived from their Go type; repeated registrations are made unique.
func TestDelimiterProcessorDefaultNames(t *testing.T) {
	events, p := traceOf(
		WithInlineParsers(
			util.Prioritized[InlineParser](&caretParser{proc: &caretProcessor{
				name: "", kind: mustKind("anonymous1"), min: 1, max: 1,
			}}, 500),
			util.Prioritized[InlineParser](&caretParser{proc: &caretProcessor{
				name: "", kind: mustKind("anonymous2"), min: 2, max: 2,
			}}, 600),
		),
	)
	p.Parse(util.StringToReadOnlyBytes("^a^"))
	p.Parse(util.StringToReadOnlyBytes("^^a^^"))
	names := map[string]bool{}
	for _, e := range *events {
		if e.Stage == DelimiterStageProcessor {
			names[e.Handler] = true
		}
	}
	// Both processors share the same unnamed Go type, so the default name is
	// derived once and disambiguated with a numeric suffix.
	if !names["caretProcessor"] || !names["caretProcessor-2"] {
		t.Fatalf("expected default names caretProcessor and caretProcessor-2, got %v", names)
	}
}

func mustKind(name string) ast.NodeKind {
	return ast.NewNodeKind(name)
}
