package dkim

import "sync"

// DiagnosticStage identifies which processing stage a DiagnosticEvent
// belongs to.
type DiagnosticStage string

const (
	// DiagnosticStageHeaderPick describes the selection of a header field,
	// e.g. which instance of a duplicated header field was chosen.
	DiagnosticStageHeaderPick DiagnosticStage = "header-pick"
	// DiagnosticStageHeader describes the canonicalization of a single
	// header field.
	DiagnosticStageHeader DiagnosticStage = "header"
	// DiagnosticStageBody describes body canonicalization.
	DiagnosticStageBody DiagnosticStage = "body"
)

// DiagnosticEvent describes a single step of DKIM message processing.
//
// Events deliberately never carry message content: they only contain byte
// offsets, byte counts and a short human-readable summary. As a consequence,
// a diagnostic trace cannot be used to reconstruct the message, and a
// signature or body hash cannot be reversed into the canonicalized input
// either. To find out why two ends disagree, the other end's original
// message or its own diagnostic trace is required.
type DiagnosticEvent struct {
	// Stage is the processing stage this event belongs to.
	Stage DiagnosticStage
	// InputOffset is the byte offset of the consumed input within the
	// current stage's input stream (the header block for header stages, the
	// body for the body stage). It is -1 when not applicable.
	InputOffset int
	// InputLength is the number of input bytes consumed by this event.
	InputLength int
	// OutputBytes is the number of canonicalized bytes emitted by this
	// event.
	OutputBytes int
	// Summary is a short human-readable description of the event, e.g.
	// which occurrence of a duplicated header field was selected or how
	// many trailing empty lines were dropped.
	Summary string
}

// Diagnostics configures optional diagnostic callbacks for signing and
// verification. It can be used to troubleshoot canonicalization mismatches
// between a signer and a verifier, e.g. to figure out which occurrence of a
// duplicated header field was selected or how trailing empty lines in the
// body took part in canonicalization.
type Diagnostics struct {
	// Callback is invoked for each diagnostic event. It must not block for
	// long, since it is called while the message is being processed.
	Callback func(DiagnosticEvent)
	// MaxEvents is a strict budget for the total number of events delivered
	// to Callback. Once the budget is exhausted, no further events are
	// emitted. If zero, the number of events is unlimited.
	MaxEvents int
}

// diagnostics is the internal event emitter. It is safe for concurrent use,
// which is required because parallel verification runs one goroutine per
// signature.
type diagnostics struct {
	mu        sync.Mutex
	cb        func(DiagnosticEvent)
	remaining int // -1 means unlimited
}

func newDiagnostics(cfg *Diagnostics) *diagnostics {
	if cfg == nil || cfg.Callback == nil {
		return nil
	}
	remaining := -1
	if cfg.MaxEvents > 0 {
		remaining = cfg.MaxEvents
	}
	return &diagnostics{cb: cfg.Callback, remaining: remaining}
}

func (d *diagnostics) emit(ev DiagnosticEvent) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.remaining == 0 {
		return
	}
	if d.remaining > 0 {
		d.remaining--
	}
	d.cb(ev)
}
