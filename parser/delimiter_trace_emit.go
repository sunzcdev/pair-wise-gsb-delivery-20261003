package parser

// traceDelimiterRun emits a run-selection event. Remaining is the candidate
// run length that is still available at decision time.
func traceDelimiterRun(pc Context, outcome DelimiterTraceOutcome,
	r *DelimiterProcessorRegistration, c byte, position, remaining int, reason string) {
	pctx, ok := pc.(*parseContext)
	if !ok || pctx.delimiterTrace == nil {
		return
	}
	pctx.delimiterTrace(DelimiterTraceEvent{
		Kind:            DelimiterTraceRun,
		Outcome:         outcome,
		Reason:          reason,
		Processor:       r.name,
		Char:            string(c),
		Position:        position,
		CloserPosition:  -1,
		RunLength:       remaining,
		RemainingLength: remaining,

		CloserRemainingLength: -1,
	})
}

func traceDelimiterPushed(pc Context, d *Delimiter) {
	pctx, ok := pc.(*parseContext)
	if !ok || pctx.delimiterTrace == nil {
		return
	}
	pctx.delimiterTrace(DelimiterTraceEvent{
		Kind:                  DelimiterTraceRun,
		Outcome:               DelimiterTraceAttempted,
		Processor:             DelimiterProcessorName(d.Processor),
		Char:                  string(d.Char),
		Position:              d.Value.Index().Start,
		CloserPosition:        -1,
		RunLength:             d.OriginalLength,
		RemainingLength:       d.Length,
		CloserRemainingLength: -1,
	})
}

// TraceDelimiterRunRejected emits a run-selection rejection event for the
// given registration. Built-in inline parsers that perform custom run-level
// checks before calling [ParseDelimiter] use this so that their rejections are
// visible in traces just like adapter-level ones.
//
// The call is a no-op unless tracing is enabled for the parse.
func TraceDelimiterRunRejected(pc Context, r *DelimiterProcessorRegistration, c byte,
	position, runLength int, reason string) {
	traceDelimiterRun(pc, DelimiterTraceRejected, r, c, position, runLength, reason)
}

// DelimiterRejectRunLength is the standard reason for a rejection caused by
// the run length falling outside the processor's declared length window.
func DelimiterRejectRunLength(r *DelimiterProcessorRegistration, n int) string {
	return delimiterRejectLength(r, n)
}

func traceDelimiterPair(pc Context, outcome DelimiterTraceOutcome, opener, closer *Delimiter,
	consumed int, reason string) {
	pctx, ok := pc.(*parseContext)
	if !ok || pctx.delimiterTrace == nil {
		return
	}
	event := DelimiterTraceEvent{
		Kind:            DelimiterTracePair,
		Outcome:         outcome,
		Reason:          reason,
		OpenerProcessor: DelimiterProcessorName(opener.Processor),
		CloserProcessor: DelimiterProcessorName(closer.Processor),
		Char:            string(closer.Char),
		Position:        opener.Value.Index().Start,
		CloserPosition:  closer.Value.Index().Start,
		RunLength:       opener.OriginalLength,
		RemainingLength: opener.Length,

		CloserRemainingLength: -1,
	}
	if outcome == DelimiterTraceConsumed {
		event.RemainingLength = opener.Length
		event.CloserRemainingLength = closer.Length
		event.Consumed = consumed
	}
	pctx.delimiterTrace(event)
}

func traceActive(pc Context) bool {
	pctx, ok := pc.(*parseContext)
	return ok && pctx.delimiterTrace != nil
}

// delimiterRejectLength explains why a candidate declined a run because its
// declared length window does not contain the scanned run length.
func delimiterRejectLength(r *DelimiterProcessorRegistration, n int) string {
	switch {
	case n < r.minLength:
		return "run length below processor minimum"
	case r.maxLength != 0 && n > r.maxLength:
		return "run length above processor maximum"
	default:
		return "run length outside processor window"
	}
}
