package extension

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/util"
)

func gfmTraceParser() parser.Parser {
	return parser.New(
		parser.WithExtensions(NewStrikethroughParser()),
	)
}

func gfmTraceRenderer() html.Renderer {
	return html.New(
		html.WithUnsafe(),
		html.WithExtensions(NewStrikethroughHTMLRenderer()),
	)
}

func renderWithTrace(t *testing.T, source string, withTrace bool) (string, []parser.DelimiterTraceEvent) {
	t.Helper()
	p := gfmTraceParser()
	r := gfmTraceRenderer()
	var events []parser.DelimiterTraceEvent
	var opts []parser.ParseOption
	if withTrace {
		opts = append(opts, parser.WithDelimiterTrace(func(e parser.DelimiterTraceEvent) {
			events = append(events, e)
		}))
	}
	src := util.StringToReadOnlyBytes(source)
	var buf bytes.Buffer
	if err := r.Render(&buf, src, p.Parse(src, opts...)); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String(), events
}

// TestDelimiterTraceEmphasisStrikethrough verifies that tracing a real
// emphasis + strikethrough combination produces the same output, a stable
// event order across parses, and events that explain the actual processor
// selection without embedding source text.
func TestDelimiterTraceEmphasisStrikethrough(t *testing.T) {
	source := "this is ~~**important**~~ and *also ~struck~ text*"

	plain, _ := renderWithTrace(t, source, false)
	traced, events1 := renderWithTrace(t, source, true)
	_, events2 := renderWithTrace(t, source, true)

	if plain != traced {
		t.Fatalf("tracing changed output:\nplain:  %q\ntraced: %q", plain, traced)
	}
	if want := "<p>this is <del><strong>important</strong></del> and " +
		"<em>also <del>struck</del> text</em></p>\n"; plain != want {
		t.Fatalf("unexpected html: %q", plain)
	}
	if len(events1) == 0 {
		t.Fatal("expected trace events")
	}
	if len(events1) != len(events2) {
		t.Fatalf("trace length unstable: %d vs %d", len(events1), len(events2))
	}
	for i := range events1 {
		if events1[i].String() != events2[i].String() {
			t.Fatalf("trace order unstable at event %d:\n%s\nvs\n%s",
				i, events1[i], events2[i])
		}
	}
	for _, e := range events1 {
		if strings.Contains(e.String(), "important") ||
			strings.Contains(e.String(), "struck") {
			t.Fatalf("trace must not embed source text: %s", e)
		}
	}

	// Emphasis and strikethrough must pair only with their own processors, and
	// the consumed events must explain the actual spans in the output.
	var consumed []parser.DelimiterTraceEvent
	processorPairs := map[string]bool{}
	for _, e := range events1 {
		if e.Kind != parser.DelimiterTracePair {
			continue
		}
		processorPairs[e.OpenerProcessor+"|"+e.CloserProcessor] = true
		if e.Outcome == parser.DelimiterTraceConsumed {
			consumed = append(consumed, e)
		}
	}
	if processorPairs["emphasis|emphasis"] != true {
		t.Fatalf("expected emphasis<->emphasis pair events, got %v", processorPairs)
	}
	if processorPairs["strikethrough|strikethrough"] != true {
		t.Fatalf("expected strikethrough<->strikethrough pair events, got %v", processorPairs)
	}
	if len(processorPairs) != 2 {
		t.Fatalf("processors must not cross-pair, got %v", processorPairs)
	}

	// Two strikethrough spans (~~...~~ and ~...~) and two emphasis spans
	// (**...** strong + *...* emphasis) are consumed, in source order.
	var kinds []string
	for _, e := range consumed {
		switch e.Consumed {
		case 2:
			kinds = append(kinds, e.CloserProcessor+":2")
		case 1:
			kinds = append(kinds, e.CloserProcessor+":1")
		}
	}
	joined := strings.Join(kinds, ",")
	if !strings.Contains(joined, "strikethrough:2") ||
		!strings.Contains(joined, "strikethrough:1") {
		t.Fatalf("expected both strikethrough consumptions, got %q", joined)
	}
	if strings.Contains(joined, "emphasis:2") == false {
		t.Fatalf("expected strong (2-char) emphasis consumption, got %q", joined)
	}

	// Remaining lengths must be present and non-negative on consumed events.
	for _, e := range consumed {
		if e.RemainingLength < 0 || e.CloserRemainingLength < 0 {
			t.Fatalf("consumed event missing remaining lengths: %s", e)
		}
	}
}

// TestDelimiterTraceStrikethroughLengthWindow documents that strikethrough
// declares the 1..2 length window: a run of three tildes rejects the built-in
// processor at run-scan time while still coexisting with emphasis on '*'.
func TestDelimiterTraceStrikethroughLengthWindow(t *testing.T) {
	_, events := renderWithTrace(t, "a ~~~not struck~~~ and **bold** b", true)
	sawReject := false
	sawEmphasisConsume := false
	for _, e := range events {
		if e.Kind == parser.DelimiterTraceRun &&
			e.Processor == "strikethrough" &&
			e.Outcome == parser.DelimiterTraceRejected &&
			strings.Contains(e.Reason, "length") {
			sawReject = true
		}
		if e.Kind == parser.DelimiterTracePair &&
			e.CloserProcessor == "emphasis" &&
			e.Outcome == parser.DelimiterTraceConsumed {
			sawEmphasisConsume = true
		}
	}
	if !sawReject {
		t.Fatal("expected a run-level length rejection for ~~~")
	}
	if !sawEmphasisConsume {
		t.Fatal("expected emphasis to remain consumable alongside the rejected tilde run")
	}
}
