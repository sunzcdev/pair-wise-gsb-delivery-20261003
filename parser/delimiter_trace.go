package parser

import "strconv"

// DelimiterTraceKind identifies a phase of the delimiter processor selection
// process.
type DelimiterTraceKind int

const (
	// DelimiterTraceRun is emitted while a candidate processor scans a run of
	// trigger characters at one source position.
	DelimiterTraceRun DelimiterTraceKind = iota

	// DelimiterTracePair is emitted while a closer searches the delimiter list
	// for a matching opener.
	DelimiterTracePair
)

// String implements fmt.Stringer.
func (k DelimiterTraceKind) String() string {
	switch k {
	case DelimiterTraceRun:
		return "run"
	case DelimiterTracePair:
		return "pair"
	default:
		return "unknown"
	}
}

// DelimiterTraceOutcome describes what happened to a delimiter candidate.
type DelimiterTraceOutcome int

const (
	// DelimiterTraceAttempted means the candidate was considered but the
	// selection process is still ongoing. It is used for run scans that push a
	// new delimiter onto the delimiter list.
	DelimiterTraceAttempted DelimiterTraceOutcome = iota

	// DelimiterTraceRejected means the candidate was tried but declined, and
	// the selection process continued with another candidate or gave up.
	DelimiterTraceRejected

	// DelimiterTraceConsumed means the candidate matched and delimiter
	// characters were consumed to build a span.
	DelimiterTraceConsumed
)

// String implements fmt.Stringer.
func (o DelimiterTraceOutcome) String() string {
	switch o {
	case DelimiterTraceAttempted:
		return "attempted"
	case DelimiterTraceRejected:
		return "rejected"
	case DelimiterTraceConsumed:
		return "consumed"
	default:
		return "unknown"
	}
}

// DelimiterTraceEvent records one decision of the delimiter processor
// selection process.
//
// Events intentionally carry only delimiter metadata, run lengths and source
// offsets. They never embed source text, so enabling tracing cannot leak the
// parsed document into the callback.
type DelimiterTraceEvent struct {
	// Kind is the selection phase that produced the event.
	Kind DelimiterTraceKind

	// Outcome is what happened to the candidate.
	Outcome DelimiterTraceOutcome

	// Reason explains why a candidate was rejected or consumed. It is empty for
	// attempted events.
	Reason string

	// Processor is the diagnostic name of the candidate processor.
	Processor string

	// OpenerProcessor and CloserProcessor are the names of the two processors
	// involved in a pair event. OpenerProcessor is empty for run events.
	OpenerProcessor string
	CloserProcessor string

	// Char is the delimiter character as a single-byte string, such as "*".
	Char string

	// Position is the absolute source offset where the scanned run starts, or
	// the opener position for pair events.
	Position int

	// CloserPosition is the absolute source offset of the closer for pair
	// events. It is -1 for run events.
	CloserPosition int

	// RunLength is the scanned delimiter-run length.
	RunLength int

	// RemainingLength is the remaining run length after the decision: for
	// rejected events the length before the decision, and for consumed events
	// the opener's remaining length after characters were consumed.
	RemainingLength int

	// CloserRemainingLength is the closer's remaining length after a pair was
	// consumed. It is -1 otherwise.
	CloserRemainingLength int

	// Consumed is the number of characters consumed from each side of a matched
	// pair. It is 0 otherwise.
	Consumed int
}

// String implements fmt.Stringer. The representation is deterministic and does
// not contain source text.
func (e DelimiterTraceEvent) String() string {
	switch e.Kind {
	case DelimiterTraceRun:
		if e.Outcome == DelimiterTraceAttempted {
			return "run " + e.Processor + " '" + e.Char + "' at " +
				strconv.Itoa(e.Position) + " attempted (run=" + strconv.Itoa(e.RunLength) +
				", remaining=" + strconv.Itoa(e.RemainingLength) + ")"
		}
		return "run " + e.Processor + " '" + e.Char + "' at " +
			strconv.Itoa(e.Position) + " " + e.Outcome.String() + ": " + e.Reason +
			" (run=" + strconv.Itoa(e.RunLength) + ", remaining=" +
			strconv.Itoa(e.RemainingLength) + ")"
	default:
		if e.Outcome == DelimiterTraceConsumed {
			return "pair " + e.OpenerProcessor + "->" + e.CloserProcessor + " '" + e.Char +
				"' at " + strconv.Itoa(e.Position) + ".." + strconv.Itoa(e.CloserPosition) +
				" consumed=" + strconv.Itoa(e.Consumed) + " (remaining=" +
				strconv.Itoa(e.RemainingLength) + "/" +
				strconv.Itoa(e.CloserRemainingLength) + ")"
		}
		return "pair " + e.OpenerProcessor + "->" + e.CloserProcessor + " '" + e.Char +
			"' at " + strconv.Itoa(e.Position) + ".." + strconv.Itoa(e.CloserPosition) +
			" " + e.Outcome.String() + ": " + e.Reason + " (remaining=" +
			strconv.Itoa(e.RemainingLength) + ")"
	}
}

// DelimiterTraceHandler receives run-time [DelimiterTraceEvent]s.
//
// The handler is called while no parser-internal lock is held. It must not
// retain the event beyond the call if mutation is a concern; events are passed
// by value.
type DelimiterTraceHandler func(DelimiterTraceEvent)

// WithDelimiterTrace is a [ParseOption] that enables run-time tracing of the
// delimiter processor selection for a single [Parser.Parse] call.
//
// Tracing is observation-only: it records the same decisions that drive
// parsing, and the produced AST is identical to parsing without tracing.
func WithDelimiterTrace(handler DelimiterTraceHandler) ParseOption {
	return func(c *parseConfig) {
		c.delimiterTrace = handler
	}
}
