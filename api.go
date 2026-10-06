package participle

import (
	"github.com/alecthomas/participle/v2/lexer"
)

// Capture can be implemented by fields in order to transform captured tokens into field values.
type Capture interface {
	Capture(values []string) error
}

// TransactionalCapture is an optional extension of Capture implemented by
// participants whose Capture method mutates state that must not survive a
// backtracked parse branch.
//
// A plain Capture is applied eagerly, the moment the enclosing production is
// recognised. That is efficient and works for self-contained values, but it
// means a Capture that ran inside a speculative branch that is ultimately
// rejected can leave mutations behind, particularly when the receiver is an
// object shared with state outside the branch.
//
// Implementing TransactionalCapture opts into the transactional capture
// protocol. The parser then treats every capture as part of the enclosing
// backtracking scope:
//
//   - CaptureState is invoked before a capture is applied to a receiver so the
//     participant can save its own state. Participants, not the parser, own
//     their saved state; copying a reflect.Value is not sufficient to undo deep
//     or shared side effects.
//   - RestoreState is invoked when the enclosing branch is rejected, undoing
//     the capture.
//   - CommitCapture is invoked once, in final syntactic order, when the whole
//     parse succeeds.
//
// A type that only implements Capture keeps the original eager behaviour and
// receives no rollback guarantees; that is the documented compatibility
// boundary.
type TransactionalCapture interface {
	Capture

	// CaptureState returns an opaque snapshot of the participant's current
	// state. The returned value is passed back to RestoreState unchanged.
	CaptureState() any

	// RestoreState restores the participant to the state returned by a previous
	// CaptureState call.
	RestoreState(state any)

	// CommitCapture is invoked once after the participant's captures have been
	// accepted by the overall parse. It is a notification; participants are
	// committed in the order in which their captures first occur in the accepted
	// syntax.
	CommitCapture()
}

// TransactionalParticipant is a participant in the transactional capture
// protocol that is not driven through a struct field Capture (for example, a
// shared counter mutated by a custom Parseable).
//
// Obtain the active Transaction from the PeekingLexer passed to a custom parse
// function via TransactionFor, and enlist shared state with Enlist before
// mutating it. The parser restores enlisted participants when a branch
// backtracks and commits them on a successful parse.
type TransactionalParticipant interface {
	CaptureState() any
	RestoreState(state any)
	CommitCapture()
}

// The Parseable interface can be implemented by any element in the grammar to provide custom parsing.
type Parseable interface {
	// Parse into the receiver.
	//
	// Should return NextMatch if no tokens matched and parsing should continue.
	// Nil should be returned if parsing was successful.
	Parse(lex *lexer.PeekingLexer) error
}
