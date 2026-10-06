package participle

import (
	"github.com/alecthomas/participle/v2/lexer"
)

// Capture can be implemented by fields in order to transform captured tokens into field values.
type Capture interface {
	Capture(values []string) error
}

// TransactionalCapture can optionally be implemented by types that already
// implement Capture, and whose Capture() method mutates state outside the AST
// (eg. shared application objects, registries, counters).
//
// Participle parses speculatively: a capture may be applied while exploring a
// branch of the grammar that is later rejected during backtracking. Plain
// Capture implementations are invoked as their match is applied and are NOT
// notified if the enclosing branch is subsequently rejected, so any external
// side effects they perform will leak. This is the compatibility boundary:
// Capture on its own gives no backtracking guarantees.
//
// If a capture target also implements TransactionalCapture, each successful
// Capture() call is tracked as a single capture operation within the branch
// that applied it:
//
//   - If that branch (or any enclosing branch, or the parse as a whole) is
//     rejected, RollbackCapture() is called exactly once for the operation.
//     Rollbacks for a scope occur in reverse order of capture.
//   - If the overall parse succeeds, CommitCapture() is called exactly once
//     for each surviving operation, in the order the captures appear in the
//     input.
//
// The participant is responsible for saving whatever state it needs during
// Capture() (eg. snapshotting a length or keeping an undo record) so that
// RollbackCapture() can fully undo the corresponding Capture() call.
// Participle does not attempt to copy or restore participant state itself.
type TransactionalCapture interface {
	Capture
	// CommitCapture is called once per surviving Capture() call, in grammar
	// order, after the parse has completed successfully.
	CommitCapture()
	// RollbackCapture is called once per Capture() call whose enclosing
	// branch was rejected, in reverse order of capture.
	RollbackCapture()
}

// The Parseable interface can be implemented by any element in the grammar to provide custom parsing.
type Parseable interface {
	// Parse into the receiver.
	//
	// Should return NextMatch if no tokens matched and parsing should continue.
	// Nil should be returned if parsing was successful.
	Parse(lex *lexer.PeekingLexer) error
}
