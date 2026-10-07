package participle

import (
	"reflect"

	"github.com/alecthomas/participle/v2/lexer"
)

// SourceRange is a half-open range [Start, End) of positions in the original
// source text.
//
// Because positions are recorded from the token stream rather than recovered
// by searching for token text, ranges remain accurate even if a Map() mapper
// rewrote token values, or if identical text occurs multiple times in the
// input.
type SourceRange struct {
	Start lexer.Position
	End   lexer.Position
}

// Slice returns the portion of "src" covered by this range.
//
// "src" must be the exact input that was parsed, in which case the result is
// the byte-for-byte source text of the range, including any comments and
// whitespace the lexer elided within it.
func (r SourceRange) Slice(src string) string {
	return src[r.Start.Offset:r.End.Offset]
}

func (r SourceRange) String() string {
	return r.Start.String() + "-" + r.End.String()
}

// A SourceRecord records the source extent of a single successful production.
//
// Records are only published for productions that matched successfully;
// productions that were explored and then abandoned during backtracking do
// not produce records. A production that matched empty input is recorded
// with a zero-width Range (Start == End).
type SourceRecord struct {
	// Type of the grammar struct that was parsed.
	Type reflect.Type
	// Range of source covered by the production.
	Range SourceRange

	start lexer.RawCursor
	end   lexer.RawCursor
}

// A SourceView collects the source extents of successful productions during a
// parse, along with the raw token stream, so that source tooling (eg. editors
// that need to preserve comments and whitespace) can map AST nodes back to
// the original input.
//
// Populate it by passing RecordSource() as a ParseOption. The view is reset
// at the beginning of each parse and only published if the parse succeeds.
type SourceView struct {
	tokens  []lexer.Token
	elide   map[lexer.TokenType]bool
	records []SourceRecord
}

// RecordSource is a ParseOption that records the source extent of each
// successful production into "view".
//
// The view is reset before parsing and only populated if the parse succeeds.
func RecordSource(view *SourceView) ParseOption {
	return func(p *parseContext) {
		*view = SourceView{}
		p.sourceView = view
	}
}

// Records returns the source records of all successful productions, in the
// order they completed parsing (inner productions before their parents).
func (v *SourceView) Records() []SourceRecord {
	return v.records
}

// Tokens returns the entire raw token stream of the parse, including elided
// tokens and the trailing EOF token.
func (v *SourceView) Tokens() []lexer.Token {
	return v.tokens
}

// TokensIn returns the raw tokens spanned by the record, including elided
// tokens.
func (v *SourceView) TokensIn(record SourceRecord) []lexer.Token {
	return v.tokens[record.start:record.end]
}

// Elided returns the elided tokens (eg. comments and whitespace) within the
// record's token interval.
func (v *SourceView) Elided(record SourceRecord) []lexer.Token {
	var out []lexer.Token
	for _, token := range v.TokensIn(record) {
		if v.elide[token.Type] {
			out = append(out, token)
		}
	}
	return out
}

func (v *SourceView) publish(ctx *parseContext) {
	v.tokens = ctx.PeekingLexer.Tokens()
	v.elide = ctx.PeekingLexer.ElideTypes()
	v.records = ctx.sourceRecords
}
