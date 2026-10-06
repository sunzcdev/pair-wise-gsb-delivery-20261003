package participle

import (
	"github.com/alecthomas/participle/v2/lexer"
)

// txFrame is one backtracking scope, opened when a speculative branch starts.
//
// It snapshots each participant the first time that participant is mutated
// within the branch. The snapshot captures the participant's state before the
// branch, whether or not an ancestor branch already mutated it. On rollback the
// snapshot is restored, undoing precisely what this branch changed; on accept
// the frame is dropped while leaving the live mutations in place.
type txFrame struct {
	// before maps a participant to its state before its first mutation in this
	// branch.
	before map[TransactionalParticipant]any
	// order lists participants in their first-mutation (syntactic) order.
	order []TransactionalParticipant
}

func newTxFrame() *txFrame {
	return &txFrame{before: map[TransactionalParticipant]any{}}
}

// snapshot records p's pre-mutation state the first time p is seen in f.
func (f *txFrame) snapshot(p TransactionalParticipant) {
	if _, seen := f.before[p]; seen {
		return
	}
	f.before[p] = p.CaptureState()
	f.order = append(f.order, p)
}

// rollback restores every participant the branch mutated, in reverse syntactic
// order, to its pre-branch state.
func (f *txFrame) rollback() {
	for i := len(f.order) - 1; i >= 0; i-- {
		p := f.order[i]
		p.RestoreState(f.before[p])
	}
}

// Transaction tracks speculative mutations made by TransactionalCapture fields
// and enlisted TransactionalParticipants over the course of a single parse.
//
// The parser opens a frame for every speculative branch (Begin), rolls it back
// when the branch is rejected (Rollback) and discards it when accepted
// (Accept). At the end of a successful parse the surviving participants are
// committed once, in the order their captures first appeared in the accepted
// syntax (Commit).
//
// Each participant owns the state it saves and restores; the parser never
// copies a reflect.Value to claim deep or shared side effects were undone.
type Transaction struct {
	frames []*txFrame
}

func newTransaction() *Transaction {
	return &Transaction{
		// A root frame guarantees Begin/Rollback are balanced even for custom
		// parse participants at the top level.
		frames: []*txFrame{newTxFrame()},
	}
}

// Begin opens a new nested backtracking scope.
func (t *Transaction) Begin() {
	if t == nil {
		return
	}
	t.frames = append(t.frames, newTxFrame())
}

// Accept closes the current scope as a successful branch. Its participants are
// folded into the enclosing scope with their pre-branch snapshots preserved,
// and noted globally for commit ordering.
func (t *Transaction) Accept() {
	if t == nil {
		return
	}
	top := t.pop()
	parent := t.frames[len(t.frames)-1]
	for _, p := range top.order {
		if _, seen := parent.before[p]; !seen {
			parent.before[p] = top.before[p]
			parent.order = append(parent.order, p)
		}
	}
}

// Rollback closes the current scope as a rejected branch, restoring every
// participant the branch mutated to its pre-branch state.
func (t *Transaction) Rollback() {
	if t == nil {
		return
	}
	top := t.pop()
	top.rollback()
}

// RollbackAll closes every open scope down to (and including) the root,
// restoring all speculative mutations. Used when the overall parse fails so
// no participant retains a rejected-branch state.
func (t *Transaction) RollbackAll() {
	if t == nil {
		return
	}
	for len(t.frames) > 0 {
		t.pop().rollback()
	}
}

func (t *Transaction) pop() *txFrame {
	last := len(t.frames) - 1
	frame := t.frames[last]
	t.frames = t.frames[:last]
	return frame
}

// changed snapshots p immediately before a capture mutates it, in every open
// frame. Accepted frames fold their snapshots upward, rejected frames restore
// from theirs; the root frame's order becomes the syntactic commit order.
func (t *Transaction) changed(p TransactionalParticipant) {
	if t == nil {
		return
	}
	for _, f := range t.frames {
		f.snapshot(p)
	}
}

// Commit commits every surviving participant exactly once, in the order its
// captures first appeared in the accepted syntax.
func (t *Transaction) Commit() {
	if t == nil {
		return
	}
	root := t.frames[0]
	for _, p := range root.order {
		p.CommitCapture()
	}
}

// txHookKey is the hook key under which the active Transaction is carried on a
// PeekingLexer.
type txHookKey struct{}

// withTransaction installs tx on lex and returns the lexer for chaining.
func withTransaction(lex *lexer.PeekingLexer, tx *Transaction) *lexer.PeekingLexer {
	lex.SetHook(txHookKey{}, tx)
	return lex
}

// TransactionFor returns the active Transaction associated with a custom parse
// function's PeekingLexer, or nil if the lexer carries no transaction.
//
// Custom Parseable implementations and ParseTypeWith functions that mutate
// shared state should enlist that state with the returned Transaction before
// mutating it, so changes made in a branch that later returns NextMatch are
// undone.
func TransactionFor(lex *lexer.PeekingLexer) *Transaction {
	if lex == nil {
		return nil
	}
	tx, _ := lex.Hook(txHookKey{}).(*Transaction)
	return tx
}

// Enlist snapshots p immediately before a custom parse function mutates it.
// Call this before mutating shared state. If t is nil (no transaction is
// active) this is a no-op, preserving ordinary behaviour.
func (t *Transaction) Enlist(p TransactionalParticipant) {
	if t == nil {
		return
	}
	t.changed(p)
}
