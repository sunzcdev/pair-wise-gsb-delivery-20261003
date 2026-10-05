package parser

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/util"
)

// canonicalAST renders a deterministic structural summary of the AST: kind,
// position and the node text source. NodeDump is avoided because it serializes
// struct fields via non-deterministic map iteration.
func canonicalAST(doc ast.Node, source []byte) string {
	var builder strings.Builder
	depth := 0
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			depth--
			return ast.WalkContinue, nil
		}
		if n.Kind() == ast.KindText {
			if tn, ok := n.(*ast.Text); ok {
				fmt.Fprintf(&builder, "%s%s text=%q pos=%d\n",
					strings.Repeat("  ", depth), n.Kind(), tn.Value.Value(source), n.Pos())
				depth++
				return ast.WalkContinue, nil
			}
		}
		fmt.Fprintf(&builder, "%s%s pos=%d\n", strings.Repeat("  ", depth), n.Kind(), n.Pos())
		depth++
		return ast.WalkContinue, nil
	})
	return builder.String()
}

// Rendering-independent AST equivalence is checked by comparing the structural
// summary: enabling tracing must not change the resulting AST.
func assertSameAST(t *testing.T, source string, options ...Option) {
	t.Helper()
	plain := New(options...)
	traced := New(append(append([]Option{}, options...), WithDelimiterTrace(func(DelimiterTraceEvent) {}))...)

	b := util.StringToReadOnlyBytes(source)
	plainAST := canonicalAST(plain.Parse(b), b)
	tracedAST := canonicalAST(traced.Parse(b), b)
	if plainAST != tracedAST {
		t.Fatalf("trace changed the AST for %q\n--- without trace ---\n%s\n--- with trace ---\n%s",
			source, plainAST, tracedAST)
	}
}

func TestDelimiterTraceDoesNotChangeAST(t *testing.T) {
	// Only the emphasis extension is available in this package; these inputs
	// exercise rejected pairs (Rule 9), partial consumption and nesting.
	sources := []string{
		"*a*",
		"**a**",
		"***a***",
		"a ***\"foo\"*** bar",
		"*a **b** c*",
		"**a*",
		"*a **",
		"***foo**",
		"*foo *bar**",
	}
	for _, source := range sources {
		assertSameAST(t, source)
	}
}

// Events must never carry source text and must report the run length and the
// remaining length rather than the contents.
func TestDelimiterTraceReportsLengthsNotSource(t *testing.T) {
	events := make([]DelimiterTraceEvent, 0)
	p := New(WithDelimiterTrace(func(e DelimiterTraceEvent) {
		events = append(events, e)
	}))
	source := "***x*y**"
	p.Parse(util.StringToReadOnlyBytes(source))
	if len(events) == 0 {
		t.Fatal("expected trace events")
	}
	for _, e := range events {
		if e.Remaining < 0 || e.RunLength < 0 {
			t.Fatalf("negative length in event: %+v", e)
		}
	}

	// The candidate sequence for ***secret*** must explain: a strong pair is
	// consumed first (2 chars), then the leftover single char consumes the
	// single closer as emphasis.
	var consumed []DelimiterTraceEvent
	for _, e := range events {
		if e.Stage == DelimiterStageCandidate && e.Result == DelimiterConsumed {
			consumed = append(consumed, e)
		}
	}
	if len(consumed) == 0 {
		t.Fatal("expected consumed candidate events")
	}
	totalConsumed := 0
	for _, e := range consumed {
		totalConsumed += e.Consumed
		if e.OpenerRemaining < 0 || e.CloserRemaining < 0 {
			t.Fatalf("negative remaining length: %+v", e)
		}
		if e.Handler == "" || e.Char == 0 {
			t.Fatalf("event must name handler and char: %+v", e)
		}
	}
	if totalConsumed != 3 {
		t.Fatalf("expected 3 delimiter chars consumed for ***, got %d", totalConsumed)
	}
}

// Repeated parses produce identical event sequences: ordering is deterministic.
func TestDelimiterTraceOrderIsStable(t *testing.T) {
	collect := func() []DelimiterTraceEvent {
		events := make([]DelimiterTraceEvent, 0)
		p := New(WithDelimiterTrace(func(e DelimiterTraceEvent) {
			events = append(events, e)
		}))
		p.Parse(util.StringToReadOnlyBytes("*a **b** c* and ***d***"))
		return events
	}
	first := collect()
	second := collect()
	if len(first) == 0 {
		t.Fatal("expected trace events")
	}
	if len(first) != len(second) {
		t.Fatalf("event count unstable: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("event %d differs between parses:\n%+v\n%+v", i, first[i], second[i])
		}
	}
}
