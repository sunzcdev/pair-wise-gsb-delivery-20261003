package participle_test

import (
	"testing"

	"github.com/alecthomas/assert/v2"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

// txSink is an application-level object shared by transactional capture
// participants, standing in for state that lives outside the AST.
type txSink struct {
	values    []string // externally visible effects of Capture()
	commits   []string // values committed, in CommitCapture() call order
	rollbacks []string // values rolled back, in RollbackCapture() call order
	captures  int      // total Capture() invocations
}

var txSinkForTest *txSink

func newTxSink(t *testing.T) *txSink {
	t.Helper()
	sink := &txSink{}
	txSinkForTest = sink
	t.Cleanup(func() { txSinkForTest = nil })
	return sink
}

// txValue captures into the shared sink, saving its own undo state as the
// TransactionalCapture protocol requires.
type txValue struct {
	undos   []int      // sink length before each pending Capture() call
	pending [][]string // values of each pending Capture() call
}

func (c *txValue) Capture(values []string) error {
	c.undos = append(c.undos, len(txSinkForTest.values))
	c.pending = append(c.pending, values)
	txSinkForTest.captures++
	txSinkForTest.values = append(txSinkForTest.values, values...)
	return nil
}

// Commits arrive in capture order, so consume the oldest pending record.
func (c *txValue) CommitCapture() {
	txSinkForTest.commits = append(txSinkForTest.commits, c.pending[0]...)
	c.pending = c.pending[1:]
	c.undos = c.undos[1:]
}

// Rollbacks arrive in reverse capture order, so consume the newest record.
func (c *txValue) RollbackCapture() {
	last := len(c.pending) - 1
	txSinkForTest.rollbacks = append(txSinkForTest.rollbacks, c.pending[last]...)
	txSinkForTest.values = txSinkForTest.values[:c.undos[last]]
	c.pending = c.pending[:last]
	c.undos = c.undos[:last]
}

type txItem struct {
	V txValue `@Ident`
}

// txChoice exercises nested choices: each alternative captures via a nested
// struct before either matching a trailing literal or failing.
type txChoice struct {
	A *txItem `  @@ "!"`
	B *txItem `| @@ "?"`
}

func TestTransactionalCaptureNestedChoiceAccepted(t *testing.T) {
	sink := newTxSink(t)
	type grammar struct {
		Choice   *txChoice `  @@ ";"`
		Fallback *txItem   `| @@`
	}
	parser := mustTestParser[grammar](t)
	actual, err := parser.ParseString("", `x ? ;`)
	assert.NoError(t, err)
	assert.True(t, actual.Choice != nil && actual.Choice.B != nil)
	// The rejected inner alternative captured "x" once and was rolled back;
	// only the accepted capture remains and is committed.
	assert.Equal(t, 2, sink.captures)
	assert.Equal(t, []string{"x"}, sink.rollbacks)
	assert.Equal(t, []string{"x"}, sink.values)
	assert.Equal(t, []string{"x"}, sink.commits)
}

func TestTransactionalCaptureNestedChoiceFallback(t *testing.T) {
	sink := newTxSink(t)
	type grammar struct {
		Choice   *txChoice `  @@ ";"`
		Fallback *txItem   `| @@`
	}
	parser := mustTestParser[grammar](t)
	actual, err := parser.ParseString("", `z`)
	assert.NoError(t, err)
	assert.True(t, actual.Fallback != nil)
	// Both inner alternatives captured "z" and were rolled back before the
	// fallback captured it for good.
	assert.Equal(t, 3, sink.captures)
	assert.Equal(t, []string{"z", "z"}, sink.rollbacks)
	assert.Equal(t, []string{"z"}, sink.values)
	assert.Equal(t, []string{"z"}, sink.commits)
}

func TestTransactionalCaptureRepetitionPartialSuccess(t *testing.T) {
	sink := newTxSink(t)
	type grammar struct {
		Items []txItem `( @@ ";" )*`
		Last  txValue  `@Ident`
	}
	parser := mustTestParser[grammar](t)
	_, err := parser.ParseString("", `a ; b ; c`)
	assert.NoError(t, err)
	// The final iteration captured "c" before failing on the missing ";",
	// so it was rolled back; "c" was then captured again by Last.
	assert.Equal(t, 4, sink.captures)
	assert.Equal(t, []string{"c"}, sink.rollbacks)
	assert.Equal(t, []string{"a", "b", "c"}, sink.values)
	assert.Equal(t, []string{"a", "b", "c"}, sink.commits)
}

func TestTransactionalCaptureCommitOrderMatchesGrammar(t *testing.T) {
	sink := newTxSink(t)
	type grammar struct {
		Items  []txItem  `( @@ ";" )*`
		Choice *txChoice `@@`
		Last   *txItem   `";" @@`
	}
	parser := mustTestParser[grammar](t)
	_, err := parser.ParseString("", `a ; b ; x ? ; z`)
	assert.NoError(t, err)
	// The Items group also speculatively captures "x" before failing on the
	// missing ";", and the rejected choice alternative captures it too.
	assert.Equal(t, []string{"x", "x"}, sink.rollbacks)
	assert.Equal(t, []string{"a", "b", "x", "z"}, sink.values)
	assert.Equal(t, []string{"a", "b", "x", "z"}, sink.commits)
}

func TestTransactionalCaptureFailedParseLeavesNoResidue(t *testing.T) {
	sink := newTxSink(t)
	type grammar struct {
		Choice *txChoice `@@`
		End    string    `@";"`
	}
	parser := mustTestParser[grammar](t, participle.UseLookahead(4))
	_, err := parser.ParseString("", `x ? y`)
	assert.Error(t, err)
	// The choice matched and captured "x", but the overall parse failed, so
	// even accepted captures must be rolled back.
	assert.Equal(t, 2, sink.captures)
	assert.Equal(t, []string{"x", "x"}, sink.rollbacks)
	assert.Equal(t, 0, len(sink.values))
	assert.Equal(t, 0, len(sink.commits))
}

// txCustomValue is parsed by a custom parser that may consume input before
// returning NextMatch.
type txCustomValue interface{ txCustom() }

type txCustom struct{ v string }

func (txCustom) txCustom() {}

func parseTxCustom(lex *lexer.PeekingLexer) (txCustomValue, error) {
	token := lex.Peek()
	if token.EOF() || token.Value == "skip" {
		if !token.EOF() {
			lex.Next() // Consume before bailing out.
		}
		return nil, participle.NextMatch
	}
	lex.Next()
	return txCustom{v: token.Value}, nil
}

func TestTransactionalCaptureCustomParserNextMatch(t *testing.T) {
	sink := newTxSink(t)
	type grammar struct {
		Item   *txItem       `  @@`
		Custom txCustomValue `  @@`
		Alt    *txItem       `| @@ "skip" "!"`
	}
	parser := mustTestParser[grammar](t,
		participle.UseLookahead(2),
		participle.ParseTypeWith[txCustomValue](parseTxCustom),
	)
	_, err := parser.ParseString("", `z skip !`)
	assert.NoError(t, err)
	// The first alternative captured "z" before the custom parser consumed
	// "skip" and returned NextMatch; the real backtracking scope is the
	// whole alternative, so the capture must be rolled back.
	assert.Equal(t, 2, sink.captures)
	assert.Equal(t, []string{"z"}, sink.rollbacks)
	assert.Equal(t, []string{"z"}, sink.values)
	assert.Equal(t, []string{"z"}, sink.commits)
}

type txParseable struct{ v string }

func (p *txParseable) Parse(lex *lexer.PeekingLexer) error {
	token := lex.Peek()
	if token.Value != "go" {
		return participle.NextMatch
	}
	lex.Next()
	p.v = token.Value
	return nil
}

func TestTransactionalCaptureParseableNextMatch(t *testing.T) {
	sink := newTxSink(t)
	type grammar struct {
		Item *txItem      `  @@`
		P    *txParseable `  @@`
		Alt  *txItem      `| @@ "!"`
	}
	parser := mustTestParser[grammar](t)
	_, err := parser.ParseString("", `z !`)
	assert.NoError(t, err)
	assert.Equal(t, 2, sink.captures)
	assert.Equal(t, []string{"z"}, sink.rollbacks)
	assert.Equal(t, []string{"z"}, sink.values)
	assert.Equal(t, []string{"z"}, sink.commits)
}

func TestTransactionalCaptureSliceElements(t *testing.T) {
	sink := newTxSink(t)
	type grammar struct {
		Values []txValue `@Ident*`
	}
	parser := mustTestParser[grammar](t)
	_, err := parser.ParseString("", `a b c`)
	assert.NoError(t, err)
	assert.Equal(t, 3, sink.captures)
	assert.Equal(t, 0, len(sink.rollbacks))
	assert.Equal(t, []string{"a", "b", "c"}, sink.values)
	assert.Equal(t, []string{"a", "b", "c"}, sink.commits)
}

func TestTransactionalCaptureLookahead(t *testing.T) {
	sink := newTxSink(t)
	type grammar struct {
		Item  *txItem `(?= @@)`
		Value txValue `@Ident`
	}
	parser := mustTestParser[grammar](t)
	_, err := parser.ParseString("", `x`)
	assert.NoError(t, err)
	// The lookahead captured "x" via the nested struct but its branch is
	// always discarded, so the capture must be rolled back.
	assert.Equal(t, 2, sink.captures)
	assert.Equal(t, []string{"x"}, sink.rollbacks)
	assert.Equal(t, []string{"x"}, sink.values)
	assert.Equal(t, []string{"x"}, sink.commits)
}

var plainSinkForTest []string

// plainValue is a plain Capture implementation with external side effects
// and no transactional guarantees.
type plainValue struct{}

func (c *plainValue) Capture(values []string) error {
	plainSinkForTest = append(plainSinkForTest, values...)
	return nil
}

type plainItem struct {
	V plainValue `@Ident`
}

// Plain Capture implementations are invoked as their match is applied and
// are not rolled back when the enclosing branch is rejected. This test pins
// that compatibility boundary.
func TestCapturePlainSideEffectsAreNotRolledBack(t *testing.T) {
	plainSinkForTest = nil
	t.Cleanup(func() { plainSinkForTest = nil })
	type grammar struct {
		A *plainItem `  @@ "!"`
		B *plainItem `| @@ "?"`
	}
	parser := mustTestParser[grammar](t)
	_, err := parser.ParseString("", `x ?`)
	assert.NoError(t, err)
	// The rejected alternative's capture remains visible to the application.
	assert.Equal(t, []string{"x", "x"}, plainSinkForTest)
}
