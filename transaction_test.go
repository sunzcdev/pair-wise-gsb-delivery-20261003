package participle_test

import (
	"testing"

	"github.com/alecthomas/assert/v2"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

// sharedState is an application object that lives outside any AST node.
// Captures update it, so merely copying a reflect.Value cannot undo a change;
// the participant must save and restore its own state.
var sharedState struct {
	count int
	order []string
}

func resetSharedState() {
	sharedState.count = 0
	sharedState.order = nil
}

// appCapture is a struct-field Capture that counts into the shared object.
type appCapture struct {
	name string
}

func (appCapture) Capture([]string) error { sharedState.count++; return nil }
func (appCapture) CaptureState() any      { return sharedState.count }
func (appCapture) RestoreState(s any)     { sharedState.count = s.(int) }
func (a appCapture) CommitCapture()       { sharedState.order = append(sharedState.order, a.name) }

// accumulator is a self-contained TransactionalCapture that deep-copies its
// own state for save/restore.
type accumulator struct {
	values []string
}

type accumulatorState struct{ values []string }

func (a *accumulator) Capture(values []string) error {
	a.values = append(a.values, values...)
	return nil
}
func (a *accumulator) CaptureState() any {
	saved := make([]string, len(a.values))
	copy(saved, a.values)
	return accumulatorState{values: saved}
}
func (a *accumulator) RestoreState(s any) {
	st := s.(accumulatorState)
	a.values = append([]string(nil), st.values...)
}
func (a *accumulator) CommitCapture() {}

// plainCapture implements only Capture, keeping the original eager behaviour.
type plainCapture struct{ value string }

func (p *plainCapture) Capture(values []string) error { p.value += values[0]; return nil }

// TestTransactionalCaptureRejectedAlternativeZeroResidue verifies a capture
// executed inside a rejected outer alternative leaves no residue on the shared
// application object, while the winning alternative commits.
func TestTransactionalCaptureRejectedAlternativeZeroResidue(t *testing.T) {
	resetSharedState()
	type inner struct {
		App appCapture `@Ident`
	}
	type grammar struct {
		X inner  `  @@ "z"`
		Y string `| @Ident`
	}
	p := participle.MustBuild[grammar](participle.UseLookahead(5))
	g, err := p.ParseString("", "w")
	assert.NoError(t, err)
	assert.Equal(t, "w", g.Y)
	assert.Equal(t, 0, sharedState.count, "rejected branch left residue")
}

// TestTransactionalCaptureNestedAlternatives checks the real backtracking scope
// through nested disjunctions: losing inner alternatives roll back, the winning
// inner alternative commits once, and outer fall-through is clean.
func TestTransactionalCaptureNestedAlternatives(t *testing.T) {
	resetSharedState()
	type grammar struct {
		A appCapture `  "x" ( "y" @Ident | "z" @Ident )`
		B appCapture `| "q" @Ident`
	}
	p := participle.MustBuild[grammar](participle.UseLookahead(5))

	_, err := p.ParseString("", "x z keep")
	assert.NoError(t, err)
	assert.Equal(t, 1, sharedState.count)

	resetSharedState()
	_, err = p.ParseString("", "q keep2")
	assert.NoError(t, err)
	assert.Equal(t, 1, sharedState.count, "failed inner alternatives leaked")
}

// TestTransactionalCaptureRepetitionPartialSuccess verifies successful
// repetitions are retained while a trailing iteration that captured before
// failing to complete is undone.
func TestTransactionalCaptureRepetitionPartialSuccess(t *testing.T) {
	resetSharedState()
	type grammar struct {
		Items appCapture `{ @Ident "," }`
		Last  appCapture `@Ident`
	}
	p := participle.MustBuild[grammar](participle.UseLookahead(5))
	// Iterations capture a, and b,; a third iteration captures c then fails on
	// the missing comma, so its capture is rolled back; c is then captured by
	// Last. With rollback the count is 3, without it the abandoned iteration
	// would leave 4.
	_, err := p.ParseString("", "a , b , c")
	assert.NoError(t, err)
	assert.Equal(t, 3, sharedState.count)
}

// TestTransactionalCaptureAccumulatorRollback ensures a self-owned accumulator
// field is rolled back in a losing alternative and populated by the winner.
func TestTransactionalCaptureAccumulatorRollback(t *testing.T) {
	type grammar struct {
		Lose accumulator `  "x" @Ident`
		Win  accumulator `| @Ident`
	}
	p := participle.MustBuild[grammar](participle.UseLookahead(5))
	g, err := p.ParseString("", "value")
	assert.NoError(t, err)
	assert.Equal(t, []string(nil), g.Lose.values)
	assert.Equal(t, []string{"value"}, g.Win.values)
}

// sharedCounter is a TransactionalParticipant mutated from a custom Parseable.
type sharedCounter struct{ n int }

var parseableShared = &sharedCounter{}

func (s *sharedCounter) CaptureState() any  { return s.n }
func (s *sharedCounter) RestoreState(v any) { s.n = v.(int) }
func (s *sharedCounter) CommitCapture()     {}

// backtrackingParseable consumes an Ident and mutates the shared participant,
// then returns NextMatch unless the following token equals want, forcing the
// enclosing alternative to backtrack.
type backtrackingParseable struct {
	v string
}

func (c *backtrackingParseable) Parse(lex *lexer.PeekingLexer) error {
	tok := lex.Peek()
	if tok.Type == lexer.EOF {
		return participle.NextMatch
	}
	tx := participle.TransactionFor(lex)
	tx.Enlist(parseableShared)
	parseableShared.n++
	lex.Next()
	if lex.Peek().Value != "!" {
		return participle.NextMatch
	}
	c.v = tok.Value
	return nil
}

// TestCustomParseableNextMatchRollback verifies a custom Parseable that mutates
// shared state and then returns NextMatch is rolled back with zero residue.
func TestCustomParseableNextMatchRollback(t *testing.T) {
	parseableShared.n = 0
	type grammar struct {
		A *backtrackingParseable `  @@`
		B string                 `| @Ident`
	}
	p := participle.MustBuild[grammar](participle.UseLookahead(5))
	// "nope" is consumed and shared mutated, but the required "!" is absent, so
	// the custom production returns NextMatch and the @Ident alternative wins.
	g, err := p.ParseString("", "nope")
	assert.NoError(t, err)
	assert.Equal(t, "nope", g.B)
	assert.Equal(t, 0, parseableShared.n, "custom Parseable NextMatch left residue")
}

// TestPlainCaptureCompatibilityBoundary confirms a type implementing only
// Capture continues to work eagerly without opting into transactions.
func TestPlainCaptureCompatibilityBoundary(t *testing.T) {
	type grammar struct {
		V plainCapture `@Ident`
	}
	p := participle.MustBuild[grammar]()
	g, err := p.ParseString("", "hello")
	assert.NoError(t, err)
	assert.Equal(t, "hello", g.V.value)
}

var commitLog []string

// orderMarker is a TransactionalCapture whose distinct named subtypes each
// record a fixed label on commit. The label is constant per type because the
// parser instantiates fresh zero-value field receivers.
type orderMarker struct{}

func (orderMarker) Capture([]string) error { return nil }
func (orderMarker) CaptureState() any      { return nil }
func (orderMarker) RestoreState(any)       {}

type betaMarker struct{ orderMarker }
type alphaMarker struct{ orderMarker }
type gammaMarker struct{ orderMarker }

func (betaMarker) CommitCapture()  { commitLog = append(commitLog, "Beta") }
func (alphaMarker) CommitCapture() { commitLog = append(commitLog, "Alpha") }
func (gammaMarker) CommitCapture() { commitLog = append(commitLog, "Gamma") }

// TestCommitOrderIsSyntactic verifies CommitCapture runs in final syntactic
// order and that participants from rejected branches never commit.
func TestCommitOrderIsSyntactic(t *testing.T) {
	commitLog = nil
	type grammar struct {
		// First alternative captures BETA then requires a missing "z", so it
		// backtracks: BETA must be rolled back and never committed.
		Beta  betaMarker  `  @Ident "z"`
		Alpha alphaMarker `| @Ident`
		Gamma gammaMarker `  @Ident`
	}
	p := participle.MustBuild[grammar](participle.UseLookahead(5))
	_, err := p.ParseString("", "x y")
	assert.NoError(t, err)
	assert.Equal(t, []string{"Alpha", "Gamma"}, commitLog)
}

// TestTransactionalCaptureRolledBackOnOverallFailure verifies that when the
// whole parse fails after captures have been eagerly applied inside a
// deepening branch, every participant is restored to its pre-parse state: zero
// residue even on a hard error.
func TestTransactionalCaptureRolledBackOnOverallFailure(t *testing.T) {
	resetSharedState()
	type grammar struct {
		A appCapture `@Ident`
		B appCapture `@Ident`
		C appCapture `@Int`
	}
	p := participle.MustBuild[grammar](participle.UseLookahead(5))
	_, err := p.ParseString("", "a b notanint")
	assert.Error(t, err)
	assert.Equal(t, 0, sharedState.count, "failed parse left residue")
	assert.Equal(t, 0, len(sharedState.order), "failed parse must not commit")
}

// TestTransactionalCaptureSharedAcrossAcceptedIterations verifies a shared
// participant updated by several successful repetitions retains every accepted
// update, while an iteration that captures and then fails is undone - the
// accepted work is not collateral damage of the rollback.
func TestTransactionalCaptureSharedAcrossAcceptedIterations(t *testing.T) {
	resetSharedState()
	type grammar struct {
		Items appCapture `{ @Ident "," }`
		Last  appCapture `@Ident`
	}
	p := participle.MustBuild[grammar](participle.UseLookahead(5))
	// Two full "x ," iterations commit, then "c" captures as a third iteration
	// but finds no comma and is rolled back; "c" is finally captured by Last.
	_, err := p.ParseString("", "a , b , c")
	assert.NoError(t, err)
	assert.Equal(t, 3, sharedState.count, "accepted iterations must survive")
	// Items is one participant captured three times and Last another: each
	// distinct participant commits exactly once.
	assert.Equal(t, 2, len(sharedState.order), "each participant commits once")
}
