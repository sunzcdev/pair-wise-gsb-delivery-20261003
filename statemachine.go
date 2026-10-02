package milter

import (
	"fmt"
	"log"
)

// mailState describes how far processing of the current message has
// progressed within a milter session. The numeric values double as phase
// numbers: each sequenced command advances the session to the phase with
// the same number.
type mailState int

const (
	// stateIdle means no message is currently being processed: either none
	// was started yet, or the previous one was completed or aborted.
	// Connection and HELO information is retained in this state.
	stateIdle mailState = iota
	stateMail
	stateRcpt
	stateHeader
	stateEOH
	stateBody
)

func (s mailState) String() string {
	switch s {
	case stateIdle:
		return "idle"
	case stateMail:
		return "MAIL"
	case stateRcpt:
		return "RCPT"
	case stateHeader:
		return "HEADER"
	case stateEOH:
		return "EOH"
	case stateBody:
		return "BODY"
	}
	return "unknown"
}

// commandPhase maps sequenced commands to their phase number in the
// MAIL -> RCPT -> HEADER -> EOH -> BODY -> EOB message flow. The second
// return value is false for commands that do not take part in the message
// sequence (e.g. OPTNEG, CONNECT, HELO, MACRO, ABORT, QUIT).
func commandPhase(code Code) (int, bool) {
	switch code {
	case CodeMail:
		return 1, true
	case CodeRcpt:
		return 2, true
	case CodeHeader:
		return 3, true
	case CodeEOH:
		return 4, true
	case CodeBody:
		return 5, true
	case CodeEOB:
		return 6, true
	}
	return 0, false
}

var phaseNames = []string{"", "MAIL", "RCPT", "HEADER", "EOH", "BODY", "EOB"}

// phaseSkippable reports whether the given message phase may be absent
// in a legal message flow. HEADER and BODY may always be absent (a message
// can have no header fields and an empty body), the other phases only if
// they were disabled during protocol negotiation.
func (m *milterSession) phaseSkippable(phase int) bool {
	switch phase {
	case 1:
		return m.protocol&OptNoMailFrom != 0
	case 2:
		return m.protocol&OptNoRcptTo != 0
	case 3:
		return true
	case 4:
		return m.protocol&OptNoEOH != 0
	case 5:
		return true
	}
	return false
}

// protocolErrorf logs a protocol violation and builds a clear error
// response for the MTA. The session is left untouched so it can continue
// with the next command.
func protocolErrorf(format string, args ...interface{}) Response {
	text := fmt.Sprintf(format, args...)
	log.Printf("milter: protocol error: %s", text)
	return NewResponseStr(byte(ActReplyCode), "451 4.7.1 "+text)
}

// checkSequence enforces the legal order of the MAIL, RCPT, HEADER, EOH,
// BODY and EOB commands. It returns nil if the command may be processed
// (advancing the session state as a side effect), or an error response to
// send back to the MTA without invoking any milter callback.
//
// Phases that were disabled during option negotiation (e.g. OptNoHeaders)
// are allowed to be missing, so sessions are not forced into the full
// sequence.
func (m *milterSession) checkSequence(code Code) Response {
	phase, ok := commandPhase(code)
	if !ok {
		return nil
	}

	current := int(m.state)

	// RCPT, HEADER and BODY may be repeated within their own phase.
	if current == phase && (code == CodeRcpt || code == CodeHeader || code == CodeBody) {
		return nil
	}

	// EOB without a message in progress (e.g. a duplicate EOB) is always
	// an error.
	if code == CodeEOB && current == int(stateIdle) {
		return protocolErrorf("unexpected EOB command: no message in progress")
	}

	if current >= phase {
		return protocolErrorf("unexpected %s command while in %s state", phaseNames[phase], m.state)
	}

	// All phases between the current state and the command's phase must
	// have been negotiated away, otherwise the command arrives too early.
	for skipped := current + 1; skipped < phase; skipped++ {
		if !m.phaseSkippable(skipped) {
			return protocolErrorf("%s command received before %s", phaseNames[phase], phaseNames[skipped])
		}
	}

	if code == CodeEOB {
		// Message is complete, the next MAIL starts a fresh one.
		m.state = stateIdle
	} else {
		m.state = mailState(phase)
	}
	return nil
}
