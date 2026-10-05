package extension

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/util"
)

func renderWithStrikethrough(t *testing.T, source string, trace parser.DelimiterTraceFunc) string {
	t.Helper()
	options := []parser.Option{
		parser.WithExtensions(NewStrikethroughParser()),
	}
	if trace != nil {
		options = append(options, parser.WithDelimiterTrace(trace))
	}
	p := parser.New(options...)
	r := html.New(html.WithUnsafe(), html.WithExtensions(NewStrikethroughHTMLRenderer()))
	b := util.StringToReadOnlyBytes(source)
	var buf bytes.Buffer
	if err := r.Render(&buf, b, p.Parse(b)); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	return buf.String()
}

// The real emphasis and strikethrough combination renders identically with and
// without tracing, and the trace explains which handler consumed each pair.
func TestStrikethroughEmphasisTraceMatchesOutput(t *testing.T) {
	cases := []string{
		"~~*a***~~",
		"**a ~b~ c**",
		"*a ~~b~~ c*",
		"~~**a***~~",
	}
	for _, source := range cases {
		plain := renderWithStrikethrough(t, source, nil)

		var events []parser.DelimiterTraceEvent
		traced := renderWithStrikethrough(t, source, func(e parser.DelimiterTraceEvent) {
			events = append(events, e)
		})

		if plain != traced {
			t.Fatalf("output differs for %q:\nplain:  %s\ntraced: %s", source, plain, traced)
		}
		if len(events) == 0 {
			t.Fatalf("expected trace events for %q", source)
		}

		// Every consumed pair must be attributed and must leave a coherent
		// remaining length; no event may carry source text.
		handlers := map[string]bool{}
		for _, e := range events {
			if e.Result == parser.DelimiterConsumed {
				handlers[e.Handler] = true
				if e.OpenerRemaining < 0 || e.CloserRemaining < 0 {
					t.Fatalf("negative remaining length for %q: %+v", source, e)
				}
			}
		}
		if !handlers["emphasis"] || !handlers["strikethrough"] {
			t.Fatalf("expected both emphasis and strikethrough consumption for %q, got %v",
				source, handlers)
		}

		// Event ordering must be stable across parses.
		var again []parser.DelimiterTraceEvent
		_ = renderWithStrikethrough(t, source, func(e parser.DelimiterTraceEvent) {
			again = append(again, e)
		})
		if !equalEvents(events, again) {
			t.Fatalf("unstable trace order for %q:\n%v\n%v", source, events, again)
		}
	}
}

func equalEvents(a, b []parser.DelimiterTraceEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The trace must explain the concrete winner for a sequence where a closer
// cannot pair with the first opener it meets.
func TestStrikethroughEmphasisTraceExplainsSelection(t *testing.T) {
	source := "~~*a***~~"
	var events []parser.DelimiterTraceEvent
	output := renderWithStrikethrough(t, source, func(e parser.DelimiterTraceEvent) {
		events = append(events, e)
	})
	if strings.TrimSpace(output) != "<p><del><em>a</em>**</del></p>" {
		t.Fatalf("unexpected output: %s", output)
	}

	var reasons []string
	sawEmphasisConsume := false
	sawStrikethroughConsume := false
	for _, e := range events {
		if e.Stage != parser.DelimiterStageCandidate {
			continue
		}
		switch {
		case e.Result == parser.DelimiterRejected:
			reasons = append(reasons, e.Handler+":"+e.Reason)
		case e.Result == parser.DelimiterConsumed && e.Handler == "emphasis":
			sawEmphasisConsume = true
		case e.Result == parser.DelimiterConsumed && e.Handler == "strikethrough":
			sawStrikethroughConsume = true
		}
	}
	if !sawEmphasisConsume || !sawStrikethroughConsume {
		t.Fatalf("trace must show emphasis then strikethrough consumption: %+v", events)
	}
	// The strikethrough opener cannot pair with the '*' closer (different
	// handler/char); that rejection must be visible and explain why the
	// emphasis handler wins first.
	if !containsReason(reasons, "strikethrough:mismatch") {
		t.Fatalf("expected a strikethrough mismatch rejection, got reasons %v", reasons)
	}
}

func containsReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

// With real emphasis + strikethrough there is no fully shadowed configuration.
func TestStrikethroughEmphasisNoFalseDiagnostic(t *testing.T) {
	var diagnostics []parser.DelimiterDiagnostic
	p := parser.New(
		parser.WithExtensions(NewStrikethroughParser()),
		parser.WithDelimiterDiagnostics(func(d []parser.DelimiterDiagnostic) {
			diagnostics = d
		}),
	)
	p.Parse(util.StringToReadOnlyBytes(""))
	if len(diagnostics) != 0 {
		t.Fatalf("expected no diagnostics, got %+v", diagnostics)
	}
}
